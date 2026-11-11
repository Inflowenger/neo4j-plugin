package actions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// ------------------------------------------------------------------ forms --

// Every form has to be a pair of parseable documents, or the drawer silently
// fails to open.
func TestFormsParse(t *testing.T) {
	registry := New()
	forms := map[string]sdkv1.FormBuilder{"settings": *registry.SettingsForm()}
	for _, action := range registry.All() {
		forms[action.Method] = action.Form
	}

	for name, form := range forms {
		if strings.TrimSpace(form.Jsonschema) == "" {
			t.Errorf("%s: no JSON schema", name)
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(form.Jsonschema), &schema); err != nil {
			t.Errorf("%s: schema does not parse: %v", name, err)
		}
		var ui map[string]any
		if err := json.Unmarshal([]byte(form.Jsonui), &ui); err != nil {
			t.Errorf("%s: UI schema does not parse: %v", name, err)
		}
		if ui["type"] != "VerticalLayout" {
			t.Errorf("%s: UI schema root is %v", name, ui["type"])
		}
	}
}

// A connection value on an action form would be a second, staler copy of
// someone's credentials — the platform supplies them as body.settings.
func TestNoConnectionFieldsOnActionForms(t *testing.T) {
	forbidden := []string{"settings", "uri", "url", "host", "port", "user", "username", "password", "token", "secret", "database", "auth", "encryption"}

	for _, action := range New().All() {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal([]byte(action.Form.Jsonschema), &schema); err != nil {
			t.Fatalf("%s: %v", action.Method, err)
		}
		for property := range schema.Properties {
			for _, bad := range forbidden {
				if strings.EqualFold(property, bad) {
					t.Errorf("%s declares a connection field %q: it belongs on the settings profile", action.Method, property)
				}
			}
		}
	}
}

// Every control in a UI schema must point at a property that exists, or the
// field just does not render.
func TestUIScopesResolve(t *testing.T) {
	registry := New()
	forms := map[string]sdkv1.FormBuilder{"settings": *registry.SettingsForm()}
	for _, action := range registry.All() {
		forms[action.Method] = action.Form
	}

	for name, form := range forms {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal([]byte(form.Jsonschema), &schema); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, scope := range scopesOf(t, form.Jsonui) {
			property := strings.TrimPrefix(scope, "#/properties/")
			if _, ok := schema.Properties[property]; !ok {
				t.Errorf("%s: control points at #/properties/%s, which the schema does not declare", name, property)
			}
		}
	}
}

// scopesOf walks a UI schema (groups included) and returns every control scope.
func scopesOf(t *testing.T, uiJSON string) []string {
	t.Helper()
	var ui map[string]any
	if err := json.Unmarshal([]byte(uiJSON), &ui); err != nil {
		t.Fatalf("UI schema does not parse: %v", err)
	}
	var walk func(any) []string
	walk = func(node any) []string {
		switch typed := node.(type) {
		case map[string]any:
			if scope, ok := typed["scope"].(string); ok {
				return []string{scope}
			}
			return walk(typed["elements"])
		case []any:
			out := make([]string, 0, len(typed))
			for _, child := range typed {
				out = append(out, walk(child)...)
			}
			return out
		default:
			return nil
		}
	}
	return walk(ui["elements"])
}

