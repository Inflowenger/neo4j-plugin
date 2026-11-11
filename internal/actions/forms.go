package actions

import (
	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// Every form this plugin serves is declared here with the SDK's formkit builder,
// which generates the JSON Schema and the JSON Forms UI schema from one
// statement per field. A malformed form panics at start-up (Build calls
// Validate), where it is a compile-time-shaped mistake rather than a dialog
// that will not open.

const (
	descCypher = `Cypher to run. Bind values with named parameters — $name — and fill them in Parameters below (never paste a value into the statement text). Pull values from the flow with {{$.path}}, e.g. MATCH (u:User {id: $id}) with Parameters {"id": "{{$.trigger.userId}}"}.`

	descParams = `Optional JSON object of parameter name → value for the $name placeholders above, e.g. {"id": 42, "tags": ["a","b"]}. Values are bound by the driver, never spliced into the statement, so a value can never alter the query. Types are kept: 42 is an integer, true a boolean, null a null, and a list or object is passed through (which is what UNWIND $rows wants). A {{$.path}} token works in both positions — {"id": {{$.user.id}}} keeps a number a number, and {"name": "{{$.user.name}}"} quotes a string.`

	descLimit = `Most rows to return. Capped at 100 — a larger number is treated as 100. This cap is applied as rows arrive, so add a LIMIT to the statement when you want the server itself to do less work. Accepts a {{$.path}} token that resolves to a number.`

	descProps = `Optional JSON object of property → value, e.g. {"email": "a@b.c", "score": 7}. When given, it wins over the property fields above. Values may be {{$.path}} tokens: {"name": "{{$.user.name}}"}. A property value may be a number, string, boolean, null, or a list of those — Neo4j does not store an object as a property.`

	// Two match fields, two different rules: an endpoint of a relationship must
	// identify one node (matching on the label alone would attach a
	// relationship to every node that has it), while a search may legitimately
	// match on the label alone.
	descMatchEndpoint = `JSON object of property → value identifying the node, e.g. {"id": 42} or {"email": "{{$.user.email}}"}. Every property given must match, and at least one is required — matching on the label alone would select every node with it, and link them all.`

	descMatchFilter = `Optional JSON object of property → value to filter by, e.g. {"active": true} or {"email": "{{$.user.email}}"}. Every property given must match. Leave it empty to match on the label alone.`
)

// ---------------------------------------------------------------- settings --

// settingsForm is stored by the platform as a reusable settings profile and
// shipped with every call as body.settings. The plugin keeps nothing.
var settingsForm = formkit.New("Neo4j connection").
	Describe("Stored by the platform as a reusable settings profile and shipped with every call as body.settings. The plugin keeps nothing. Works against any Neo4j the Bolt protocol reaches — a local container, a self-managed server or cluster, or Aura.").
	SubmitTo("neo4j.meta.ping.check").
	Add(
		formkit.Text("uri", "Connection URI").Required().
			Describe("e.g. neo4j://localhost:7687 for a local or on-prem server, neo4j+s://xxxxxxxx.databases.neo4j.io for Aura. Use neo4j:// to let the driver route across a cluster, or bolt:// to pin to exactly one server. The port defaults to 7687."),
		formkit.Text("database", "Database").
			Describe("e.g. neo4j, or an application database. Empty uses the server's default — which is the only option on a server older than 4.0. Press ↻ to list the databases this connection can see.").
			Lookup("neo4j.meta.database.pick", "Databases"),
		formkit.Enum("auth", "Authentication", "basic", "none", "bearer", "kerberos").
			Default("basic").
			Describe("basic is a user and password, which is what a self-managed server and Aura both use. none is for a server started with authentication disabled (dbms.security.auth_enabled=false). bearer carries an SSO/OIDC token and kerberos a base64 service ticket — for those two, put the token in the password field."),
		formkit.Text("user", "User").Default("neo4j").
			Describe("The Neo4j user to act as. Every install ships with `neo4j`; a flow is better off with a user of its own, granted only what it needs."),
		formkit.Secret("password", "Password / token").
			Describe("The user's password — or, for bearer and kerberos authentication, the token or ticket."),
		formkit.Enum("encryption", "Encryption", "from URI", "off", "on", "self-signed").
			Default("from URI").
			Describe("Neo4j takes encryption from the URI scheme, so this rewrites it: off → neo4j://, on → neo4j+s:// (the certificate must be trusted), self-signed → neo4j+ssc:// (any certificate is accepted, which is what a development or on-prem server with its own certificate needs). Leave it on \"from URI\" when the URI already says — an Aura neo4j+s:// URI does."),
		formkit.Integer("connectTimeout", "Connect timeout (seconds)").Default(10).
			Describe("How long to wait for the TCP connection before giving up. 10 suits a server on the same network; raise it for a distant one."),
		// The button hangs off the last field and its answer appears under it, so
		// a bad connection is reported while it is being entered rather than by
		// every node that later fails. It writes no value — saying what it found
		// is the whole point.
		formkit.Text("test", "Test connection").
			Describe("Press ↻ to check these values against the live server before saving.").
			Lookup("neo4j.meta.ping.check", "Test connection"),
	).
	Build()

// ------------------------------------------------------------------ actions --

var queryForm = formkit.New("Run Cypher (read)").Add(
	formkit.TextArea("cypher", "Cypher").Required().
		Describe(descCypher).
		Help("Read-only: this runs in a read transaction, which the server refuses to let write — and which a cluster can answer from a follower. Writes belong on the Execute Cypher action."),
	formkit.TextArea("params", "Parameters").Describe(descParams).
		Help(`For the example above: {"id": "{{$.trigger.userId}}"}`),
	formkit.Text("limit", "Max rows").Default("100").
		Describe(descLimit),
).Build()

var executeForm = formkit.New("Execute Cypher (write)").Add(
	formkit.TextArea("cypher", "Cypher").Required().
		Describe(descCypher).
		Help("CREATE / MERGE / SET / DELETE, and the schema commands. Runs in a write transaction, routed to the leader. Anything the statement RETURNs comes back alongside the counters."),
	formkit.TextArea("params", "Parameters").Describe(descParams),
).Build()

// nodeUpsertFormDef is kept as the builder (not just its Built form) because
// the property-load meta rebuilds the dialog from it — SchemaMap / UIMap give it
// a fresh copy to splice the discovered property fields into. See
// propertiesEnvelope.
var nodeUpsertFormDef = formkit.New("Upsert node (MERGE)").
	Describe("Create a node, or update the one that already matches. Pick the label, press ↻ again to load the properties nodes with that label already carry, and fill the ones you want — or paste a JSON object below.").
	Add(
		// One button does both steps: with the box empty it lists the labels to
		// pick from; once a label is set, pressing it again loads that label's
		// properties as the value fields below. See metaLabelPick.
		formkit.Text("label", "Label").Required().
			Describe("The node label, e.g. Person. Press ↻ with the box empty to list the labels in use; pick one, then press ↻ again to load its properties as fields below. Several labels may be given as Person:Employee.").
			Lookup("neo4j.meta.label.pick", "Labels / properties").
			Picks("neo4j.node.upsert"),
		formkit.Text("key", "Key properties").
			Describe("Comma-separated names of the properties that identify the node, e.g. id or email. The node is MERGEd on those, so running the flow twice updates the same node instead of making a second one. Leave it empty to CREATE a new node every time. A uniqueness constraint on the key properties is what makes this both fast and safe — the Create constraint action adds one."),
		formkit.TextArea("record", "JSON properties (optional)").
			Describe(descProps),
	)

var nodeUpsertForm = nodeUpsertFormDef.Build()

var relationshipForm = formkit.New("Link nodes (MERGE relationship)").
	Describe("Create a relationship between two existing nodes, or update the one that is already there.").
	Add(
		formkit.Text("type", "Relationship type").Required().
			Describe("e.g. WORKS_AT. By convention a type is UPPER_SNAKE_CASE. Press ↻ to list the types already in use.").
			Lookup("neo4j.meta.reltype.pick", "Types").
			Picks("neo4j.relationship.upsert"),
	).
	Group("Start node",
		formkit.Text("fromLabel", "Start label").Required().
			Describe("The label of the node the relationship points from, e.g. Person.").
			Lookup("neo4j.meta.label.pick", "Labels").
			Picks("neo4j.relationship.upsert"),
		formkit.TextArea("fromMatch", "Start node match").Required().
			Describe(descMatchEndpoint),
	).
	Group("End node",
		formkit.Text("toLabel", "End label").Required().
			Describe("The label of the node the relationship points to, e.g. Company.").
			Lookup("neo4j.meta.label.pick", "Labels").
			Picks("neo4j.relationship.upsert"),
		formkit.TextArea("toMatch", "End node match").Required().
			Describe(descMatchEndpoint),
	).
	Add(
		formkit.TextArea("properties", "Relationship properties").
			Describe(`Optional JSON object of property → value set on the relationship, e.g. {"since": 2019}. Values may be {{$.path}} tokens.`),
		formkit.Bool("createMissing", "Create the nodes if they are missing").Default(false).
			Describe("Off, both nodes must already exist and the action fails if either match finds nothing — which is what you want when the nodes come from an earlier step. On, a missing node is created from the label and the match properties (a MERGE), which is what an ingest flow with no ordering guarantee wants."),
	).
	Build()

var nodeFindForm = formkit.New("Find nodes").Add(
	formkit.Text("label", "Label").
		Describe("The node label to match, e.g. Person. Empty matches nodes of any label — with a Match below, or a small Max nodes, since that scans the graph.").
		Lookup("neo4j.meta.label.pick", "Labels").
		Picks("neo4j.node.find"),
	formkit.TextArea("match", "Match").
		Describe(descMatchFilter),
	formkit.Text("limit", "Max nodes").Default("100").
		Describe("Most nodes to return, capped at 100. Unlike the Cypher read action, this becomes a real LIMIT in the statement, so the server stops looking once it has enough."),
).Build()

var constraintForm = formkit.New("Create constraint / index (if not exists)").
	Describe("Add the schema a graph needs to stay correct and fast. Re-running is a no-op, so this belongs at the front of a flow that writes.").
	Add(
		formkit.Enum("kind", "Kind", "unique", "exists", "node key", "index").
			Default("unique").
			Describe("unique — no two nodes with this label may share these property values (and MERGE on them becomes an index lookup). exists — the property may not be null. node key — unique and not-null together, over the whole set of properties. index — no constraint, just an index to make lookups fast. exists and node key need Neo4j Enterprise; unique and index work on Community too."),
		formkit.Text("label", "Label").Required().
			Describe("The node label the constraint is on, e.g. Person.").
			Lookup("neo4j.meta.label.pick", "Labels").
			Picks("neo4j.constraint.create"),
		formkit.Text("properties", "Properties").Required().
			Describe("Comma-separated property names, e.g. email, or tenantId, externalId for a constraint over both together."),
		formkit.Text("name", "Name").
			Describe("Optional name for the constraint or index, e.g. person_email_unique. Empty lets the server name it. A name is what makes it droppable later by anything other than its definition."),
	).Build()

// formByMethod lets one lookup handler serve several forms: a label picker
// rebuilds whichever action's dialog is open, and the button says which that is
// (Field.Picks, posted as `form`).
var formByMethod = map[string]sdkv1.FormBuilder{
	"neo4j.node.upsert":         nodeUpsertForm,
	"neo4j.relationship.upsert": relationshipForm,
	"neo4j.node.find":           nodeFindForm,
	"neo4j.constraint.create":   constraintForm,
}
