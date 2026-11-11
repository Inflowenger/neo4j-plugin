package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/neo4j-plugin/internal/neo4j"
)

// maxRowLimit is the ceiling on how many rows an action returns. A larger
// request is clamped to it rather than refused, and it is also the default.
const maxRowLimit = 100

// ------------------------------------------------------------------- read --

type queryInput struct {
	Cypher string `json:"cypher"`
	Params string `json:"params"`
	Limit  string `json:"limit"`
}

func (r *Registry) query() sdkv1.Action {
	return sdkv1.Action{
		Method:      "neo4j.query",
		Title:       "Run Cypher (read)",
		Description: "Run a read Cypher statement and get the records back, with nodes, relationships and paths mapped to plain JSON. Runs in a read transaction, which a cluster can answer from a follower. At most 100 rows are returned.",
		Icon:        sdkv1.Icon{Icon: "mdi-graph-outline"},
		Form:        queryForm,
		RequestHandler: run(r, "query", func(ctx context.Context, job *sdkv1.Job, client *neo4j.Client, in queryInput) (map[string]any, error) {
			cypher := strings.TrimSpace(in.Cypher)
			if cypher == "" {
				return nil, fmt.Errorf("missing required input: cypher")
			}
			// Caught here rather than at the server so the message names the
			// action to use. A write buried mid-statement still reaches the
			// server, which refuses it in read mode — see describeError.
			if neo4j.IsWrite(cypher) {
				return nil, fmt.Errorf("%s is a write: this action runs in a read transaction, so use the Execute Cypher (write) action instead", neo4j.LeadingKeyword(cypher))
			}
			params, err := parseParams(in.Params)
			if err != nil {
				return nil, err
			}
			limit, err := resolveLimit(in.Limit)
			if err != nil {
				return nil, err
			}

			job.Progress(50, sdkv1.Frame{Title: "query", Content: preview(cypher)})

			res, err := client.Read(ctx, cypher, params, limit)
			if err != nil {
				return nil, err
			}
			return resultOutput(res), nil
		}),
	}
}

// ------------------------------------------------------------------ write --

type executeInput struct {
	Cypher string `json:"cypher"`
	Params string `json:"params"`
}

func (r *Registry) execute() sdkv1.Action {
	return sdkv1.Action{
		Method:      "neo4j.execute",
		Title:       "Execute Cypher (write)",
		Description: "Run a write Cypher statement — CREATE / MERGE / SET / DELETE, or a schema command — and get the server's counters back, plus anything the statement returns. Runs in a write transaction, routed to the leader.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-edit"},
		Form:        executeForm,
		RequestHandler: run(r, "execute", func(ctx context.Context, job *sdkv1.Job, client *neo4j.Client, in executeInput) (map[string]any, error) {
			cypher := strings.TrimSpace(in.Cypher)
			if cypher == "" {
				return nil, fmt.Errorf("missing required input: cypher")
			}
			params, err := parseParams(in.Params)
			if err != nil {
				return nil, err
			}

			job.Progress(50, sdkv1.Frame{Title: "execute", Content: preview(cypher)})

			res, err := client.Write(ctx, cypher, params, maxRowLimit)
			if err != nil {
				return nil, err
			}
			out := resultOutput(res)
			out["command"] = neo4j.LeadingKeyword(cypher)
			return out, nil
		}),
	}
}

// ---------------------------------------------------------------- helpers --

// resultOutput is the committed shape every statement-running action shares:
// the records, and the server's own account of what happened. The counters are
// lifted out of the summary as well as left in it, because `counters.nodesCreated`
// is what a downstream node will actually reference.
func resultOutput(res neo4j.Result) map[string]any {
	return map[string]any{
		"keys":            res.Keys,
		"rows":            res.Rows,
		"rowCount":        res.RowCount,
		"truncated":       res.Truncated,
		"counters":        res.Summary.Counters,
		"containsUpdates": res.Summary.ContainsUpdates,
		"summary":         res.Summary,
	}
}

// resolveLimit turns the (already {{$.path}}-resolved) limit field into the row
// cap to apply. Empty means the default; anything over the ceiling is clamped to
// it, so a read never returns more than maxRowLimit rows. A non-number is an
// error, since it is almost always an unresolved token the user meant to fill.
func resolveLimit(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return maxRowLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("max rows must be a whole number up to %d: %q", maxRowLimit, s)
	}
	if n <= 0 || n > maxRowLimit {
		return maxRowLimit, nil
	}
	return n, nil
}

// parseParams reads the Parameters field into the named parameters the driver
// binds. Empty is no parameters, not an error — plenty of statements need none.
//
// The field is a JSON object because Cypher's parameters are named, and because
// Neo4j takes a list or a map as a parameter value (which is what `UNWIND $rows`
// is for) — so the structure is passed through rather than flattened.
func parseParams(text string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	params, err := parseJSONObject(text)
	if err != nil {
		return nil, fmt.Errorf("Parameters does not parse as a JSON object: %w", err)
	}
	return params, nil
}

// parseJSONObject decodes a JSON object, keeping whole numbers off float64.
// Every JSON-object field in this plugin goes through it, so a number typed into
// a form reaches the driver as an integer and is stored as one.
func parseJSONObject(text string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("there is more than one JSON value here")
	}
	out := make(map[string]any, len(obj))
	for key, value := range obj {
		out[key] = normalizeNumbers(value)
	}
	return out, nil
}

// normalizeNumbers walks a decoded JSON value and turns json.Number into int64
// (when integral) or float64. Without it every number would reach Neo4j as a
// float and be stored as one — `42.0` where the graph wants `42`.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case []any:
		for i := range t {
			t[i] = normalizeNumbers(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = normalizeNumbers(t[k])
		}
		return t
	default:
		return v
	}
}

// coerceValue reads one value typed into a per-property form field. A clean JSON
// value wins — so 42 is an integer, true a boolean, null a null, and ["a","b"] a
// list — and anything else is the string as typed, which is what a hand-written
// name or e-mail is. To force a value to stay a string, quote it: "42".
func coerceValue(s string) any {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	// A trailing token means it was not a single JSON value (e.g. `1 2`), so it
	// is really a string.
	if dec.More() {
		return s
	}
	return normalizeNumbers(v)
}

// sortedMapKeys orders a value map's keys, so a generated pattern and the
// parameters bound to it line up deterministically.
func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// preview trims Cypher to one short line for a progress frame.
func preview(cypher string) string {
	cypher = strings.Join(strings.Fields(cypher), " ")
	if len(cypher) > 120 {
		return cypher[:117] + "…"
	}
	return cypher
}
