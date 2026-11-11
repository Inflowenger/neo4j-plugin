package neo4j

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	neo "github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Client is a ready-to-use Bolt driver for one connection. Read and Write are
// the two halves of the plugin, and both run their Cypher inside a *managed*
// transaction: the driver retries a transient failure (a leader election, a
// deadlock, a dropped connection) on its own, which is the behaviour a workflow
// node wants and the reason the plugin never opens an explicit transaction.
type Client struct {
	driver   neo.DriverWithContext
	cfg      Config
	connInfo string
}

// Driver limits for one settings profile. The pool stays small — a workflow node
// runs a statement and leaves, so connections idle more than they work — and the
// retry window is kept well inside the job timeout so a failing cluster fails
// the node with the server's own error rather than by running out of time.
const (
	maxConnectionPoolSize   = 10
	maxTransactionRetryTime = 20 * time.Second
	defaultConnectTimeout   = 10 * time.Second
	userAgent               = "inflow-neo4j-plugin"
)

// New builds the driver for a config. The driver is lazy — nothing is dialled
// until the first session needs a connection — so a wrong host fails on the
// first statement, or in the connection test, rather than here. A malformed URI
// or an unusable auth scheme does fail here.
func New(cfg Config) (*Client, error) {
	token, err := authToken(cfg)
	if err != nil {
		return nil, err
	}

	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}

	// Encryption is carried by the URI scheme (see Config.URI): the driver
	// refuses a TLS config supplied alongside a +s or +ssc scheme, so none is
	// set here.
	driver, err := neo.NewDriverWithContext(cfg.URI(), token, func(c *neo.Config) {
		c.MaxConnectionPoolSize = maxConnectionPoolSize
		c.MaxTransactionRetryTime = maxTransactionRetryTime
		c.SocketConnectTimeout = connectTimeout
		c.ConnectionAcquisitionTimeout = connectTimeout + maxTransactionRetryTime
		c.UserAgent = userAgent
	})
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	return &Client{driver: driver, cfg: cfg, connInfo: cfg.Describe()}, nil
}

// authToken renders the profile's authentication choice as the driver's token.
func authToken(cfg Config) (neo.AuthToken, error) {
	switch cfg.Auth {
	case AuthNone:
		return neo.NoAuth(), nil
	case AuthBearer:
		return neo.BearerAuth(cfg.Password), nil
	case AuthKerberos:
		return neo.KerberosAuth(cfg.Password), nil
	case AuthBasic, "":
		user := cfg.User
		if user == "" {
			// Every Neo4j install ships with this one administrative user, and a
			// profile that gives a password but no user means it.
			user = "neo4j"
		}
		return neo.BasicAuth(user, cfg.Password, cfg.Realm), nil
	default:
		return neo.AuthToken{}, fmt.Errorf("unknown authentication type %q: use basic, none, bearer or kerberos", cfg.Auth)
	}
}

// Close releases the driver and its connection pool. It is called when a client
// is evicted from the pool's cache.
func (c *Client) Close(ctx context.Context) { _ = c.driver.Close(ctx) }

// ConnInfo is host:port/database — enough to say which database a node is
// talking to, with no credentials in it.
func (c *Client) ConnInfo() string { return c.connInfo }

// Database is the database statements run against, or "" for the server's
// default.
func (c *Client) Database() string { return c.cfg.Database }

// ServerInfo is what the connection test reports back.
type ServerInfo struct {
	Address  string `json:"address"`
	Agent    string `json:"agent"`
	Protocol string `json:"protocol"`
	Database string `json:"database,omitempty"`
	Routing  bool   `json:"routing"`
}

// Ping verifies the connection is live and names the server, for the "Test
// connection" button.
//
// VerifyConnectivity is the cheap half — it acquires a connection (and, on a
// `neo4j://` URI, fetches the routing table) without running a statement.
// GetServerInfo then reports the agent string, which is the one piece of
// feedback that proves the plugin reached Neo4j itself and not something else
// listening on the port.
func (c *Client) Ping(ctx context.Context) (ServerInfo, error) {
	if err := c.driver.VerifyConnectivity(ctx); err != nil {
		return ServerInfo{}, err
	}
	info := ServerInfo{
		Address:  c.cfg.Address,
		Database: c.cfg.Database,
		Routing:  c.cfg.Routing(),
	}
	server, err := c.driver.GetServerInfo(ctx)
	if err != nil {
		// Connectivity is proven; not being able to describe the server is not
		// a failed test.
		return info, nil
	}
	info.Agent = server.Agent()
	info.Address = server.Address()
	info.Protocol = fmt.Sprintf("%d.%d", server.ProtocolVersion().Major, server.ProtocolVersion().Minor)
	return info, nil
}

