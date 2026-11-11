// Package neo4j turns the settings profile the platform ships with every call
// into a live Bolt driver, and runs read and write Cypher over it.
//
// The plugin holds no database configuration of its own. A connection travels
// in each request as `body.settings`, the same named profile the runtime folds
// into action bodies — so one running plugin can serve many Neo4j instances
// (a local container, an on-prem cluster, an Aura database) and rotating a
// password needs no redeploy.
package neo4j

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNoSettings is returned when a call arrives without a connection. It is
// phrased for the person looking at the node, since that is where the fix is.
var ErrNoSettings = errors.New("this node has no Neo4j connection: pick a settings profile in the node drawer (URI, user, password) or create one from the plugin's set-up form")

// Auth names how the driver authenticates. Basic covers a self-managed server
// and Aura alike; none is an instance started with authentication disabled;
// bearer carries an SSO/OIDC token, and kerberos a service ticket.
type Auth string

const (
	AuthBasic    Auth = "basic"
	AuthNone     Auth = "none"
	AuthBearer   Auth = "bearer"
	AuthKerberos Auth = "kerberos"
)

// Encryption is what the profile asks for on the wire. Neo4j takes encryption
// from the URI scheme rather than from a flag, so this is applied by rewriting
// the scheme — see Config.URI. Empty means "whatever the URI already says",
// which is what a pasted Aura URI (neo4j+s://…) wants.
type Encryption string

const (
	EncryptFromURI    Encryption = ""
	EncryptOff        Encryption = "off"
	EncryptOn         Encryption = "on"
	EncryptSelfSigned Encryption = "self-signed"
)

// defaultBoltPort is the port Neo4j listens on for Bolt out of the box.
const defaultBoltPort = "7687"

// Config is a resolved Neo4j connection.
type Config struct {
	// Raw is the URI as the profile gave it, before the scheme was normalised.
	Raw string
	// Scheme is one of the eight the driver accepts: neo4j, neo4j+s,
	// neo4j+ssc, bolt, bolt+s, bolt+ssc, bolt+unix.
	Scheme string
	// Address is host:port, or the socket path for bolt+unix.
	Address string
	// Database is the database to run against. Empty uses the server's default
	// (`neo4j` on a stock install), which is also the only option on a server
	// older than 4.0.
	Database       string
	Auth           Auth
	User           string
	Password       string
	Realm          string
	Encryption     Encryption
	ConnectTimeout time.Duration
}

// ParseConfig reads a connection out of the settings profile the platform ships
// with every action call as `body.settings`.
//
// The profile is a free-form key/value record: it is filled from the plugin's
// own set-up form when the platform could read it, and hand-typed otherwise. So
// keys are matched leniently — case, spaces, dashes and underscores are ignored,
// and the usual synonyms for each field are accepted.
func ParseConfig(settings map[string]any) (Config, error) {
	if len(settings) == 0 {
		return Config{}, ErrNoSettings
	}

	values := make(map[string]string, len(settings))
	for key, value := range settings {
		if text := toString(value); text != "" {
			values[canonicalKey(key)] = text
		}
	}

	cfg := Config{
		Raw:        pick(values, uriKeys),
		Database:   pick(values, databaseKeys),
		User:       pick(values, userKeys),
		Password:   pick(values, passwordKeys),
		Realm:      pick(values, realmKeys),
		Auth:       parseAuth(pick(values, authKeys)),
		Encryption: parseEncryption(pick(values, encryptKeys)),
	}
	if seconds := pick(values, timeoutKeys); seconds != "" {
		if n, err := strconv.Atoi(seconds); err == nil && n > 0 {
			cfg.ConnectTimeout = time.Duration(n) * time.Second
		}
	}

	// A profile may carry a whole URI, or just a host (and maybe a port) the way
	// a person would write one down. Either is resolved to scheme + address.
	if err := cfg.resolveTarget(pick(values, hostKeys), pick(values, portKeys)); err != nil {
		return Config{}, err
	}
	if cfg.Address == "" && cfg.Scheme != "bolt+unix" {
		// Something was configured, but none of it is a Neo4j connection — most
		// likely the wrong profile is selected on the node.
		return Config{}, fmt.Errorf("%w (the selected profile has none of these: %s)", ErrNoSettings, strings.Join(sortedKeys(settings), ", "))
	}
	return cfg, nil
}

