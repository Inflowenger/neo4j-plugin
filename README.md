# Inflowenger Neo4j plugin

A Neo4j node for the Inflowenger workflow canvas, built on the
[`go-plugin-sdk`](https://github.com/Inflowenger/go-plugin-sdk) (`sdkv1`) under
the **inflowv1** protocol. It runs as a long-running Go process and is called by
the Fractal runtime over NATS.

It talks to any Neo4j reachable over Bolt with the official
[`neo4j-go-driver/v5`](https://github.com/neo4j/neo4j-go-driver) — a local
container, a self-managed server or cluster on-prem, or Aura — on Community or
Enterprise, version 4.4 and later.

The plugin holds **no** database configuration. A connection travels in every
call as a settings profile (`body.settings`), so one running plugin serves many
databases and rotating a password needs no redeploy.

## Actions

| Action | Method | What it does |
| --- | --- | --- |
| **Run Cypher (read)** | `neo4j.query` | Runs a read statement and returns the records, their key order, and the server's summary. Nodes, relationships, paths, temporals and points are mapped to plain JSON. Runs in a **read** transaction, which a cluster can answer from a follower. Returns **at most 100 rows**. |
| **Execute Cypher (write)** | `neo4j.execute` | Runs a write statement — `CREATE` / `MERGE` / `SET` / `DELETE`, or a schema command — and returns the server's counters plus anything the statement returns. Runs in a **write** transaction, routed to the leader. |
| **Upsert Node (MERGE)** | `neo4j.node.upsert` | Creates a node, or updates the one that already matches on the key properties. No Cypher to write: pick the label, load its properties into the form, fill them in. |
| **Link Nodes (MERGE relationship)** | `neo4j.relationship.upsert` | Creates a relationship between two nodes, or updates the one already between them. The endpoints are matched by label and properties, and optionally created when missing. |
| **Find Nodes** | `neo4j.node.find` | Matches nodes by label and properties and returns them, with a real `LIMIT` in the statement. |
| **Create Constraint / Index (if not exists)** | `neo4j.constraint.create` | Adds a uniqueness or existence constraint, a node key, or an index — only if it is not already there. |

Meta RPCs: `neo4j.meta.ping.check`, `neo4j.meta.database.pick`,
`neo4j.meta.label.pick`, `neo4j.meta.reltype.pick`.

Every action runs inside a **managed transaction**, so the driver retries a
transient failure — a leader election, a deadlock, a dropped connection — on its
own. The plugin never opens an explicit transaction.

## Connection

The settings profile is the only place a credential lives. Keys are matched
leniently: case, spaces, dashes and underscores are ignored, and the usual
synonyms are accepted (`uri` / `url` / `connectionString` / `boltUrl`,
`user` / `username` / `login`, `password` / `pass` / `secret` / `token`).

| Key | Required | What it is |
| --- | --- | --- |
| `uri` | **yes** | `neo4j://localhost:7687` for a local or on-prem server, `neo4j+s://xxxxxxxx.databases.neo4j.io` for Aura. `neo4j://` lets the driver route across a cluster; `bolt://` pins to one server. The port defaults to **7687**. A `host` (and optional `port`) is accepted instead. |
| `database` | no | e.g. `neo4j`. Empty uses the server's default — the only option before 4.0. The **Databases** button fills it in when the server has exactly one. |
| `auth` | no | `basic` (the default), `none`, `bearer` or `kerberos`. |
| `user` | no | Defaults to `neo4j`, the user every install ships with. |
| `password` | for `basic` | The password — or, for `bearer` / `kerberos`, the token or ticket. |
| `encryption` | no | `from URI` (default), `off`, `on`, `self-signed`. |
| `connectTimeout` | no | Seconds to wait for the TCP connection. Default 10. |

Credentials pasted into the URI (`neo4j://neo4j:secret@host:7687`) are lifted
out of it: the driver takes its credentials from the auth token and ignores a
URI's userinfo, so leaving them in place would silently mean "no password".

### Encryption is a URI scheme, not a flag

Neo4j selects TLS with the scheme — `neo4j+s://` (certificate must be trusted),
`neo4j+ssc://` (any certificate accepted) — and the driver **refuses** a TLS
config supplied alongside one of them. So the **Encryption** setting rewrites
the scheme rather than configuring TLS:

| Setting | `neo4j://db:7687` becomes |
| --- | --- |
| `from URI` | unchanged — what an Aura `neo4j+s://` URI wants |
| `off` | `neo4j://db:7687` |
| `on` | `neo4j+s://db:7687` |
| `self-signed` | `neo4j+ssc://db:7687` |

`self-signed` is what a development or on-prem server with its own certificate
needs. A managed service with a real certificate wants `on`.

### Connection test

The settings form's **Test connection** button verifies connectivity and names
the server it reached (`Neo4j/5.26.0 (localhost:7687)`), so a wrong URI or
password is reported while it is being typed rather than by every node that
later fails.

## Parameters, and values from the flow

Two different mechanisms, and it is worth keeping them straight.

**Cypher parameters** (`$name`) are bound by the driver. A value bound this way
can never alter the statement, and the server can cache the plan. Fill them in
the **Parameters** field as a JSON object:

```cypher
MATCH (u:User {id: $id})-[:FOLLOWS]->(f) RETURN f.name AS name LIMIT $limit
```

```json
{ "id": 42, "limit": 10 }
```

Types are kept: `42` is an integer, `true` a boolean, `null` a null, and a list
or object is passed through — which is what `UNWIND $rows AS row` wants.

**`{{$.path}}` tokens** are resolved by the plugin against the flow's scope
*before* the statement runs, in every text field. They work in both positions:

```json
{ "id": {{$.trigger.userId}}, "name": "{{$.user.name}}" }
```

A scope value that is a JSON string is substituted unquoted, so quote it
yourself in the JSON; anything else (a number, a boolean, an object) is
substituted as raw JSON, so it keeps its type. `$this` works too — it is the
location the current run was handed, so `{{$this.email}}` is that run's own
record.

A token the scope cannot supply is left in place verbatim rather than silently
blanked, which is why `Max rows` reports an error on `{{$.limit}}` instead of
quietly falling back.

## Upsert Node

Write one node, given one of two ways:

- **Per-property fields.** The **Label** field has a single ↻ *Labels /
  properties* button: press it with the box empty to list the labels in use,
  pick one, then press it again to load the properties nodes with that label
  carry (sampled from the data, so no special privilege is needed). Fill the
  ones you want; each accepts a literal or a `{{$.path}}` token, and a blank
  field is left alone.
- **A JSON object.** Paste `{ "property": value }` into **JSON properties**; it
  wins over the per-property fields when present.

**Key properties** is the field that decides whether the action is idempotent:

| Key properties | Statement | Re-running the flow |
| --- | --- | --- |
| `email` | `MERGE (n:Person {email: $key0}) SET n += $props` | updates the same node |
| *empty* | `CREATE (n:Person) SET n += $props` | makes another node |

A key naming a property with no value is refused rather than merged on a null —
that would create a node with a null property instead of finding the one meant.

Pair it with a uniqueness constraint on the same properties (the **Create
constraint** action): that is what turns the `MERGE` into an index lookup, and
what stops two concurrent runs both creating the node.

## Link Nodes

Matches both endpoints by label and properties, then `MERGE`s the relationship
between them, so re-running updates rather than duplicates:

```cypher
MATCH (a:`Person`  {`email`: $a0})
MATCH (b:`Company` {`name`:  $b0})
MERGE (a)-[r:`WORKS_AT`]->(b)
SET r += $props
RETURN r
```

A match is required on both ends: matching on the label alone would select every
node with it, and the `MERGE` would then fan out into one relationship per
matched row.

**Create the nodes if they are missing** switches both `MATCH`es to `MERGE`,
which is what an ingest flow with no ordering guarantee wants. Left off, a
missing endpoint fails the node — and the error says *which* endpoint and what
it looked for, which is the difference between a typo in a label and a node an
earlier step failed to write.

## Create Constraint / Index

| Kind | Statement | Needs |
| --- | --- | --- |
| `unique` | `REQUIRE (n.email) IS UNIQUE` | Community or Enterprise |
| `index` | `CREATE INDEX … ON (n.name)` | Community or Enterprise |
| `exists` | `REQUIRE n.email IS NOT NULL` | **Enterprise** |
| `node key` | `REQUIRE (n.tenantId, n.externalId) IS NODE KEY` | **Enterprise** |

Every statement carries `IF NOT EXISTS`, so this belongs at the front of a flow
that writes: re-running is a no-op, and the committed `created` field says
whether it was already there.

`REQUIRE` is Neo4j 5. A 4.x server rejects it as a syntax error, and the action
then retries the same command in the 4.x dialect (`ASSERT`, and `exists(n.p)`
for existence) — so it works against an on-prem server that has not moved to 5
yet, at the cost of one extra round trip on a server where the first form could
never have worked.

## What a node commits

The two Cypher actions commit the same shape:

```jsonc
{
  "keys":     ["n"],                  // the RETURN keys, in order
  "rows":     [ { "n": { … } } ],
  "rowCount": 1,
  "truncated": false,                 // true when the 100-row cap stopped the read
  "containsUpdates": true,
  "counters": { "nodesCreated": 1, "propertiesSet": 3, … },
  "summary":  { "database": "neo4j", "server": "Neo4j/5.26.0",
                "availableAfterMs": 2, "consumedAfterMs": 1,
                "notifications": [ … ] }
}
```

Neo4j's type system is wider than JSON's, so each value is mapped to an explicit
shape a downstream node can read:

| Neo4j | Committed as |
| --- | --- |
| node | `{ "elementId", "labels", "properties" }` |
| relationship | `{ "elementId", "type", "startElementId", "endElementId", "properties" }` |
| path | `{ "nodes", "relationships", "length" }` |
| `DATE` / `LOCAL TIME` / `TIME` / `LOCAL DATETIME` / `DATETIME` | ISO-8601 text at that type's own precision |
| `DURATION` | ISO-8601 text (`P1M2DT3S`) — months, days and seconds are not interchangeable |
| `POINT` | `{ "srid", "x", "y" }`, plus `"z"` in 3D |
| bytes | the string if it is UTF-8, otherwise base64 |

`elementId` is the identity to carry around. The old numeric `id` is deprecated
in the driver and is not stable across a restart or a cluster, so it is not
reported.

The graph actions commit what they wrote — `node` / `relationship` with its
`elementId` and `properties` lifted out, `created` (vs. updated), and the same
`counters`.

**Notifications** in the summary are the server's own advice: an index that
would help, or a label that does not exist so the match can never hit. A typo in
a label is otherwise a silent empty result.

## Safety

- **Values are always parameters.** Nothing a form collects is ever concatenated
  into a statement.
- **Identifiers are quoted.** Labels, relationship types and property names
  cannot be parameters in Cypher, so each is validated and backtick-quoted (an
  inner backtick is doubled) before it reaches the statement text.
- **The read action is a read.** It runs in a read transaction, which the server
  refuses to let write. A statement *starting* with a write is caught before the
  round trip and names the Execute action instead; a write buried mid-statement
  is caught by the server, and its refusal is translated into the same advice.
- **One driver per connection.** Clients are pooled on a hash of the resolved
  settings — never a package-level singleton — so one process serving many
  profiles cannot leak one tenant's connection into another's job. The hash
  never contains the password in plain text.
- **No credential is logged**, and none appears in any action form: the
  `TestNoConnectionFieldsOnActionForms` test walks every action's schema and
  fails if one does.

## Run

```bash
git clone <this repo>
cd neo4j
cp .env.inflow.example .env.inflow   # PLUGIN_ID / INFRA_CRED / INFRA_URL
go run .
```

The plugin must already be provisioned in a space — that is where `PLUGIN_ID`,
`INFRA_CRED` and `INFRA_URL` come from. See
[build-a-plugin.md § Provision](https://github.com/Inflowenger/plugin-catalog/blob/main/docs/build-a-plugin.md#1-provision-the-plugin).

On startup the SDK logs every subject it subscribed to; **that log is the
confirmation the plugin registered with Infra.** Then add the node to a flow and
bind a settings profile to it.

`INFLOW_ENV_FILE` reads a different dotenv (default `.env.inflow`).

## Test

```bash
go vet ./...
go test ./...
```

Everything worth testing runs offline: the forms (both documents parse, every
control points at a property the schema declares, every button calls a
registered meta method, no action form carries a connection field), the lenient
settings parsing (including the wrong-profile case and the encryption→scheme
rewrite), the request-envelope decoding, the value mapping, and every statement
builder — with the injection attempts that prove values are bound and
identifiers are quoted.

A live round trip against a throwaway server:

```bash
docker run -d --name neo4j-it -p 7474:7474 -p 7687:7687 \
  -e NEO4J_AUTH=neo4j/Your_password123 neo4j:5

NEO4J_TEST_URI=neo4j://localhost:7687 \
NEO4J_TEST_USER=neo4j NEO4J_TEST_PASSWORD=Your_password123 \
  go test ./internal/actions -run TestIntegration -v
```

It exercises the constraint and its no-op re-run, an upsert that creates and
then updates the same node, a relationship merged (and the diagnosis when an
endpoint is missing), a typed-parameter read, the refusal of a write in a read
transaction, the schema lookups the forms use, and a truncated read. Everything
it writes carries the `:InflowIT` label and is deleted on the way out — safe
against a scratch database, not against one with data you care about.

## Notes

- **100 rows** is the read cap. Cypher has a real `LIMIT`, so put the cap in the
  statement when you want the server to do less work — the plugin's cap can only
  be applied to rows as they arrive, and sets `truncated` when it stopped early.
  **Find Nodes** pushes its limit into the statement.
- **One statement per action.** Neo4j's Bolt protocol takes one statement per
  run, so a semicolon-separated script is a syntax error. Use several nodes, or
  `UNWIND` a list of parameters.
- **A schema command cannot share a transaction with data changes.** Neo4j
  refuses `CREATE CONSTRAINT` in the same transaction as a write, which is why
  the constraint action is its own node rather than a flag on the others.
- **`SHOW DATABASES`** needs a 4.0+ server and the privilege to run it. Without
  either, the **Databases** button says so and the field stays typable — it is
  advice, not a gate.
- **Enterprise-only kinds** (`exists`, `node key`) fail on Community with the
  server's own message. So does a composite `unique` constraint on 4.4, where
  only `node key` covered several properties.