// Result is what one statement returns: the return keys in order, the records
// as JSON-able maps, and the server's own summary of what it did.
type Result struct {
	Keys      []string         `json:"keys"`
	Rows      []map[string]any `json:"rows"`
	RowCount  int              `json:"rowCount"`
	Truncated bool             `json:"truncated"`
	Summary   Summary          `json:"summary"`
}

// Summary is the part of the server's result summary worth committing to the
// flow: what the statement changed, and where it ran.
type Summary struct {
	Counters         Counters `json:"counters"`
	ContainsUpdates  bool     `json:"containsUpdates"`
	Database         string   `json:"database,omitempty"`
	Server           string   `json:"server,omitempty"`
	AvailableAfterMs int64    `json:"availableAfterMs"`
	ConsumedAfterMs  int64    `json:"consumedAfterMs"`
	Notifications    []string `json:"notifications,omitempty"`
}

// Counters is the server's own account of what a write did. Every field is
// reported, including the zeros, so a flow can read one without checking
// whether the key exists.
type Counters struct {
	NodesCreated         int `json:"nodesCreated"`
	NodesDeleted         int `json:"nodesDeleted"`
	RelationshipsCreated int `json:"relationshipsCreated"`
	RelationshipsDeleted int `json:"relationshipsDeleted"`
	PropertiesSet        int `json:"propertiesSet"`
	LabelsAdded          int `json:"labelsAdded"`
	LabelsRemoved        int `json:"labelsRemoved"`
	IndexesAdded         int `json:"indexesAdded"`
	IndexesRemoved       int `json:"indexesRemoved"`
	ConstraintsAdded     int `json:"constraintsAdded"`
	ConstraintsRemoved   int `json:"constraintsRemoved"`
	SystemUpdates        int `json:"systemUpdates"`
}

// Read runs Cypher in a read transaction. On a `neo4j://` (routing) URI the
// driver sends it to a follower, which is what keeps reporting queries off the
// leader; the server also refuses a write in this mode, so a mistyped MERGE
// fails here instead of changing the graph.
//
// maxRows caps how many records are read; 0 means no cap. Cypher has a real
// LIMIT clause, so prefer putting the cap in the statement — this one is applied
// as the records arrive, and sets Truncated when it stopped early.
func (c *Client) Read(ctx context.Context, cypher string, params map[string]any, maxRows int) (Result, error) {
	return c.run(ctx, neo.AccessModeRead, cypher, params, maxRows)
}

// Write runs Cypher in a write transaction, routed to the leader.
func (c *Client) Write(ctx context.Context, cypher string, params map[string]any, maxRows int) (Result, error) {
	return c.run(ctx, neo.AccessModeWrite, cypher, params, maxRows)
}