// resolveTarget works out the scheme and address to dial. The URI wins when one
// was given; otherwise a bare host (plus optional port) is turned into the
// routing URI a cluster-or-single server both answer on.
//
// Credentials written into the URI — neo4j://neo4j:secret@host:7687, which is
// how a connection string is often pasted — are lifted out into User/Password
// rather than left in place: the driver takes its credentials from the auth
// token, and ignores a URI's userinfo entirely.
func (c *Config) resolveTarget(host, port string) error {
	raw := strings.TrimSpace(c.Raw)
	if raw == "" {
		if host = strings.TrimSpace(host); host == "" {
			return nil // reported by the caller, with the profile's keys listed
		}
		raw = host
		if !strings.Contains(raw, "://") {
			raw = "neo4j://" + raw
		}
		if port = strings.TrimSpace(port); port != "" {
			// A port column alongside a host that already carries one (or a
			// whole URI) is ignored: the explicit one in the address wins.
			if _, _, err := net.SplitHostPort(strings.TrimPrefix(raw, "neo4j://")); err != nil {
				raw = raw + ":" + port
			}
		}
	}
	// A URI written without a scheme (localhost:7687) parses as one with the
	// host in the path, so give it the default scheme before parsing.
	if !strings.Contains(raw, "://") {
		raw = "neo4j://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("the connection URI %q is not a URL: %w", raw, err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if !validSchemes[scheme] {
		return fmt.Errorf("unsupported URI scheme %q: use neo4j:// (routing) or bolt:// (one server), with +s for TLS or +ssc for TLS with a self-signed certificate", parsed.Scheme)
	}
	c.Scheme = scheme

	if user := parsed.User; user != nil {
		if c.User == "" {
			c.User = user.Username()
		}
		if c.Password == "" {
			if password, ok := user.Password(); ok {
				c.Password = password
			}
		}
	}

	if scheme == "bolt+unix" {
		// bolt+unix://<path to socket> — the host part must be empty and the
		// socket path is the URL's path.
		c.Address = parsed.Path
		return nil
	}

	address := parsed.Host
	if address == "" {
		return fmt.Errorf("the connection URI %q names no host", raw)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, defaultBoltPort)
	}
	c.Address = address

	// A database written into the URI path (neo4j://host:7687/movies) is not
	// something the driver reads — it selects the database per session — so it
	// is lifted onto the config, where the session config can use it.
	if c.Database == "" {
		if database := strings.Trim(parsed.Path, "/"); database != "" {
			c.Database = database
		}
	}
	return nil
}

// validSchemes is every scheme the Go driver accepts. The `+s` and `+ssc`
// variants are how encryption is selected: the driver refuses a TLS config
// passed alongside one of them, so the scheme is the only lever this plugin
// pulls.
var validSchemes = map[string]bool{
	"neo4j": true, "neo4j+s": true, "neo4j+ssc": true,
	"bolt": true, "bolt+s": true, "bolt+ssc": true,
	"bolt+unix": true,
}

// URI renders the target the driver dials, with the encryption choice folded
// into the scheme and any credentials from the original URI left out.
func (c Config) URI() string {
	scheme := c.scheme()
	if scheme == "bolt+unix" {
		return scheme + "://" + c.Address
	}
	return scheme + "://" + c.Address
}

// scheme applies the profile's encryption choice to the base scheme. A profile
// that says nothing leaves the URI's own scheme alone, so a pasted
// `neo4j+s://…` Aura URI keeps its TLS.
func (c Config) scheme() string {
	base, _, _ := strings.Cut(c.Scheme, "+")
	if c.Scheme == "bolt+unix" {
		return c.Scheme
	}
	switch c.Encryption {
	case EncryptOff:
		return base
	case EncryptOn:
		return base + "+s"
	case EncryptSelfSigned:
		return base + "+ssc"
	default:
		return c.Scheme
	}
}

// Routing reports whether the URI asks the driver to route (the `neo4j` family)
// rather than to talk to exactly one server (`bolt`). It is what decides whether
// a read transaction can be served by a follower.
func (c Config) Routing() bool { return strings.HasPrefix(c.scheme(), "neo4j") }

// Describe renders host:port/database — enough to say which server a node is
// talking to, with no credentials in it.
func (c Config) Describe() string {
	target := c.Address
	if c.Database != "" {
		return target + "/" + c.Database
	}
	return target
}

