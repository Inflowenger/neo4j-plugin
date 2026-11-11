package neo4j

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"
)

// QuoteIdent wraps a Cypher identifier in backticks, which is how Neo4j quotes
// a name that is a keyword, holds a space, or is otherwise not a bare
// identifier. A backtick inside the name is doubled, as Cypher escapes it, so a
// name can never end the quoting early and continue as Cypher.
//
// Labels, relationship types and property keys are identifiers, not values, and
// Cypher has no parameter form for any of them — so this is what keeps a name a
// flow author supplied out of the statement's grammar. Use ValidIdent first on
// anything that came from a form.
func QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// ValidIdent rejects a name that cannot be safely backtick-quoted: an empty one,
// or one holding a character that ends or splits a statement rather than naming
// anything.
func ValidIdent(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("missing %s", kind)
	}
	if strings.ContainsAny(name, "\x00\n\r") {
		return fmt.Errorf("invalid %s %q: it contains a line break or a null character", kind, name)
	}
	return nil
}

// Labels renders one or more labels for a node pattern — `Person`:`Employee` —
// each quoted. It accepts the way a person writes several: "Person:Employee" or
// "Person, Employee".
func Labels(spec string) (string, []string, error) {
	fields := strings.FieldsFunc(spec, func(r rune) bool { return r == ':' || r == ',' })
	names := make([]string, 0, len(fields))
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		name := strings.TrimSpace(field)
		if name == "" {
			continue
		}
		if err := ValidIdent("label", name); err != nil {
			return "", nil, err
		}
		names = append(names, name)
		quoted = append(quoted, QuoteIdent(name))
	}
	if len(quoted) == 0 {
		return "", nil, fmt.Errorf("missing label")
	}
	return strings.Join(quoted, ":"), names, nil
}

