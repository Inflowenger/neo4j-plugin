package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/neo4j-plugin/internal/neo4j"
)

// metaTimeout keeps meta RPCs snappy: they answer a form a user is waiting on,
// so a slow server should fail fast rather than hang the drawer.
const metaTimeout = 15 * time.Second

// Metas are synchronous helper RPCs served outside the job lifecycle: the
// settings form's connection test and database list, and the pickers behind the
// label and relationship-type fields.
func (r *Registry) Metas() []sdkv1.Meta {
	return []sdkv1.Meta{
		{Method: "neo4j.meta.ping.check", RequestHandler: r.metaPingCheck},
		{Method: "neo4j.meta.database.pick", RequestHandler: r.metaDatabasePick},
		{Method: "neo4j.meta.label.pick", RequestHandler: r.metaLabelPick},
		{Method: "neo4j.meta.reltype.pick", RequestHandler: r.metaRelTypePick},
	}
}

// metaLabelPick backs every Label button in the plugin, and on the upsert form
// it backs both of that form's steps, so the dialog needs no throwaway field
// just to anchor a second button. It reads what the Label box holds (posted as
// `value`) and branches:
//
//   - empty box → list the labels in use, rebuilding Label as a drop-down;
//   - a label set, on the upsert form → load the properties nodes with that
//     label carry, and re-render with a field per property (see
//     propertiesEnvelope).
//
// On the other forms there is no second step, so a label already set just
// re-lists. Either branch is a whole-form answer, not a patch, because the
// options and the property fields did not exist when the plugin was compiled.
func (r *Registry) metaLabelPick(req sdkv1.Request) any {
	call := decodeMeta[map[string]any](req.Data)
	target := metaTarget(call, "label")

	client, err := r.clientForMeta(call)
	if err != nil {
		return formkit.Failure("%s", err).About(target).Patch(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()

	label := strings.TrimSpace(metaString(call, "value"))
	if label == "" {
		label = strings.TrimSpace(metaString(call, target))
	}

	// A label already chosen on the upsert form: load its properties and
	// re-render with a field for each. A label matching nothing falls through to
	// the label list below.
	if label != "" && metaString(call, "form") == "neo4j.node.upsert" {
		keys, err := client.PropertyKeys(ctx, label)
		if err != nil {
			return formkit.Failure("cannot read the properties of :%s — %s", label, err).About(target).Patch(nil)
		}
		if len(keys) > 0 {
			return propertiesEnvelope(keys, formkit.FormData(call))
		}
		// A label with no nodes yet is a normal state — the upsert is about to
		// make the first one — so say so and leave the JSON field as the way in.
		return formkit.Warning("no nodes are labelled :%s yet, so there are no properties to load — paste a JSON object into \"JSON properties\" instead", label).About(target).Patch(nil)
	}

	labels, err := client.Labels(ctx)
	if err != nil {
		return formkit.Failure("cannot list labels: %s", err).About(target).Patch(nil)
	}
	if len(labels) == 0 {
		return formkit.Warning("no labels on %s — the graph is empty, so type the label you are about to create", client.ConnInfo()).About(target).Patch(nil)
	}

	heading := formkit.Success("%d labels on %s — pick one", len(labels), client.ConnInfo()).About(target)
	if label != "" {
		heading = formkit.Warning("no label :%s on %s — pick one below", label, client.ConnInfo()).About(target)
	} else if metaString(call, "form") == "neo4j.node.upsert" {
		heading = formkit.Success("%d labels on %s — pick one, then press ↻ again to load its properties", len(labels), client.ConnInfo()).About(target)
	}
	return r.chooseFor(call, target, options(labels), heading)
}

// metaRelTypePick lists the relationship types in use, for the link action's
// Type field.
func (r *Registry) metaRelTypePick(req sdkv1.Request) any {
	call := decodeMeta[map[string]any](req.Data)
	target := metaTarget(call, "type")

	client, err := r.clientForMeta(call)
	if err != nil {
		return formkit.Failure("%s", err).About(target).Patch(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()

	types, err := client.RelationshipTypes(ctx)
	if err != nil {
		return formkit.Failure("cannot list relationship types: %s", err).About(target).Patch(nil)
	}
	if len(types) == 0 {
		return formkit.Warning("no relationship types on %s yet — type the one you are about to create, e.g. WORKS_AT", client.ConnInfo()).About(target).Patch(nil)
	}
	heading := formkit.Success("%d relationship types on %s — pick one", len(types), client.ConnInfo()).About(target)
	return r.chooseFor(call, target, options(types), heading)
}

// metaDatabasePick fills the settings form's Database field.
//
// It answers with a patch rather than a rebuilt dialog: the settings editor is
// the host's own, not one of this plugin's action forms, so there is no form of
// ours to re-render. One user database is filled in; several are listed to type
// from.
func (r *Registry) metaDatabasePick(req sdkv1.Request) any {
	call := decodeMeta[map[string]any](req.Data)

	client, err := r.clientForMeta(call)
	if err != nil {
		return formkit.Failure("%s", err).About("database").Patch(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()

	databases, err := client.Databases(ctx)
	if err != nil {
		// SHOW DATABASES needs a 4.0+ server and the privilege to run it.
		// Neither is required to use this plugin, so a failure here is advice,
		// not an error.
		return formkit.Warning("cannot list databases (%s) — leave this empty for the server's default, or type the name", err).About("database").Patch(nil)
	}

	user := make([]string, 0, len(databases))
	for _, name := range databases {
		if name != "system" {
			user = append(user, name)
		}
	}
	switch {
	case len(user) == 1:
		return formkit.Success("One database on this server: %s.", user[0]).About("database").
			Patch(map[string]any{"database": user[0]})
	case len(user) == 0:
		return formkit.Warning("the server reports no database other than `system` — leave this empty for the default").About("database").Patch(nil)
	default:
		return formkit.Info("%d databases — type the one to use:\n%s", len(user), formkit.Lines(options(user))).About("database").Patch(nil)
	}
}

// metaPingCheck backs the settings form's connection test and its submit
// validation. It opens the connection being edited and verifies it, so a wrong
// URI or password is reported in the dialog instead of failing every node later.
func (r *Registry) metaPingCheck(req sdkv1.Request) any {
	call := decodeMeta[map[string]any](req.Data)

	client, err := r.clientForMeta(call)
	if err != nil {
		return formkit.Failure("%s", err).About("test").Patch(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()

	info, err := client.Ping(ctx)
	if err != nil {
		return formkit.Failure("cannot reach %s: %s", client.ConnInfo(), err).About("test").Patch(nil)
	}
	// A patch with nothing but a message: the test writes no value into the
	// profile, and saying what it found is the whole point of the button.
	return formkit.Success("Connected to %s%s.", describeServer(info), databaseSuffix(info)).About("test").Patch(nil)
}

func describeServer(info neo4j.ServerInfo) string {
	if info.Agent != "" {
		return fmt.Sprintf("%s (%s)", info.Agent, info.Address)
	}
	return info.Address
}

func databaseSuffix(info neo4j.ServerInfo) string {
	if info.Database == "" {
		return ", using the server's default database"
	}
	return ", database " + info.Database
}

// ---------------------------------------------------------- form rebuilds --

// chooseFor rebuilds whichever action form the button belongs to, turning the
// target field into a drop-down of what was found. The button says which form
// that is (formkit's Field.Picks, posted as `form`); when it names one this
// plugin does not serve, Choose falls back to listing the candidates as text.
func (r *Registry) chooseFor(call map[string]any, target string, opts []formkit.Option, heading formkit.Notification) any {
	form := formByMethod[metaString(call, "form")]
	return formkit.Choose(form, target, opts, formkit.FormData(call), heading)
}

// propertiesEnvelope rebuilds the upsert dialog with a field per property. It
// starts from the form's own schema and layout (a fresh copy from the builder),
// adds a `values` object whose properties are the ones found on the label, and
// drops a control for each into a "Properties" group placed just above the JSON
// field. The returned map carries only envelope keys, which is what tells the
// host to re-render the dialog rather than patch a single field.
func propertiesEnvelope(keys []string, data map[string]any) map[string]any {
	schema := nodeUpsertFormDef.SchemaMap()
	ui := nodeUpsertFormDef.UIMap()

	valueProps := make(map[string]any, len(keys))
	controls := make([]any, 0, len(keys))
	for _, key := range keys {
		valueProps[key] = map[string]any{
			"type":        "string",
			"title":       key,
			"description": "a literal or a {{$.path}} token; blank to leave the property alone",
		}
		controls = append(controls, map[string]any{
			"type":  "Control",
			"scope": "#/properties/values/properties/" + key,
		})
	}

	if props, ok := schema["properties"].(map[string]any); ok {
		props["values"] = map[string]any{
			"type":       "object",
			"title":      "Properties",
			"properties": valueProps,
		}
	}

	group := map[string]any{"type": "Group", "label": "Properties", "elements": controls}
	if elements, ok := ui["elements"].([]any); ok {
		ui["elements"] = insertBefore(elements, "#/properties/record", group)
	}

	return map[string]any{
		"schema":         schema,
		"uischema":       ui,
		"data":           data,
		formkit.NotifKey: formkit.Success("Loaded %d properties — fill the ones you want, and name the identifying ones in Key properties", len(controls)).About("label"),
	}
}

// insertBefore splices an element into a UI Schema's element list just ahead of
// the control at the given scope, appending it if that control is not found.
func insertBefore(elements []any, scope string, element any) []any {
	out := make([]any, 0, len(elements)+1)
	inserted := false
	for _, el := range elements {
		if !inserted {
			if m, ok := el.(map[string]any); ok {
				if s, _ := m["scope"].(string); s == scope {
					out = append(out, element)
					inserted = true
				}
			}
		}
		out = append(out, el)
	}
	if !inserted {
		out = append(out, element)
	}
	return out
}

// ---------------------------------------------------------------- settings --

// Settings declares what a Neo4j settings profile must contain. The platform
// renders this form when the plugin is set up, stores the answers as a named,
// reusable profile, and ships the values back with every call as body.settings.
//
// The submit handler is a validator, not a store: it checks the values against
// the live server so a wrong connection is reported in the dialog.
func (r *Registry) Settings() *sdkv1.Settings {
	return &sdkv1.Settings{
		FormBuilder: settingsForm,
		SubmitHandler: func(req sdkv1.Request) sdkv1.Response {
			submitted := decodeMeta[map[string]any](req.Data)
			if nested, ok := submitted["settings"].(map[string]any); ok {
				submitted = nested
			}

			client, err := r.pool.Client(submitted)
			if err != nil {
				return sdkv1.Response{Error: err.Error()}
			}

			ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
			defer cancel()

			info, err := client.Ping(ctx)
			if err != nil {
				return sdkv1.Response{Error: "connection test failed: " + err.Error()}
			}
			return sdkv1.Response{Data: map[string]any{
				"ok":          true,
				"connectedTo": client.ConnInfo(),
				"server":      info,
			}}
		},
	}
}

// SettingsForm exposes the same form for PluginIntro.Settings — the document
// the platform's plugin set-up dialog reads to build the profile editor.
func (r *Registry) SettingsForm() *sdkv1.FormBuilder {
	settings := r.Settings().FormBuilder
	return &settings
}

// ---------------------------------------------------------------- plumbing --

// clientForMeta resolves the connection a meta call carries.
//
// The values arrive one of two ways: on an action's drawer the bound profile is
// under "settings"; the plugin set-up dialog has no bound profile and sends the
// values being edited at the top level, alongside the keys the host adds.
func (r *Registry) clientForMeta(call map[string]any) (*neo4j.Client, error) {
	conn, _ := call["settings"].(map[string]any)
	if len(conn) == 0 {
		conn = make(map[string]any, len(call))
		for key, value := range call {
			if !hostKeys[key] {
				conn[key] = value
			}
		}
	}
	return r.pool.Client(conn)
}

// hostKeys are the keys the host adds to a meta call's body, which are not part
// of the settings profile being edited.
var hostKeys = map[string]bool{"settings": true, "value": true, "targetField": true, "form": true}

// metaString reads a string field from a meta call's flat body, or "" if it is
// absent or not a string.
func metaString(call map[string]any, key string) string {
	if v, ok := call[key].(string); ok {
		return v
	}
	return ""
}

// metaTarget is the field the pressed button writes into. The host posts it as
// `targetField` (formkit puts it there); fallback is the field the caller
// expects, so a handler always has somewhere to put its message.
func metaTarget(call map[string]any, fallback string) string {
	if target := metaString(call, "targetField"); target != "" {
		return target
	}
	return fallback
}

// options renders a list of names as picker options.
func options(names []string) []formkit.Option {
	out := make([]formkit.Option, 0, len(names))
	for _, name := range names {
		out = append(out, formkit.Option{Value: name, Label: name})
	}
	return out
}

// decodeMeta reads a meta RPC's arguments. Meta calls come from the form
// renderer rather than the job pipeline, so the payload may or may not be
// wrapped in the {_registry, body} envelope — try both, and treat anything
// unreadable as "no arguments" rather than an error.
func decodeMeta[T any](data []byte) T {
	var out T
	if len(bytes.TrimSpace(data)) == 0 {
		return out
	}

	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Body) > 0 {
		if json.Unmarshal(envelope.Body, &out) == nil {
			return out
		}
	}

	_ = json.Unmarshal(data, &out)
	return out
}