// run opens a session, executes the statement in a managed transaction, and
// drains the records into JSON-able maps.
//
// Everything is read inside the transaction function on purpose: a record is
// only valid while its result is open, and the managed transaction commits (or
// rolls back and retries) around the function. Collecting into plain maps there
// is what makes the result safe to return and commit.
func (c *Client) run(ctx context.Context, mode neo.AccessMode, cypher string, params map[string]any, maxRows int) (Result, error) {
	session := c.driver.NewSession(ctx, neo.SessionConfig{
		DatabaseName: c.cfg.Database,
		AccessMode:   mode,
	})
	defer func() { _ = session.Close(ctx) }()

	work := func(tx neo.ManagedTransaction) (any, error) {
		cursor, err := tx.Run(ctx, cypher, params)
		if err != nil {
			return nil, err
		}

		keys, err := cursor.Keys()
		if err != nil {
			return nil, err
		}

		out := Result{Keys: keys, Rows: make([]map[string]any, 0, 16)}
		for cursor.Next(ctx) {
			if maxRows > 0 && len(out.Rows) >= maxRows {
				out.Truncated = true
				break
			}
			record := cursor.Record()
			row := make(map[string]any, len(record.Keys))
			for i, key := range record.Keys {
				if i < len(record.Values) {
					row[key] = normalize(record.Values[i])
				}
			}
			out.Rows = append(out.Rows, row)
		}
		if err := cursor.Err(); err != nil {
			return nil, err
		}

		// Consume both finishes reading the stream (discarding anything left
		// after a truncated read) and yields the summary — the counters, where
		// it ran, and any notifications the planner raised.
		summary, err := cursor.Consume(ctx)
		if err != nil {
			return nil, err
		}
		out.RowCount = len(out.Rows)
		out.Summary = summarize(summary)
		return out, nil
	}

	var raw any
	var err error
	if mode == neo.AccessModeRead {
		raw, err = session.ExecuteRead(ctx, work)
	} else {
		raw, err = session.ExecuteWrite(ctx, work)
	}
	if err != nil {
		return Result{}, describeError(err)
	}
	result, ok := raw.(Result)
	if !ok {
		return Result{}, errors.New("unexpected result type from the transaction")
	}
	return result, nil
}

// summarize copies the parts of the driver's summary the flow can use.
func summarize(summary neo.ResultSummary) Summary {
	counters := summary.Counters()
	out := Summary{
		ContainsUpdates:  counters.ContainsUpdates(),
		AvailableAfterMs: summary.ResultAvailableAfter().Milliseconds(),
		ConsumedAfterMs:  summary.ResultConsumedAfter().Milliseconds(),
		Counters: Counters{
			NodesCreated:         counters.NodesCreated(),
			NodesDeleted:         counters.NodesDeleted(),
			RelationshipsCreated: counters.RelationshipsCreated(),
			RelationshipsDeleted: counters.RelationshipsDeleted(),
			PropertiesSet:        counters.PropertiesSet(),
			LabelsAdded:          counters.LabelsAdded(),
			LabelsRemoved:        counters.LabelsRemoved(),
			IndexesAdded:         counters.IndexesAdded(),
			IndexesRemoved:       counters.IndexesRemoved(),
			ConstraintsAdded:     counters.ConstraintsAdded(),
			ConstraintsRemoved:   counters.ConstraintsRemoved(),
			SystemUpdates:        counters.SystemUpdates(),
		},
	}
	if database := summary.Database(); database != nil {
		out.Database = database.Name()
	}
	if server := summary.Server(); server != nil {
		out.Server = server.Agent()
	}
	// Notifications are the server's advice about the statement — an index that
	// would help, a label that does not exist (so the match can never hit). They
	// are worth surfacing: a typo in a label is otherwise a silent empty result.
	for _, n := range summary.Notifications() {
		out.Notifications = append(out.Notifications, fmt.Sprintf("%s: %s", n.Code(), n.Description()))
	}
	return out
}

// describeError turns a driver error into one a flow author can act on. The
// server's own message is kept — it is specific and good — and the cases where
// it is not self-explanatory get the fix named.
func describeError(err error) error {
	var usage *neo.UsageError
	if errors.As(err, &usage) {
		return fmt.Errorf("the driver rejected the request: %s", usage.Message)
	}

	var neoErr *neo.Neo4jError
	if errors.As(err, &neoErr) {
		switch {
		case neoErr.Code == "Neo.ClientError.Security.Unauthorized":
			return fmt.Errorf("%s (check the user and password on the settings profile)", neoErr.Msg)
		case neoErr.Code == "Neo.ClientError.Security.AuthenticationRateLimit":
			return fmt.Errorf("%s (too many failed sign-ins — the server is throttling this client)", neoErr.Msg)
		case neoErr.Code == "Neo.ClientError.Database.DatabaseNotFound":
			return fmt.Errorf("%s (check the Database field on the settings profile)", neoErr.Msg)
		case neoErr.Code == "Neo.ClientError.Statement.AccessMode":
			return fmt.Errorf("%s — this is the read action: run a write with the Execute Cypher action", neoErr.Msg)
		case neoErr.Code == "Neo.ClientError.Statement.NotSystemDatabaseError":
			return fmt.Errorf("%s (set Database to \"system\" on the settings profile for this statement)", neoErr.Msg)
		}
		return fmt.Errorf("%s: %s", neoErr.Code, neoErr.Msg)
	}

	if neo.IsConnectivityError(err) {
		return fmt.Errorf("cannot reach the server: %w (check the URI, and whether this host may open a Bolt connection to it)", err)
	}
	return err
}