// PropertyNames splits a comma-separated list of property names and validates
// each one.
func PropertyNames(spec string) ([]string, error) {
	fields := strings.Split(spec, ",")
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		name := strings.TrimSpace(field)
		if name == "" {
			continue
		}
		if err := ValidIdent("property name", name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

var (
	// lineComment, blockComment and stringLiteral are stripped before a
	// statement is classified, so a word inside a comment or a quoted value
	// cannot be mistaken for a clause.
	lineComment    = regexp.MustCompile(`//[^\n]*`)
	blockComment   = regexp.MustCompile(`(?s)/\*.*?\*/`)
	singleQuoted   = regexp.MustCompile(`'(?:[^'\\]|\\.)*'`)
	doubleQuoted   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	backQuoted     = regexp.MustCompile("`(?:[^`]|``)*`")
	leadingKeyword = regexp.MustCompile(`(?i)^[^\pL]*([\pL]+)`)
)

// strip removes comments, string literals and quoted identifiers from a
// statement, leaving a skeleton safe to pattern-match against. It is never run —
// only inspected.
func strip(cypher string) string {
	s := blockComment.ReplaceAllString(cypher, " ")
	s = lineComment.ReplaceAllString(s, " ")
	s = backQuoted.ReplaceAllString(s, "`id`")
	s = singleQuoted.ReplaceAllString(s, "''")
	return doubleQuoted.ReplaceAllString(s, `""`)
}

// writeKeywords are the statement-leading words that always change the graph or
// the schema.
var writeKeywords = map[string]bool{
	"CREATE": true, "MERGE": true, "DELETE": true, "DETACH": true,
	"SET": true, "REMOVE": true, "DROP": true, "FOREACH": true,
	"LOAD": true, // LOAD CSV
}

// IsWrite reports whether a statement *starts* with a write, which is how the
// read action names the fix before spending a round trip.
//
// Only the leading keyword is read, deliberately. Scanning the whole statement
// for a write clause would flag a read that merely returns a property called
// `set`, and the server is the real authority anyway: it refuses any write in a
// read transaction, and that refusal is already translated into a message
// naming the write action (see describeError). So this catches the obvious
// mistake and leaves the subtle one to the server.
func IsWrite(cypher string) bool {
	return writeKeywords[LeadingKeyword(cypher)]
}

// LeadingKeyword is the statement's first word in upper case — MATCH, MERGE,
// CALL, SHOW — or "" when it has none. Cypher's answer to a SQL command tag,
// read off the text because Bolt reports counters rather than a verb.
func LeadingKeyword(cypher string) string {
	match := leadingKeyword.FindStringSubmatch(strip(cypher))
	if len(match) < 2 {
		return ""
	}
	return strings.ToUpper(match[1])
}

// normalize turns a value the driver decoded from Bolt into something that
// marshals to clean, readable JSON.
//
// The driver is faithful to Neo4j's type system, which is wider than JSON's: a
// node is a struct with its own identity, a date is one of four distinct
// temporal types, a point is a coordinate in a reference system. Left as-is,
// those marshal as anonymous objects of exported Go fields (or, for the
// time-based aliases, as a bare RFC-3339 string with a misleading zero time), so
// each is mapped to an explicit shape a downstream node can read:
//
//   - a node       → {elementId, labels, properties}
//   - a relationship → {elementId, type, start/end elementId, properties}
//   - a path       → {nodes, relationships, length}
//   - a temporal   → its ISO-8601 text, in the precision of its own type
//   - a point      → {srid, x, y[, z]}
//   - bytes        → the string if it is UTF-8, else base64
func normalize(value any) any {
	switch v := value.(type) {
	case nil:
		return nil

	case dbtype.Node:
		return normalizeNode(v)
	case dbtype.Relationship:
		return normalizeRelationship(v)
	case dbtype.Path:
		nodes := make([]any, 0, len(v.Nodes))
		for _, n := range v.Nodes {
			nodes = append(nodes, normalizeNode(n))
		}
		rels := make([]any, 0, len(v.Relationships))
		for _, r := range v.Relationships {
			rels = append(rels, normalizeRelationship(r))
		}
		return map[string]any{
			"nodes":         nodes,
			"relationships": rels,
			"length":        len(v.Relationships),
		}

	// The four temporal types are distinct aliases of time.Time, so each is
	// rendered at its own precision: a Date must not come back with a midnight
	// time attached, and a LocalDateTime must not come back with a zone.
	case dbtype.Date:
		return v.Time().Format("2006-01-02")
	case dbtype.LocalTime:
		return v.Time().Format("15:04:05.999999999")
	case dbtype.Time:
		return v.Time().Format("15:04:05.999999999Z07:00")
	case dbtype.LocalDateTime:
		return v.Time().Format("2006-01-02T15:04:05.999999999")
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case dbtype.Duration:
		// Months/days/seconds are kept apart by Neo4j (a month is not 30 days),
		// so the ISO-8601 text the driver prints is the only lossless rendering.
		return v.String()

	case dbtype.Point2D:
		return map[string]any{"srid": v.SpatialRefId, "x": v.X, "y": v.Y}
	case dbtype.Point3D:
		return map[string]any{"srid": v.SpatialRefId, "x": v.X, "y": v.Y, "z": v.Z}

	case []byte:
		if utf8.Valid(v) {
			return string(v)
		}
		return base64.StdEncoding.EncodeToString(v)

	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = normalize(item)
		}
		return out

	default:
		// bool, int64, float64, string — already JSON.
		return v
	}
}

// normalizeNode renders a node. ElementId is the identity to carry around:
// the old numeric Id is deprecated in the driver and is not stable across a
// restart or a cluster, so it is not reported.
func normalizeNode(n dbtype.Node) map[string]any {
	return map[string]any{
		"elementId":  n.ElementId,
		"labels":     n.Labels,
		"properties": normalize(n.Props),
	}
}

func normalizeRelationship(r dbtype.Relationship) map[string]any {
	return map[string]any{
		"elementId":      r.ElementId,
		"type":           r.Type,
		"startElementId": r.StartElementId,
		"endElementId":   r.EndElementId,
		"properties":     normalize(r.Props),
	}
}

// Normalize is normalize, exported for the actions that build their own result
// shapes out of raw driver values.
func Normalize(value any) any { return normalize(value) }