// Validate reports the settings a connection cannot be made without. It runs
// before the driver is built so the message names the field, not the symptom.
func (c Config) Validate() error {
	if c.Address == "" {
		return ErrNoSettings
	}
	switch c.Auth {
	case AuthBasic:
		if c.Password == "" {
			return errors.New("the settings profile has no password: fill it in, or set Authentication to \"none\" for a server started with authentication disabled")
		}
	case AuthBearer:
		if c.Password == "" {
			return errors.New("bearer authentication needs the token in the password field of the settings profile")
		}
	case AuthKerberos:
		if c.Password == "" {
			return errors.New("kerberos authentication needs the base64 service ticket in the password field of the settings profile")
		}
	}
	return nil
}

func parseAuth(value string) Auth {
	switch canonicalKey(value) {
	case "none", "no", "off", "disabled", "anonymous":
		return AuthNone
	case "bearer", "token", "sso", "oidc", "jwt":
		return AuthBearer
	case "kerberos":
		return AuthKerberos
	default:
		return AuthBasic
	}
}

// parseEncryption reads the encryption choice. A profile typed by hand is as
// likely to say `true` or `require` as to use the form's own wording, and an
// sslmode-style `verify-full` means the same thing as "on".
func parseEncryption(value string) Encryption {
	switch canonicalKey(value) {
	case "":
		return EncryptFromURI
	case "off", "false", "no", "none", "disable", "disabled", "0":
		return EncryptOff
	case "selfsigned", "ssc", "trust", "trustall", "insecure", "noverify", "verifynone":
		return EncryptSelfSigned
	case "on", "true", "yes", "1", "require", "required", "verifyfull", "verifyca", "full":
		return EncryptOn
	default:
		return EncryptFromURI
	}
}

// Field synonyms, in preference order. Each entry is already canonical (lower
// case, letters and digits only) — see canonicalKey.
var (
	uriKeys      = []string{"uri", "url", "connectionuri", "connectionurl", "connectionstring", "connstring", "bolturl", "bolturi", "dsn", "endpoint"}
	hostKeys     = []string{"host", "hostname", "server", "address", "instance"}
	portKeys     = []string{"port", "boltport"}
	databaseKeys = []string{"database", "db", "dbname", "databasename", "defaultdatabase"}
	userKeys     = []string{"user", "username", "userid", "login", "principal", "account"}
	passwordKeys = []string{"password", "pass", "pwd", "secret", "credentials", "token", "apikey"}
	realmKeys    = []string{"realm"}
	authKeys     = []string{"auth", "authtype", "authscheme", "authentication", "scheme"}
	encryptKeys  = []string{"encryption", "encrypt", "tls", "ssl", "sslmode", "encrypted"}
	timeoutKeys  = []string{"connecttimeout", "connectiontimeout", "timeout", "sockettimeout"}
)

func pick(values map[string]string, keys []string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return ""
}

// canonicalKey reduces a settings key to letters and digits, lower case, so
// "Connect Timeout", "connect_timeout" and "connectTimeout" are one key.
func canonicalKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// toString renders a profile value. The settings editor stores anything that
// parses as JSON (numbers, booleans) as that type, so values are not always
// strings — a port arrives as a float64.
func toString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%v", typed)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

func sortedKeys(settings map[string]any) []string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Pool hands out one client per distinct connection, so every job using the
// same settings profile shares one driver — and therefore one Bolt connection
// pool — instead of dialling afresh. Clients are safe for concurrent use.
type Pool struct {
	mu      sync.Mutex
	clients map[string]*Client
}

// maxPooledClients bounds the cache: a handful of profiles is the norm, and the
// pool is drained rather than grown without limit if that ever changes.
const maxPooledClients = 16

func NewPool() *Pool { return &Pool{clients: map[string]*Client{}} }

// Client resolves the settings profile shipped with a call into a client,
// building (and caching) a driver for it on first use.
func (p *Pool) Client(settings map[string]any) (*Client, error) {
	cfg, err := ParseConfig(settings)
	if err != nil {
		return nil, err
	}
	return p.For(cfg)
}

// For returns the client for an already-parsed config.
func (p *Pool) For(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	key := cfg.fingerprint()

	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clients[key]; ok {
		return client, nil
	}
	client, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if len(p.clients) >= maxPooledClients {
		for k, c := range p.clients {
			c.Close(context.Background())
			delete(p.clients, k)
		}
	}
	p.clients[key] = client
	return client, nil
}

// fingerprint identifies a connection without keeping the password in a map key.
func (c Config) fingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		c.URI(), c.Database, string(c.Auth), c.User, c.Password, c.Realm,
		c.ConnectTimeout.String(),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
