package actions

import (
	"strings"
	"testing"
)

// propertyPattern is where a form's property names become part of the statement
// and its values do not.
func TestPropertyPattern(t *testing.T) {
	pattern, params, err := propertyPattern("key", map[string]any{"email": "a@b.c", "tenant": int64(7)})
	if err != nil {
		t.Fatalf("propertyPattern: %v", err)
	}
	// Keys are sorted, so the generated statement is stable and the server can
	// cache its plan.
	if pattern != " {`email`: $key0, `tenant`: $key1}" {
		t.Errorf("pattern = %q", pattern)
	}
	if params["key0"] != "a@b.c" || params["key1"] != int64(7) {
		t.Errorf("params = %#v", params)
	}

	empty, params, err := propertyPattern("p", nil)
	if err != nil || empty != "" || len(params) != 0 {
		t.Errorf("no properties should give no pattern, got %q / %#v / %v", empty, params, err)
	}
}

// No value may ever reach the statement text — only parameters.
func TestPropertyPatternBindsValues(t *testing.T) {
	hostile := "' OR 1=1 //"
	pattern, params, err := propertyPattern("p", map[string]any{"name": hostile})
	if err != nil {
		t.Fatalf("propertyPattern: %v", err)
	}
	if strings.Contains(pattern, hostile) {
		t.Errorf("the value was spliced into the statement: %q", pattern)
	}
	if params["p0"] != hostile {
		t.Errorf("the value should be bound verbatim, got %#v", params["p0"])
	}
}

// A property name cannot be a parameter, so it is quoted — and a name that
// cannot be quoted safely is refused.
func TestPropertyPatternQuotesNames(t *testing.T) {
	pattern, _, err := propertyPattern("p", map[string]any{"we`ird": 1})
	if err != nil {
		t.Fatalf("propertyPattern: %v", err)
	}
	if !strings.Contains(pattern, "`we``ird`") {
		t.Errorf("the name should be backtick-escaped, got %q", pattern)
	}

	if _, _, err := propertyPattern("p", map[string]any{"a\nb": 1}); err == nil {
		t.Error("a property name with a line break should be refused")
	}
}

func TestUpsertNodeStatement(t *testing.T) {
	props := map[string]any{"email": "a@b.c", "name": "Ada"}

	// With a key: a MERGE on exactly that key, then SET for the rest.
	cypher, params, err := upsertNodeStatement("`Person`", map[string]any{"email": "a@b.c"}, props)
	if err != nil {
		t.Fatalf("upsertNodeStatement: %v", err)
	}
	if !strings.HasPrefix(cypher, "MERGE (n:`Person` {`email`: $key0})") {
		t.Errorf("cypher = %q", cypher)
	}
	for _, want := range []string{"SET n += $props", "RETURN n"} {
		if !strings.Contains(cypher, want) {
			t.Errorf("cypher is missing %q: %s", want, cypher)
		}
	}
	if gotProps, ok := params["props"].(map[string]any); !ok || gotProps["name"] != "Ada" {
		t.Errorf("params.props = %#v", params["props"])
	}
	if params["key0"] != "a@b.c" {
		t.Errorf("params.key0 = %#v", params["key0"])
	}

	// With no key there is nothing to match on, so it is a CREATE — which is
	// what makes Key properties the field that decides idempotence.
	cypher, _, err = upsertNodeStatement("`Person`", nil, props)
	if err != nil {
		t.Fatalf("upsertNodeStatement: %v", err)
	}
	if !strings.HasPrefix(cypher, "CREATE (n:`Person`)") {
		t.Errorf("without a key it should CREATE, got %q", cypher)
	}
	if strings.Contains(cypher, "MERGE") {
		t.Errorf("without a key there is nothing to MERGE on: %q", cypher)
	}
}

func TestRelationshipStatement(t *testing.T) {
	from := nodeRef{side: "start", labels: "`Person`", props: map[string]any{"id": int64(1)}}
	to := nodeRef{side: "end", labels: "`Company`", props: map[string]any{"name": "Acme"}}

	// Default: both ends must already exist.
	cypher, params, err := relationshipStatement("WORKS_AT", from, to, map[string]any{"since": int64(2019)}, false)
	if err != nil {
		t.Fatalf("relationshipStatement: %v", err)
	}
	for _, want := range []string{
		"MATCH (a:`Person` {`id`: $a0})",
		"MATCH (b:`Company` {`name`: $b0})",
		"MERGE (a)-[r:`WORKS_AT`]->(b)",
		"SET r += $props",
		"RETURN r",
	} {
		if !strings.Contains(cypher, want) {
			t.Errorf("cypher is missing %q:\n%s", want, cypher)
		}
	}
	if params["a0"] != int64(1) || params["b0"] != "Acme" {
		t.Errorf("params = %#v", params)
	}

	// createMissing: the ends are MERGEd rather than MATCHed.
	cypher, _, err = relationshipStatement("WORKS_AT", from, to, nil, true)
	if err != nil {
		t.Fatalf("relationshipStatement: %v", err)
	}
	if strings.Contains(cypher, "MATCH") {
		t.Errorf("with createMissing there should be no MATCH:\n%s", cypher)
	}
	if strings.Count(cypher, "MERGE") != 3 {
		t.Errorf("want three MERGEs (both ends and the relationship):\n%s", cypher)
	}
	// No properties: no SET at all, so a re-run does not count as an update.
	if strings.Contains(cypher, "SET") {
		t.Errorf("with no properties there should be no SET:\n%s", cypher)
	}

	// The relationship type is an identifier and is quoted.
	cypher, _, err = relationshipStatement("WORKS`AT", from, to, nil, false)
	if err != nil {
		t.Fatalf("relationshipStatement: %v", err)
	}
	if !strings.Contains(cypher, "[r:`WORKS``AT`]") {
		t.Errorf("the type should be backtick-escaped:\n%s", cypher)
	}
}

