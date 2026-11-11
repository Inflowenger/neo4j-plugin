package neo4j

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"
)

// A label or property name cannot be a parameter, so quoting is the only thing
// between a form field and the statement's grammar.
func TestQuoteIdent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Person", "`Person`"},
		{"my label", "`my label`"},
		{"MATCH", "`MATCH`"},
		// A backtick is doubled, so the name cannot end the quoting early.
		{"we`ird", "`we``ird`"},
		{"a`) DETACH DELETE n //", "`a``) DETACH DELETE n //`"},
	}
	for _, tc := range tests {
		if got := QuoteIdent(tc.in); got != tc.want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidIdent(t *testing.T) {
	if err := ValidIdent("label", "Person"); err != nil {
		t.Errorf("Person should be valid: %v", err)
	}
	for _, bad := range []string{"", "   ", "a\nb", "a\x00b", "a\rb"} {
		if err := ValidIdent("label", bad); err == nil {
			t.Errorf("ValidIdent(%q) = nil, want an error", bad)
		}
	}
}

func TestLabels(t *testing.T) {
	tests := []struct {
		in         string
		wantQuoted string
		wantNames  int
	}{
		{"Person", "`Person`", 1},
		{"Person:Employee", "`Person`:`Employee`", 2},
		{"Person, Employee", "`Person`:`Employee`", 2},
		{" Person ", "`Person`", 1},
	}
	for _, tc := range tests {
		quoted, names, err := Labels(tc.in)
		if err != nil {
			t.Fatalf("Labels(%q): %v", tc.in, err)
		}
		if quoted != tc.wantQuoted {
			t.Errorf("Labels(%q) = %q, want %q", tc.in, quoted, tc.wantQuoted)
		}
		if len(names) != tc.wantNames {
			t.Errorf("Labels(%q) gave %d names, want %d", tc.in, len(names), tc.wantNames)
		}
	}
	if _, _, err := Labels("  "); err == nil {
		t.Error("an empty label spec should be an error")
	}
}

func TestPropertyNames(t *testing.T) {
	got, err := PropertyNames(" tenantId , externalId ")
	if err != nil {
		t.Fatalf("PropertyNames: %v", err)
	}
	if len(got) != 2 || got[0] != "tenantId" || got[1] != "externalId" {
		t.Errorf("PropertyNames = %v, want [tenantId externalId]", got)
	}
	if got, _ := PropertyNames(""); len(got) != 0 {
		t.Errorf("an empty spec should give no names, got %v", got)
	}
}

func TestLeadingKeyword(t *testing.T) {
	tests := []struct{ in, want string }{
		{"MATCH (n) RETURN n", "MATCH"},
		{"  merge (n:A) return n", "MERGE"},
		{"// a comment\nMERGE (n:A)", "MERGE"},
		{"/* banner */ CREATE (n)", "CREATE"},
		{"", ""},
		{"CALL db.labels()", "CALL"},
		{"SHOW DATABASES", "SHOW"},
	}
	for _, tc := range tests {
		if got := LeadingKeyword(tc.in); got != tc.want {
			t.Errorf("LeadingKeyword(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// IsWrite reads only the leading keyword, on purpose: scanning the whole
// statement would reject reads that merely mention a property called `set`.
func TestIsWrite(t *testing.T) {
	writes := []string{
		"CREATE (n:Person {name: $name})",
		"MERGE (n:Person {id: $id})",
		"DETACH DELETE n",
		"DROP CONSTRAINT x",
		"  merge (n) return n",
	}
	for _, cypher := range writes {
		if !IsWrite(cypher) {
			t.Errorf("IsWrite(%q) = false, want true", cypher)
		}
	}

	reads := []string{
		"MATCH (n:Person) RETURN n",
		// The false positives a clause scan would produce.
		"MATCH (n) RETURN n.set",
		"MATCH (n) RETURN n.created_at AS created",
		"MATCH (n) WHERE n.removed IS NULL RETURN n",
		"MATCH (n) RETURN n {.name, .merge_id}",
		"CALL db.labels()",
		"SHOW DATABASES",
		"RETURN 1",
	}
	for _, cypher := range reads {
		if IsWrite(cypher) {
			t.Errorf("IsWrite(%q) = true, want false", cypher)
		}
	}
}

// Every value the driver can hand back must marshal to JSON a flow can read.
func TestNormalize(t *testing.T) {
	node := dbtype.Node{
		ElementId: "4:abc:1",
		Labels:    []string{"Person"},
		Props:     map[string]any{"name": "Ada", "born": int64(1815)},
	}
	gotNode, ok := normalize(node).(map[string]any)
	if !ok {
		t.Fatalf("a node should normalize to a map, got %T", normalize(node))
	}
	if gotNode["elementId"] != "4:abc:1" {
		t.Errorf("elementId = %v", gotNode["elementId"])
	}
	props, ok := gotNode["properties"].(map[string]any)
	if !ok || props["name"] != "Ada" {
		t.Errorf("properties = %v", gotNode["properties"])
	}

	rel := dbtype.Relationship{
		ElementId:      "5:abc:9",
		StartElementId: "4:abc:1",
		EndElementId:   "4:abc:2",
		Type:           "WORKS_AT",
		Props:          map[string]any{"since": int64(2019)},
	}
	gotRel, _ := normalize(rel).(map[string]any)
	if gotRel["type"] != "WORKS_AT" || gotRel["startElementId"] != "4:abc:1" {
		t.Errorf("relationship = %v", gotRel)
	}

	path := dbtype.Path{Nodes: []dbtype.Node{node}, Relationships: []dbtype.Relationship{rel}}
	gotPath, _ := normalize(path).(map[string]any)
	if gotPath["length"] != 1 {
		t.Errorf("path length = %v, want 1", gotPath["length"])
	}

	// Each temporal type has to keep its own precision: a Date must not come
	// back with a midnight time, nor a LocalDateTime with a zone.
	stamp := time.Date(2024, 3, 1, 14, 30, 5, 0, time.UTC)
	temporals := []struct {
		in   any
		want string
	}{
		{dbtype.Date(stamp), "2024-03-01"},
		{dbtype.LocalTime(stamp), "14:30:05"},
		{dbtype.LocalDateTime(stamp), "2024-03-01T14:30:05"},
		{stamp, "2024-03-01T14:30:05Z"},
	}
	for _, tc := range temporals {
		if got := normalize(tc.in); got != tc.want {
			t.Errorf("normalize(%T) = %v, want %v", tc.in, got, tc.want)
		}
	}

	duration := dbtype.Duration{Months: 1, Days: 2, Seconds: 3}
	if got, ok := normalize(duration).(string); !ok || got == "" {
		t.Errorf("a duration should normalize to its ISO-8601 text, got %v", normalize(duration))
	}

	point := normalize(dbtype.Point2D{X: 1.5, Y: 2.5, SpatialRefId: 7203}).(map[string]any)
	if point["x"] != 1.5 || point["srid"] != uint32(7203) {
		t.Errorf("point = %v", point)
	}

	// Bytes are kept as data: readable if they are text, base64 if not.
	if got := normalize([]byte("hello")); got != "hello" {
		t.Errorf("utf-8 bytes = %v, want hello", got)
	}
	if got := normalize([]byte{0xff, 0xfe}); got != "//4=" {
		t.Errorf("binary bytes = %v, want base64", got)
	}

	// Nesting is walked, so a node inside a list inside a map is mapped too.
	nested := normalize(map[string]any{"items": []any{node}})
	encoded, err := json.Marshal(nested)
	if err != nil {
		t.Fatalf("nested value does not marshal: %v", err)
	}
	if want := `{"items":[{"elementId":"4:abc:1"`; string(encoded[:len(want)]) != want {
		t.Errorf("nested = %s", encoded)
	}

	if normalize(nil) != nil {
		t.Error("nil should stay nil")
	}
}

func TestIsSyntaxError(t *testing.T) {
	if IsSyntaxError(nil) {
		t.Error("nil is not a syntax error")
	}
	// The form the error takes after describeError has rendered it.
	rendered := &stringError{SyntaxErrorCode + ": Invalid input 'REQUIRE'"}
	if !IsSyntaxError(rendered) {
		t.Error("a rendered syntax error should be recognised, so the 4.x retry can happen")
	}
	if IsSyntaxError(&stringError{"Neo.ClientError.Security.Unauthorized: nope"}) {
		t.Error("an auth failure is not a syntax error")
	}
}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }
