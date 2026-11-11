package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/neo4j-plugin/internal/neo4j"
)

type constraintInput struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Properties string `json:"properties"`
	Name       string `json:"name"`
}

// Constraint kinds, as the form offers them.
const (
	kindUnique  = "unique"
	kindExists  = "exists"
	kindNodeKey = "node key"
	kindIndex   = "index"
)

func (r *Registry) constraintCreate() sdkv1.Action {
	return sdkv1.Action{
		Method:      "neo4j.constraint.create",
		Title:       "Create Constraint / Index (if not exists)",
		Description: "Add a uniqueness or existence constraint, a node key, or an index — only if it is not already there. A uniqueness constraint on the properties an upsert merges by is what makes that upsert both correct and fast.",
		Icon:        sdkv1.Icon{Icon: "mdi-key-chain"},
		Form:        constraintForm,
		RequestHandler: run(r, "create constraint", func(ctx context.Context, job *sdkv1.Job, client *neo4j.Client, in constraintInput) (map[string]any, error) {
			kind, err := constraintKind(in.Kind)
			if err != nil {
				return nil, err
			}

			label := strings.TrimSpace(in.Label)
			if err := neo4j.ValidIdent("label", label); err != nil {
				return nil, err
			}

			properties, err := neo4j.PropertyNames(in.Properties)
			if err != nil {
				return nil, err
			}
			if len(properties) == 0 {
				return nil, fmt.Errorf("missing required input: properties")
			}
			if kind == kindExists && len(properties) > 1 {
				return nil, fmt.Errorf("an existence constraint covers exactly one property, but %d were given: add one action per property, or use \"node key\" to cover them together", len(properties))
			}

			name := strings.TrimSpace(in.Name)
			if name != "" {
				if err := neo4j.ValidIdent("name", name); err != nil {
					return nil, err
				}
			}

			cypher := constraintStatement(kind, name, label, properties, dialectCurrent)

			job.Progress(50, sdkv1.Frame{Title: "create constraint", Content: preview(cypher)})

			res, err := client.Write(ctx, cypher, nil, 0)
			if err != nil && neo4j.IsSyntaxError(err) && kind != kindIndex {
				// The REQUIRE keyword is Neo4j 5; a 4.x server spells the same
				// command with ASSERT and rejects this one as a syntax error.
				// Retrying in the older dialect is what makes the action work
				// against an on-prem server that has not moved to 5 yet — and
				// it costs a round trip only on a server where the first form
				// could never have worked.
				legacy := constraintStatement(kind, name, label, properties, dialectLegacy)
				if legacy != cypher {
					job.Progress(70, sdkv1.Frame{Title: "create constraint", Content: "retrying in the Neo4j 4 dialect"})
					if res, err = client.Write(ctx, legacy, nil, 0); err == nil {
						cypher = legacy
					}
				}
			}
			if err != nil {
				return nil, err
			}

			counters := res.Summary.Counters
			created := counters.ConstraintsAdded > 0 || counters.IndexesAdded > 0
			out := map[string]any{
				"command":    "CREATE " + strings.ToUpper(objectOf(kind)),
				"kind":       kind,
				"label":      label,
				"properties": properties,
				"cypher":     cypher,
				// False means it was already there: the statement carries IF NOT
				// EXISTS, so a re-run is a no-op rather than an error.
				"created":  created,
				"counters": counters,
			}
			if name != "" {
				out["name"] = name
			}
			return out, nil
		}),
	}
}

// constraintKind reads the Kind field leniently — the enum's own wording, and
// the spellings a hand-typed profile or an upstream node is likely to use.
func constraintKind(raw string) (string, error) {
	switch strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(raw, "_", " "))), " ") {
	case "", kindUnique, "uniqueness", "unique constraint":
		return kindUnique, nil
	case kindExists, "existence", "not null", "exist":
		return kindExists, nil
	case kindNodeKey, "key", "nodekey":
		return kindNodeKey, nil
	case kindIndex, "range index", "btree":
		return kindIndex, nil
	default:
		return "", fmt.Errorf("unknown kind %q: use unique, exists, node key or index", raw)
	}
}

// dialect selects how the constraint command is spelled. Neo4j 5 introduced
// REQUIRE for the predicate; 4.x uses ASSERT, and spells existence with the
// exists() function rather than IS NOT NULL.
type dialect int

const (
	dialectCurrent dialect = iota // Neo4j 5 and later
	dialectLegacy                 // Neo4j 4.x
)

// constraintStatement renders the schema command. Every identifier — the name,
// the label, each property — is backtick-quoted, and there are no values to
// bind: a schema command takes none.
//
// IF NOT EXISTS is always included, which is what makes this action safe at the
// front of a flow that runs repeatedly.
func constraintStatement(kind, name, label string, properties []string, d dialect) string {
	quotedName := ""
	if name != "" {
		quotedName = " " + neo4j.QuoteIdent(name)
	}
	target := fmt.Sprintf("FOR (n:%s)", neo4j.QuoteIdent(label))

	refs := make([]string, 0, len(properties))
	for _, property := range properties {
		refs = append(refs, "n."+neo4j.QuoteIdent(property))
	}
	list := strings.Join(refs, ", ")

	if kind == kindIndex {
		// An index is spelled the same on 4.x and 5.x.
		return fmt.Sprintf("CREATE INDEX%s IF NOT EXISTS %s ON (%s)", quotedName, target, list)
	}

	predicate := "REQUIRE"
	if d == dialectLegacy {
		predicate = "ASSERT"
	}

	switch kind {
	case kindNodeKey:
		return fmt.Sprintf("CREATE CONSTRAINT%s IF NOT EXISTS %s %s (%s) IS NODE KEY", quotedName, target, predicate, list)
	case kindExists:
		if d == dialectLegacy {
			return fmt.Sprintf("CREATE CONSTRAINT%s IF NOT EXISTS %s ASSERT exists(%s)", quotedName, target, list)
		}
		return fmt.Sprintf("CREATE CONSTRAINT%s IF NOT EXISTS %s REQUIRE %s IS NOT NULL", quotedName, target, list)
	default: // kindUnique
		return fmt.Sprintf("CREATE CONSTRAINT%s IF NOT EXISTS %s %s (%s) IS UNIQUE", quotedName, target, predicate, list)
	}
}

// objectOf names what the command creates, for the committed `command` field.
func objectOf(kind string) string {
	if kind == kindIndex {
		return "index"
	}
	return "constraint"
}