// Required fields are the contract the handlers rely on.
func TestRequiredFields(t *testing.T) {
	want := map[string][]string{
		"neo4j.query":               {"cypher"},
		"neo4j.execute":             {"cypher"},
		"neo4j.node.upsert":         {"label"},
		"neo4j.relationship.upsert": {"type", "fromLabel", "fromMatch", "toLabel", "toMatch"},
		"neo4j.constraint.create":   {"label", "properties"},
	}

	for _, action := range New().All() {
		required, ok := want[action.Method]
		if !ok {
			continue
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal([]byte(action.Form.Jsonschema), &schema); err != nil {
			t.Fatalf("%s: %v", action.Method, err)
		}
		for _, field := range required {
			found := false
			for _, declared := range schema.Required {
				if declared == field {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: %q should be required, required = %v", action.Method, field, schema.Required)
			}
		}
	}
}

// ---------------------------------------------------------------- registry --

func TestActionsAreWellFormed(t *testing.T) {
	registry := New()
	all := registry.All()
	if len(all) != 6 {
		t.Fatalf("got %d actions, want 6", len(all))
	}

	seen := map[string]bool{}
	for _, action := range all {
		switch {
		case action.Method == "":
			t.Error("an action has no method")
		case seen[action.Method]:
			t.Errorf("duplicate method %q — a method is the wire contract", action.Method)
		case !strings.HasPrefix(action.Method, "neo4j."):
			t.Errorf("%q is outside the plugin's namespace", action.Method)
		case action.RequestHandler == nil:
			t.Errorf("%s has no handler: the node would hang", action.Method)
		case action.Title == "":
			t.Errorf("%s has no title", action.Method)
		case action.Description == "":
			t.Errorf("%s has no description", action.Method)
		}
		seen[action.Method] = true
	}

	// Every method a form's button calls must be registered, or the button
	// silently does nothing.
	metas := map[string]bool{}
	for _, meta := range registry.Metas() {
		if meta.RequestHandler == nil {
			t.Errorf("%s has no handler", meta.Method)
		}
		metas[meta.Method] = true
	}
	for name, form := range formsToCheck(registry) {
		for _, called := range metaCallsOf(t, form.Jsonui) {
			if !metas[called] {
				t.Errorf("%s: a button calls %q, which is not registered", name, called)
			}
		}
		if form.SubmitTo != "" && !metas[form.SubmitTo] {
			t.Errorf("%s: submit_to names %q, which is not registered", name, form.SubmitTo)
		}
	}

	// A form that names the action whose dialog it rebuilds (formkit's Picks)
	// must name one this plugin actually serves.
	for name, form := range formsToCheck(registry) {
		for _, picked := range pickedFormsOf(t, form.Jsonui) {
			if _, ok := formByMethod[picked]; !ok {
				t.Errorf("%s: a button rebuilds %q, which formByMethod does not hold", name, picked)
			}
		}
	}
}

func formsToCheck(registry *Registry) map[string]sdkv1.FormBuilder {
	forms := map[string]sdkv1.FormBuilder{"settings": *registry.SettingsForm()}
	for _, action := range registry.All() {
		forms[action.Method] = action.Form
	}
	return forms
}

// metaCallsOf finds every meta method a form's buttons call.
func metaCallsOf(t *testing.T, uiJSON string) []string {
	return uiStrings(t, uiJSON, "fn")
}

// pickedFormsOf finds every action form a button asks to have rebuilt.
func pickedFormsOf(t *testing.T, uiJSON string) []string {
	return uiStrings(t, uiJSON, "form")
}

// uiStrings collects one key's string values from anywhere in a UI schema.
func uiStrings(t *testing.T, uiJSON, key string) []string {
	t.Helper()
	var ui any
	if err := json.Unmarshal([]byte(uiJSON), &ui); err != nil {
		t.Fatalf("UI schema does not parse: %v", err)
	}
	out := make([]string, 0, 4)
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for k, v := range typed {
				if k == key {
					if text, ok := v.(string); ok && text != "" {
						out = append(out, text)
					}
					continue
				}
				walk(v)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(ui)
	return out
}

// ---------------------------------------------------------------- envelope --

// The runtime delivers {"_registry":…, "body":…}; the input struct's JSON tags
// have to match what the form sends.
func TestRequestEnvelopeDecodes(t *testing.T) {
	payload := []byte(`{
      "_registry": {"jobId": "job-1", "doneAt": 0},
      "body": {
        "cypher": "MATCH (n:Person {id: $id}) RETURN n",
        "params": "{\"id\": 42}",
        "limit": "25",
        "settings": {"uri": "neo4j://localhost:7687", "password": "p"}
      }
    }`)

	req, err := sdkv1.CastRequestTo[queryInput](payload)
	if err != nil {
		t.Fatalf("CastRequestTo: %v", err)
	}
	if req.Body.Cypher == "" || req.Body.Limit != "25" {
		t.Errorf("body decoded as %+v", req.Body)
	}
	if req.Registry["jobId"] != "job-1" {
		t.Errorf("registry = %v", req.Registry)
	}

	// The settings travel in the same body and are read separately, so the
	// action's own input struct never carries them.
	conn, err := sdkv1.CastRequestTo[connection](payload)
	if err != nil {
		t.Fatalf("CastRequestTo[connection]: %v", err)
	}
	if conn.Body.Settings["uri"] != "neo4j://localhost:7687" {
		t.Errorf("settings = %v", conn.Body.Settings)
	}
}

// ----------------------------------------------------------------- inputs --

func TestResolveLimit(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", maxRowLimit, false},
		{"10", 10, false},
		{"  10 ", 10, false},
		{"0", maxRowLimit, false},
		{"-5", maxRowLimit, false},
		{"1000", maxRowLimit, false}, // clamped, not refused
		{"all", 0, true},
		{"{{$.limit}}", 0, true}, // an unresolved token is a mistake worth naming
	}
	for _, tc := range tests {
		got, err := resolveLimit(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("resolveLimit(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("resolveLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Numbers must not reach the graph as floats: a property set from `42` has to
// be an integer, or every id in the graph becomes 42.0.
func TestParseJSONObjectKeepsIntegers(t *testing.T) {
	params, err := parseJSONObject(`{"id": 42, "score": 1.5, "name": "Ada", "ok": true, "tags": ["a", 7], "nested": {"n": 3}}`)
	if err != nil {
		t.Fatalf("parseJSONObject: %v", err)
	}
	if got, ok := params["id"].(int64); !ok || got != 42 {
		t.Errorf("id = %#v, want int64(42)", params["id"])
	}
	if got, ok := params["score"].(float64); !ok || got != 1.5 {
		t.Errorf("score = %#v, want float64(1.5)", params["score"])
	}
	if params["name"] != "Ada" || params["ok"] != true {
		t.Errorf("name/ok = %v / %v", params["name"], params["ok"])
	}
	tags, _ := params["tags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("tags = %#v", params["tags"])
	}
	if got, ok := tags[1].(int64); !ok || got != 7 {
		t.Errorf("tags[1] = %#v, want int64(7)", tags[1])
	}
	nested, _ := params["nested"].(map[string]any)
	if got, ok := nested["n"].(int64); !ok || got != 3 {
		t.Errorf("nested.n = %#v, want int64(3)", nested["n"])
	}
}

func TestParseParams(t *testing.T) {
	if params, err := parseParams("   "); err != nil || params != nil {
		t.Errorf("empty parameters should be no parameters, got %v / %v", params, err)
	}
	if _, err := parseParams(`not json`); err == nil {
		t.Error("unparseable parameters should be an error")
	}
	if _, err := parseParams(`[1,2]`); err == nil {
		t.Error("a JSON array is not a parameter map")
	}
}

func TestCoerceValue(t *testing.T) {
	tests := []struct {
		in   string
		want any
	}{
		{"42", int64(42)},
		{`"42"`, "42"}, // quoting forces a string
		{"1.5", 1.5},
		{"true", true},
		{"null", nil},
		{"Ada", "Ada"},
		{"a@b.c", "a@b.c"},
		{"007", "007"}, // not valid JSON, so the zero padding survives
		{"1 2", "1 2"}, // two tokens: really a string
		{"", ""},
	}
	for _, tc := range tests {
		if got := coerceValue(tc.in); got != tc.want {
			t.Errorf("coerceValue(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	if got, ok := coerceValue(`["a","b"]`).([]any); !ok || len(got) != 2 {
		t.Errorf("a JSON list should stay a list, got %#v", coerceValue(`["a","b"]`))
	}
}
