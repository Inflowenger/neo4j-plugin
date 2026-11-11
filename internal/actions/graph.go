package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/neo4j-plugin/internal/neo4j"
)

// The three actions here are the ones that need no Cypher written by hand. Each
// builds its own statement, and in every one of them the *identifiers* — labels,
// relationship types, property names — are validated and backtick-quoted into
// the text, while every *value* is bound as a named parameter. Cypher has no
// parameter form for an identifier, so that split is what keeps a form field
// from becoming part of the query.

// --------------------------------------------------------- upsert a node --

// nodeUpsertInput is a node to write, given one of two ways. The property fields
// loaded into the form arrive under `values`; a pasted JSON object arrives as
// `record` and, when present, wins.
type nodeUpsertInput struct {
	Label  string         `json:"label"`
	Key    string         `json:"key"`
	Values map[string]any `json:"values"`
	Record string         `json:"record"`
}

func (r *Registry) nodeUpsert() sdkv1.Action {
	return sdkv1.Action{
		Method:      "neo4j.node.upsert",
		Title:       "Upsert Node (MERGE)",
		Description: "Create a node, or update the one that already matches on the key properties — a MERGE, so running the flow twice does not make a second node. Properties accept {{$.path}} tokens.",
		Icon:        sdkv1.Icon{Icon: "mdi-vector-point-plus"},
		Form:        nodeUpsertForm,
		RequestHandler: run(r, "upsert node", func(ctx context.Context, job *sdkv1.Job, client *neo4j.Client, in nodeUpsertInput) (map[string]any, error) {
			labels, labelNames, err := neo4j.Labels(in.Label)
			if err != nil {
				return nil, err
			}

			props, err := propertyData(job, in.Values, in.Record)
			if err != nil {
				return nil, err
			}
			if len(props) == 0 {
				return nil, fmt.Errorf("no properties: fill in at least one property field, or a JSON object")
			}

			keys, err := neo4j.PropertyNames(in.Key)
			if err != nil {
				return nil, err
			}
			// A key naming a property that was not supplied cannot identify
			// anything, and MERGEing on a null would make a node with a null
			// property rather than find the one meant.
			keyProps := make(map[string]any, len(keys))
			for _, key := range keys {
				value, ok := props[key]
				if !ok {
					return nil, fmt.Errorf("key property %q has no value: either give it one, or take it out of Key properties", key)
				}
				keyProps[key] = value
			}

			cypher, params, err := upsertNodeStatement(labels, keyProps, props)
			if err != nil {
				return nil, err
			}

			job.Progress(50, sdkv1.Frame{Title: "upsert node", Content: preview(cypher)})

			res, err := client.Write(ctx, cypher, params, 1)
			if err != nil {
				return nil, err
			}
			if res.RowCount == 0 {
				return nil, fmt.Errorf("the statement wrote no node — this should not happen; the statement was: %s", cypher)
			}

			out := map[string]any{
				"node":     res.Rows[0]["n"],
				"labels":   labelNames,
				"created":  res.Summary.Counters.NodesCreated > 0,
				"merged":   len(keyProps) > 0,
				"counters": res.Summary.Counters,
			}
			// The node's own identity and properties are lifted out as well, so
			// a downstream node can read $.node.elementId or go straight to
			// $.properties.email without knowing this shape.
			if node, ok := res.Rows[0]["n"].(map[string]any); ok {
				out["elementId"] = node["elementId"]
				out["properties"] = node["properties"]
			}
			return out, nil
		}),
	}
}

// upsertNodeStatement renders the write. With key properties it is a MERGE on
// exactly those, followed by `SET n += $props` so the rest are applied whether
// the node was found or created. With none it is a CREATE, because there is
// nothing to match on — which is why Key properties is the field that decides
// whether this action is idempotent.
func upsertNodeStatement(labels string, keyProps, props map[string]any) (string, map[string]any, error) {
	params := map[string]any{"props": props}

	if len(keyProps) == 0 {
		return fmt.Sprintf("CREATE (n:%s)\nSET n += $props\nRETURN n", labels), params, nil
	}

	pattern, keyParams, err := propertyPattern("key", keyProps)
	if err != nil {
		return "", nil, err
	}
	for name, value := range keyParams {
		params[name] = value
	}
	return fmt.Sprintf("MERGE (n:%s%s)\nSET n += $props\nRETURN n", labels, pattern), params, nil
}

