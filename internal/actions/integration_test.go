package actions

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Inflowenger/neo4j-plugin/internal/neo4j"
)

// TestIntegration exercises the real database path — a constraint created and
// then re-created as a no-op, a MERGE upsert that creates and then updates the
// same node, a relationship merged between two nodes, a typed-parameter read,
// and the schema lookups the forms use — against a live Neo4j. It is skipped
// unless NEO4J_TEST_URI is set, so `go test ./...` stays hermetic.
//
//	NEO4J_TEST_URI=neo4j://localhost:7687 \
//	NEO4J_TEST_USER=neo4j NEO4J_TEST_PASSWORD=Your_password123 \
//	  go test ./internal/actions -run TestIntegration -v
//
// A throwaway server to point it at:
//
//	docker run -d --name neo4j-it -p 7474:7474 -p 7687:7687 \
//	  -e NEO4J_AUTH=neo4j/Your_password123 neo4j:5
//
// Everything it writes carries the :InflowIT label and is deleted on the way
// out, so it is safe against a scratch database — but not against one with data
// you care about.
func TestIntegration(t *testing.T) {
	uri := os.Getenv("NEO4J_TEST_URI")
	if uri == "" {
		t.Skip("set NEO4J_TEST_URI to run the live-database integration test")
	}

	settings := map[string]any{
		"uri":      uri,
		"user":     os.Getenv("NEO4J_TEST_USER"),
		"password": os.Getenv("NEO4J_TEST_PASSWORD"),
		"database": os.Getenv("NEO4J_TEST_DATABASE"),
	}
	if settings["password"] == "" {
		settings["auth"] = "none"
	}

	client, err := neo4j.NewPool().Client(settings)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := client.Ping(ctx)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Logf("connected to %s (%s)", client.ConnInfo(), info.Agent)

	const label = "InflowIT"
	const company = "InflowITCompany"

	wipe := func(c context.Context) {
		for _, l := range []string{label, company} {
			_, _ = client.Write(c, fmt.Sprintf("MATCH (n:%s) DETACH DELETE n", neo4j.QuoteIdent(l)), nil, 0)
		}
	}
	// Start clean, however a previous run left off.
	wipe(ctx)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		wipe(cleanup)
		_, _ = client.Write(cleanup, "DROP CONSTRAINT inflow_it_email IF EXISTS", nil, 0)
	})

	// ------------------------------------------- constraint, and the re-run --

	statement := constraintStatement(kindUnique, "inflow_it_email", label, []string{"email"}, dialectCurrent)
	res, err := client.Write(ctx, statement, nil, 0)
	if err != nil {
		// A 4.x server rejects REQUIRE; the action retries in the old dialect,
		// so the test does too.
		if !neo4j.IsSyntaxError(err) {
			t.Fatalf("create constraint: %v\n%s", err, statement)
		}
		statement = constraintStatement(kindUnique, "inflow_it_email", label, []string{"email"}, dialectLegacy)
		if res, err = client.Write(ctx, statement, nil, 0); err != nil {
			t.Fatalf("create constraint (legacy dialect): %v\n%s", err, statement)
		}
		t.Log("server speaks the Neo4j 4 constraint dialect")
	}
	if res.Summary.Counters.ConstraintsAdded != 1 {
		t.Errorf("constraintsAdded = %d, want 1", res.Summary.Counters.ConstraintsAdded)
	}
	// The whole point of IF NOT EXISTS: running the same node twice is a no-op,
	// not an "already exists" failure.
	res, err = client.Write(ctx, statement, nil, 0)
	if err != nil {
		t.Fatalf("re-running the constraint should be a no-op: %v", err)
	}
	if res.Summary.Counters.ConstraintsAdded != 0 {
		t.Errorf("re-run added %d constraints, want 0", res.Summary.Counters.ConstraintsAdded)
	}

	// ----------------------------------------------- upsert: create, then update --

	quoted, _, err := neo4j.Labels(label)
	if err != nil {
		t.Fatal(err)
	}
	keyProps := map[string]any{"email": "ada@example.com"}
	props := map[string]any{"email": "ada@example.com", "name": "Ada", "born": int64(1815)}

	cypher, params, err := upsertNodeStatement(quoted, keyProps, props)
	if err != nil {
		t.Fatal(err)
	}
	res, err = client.Write(ctx, cypher, params, 1)
	if err != nil {
		t.Fatalf("upsert (create): %v\n%s", err, cypher)
	}
	if res.Summary.Counters.NodesCreated != 1 {
		t.Errorf("nodesCreated = %d, want 1", res.Summary.Counters.NodesCreated)
	}

	// The same key again must update the node, not make a second one — which is
	// what the uniqueness constraint above would otherwise refuse.
	props["name"] = "Ada Lovelace"
	cypher, params, err = upsertNodeStatement(quoted, keyProps, props)
	if err != nil {
		t.Fatal(err)
	}
	res, err = client.Write(ctx, cypher, params, 1)
	if err != nil {
		t.Fatalf("upsert (update): %v\n%s", err, cypher)
	}
	if res.Summary.Counters.NodesCreated != 0 {
		t.Errorf("the second upsert created %d nodes, want 0", res.Summary.Counters.NodesCreated)
	}
	node, ok := res.Rows[0]["n"].(map[string]any)
	if !ok {
		t.Fatalf("the upsert returned %#v, want a node", res.Rows[0]["n"])
	}
	returned, _ := node["properties"].(map[string]any)
	if returned["name"] != "Ada Lovelace" {
		t.Errorf("name = %v, want Ada Lovelace", returned["name"])
	}
	// A whole number must come back a whole number, not 1815.0.
	if born, isInt := returned["born"].(int64); !isInt || born != 1815 {
		t.Errorf("born = %#v, want int64(1815)", returned["born"])
	}

	// ------------------------------------------------------------ relationship --

	from := nodeRef{side: "start", labels: quoted, rawLabel: label, props: keyProps}
	to := nodeRef{side: "end", labels: neo4j.QuoteIdent(company), rawLabel: company, props: map[string]any{"name": "Acme"}}

	// Without createMissing the end node does not exist yet, so nothing is
	// written — and the diagnosis must name the end node.
	cypher, params, err = relationshipStatement("WORKS_AT", from, to, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err = client.Write(ctx, cypher, params, 1)
	if err != nil {
		t.Fatalf("relationship (match): %v\n%s", err, cypher)
	}
	if res.RowCount != 0 {
		t.Errorf("rowCount = %d, want 0 — the end node should not exist yet", res.RowCount)
	}
	if err := missingEndpoint(ctx, client, from, to); err == nil {
		t.Error("missingEndpoint should report an error")
	} else if !strings.Contains(err.Error(), company) {
		t.Errorf("the error should name the missing end node: %s", err)
	}

	// With createMissing both ends and the relationship are written.
	cypher, params, err = relationshipStatement("WORKS_AT", from, to, map[string]any{"since": int64(2019)}, true)
	if err != nil {
		t.Fatal(err)
	}
	res, err = client.Write(ctx, cypher, params, 1)
	if err != nil {
		t.Fatalf("relationship (merge): %v\n%s", err, cypher)
	}
	if res.Summary.Counters.RelationshipsCreated != 1 {
		t.Errorf("relationshipsCreated = %d, want 1", res.Summary.Counters.RelationshipsCreated)
	}
	// Re-running must update rather than duplicate.
	res, err = client.Write(ctx, cypher, params, 1)
	if err != nil {
		t.Fatalf("relationship (re-run): %v", err)
	}
	if res.Summary.Counters.RelationshipsCreated != 0 {
		t.Errorf("the second run created %d relationships, want 0", res.Summary.Counters.RelationshipsCreated)
	}

	// -------------------------------------------------------- read with params --

	read, err := client.Read(ctx,
		fmt.Sprintf("MATCH (p:%s)-[r:`WORKS_AT`]->(c) WHERE p.email = $email RETURN p, r, c", quoted),
		map[string]any{"email": "ada@example.com"}, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.RowCount != 1 {
		t.Fatalf("rowCount = %d, want 1", read.RowCount)
	}
	for _, key := range []string{"p", "r", "c"} {
		if _, ok := read.Rows[0][key].(map[string]any); !ok {
			t.Errorf("%s = %#v, want a mapped entity", key, read.Rows[0][key])
		}
	}

	// A write refused in a read transaction is what the read action relies on
	// for the writes its leading-keyword check cannot see.
	if _, err := client.Read(ctx, fmt.Sprintf("MATCH (n:%s) SET n.touched = true RETURN n", quoted), nil, 1); err == nil {
		t.Error("a write inside a read transaction should be refused by the server")
	}

	// ------------------------------------------------------- the form lookups --

	labels, err := client.Labels(ctx)
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	if !containsString(labels, label) {
		t.Errorf("labels %v should include %s", labels, label)
	}

	types, err := client.RelationshipTypes(ctx)
	if err != nil {
		t.Fatalf("relationship types: %v", err)
	}
	if !containsString(types, "WORKS_AT") {
		t.Errorf("types %v should include WORKS_AT", types)
	}

	keys, err := client.PropertyKeys(ctx, label)
	if err != nil {
		t.Fatalf("property keys: %v", err)
	}
	for _, want := range []string{"email", "name", "born"} {
		if !containsString(keys, want) {
			t.Errorf("property keys %v should include %s", keys, want)
		}
	}

	// ------------------------------------------- the wider type system, live --

	// Each of these arrives over Bolt as a distinct driver type, and the mapping
	// is a type switch — so a value that falls through to the default case
	// marshals as an anonymous struct instead of the shape documented in the
	// README. Reading them from the server is the only way to prove the switch
	// sees what the driver actually sends.
	typed, err := client.Read(ctx, `RETURN
		date('2024-03-01')                            AS d,
		localtime('14:30:05')                         AS lt,
		time('14:30:05+01:00')                        AS t,
		localdatetime('2024-03-01T14:30:05')          AS ldt,
		datetime('2024-03-01T14:30:05Z')              AS dt,
		duration({months: 1, days: 2, seconds: 3})    AS dur,
		point({x: 1.5, y: 2.5})                       AS p2,
		point({x: 1.0, y: 2.0, z: 3.0})               AS p3,
		[1, 2, 3]                                     AS list,
		{a: 1}                                        AS map,
		null                                          AS nothing`, nil, 1)
	if err != nil {
		t.Fatalf("typed read: %v", err)
	}
	row := typed.Rows[0]

	for _, tc := range []struct{ key, want string }{
		{"d", "2024-03-01"},
		{"lt", "14:30:05"},
		{"t", "14:30:05+01:00"},
		{"ldt", "2024-03-01T14:30:05"},
		{"dt", "2024-03-01T14:30:05Z"},
	} {
		if got, ok := row[tc.key].(string); !ok || got != tc.want {
			t.Errorf("%s = %#v, want %q", tc.key, row[tc.key], tc.want)
		}
	}
	// Months, days and seconds are not interchangeable, so only the ISO text is
	// lossless.
	if got, ok := row["dur"].(string); !ok || !strings.HasPrefix(got, "P") {
		t.Errorf("dur = %#v, want ISO-8601 duration text", row["dur"])
	}
	p2, ok := row["p2"].(map[string]any)
	if !ok || p2["x"] != 1.5 || p2["y"] != 2.5 || p2["srid"] == nil {
		t.Errorf("p2 = %#v, want {srid, x, y}", row["p2"])
	}
	if p3, ok := row["p3"].(map[string]any); !ok || p3["z"] != 3.0 {
		t.Errorf("p3 = %#v, want a z as well", row["p3"])
	}
	if list, ok := row["list"].([]any); !ok || len(list) != 3 || list[0] != int64(1) {
		t.Errorf("list = %#v, want [1 2 3] of int64", row["list"])
	}
	if m, ok := row["map"].(map[string]any); !ok || m["a"] != int64(1) {
		t.Errorf("map = %#v, want {a: int64(1)}", row["map"])
	}
	if row["nothing"] != nil {
		t.Errorf("nothing = %#v, want nil", row["nothing"])
	}

	// SHOW DATABASES backs the settings form's Databases button. It needs a 4.0+
	// server and the privilege to run it, so a failure here is advice rather
	// than a broken lookup — but on a stock server it must work.
	if databases, err := client.Databases(ctx); err != nil {
		t.Logf("SHOW DATABASES not available (the button degrades to a typed field): %v", err)
	} else if !containsString(databases, "system") {
		t.Errorf("databases %v should include system", databases)
	}

	// A truncated read has to say so, since the cap is applied as rows arrive.
	many, err := client.Read(ctx, "UNWIND range(1, 50) AS i RETURN i", nil, 10)
	if err != nil {
		t.Fatalf("truncated read: %v", err)
	}
	if many.RowCount != 10 || !many.Truncated {
		t.Errorf("rowCount = %d, truncated = %v, want 10 and true", many.RowCount, many.Truncated)
	}
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