func TestConstraintStatement(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		constraint string
		label      string
		properties []string
		dialect    dialect
		want       []string
		notWant    []string
	}{
		{
			name:       "unique on neo4j 5",
			kind:       kindUnique,
			label:      "Person",
			properties: []string{"email"},
			dialect:    dialectCurrent,
			want:       []string{"CREATE CONSTRAINT IF NOT EXISTS FOR (n:`Person`) REQUIRE (n.`email`) IS UNIQUE"},
		},
		{
			name:       "unique on neo4j 4",
			kind:       kindUnique,
			label:      "Person",
			properties: []string{"email"},
			dialect:    dialectLegacy,
			want:       []string{"ASSERT (n.`email`) IS UNIQUE"},
			notWant:    []string{"REQUIRE"},
		},
		{
			name:       "named composite node key",
			kind:       kindNodeKey,
			constraint: "person_key",
			label:      "Person",
			properties: []string{"tenantId", "externalId"},
			dialect:    dialectCurrent,
			want:       []string{"CREATE CONSTRAINT `person_key` IF NOT EXISTS", "REQUIRE (n.`tenantId`, n.`externalId`) IS NODE KEY"},
		},
		{
			name:       "exists on neo4j 5",
			kind:       kindExists,
			label:      "Person",
			properties: []string{"email"},
			dialect:    dialectCurrent,
			want:       []string{"REQUIRE n.`email` IS NOT NULL"},
		},
		{
			// 4.x spells existence with a function, not a predicate.
			name:       "exists on neo4j 4",
			kind:       kindExists,
			label:      "Person",
			properties: []string{"email"},
			dialect:    dialectLegacy,
			want:       []string{"ASSERT exists(n.`email`)"},
			notWant:    []string{"IS NOT NULL"},
		},
		{
			name:       "index",
			kind:       kindIndex,
			label:      "Person",
			properties: []string{"name"},
			dialect:    dialectCurrent,
			want:       []string{"CREATE INDEX IF NOT EXISTS FOR (n:`Person`) ON (n.`name`)"},
			notWant:    []string{"CONSTRAINT", "REQUIRE", "ASSERT"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := constraintStatement(tc.kind, tc.constraint, tc.label, tc.properties, tc.dialect)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("should not contain %q:\n%s", bad, got)
				}
			}
			// Every one of them must be safe to re-run.
			if !strings.Contains(got, "IF NOT EXISTS") {
				t.Errorf("the statement must be re-runnable:\n%s", got)
			}
		})
	}
}

// An index is spelled the same on both dialects, so the retry must not fire for
// one — it would only repeat the same failing statement.
func TestConstraintStatementIndexHasNoLegacyForm(t *testing.T) {
	current := constraintStatement(kindIndex, "", "Person", []string{"name"}, dialectCurrent)
	legacy := constraintStatement(kindIndex, "", "Person", []string{"name"}, dialectLegacy)
	if current != legacy {
		t.Errorf("an index should be dialect-independent:\n%s\n%s", current, legacy)
	}
}

func TestConstraintStatementQuotesIdentifiers(t *testing.T) {
	got := constraintStatement(kindUnique, "n`ame", "La`bel", []string{"pro`p"}, dialectCurrent)
	for _, want := range []string{"`n``ame`", "`La``bel`", "n.`pro``p`"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing escaped %q:\n%s", want, got)
		}
	}
}

func TestConstraintKind(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"unique", kindUnique, false},
		{"", kindUnique, false},
		{"Uniqueness", kindUnique, false},
		{"exists", kindExists, false},
		{"not null", kindExists, false},
		{"node key", kindNodeKey, false},
		{"node_key", kindNodeKey, false},
		{"NODE KEY", kindNodeKey, false},
		{"index", kindIndex, false},
		{"btree", kindIndex, false},
		{"full-text", "", true},
	}
	for _, tc := range tests {
		got, err := constraintKind(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("constraintKind(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("constraintKind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEndpoint(t *testing.T) {
	ref, err := endpoint("start", "Person", `{"id": 42}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if ref.labels != "`Person`" || ref.props["id"] != int64(42) {
		t.Errorf("ref = %+v", ref)
	}

	// Matching on the label alone would select every node with it, and the
	// MERGE would then fan out into one relationship per matched row.
	for _, match := range []string{"", "   ", "{}"} {
		if _, err := endpoint("start", "Person", match); err == nil {
			t.Errorf("endpoint with match %q should be refused", match)
		}
	}
	if _, err := endpoint("end", "Person", "not json"); err == nil {
		t.Error("an unparseable match should be refused")
	}
	if _, err := endpoint("end", "", `{"id":1}`); err == nil {
		t.Error("an endpoint with no label should be refused")
	}
}

func TestPreview(t *testing.T) {
	if got := preview("MATCH (n)\n  RETURN  n"); got != "MATCH (n) RETURN n" {
		t.Errorf("preview = %q", got)
	}
	long := preview(strings.Repeat("MATCH (n) RETURN n ", 40))
	if len([]rune(long)) > 118 {
		t.Errorf("preview should be trimmed for a progress frame, got %d runes", len([]rune(long)))
	}
}