// propertyData turns the action's two input shapes into the property → value map
// to write. A JSON object wins when present; otherwise the loaded property
// fields are used, with each value resolved and typed the way a parameter is.
func propertyData(job *sdkv1.Job, values map[string]any, record string) (map[string]any, error) {
	// A pasted object. Its {{$.path}} tokens were resolved by run() before it
	// reached here, since Record is a plain string field, so it now parses as
	// ordinary JSON.
	if text := strings.TrimSpace(record); text != "" {
		props, err := parseJSONObject(text)
		if err != nil {
			return nil, fmt.Errorf("JSON properties does not parse as an object: %w", err)
		}
		return props, nil
	}

	// Property fields. run()'s reflect walker rewrites strings and []string on
	// the input, but not the values inside this map, so resolve them here. A
	// blank field is omitted rather than written as an empty string.
	vr := newVarResolver(job)
	props := make(map[string]any, len(values))
	for key, raw := range values {
		text, ok := raw.(string)
		if !ok {
			if raw != nil {
				props[key] = raw
			}
			continue
		}
		if text = strings.TrimSpace(vr.resolve(text)); text == "" {
			continue
		}
		props[key] = coerceValue(text)
	}
	return props, nil
}

// ------------------------------------------------- link two nodes together --

type relationshipInput struct {
	Type          string `json:"type"`
	FromLabel     string `json:"fromLabel"`
	FromMatch     string `json:"fromMatch"`
	ToLabel       string `json:"toLabel"`
	ToMatch       string `json:"toMatch"`
	Properties    string `json:"properties"`
	CreateMissing bool   `json:"createMissing"`
}

func (r *Registry) relationshipUpsert() sdkv1.Action {
	return sdkv1.Action{
		Method:      "neo4j.relationship.upsert",
		Title:       "Link Nodes (MERGE relationship)",
		Description: "Create a relationship between two nodes, or update the one already between them. The endpoints are matched by label and properties; optionally they are created when missing.",
		Icon:        sdkv1.Icon{Icon: "mdi-vector-link"},
		Form:        relationshipForm,
		RequestHandler: run(r, "link nodes", func(ctx context.Context, job *sdkv1.Job, client *neo4j.Client, in relationshipInput) (map[string]any, error) {
			relType := strings.TrimSpace(in.Type)
			if err := neo4j.ValidIdent("relationship type", relType); err != nil {
				return nil, err
			}

			from, err := endpoint("start", in.FromLabel, in.FromMatch)
			if err != nil {
				return nil, err
			}
			to, err := endpoint("end", in.ToLabel, in.ToMatch)
			if err != nil {
				return nil, err
			}

			var props map[string]any
			if text := strings.TrimSpace(in.Properties); text != "" {
				if props, err = parseJSONObject(text); err != nil {
					return nil, fmt.Errorf("Relationship properties does not parse as an object: %w", err)
				}
			}

			cypher, params, err := relationshipStatement(relType, from, to, props, in.CreateMissing)
			if err != nil {
				return nil, err
			}

			job.Progress(50, sdkv1.Frame{Title: "link nodes", Content: preview(cypher)})

			res, err := client.Write(ctx, cypher, params, 1)
			if err != nil {
				return nil, err
			}
			if res.RowCount == 0 {
				// Only reachable when the endpoints were MATCHed: one of them
				// found nothing, so the MERGE never ran. Which one is worth a
				// second look — it is the difference between a typo in a label
				// and a node an earlier step failed to write.
				return nil, missingEndpoint(ctx, client, from, to)
			}

			out := map[string]any{
				"relationship": res.Rows[0]["r"],
				"type":         relType,
				"created":      res.Summary.Counters.RelationshipsCreated > 0,
				"counters":     res.Summary.Counters,
			}
			if rel, ok := res.Rows[0]["r"].(map[string]any); ok {
				out["elementId"] = rel["elementId"]
				out["startElementId"] = rel["startElementId"]
				out["endElementId"] = rel["endElementId"]
				out["properties"] = rel["properties"]
			}
			return out, nil
		}),
	}
}