// SyntaxErrorCode is what the server answers with when a statement is not
// Cypher it understands — including Cypher that a *newer* server would accept,
// which is how a version difference shows up.
const SyntaxErrorCode = "Neo.ClientError.Statement.SyntaxError"

// IsSyntaxError reports whether the server rejected the statement as Cypher.
// It is what lets a caller retry a schema command in an older dialect.
//
// The error may arrive either as the driver's own type or as one describeError
// has already rendered, so both are checked — the second by the code it prints,
// which is why describeError keeps the code in the message.
func IsSyntaxError(err error) bool {
	if err == nil {
		return false
	}
	var neoErr *neo.Neo4jError
	if errors.As(err, &neoErr) {
		return neoErr.Code == SyntaxErrorCode
	}
	return strings.Contains(err.Error(), SyntaxErrorCode)
}

// metaLimit bounds how much the schema lookups read. They fill a form a person
// is waiting on, so they sample rather than scan.
const metaLimit = 500

// Labels lists the node labels in use, for the label pickers. db.labels() is a
// built-in procedure available on Community and Enterprise alike, and it reads
// the label token store rather than the graph, so it is cheap.
func (c *Client) Labels(ctx context.Context) ([]string, error) {
	res, err := c.Read(ctx, "CALL db.labels() YIELD label RETURN label ORDER BY label", nil, metaLimit)
	if err != nil {
		return nil, err
	}
	return columnOf(res, "label"), nil
}

// RelationshipTypes lists the relationship types in use, for the type picker.
func (c *Client) RelationshipTypes(ctx context.Context) ([]string, error) {
	res, err := c.Read(ctx, "CALL db.relationshipTypes() YIELD relationshipType RETURN relationshipType ORDER BY relationshipType", nil, metaLimit)
	if err != nil {
		return nil, err
	}
	return columnOf(res, "relationshipType"), nil
}

// PropertyKeys lists the property names actually present on nodes with a label,
// so the upsert form can offer a field per property.
//
// It samples the data instead of calling db.schema.nodeTypeProperties(): the
// sample is a plain read any user with read access can run, it reflects what the
// nodes really carry, and it cannot be refused for want of a privilege. The
// label is an identifier, so it is quoted into the statement rather than bound —
// Cypher has no parameter for a label.
func (c *Client) PropertyKeys(ctx context.Context, label string) ([]string, error) {
	if err := ValidIdent("label", label); err != nil {
		return nil, err
	}
	cypher := fmt.Sprintf(
		"MATCH (n:%s) WITH n LIMIT $sample UNWIND keys(n) AS key RETURN DISTINCT key ORDER BY key",
		QuoteIdent(label))
	res, err := c.Read(ctx, cypher, map[string]any{"sample": int64(metaLimit)}, metaLimit)
	if err != nil {
		return nil, err
	}
	return columnOf(res, "key"), nil
}

// Databases lists the databases on the server, for the settings form's picker.
// SHOW DATABASES needs a server at 4.0 or newer and the privilege to see them;
// on anything older, or without it, the caller treats the error as "just type
// the name".
func (c *Client) Databases(ctx context.Context) ([]string, error) {
	res, err := c.Read(ctx, "SHOW DATABASES YIELD name RETURN DISTINCT name ORDER BY name", nil, metaLimit)
	if err != nil {
		return nil, err
	}
	return columnOf(res, "name"), nil
}

// columnOf pulls one string column out of a result, skipping anything that is
// not a string.
func columnOf(res Result, key string) []string {
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		if value, ok := row[key].(string); ok && value != "" {
			out = append(out, value)
		}
	}
	return out
}