// nodeRef is one end of a relationship: the quoted labels to put in the
// pattern, and the properties to match (or create) it by.
type nodeRef struct {
	side     string // "start" / "end", for the error messages
	labels   string
	rawLabel string
	props    map[string]any
}

// endpoint validates and parses one end of the relationship.
func endpoint(side, label, match string) (nodeRef, error) {
	labels, _, err := neo4j.Labels(label)
	if err != nil {
		return nodeRef{}, fmt.Errorf("%s node: %w", side, err)
	}
	text := strings.TrimSpace(match)
	if text == "" {
		// Matching on the label alone would select every node with it, and a
		// MERGE per matched row would fan out into one relationship each.
		return nodeRef{}, fmt.Errorf("%s node match is empty: give the properties that identify one node, e.g. {\"id\": 42}", side)
	}
	props, err := parseJSONObject(text)
	if err != nil {
		return nodeRef{}, fmt.Errorf("%s node match does not parse as a JSON object: %w", side, err)
	}
	if len(props) == 0 {
		return nodeRef{}, fmt.Errorf("%s node match is an empty object: give the properties that identify one node", side)
	}
	return nodeRef{side: side, labels: labels, rawLabel: strings.TrimSpace(label), props: props}, nil
}

// relationshipStatement renders the write: find (or create) both ends, then
// MERGE the relationship between them so re-running updates rather than
// duplicates.
//
// The direction is start → end, as the form reads. Properties are applied with
// `SET r +=` and only when there are some, so a re-run with no properties does
// not count as an update.
func relationshipStatement(relType string, from, to nodeRef, props map[string]any, createMissing bool) (string, map[string]any, error) {
	clause := "MATCH"
	if createMissing {
		clause = "MERGE"
	}

	fromPattern, fromParams, err := propertyPattern("a", from.props)
	if err != nil {
		return "", nil, fmt.Errorf("start node: %w", err)
	}
	toPattern, toParams, err := propertyPattern("b", to.props)
	if err != nil {
		return "", nil, fmt.Errorf("end node: %w", err)
	}

	params := make(map[string]any, len(fromParams)+len(toParams)+1)
	for name, value := range fromParams {
		params[name] = value
	}
	for name, value := range toParams {
		params[name] = value
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s (a:%s%s)\n", clause, from.labels, fromPattern)
	fmt.Fprintf(&b, "%s (b:%s%s)\n", clause, to.labels, toPattern)
	fmt.Fprintf(&b, "MERGE (a)-[r:%s]->(b)\n", neo4j.QuoteIdent(relType))
	if len(props) > 0 {
		b.WriteString("SET r += $props\n")
		params["props"] = props
	}
	b.WriteString("RETURN r")
	return b.String(), params, nil
}

// missingEndpoint works out which end of the relationship matched nothing, so
// the node's error says which half to fix. It runs only on the failure path —
// two cheap counts — so the happy path stays one round trip.
func missingEndpoint(ctx context.Context, client *neo4j.Client, from, to nodeRef) error {
	missing := make([]string, 0, 2)
	for _, ref := range []nodeRef{from, to} {
		found, err := matches(ctx, client, ref)
		if err != nil {
			// The diagnosis failed; report the thing we do know.
			return fmt.Errorf("no relationship was written: the start or end node matched nothing")
		}
		if !found {
			missing = append(missing, fmt.Sprintf("no %s node :%s matches %s", ref.side, ref.rawLabel, compactJSON(ref.props)))
		}
	}
	if len(missing) == 0 {
		return fmt.Errorf("no relationship was written although both nodes matched — check whether the match selects more than one node")
	}
	return fmt.Errorf("%s. Either write the node first, or switch on \"Create the nodes if they are missing\"", strings.Join(missing, "; "))
}

// matches reports whether an endpoint's match finds any node at all.
func matches(ctx context.Context, client *neo4j.Client, ref nodeRef) (bool, error) {
	pattern, params, err := propertyPattern("p", ref.props)
	if err != nil {
		return false, err
	}
	cypher := fmt.Sprintf("MATCH (n:%s%s) RETURN count(n) AS found", ref.labels, pattern)
	res, err := client.Read(ctx, cypher, params, 1)
	if err != nil || res.RowCount == 0 {
		return false, err
	}
	count, _ := res.Rows[0]["found"].(int64)
	return count > 0, nil
}

// ---------------------------------------------------------- find nodes --

type nodeFindInput struct {
	Label string `json:"label"`
	Match string `json:"match"`
	Limit string `json:"limit"`
}

func (r *Registry) nodeFind() sdkv1.Action {
	return sdkv1.Action{
		Method:      "neo4j.node.find",
		Title:       "Find Nodes",
		Description: "Match nodes by label and properties and return them, without writing any Cypher. At most 100 nodes, as a real LIMIT in the statement.",
		Icon:        sdkv1.Icon{Icon: "mdi-text-search"},
		Form:        nodeFindForm,
		RequestHandler: run(r, "find nodes", func(ctx context.Context, job *sdkv1.Job, client *neo4j.Client, in nodeFindInput) (map[string]any, error) {
			limit, err := resolveLimit(in.Limit)
			if err != nil {
				return nil, err
			}

			labels := ""
			if strings.TrimSpace(in.Label) != "" {
				quoted, _, err := neo4j.Labels(in.Label)
				if err != nil {
					return nil, err
				}
				labels = ":" + quoted
			}

			var props map[string]any
			if text := strings.TrimSpace(in.Match); text != "" {
				if props, err = parseJSONObject(text); err != nil {
					return nil, fmt.Errorf("Match does not parse as a JSON object: %w", err)
				}
			}
			pattern, params, err := propertyPattern("p", props)
			if err != nil {
				return nil, err
			}

			// The limit is bound as a parameter and pushed into the statement,
			// so the server stops looking once it has enough — unlike the Cypher
			// read action, where the cap can only be applied to what arrives.
			cypher := fmt.Sprintf("MATCH (n%s%s)\nRETURN n\nLIMIT $limit", labels, pattern)
			params["limit"] = int64(limit)

			job.Progress(50, sdkv1.Frame{Title: "find nodes", Content: preview(cypher)})

			res, err := client.Read(ctx, cypher, params, limit)
			if err != nil {
				return nil, err
			}

			nodes := make([]any, 0, res.RowCount)
			for _, row := range res.Rows {
				nodes = append(nodes, row["n"])
			}
			return map[string]any{
				"nodes":   nodes,
				"count":   len(nodes),
				"limited": len(nodes) >= limit,
				"summary": res.Summary,
			}, nil
		}),
	}
}

// ---------------------------------------------------------------- helpers --

// propertyPattern renders the inline property map of a node pattern —
// ` {`email`: $p0, `tenant`: $p1}` — together with the parameters to bind.
//
// Property names are identifiers: they cannot be parameters, so each is
// validated and backtick-quoted into the text. Every value is a parameter, so
// no value the user typed is ever part of the statement. Keys are sorted, which
// makes the generated statement stable — and therefore cacheable by the server's
// query planner.
func propertyPattern(prefix string, props map[string]any) (string, map[string]any, error) {
	params := make(map[string]any, len(props)+1)
	if len(props) == 0 {
		return "", params, nil
	}

	keys := sortedMapKeys(props)
	parts := make([]string, 0, len(keys))
	for i, key := range keys {
		if err := neo4j.ValidIdent("property name", key); err != nil {
			return "", nil, err
		}
		name := fmt.Sprintf("%s%d", prefix, i)
		parts = append(parts, fmt.Sprintf("%s: $%s", neo4j.QuoteIdent(key), name))
		params[name] = props[key]
	}
	return " {" + strings.Join(parts, ", ") + "}", params, nil
}

// compactJSON renders a match map for an error message.
func compactJSON(props map[string]any) string {
	parts := make([]string, 0, len(props))
	for _, key := range sortedMapKeys(props) {
		parts = append(parts, fmt.Sprintf("%s=%v", key, props[key]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
