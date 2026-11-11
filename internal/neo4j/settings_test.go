package neo4j

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseConfigEmpty(t *testing.T) {
	if _, err := ParseConfig(nil); !errors.Is(err, ErrNoSettings) {
		t.Fatalf("empty settings: got %v, want ErrNoSettings", err)
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]any
		wantURI  string
		wantDB   string
		wantUser string
		wantPass string
		wantAuth Auth
	}{
		{
			name:     "uri with discrete credentials",
			settings: map[string]any{"uri": "neo4j://localhost:7687", "user": "neo4j", "password": "secret", "database": "movies"},
			wantURI:  "neo4j://localhost:7687",
			wantDB:   "movies",
			wantUser: "neo4j",
			wantPass: "secret",
			wantAuth: AuthBasic,
		},
		{
			// A hand-typed profile: spaces and capitals in the keys, a port
			// stored as a number, and synonyms for every field.
			name: "lenient keys",
			settings: map[string]any{
				"Connection URI": "bolt://graph.internal:7687",
				"User Name":      "reader",
				"Pass Word":      "hunter2",
				"DB Name":        "ops",
			},
			wantURI:  "bolt://graph.internal:7687",
			wantDB:   "ops",
			wantUser: "reader",
			wantPass: "hunter2",
			wantAuth: AuthBasic,
		},
		{
			name:     "host only gets the default scheme and port",
			settings: map[string]any{"host": "graph.internal", "password": "p"},
			wantURI:  "neo4j://graph.internal:7687",
			wantPass: "p",
			wantAuth: AuthBasic,
		},
		{
			name:     "host and port columns",
			settings: map[string]any{"host": "graph.internal", "port": float64(7688), "password": "p"},
			wantURI:  "neo4j://graph.internal:7688",
			wantPass: "p",
			wantAuth: AuthBasic,
		},
		{
			// Credentials pasted into the URI are lifted out: the driver takes
			// them from the auth token and ignores a URI's userinfo.
			name:     "credentials in the uri",
			settings: map[string]any{"uri": "neo4j+s://alice:s3cret@xyz.databases.neo4j.io"},
			wantURI:  "neo4j+s://xyz.databases.neo4j.io:7687",
			wantUser: "alice",
			wantPass: "s3cret",
			wantAuth: AuthBasic,
		},
		{
			name:     "database in the uri path",
			settings: map[string]any{"uri": "neo4j://db:7687/movies", "password": "p"},
			wantURI:  "neo4j://db:7687",
			wantDB:   "movies",
			wantPass: "p",
			wantAuth: AuthBasic,
		},
		{
			name:     "no auth",
			settings: map[string]any{"uri": "bolt://localhost:7687", "auth": "none"},
			wantURI:  "bolt://localhost:7687",
			wantAuth: AuthNone,
		},
		{
			name:     "bearer token",
			settings: map[string]any{"uri": "neo4j+s://x.io:7687", "auth": "bearer", "password": "ey.."},
			wantURI:  "neo4j+s://x.io:7687",
			wantPass: "ey..",
			wantAuth: AuthBearer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig(tc.settings)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if got := cfg.URI(); got != tc.wantURI {
				t.Errorf("URI = %q, want %q", got, tc.wantURI)
			}
			if cfg.Database != tc.wantDB {
				t.Errorf("Database = %q, want %q", cfg.Database, tc.wantDB)
			}
			if cfg.User != tc.wantUser {
				t.Errorf("User = %q, want %q", cfg.User, tc.wantUser)
			}
			if cfg.Password != tc.wantPass {
				t.Errorf("Password = %q, want %q", cfg.Password, tc.wantPass)
			}
			if cfg.Auth != tc.wantAuth {
				t.Errorf("Auth = %q, want %q", cfg.Auth, tc.wantAuth)
			}
		})
	}
}

// The encryption choice must reach the driver as a URI scheme: the driver
// refuses a TLS config passed alongside a +s or +ssc scheme, so the scheme is
// the only lever.
func TestEncryptionRewritesScheme(t *testing.T) {
	tests := []struct {
		uri, encryption, want string
	}{
		{"neo4j://db:7687", "", "neo4j://db:7687"},
		{"neo4j://db:7687", "on", "neo4j+s://db:7687"},
		{"neo4j://db:7687", "self-signed", "neo4j+ssc://db:7687"},
		{"neo4j+s://db:7687", "off", "neo4j://db:7687"},
		{"bolt://db:7687", "on", "bolt+s://db:7687"},
		{"bolt+ssc://db:7687", "", "bolt+ssc://db:7687"},
		// An Aura URI already says TLS; "from URI" must leave it alone.
		{"neo4j+s://x.databases.neo4j.io:7687", "from URI", "neo4j+s://x.databases.neo4j.io:7687"},
		// A hand-typed profile says true/false as often as the form's wording.
		{"neo4j://db:7687", "true", "neo4j+s://db:7687"},
		{"neo4j+s://db:7687", "false", "neo4j://db:7687"},
		{"neo4j://db:7687", "verify-full", "neo4j+s://db:7687"},
		{"neo4j://db:7687", "insecure", "neo4j+ssc://db:7687"},
	}
	for _, tc := range tests {
		cfg, err := ParseConfig(map[string]any{"uri": tc.uri, "encryption": tc.encryption, "password": "p"})
		if err != nil {
			t.Fatalf("%s + %q: %v", tc.uri, tc.encryption, err)
		}
		if got := cfg.URI(); got != tc.want {
			t.Errorf("%s + %q → %q, want %q", tc.uri, tc.encryption, got, tc.want)
		}
	}
}

// A profile bound from another plugin is the common support ticket, so the
// error has to say what the profile did contain.
func TestParseConfigWrongProfile(t *testing.T) {
	_, err := ParseConfig(map[string]any{"baseUrl": "https://acme.atlassian.net", "projectKey": "OPS"})
	if !errors.Is(err, ErrNoSettings) {
		t.Fatalf("got %v, want ErrNoSettings", err)
	}
	for _, key := range []string{"baseUrl", "projectKey"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not list %q: %s", key, err)
		}
	}
}

func TestParseConfigBadScheme(t *testing.T) {
	_, err := ParseConfig(map[string]any{"uri": "http://db:7474", "password": "p"})
	if err == nil {
		t.Fatal("an http:// URI should be rejected: Neo4j is reached over Bolt")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error should name the scheme: %s", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]any
		wantErr  bool
	}{
		{"basic without a password", map[string]any{"uri": "neo4j://db:7687", "user": "neo4j"}, true},
		{"basic with a password", map[string]any{"uri": "neo4j://db:7687", "password": "p"}, false},
		{"none needs nothing", map[string]any{"uri": "neo4j://db:7687", "auth": "none"}, false},
		{"bearer without a token", map[string]any{"uri": "neo4j://db:7687", "auth": "bearer"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig(tc.settings)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if got := cfg.Validate() != nil; got != tc.wantErr {
				t.Errorf("Validate error = %v, want error %v (%v)", cfg.Validate(), tc.wantErr, cfg)
			}
		})
	}
}

func TestRoutingAndDescribe(t *testing.T) {
	routing, _ := ParseConfig(map[string]any{"uri": "neo4j://db:7687", "database": "movies", "password": "p"})
	if !routing.Routing() {
		t.Error("neo4j:// should route")
	}
	if got, want := routing.Describe(), "db:7687/movies"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}

	pinned, _ := ParseConfig(map[string]any{"uri": "bolt://db:7687", "password": "p"})
	if pinned.Routing() {
		t.Error("bolt:// should not route")
	}
	if got, want := pinned.Describe(), "db:7687"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
}

// The pool keys clients on the connection, so two profiles that differ only by
// password must not share one driver.
func TestFingerprintSeparatesConnections(t *testing.T) {
	a, _ := ParseConfig(map[string]any{"uri": "neo4j://db:7687", "user": "u", "password": "one"})
	b, _ := ParseConfig(map[string]any{"uri": "neo4j://db:7687", "user": "u", "password": "two"})
	if a.fingerprint() == b.fingerprint() {
		t.Error("two passwords share a fingerprint: one tenant's driver would serve another's job")
	}

	c, _ := ParseConfig(map[string]any{"uri": "neo4j://db:7687", "user": "u", "password": "one", "database": "other"})
	if a.fingerprint() == c.fingerprint() {
		t.Error("two databases share a fingerprint")
	}
}

// The fingerprint must not be the password in plain text — it is a map key, and
// map keys end up in dumps.
func TestFingerprintHidesPassword(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{"uri": "neo4j://db:7687", "user": "u", "password": "s3cret-value"})
	if strings.Contains(cfg.fingerprint(), "s3cret-value") {
		t.Error("the fingerprint carries the password verbatim")
	}
}

func TestConnectTimeout(t *testing.T) {
	cfg, err := ParseConfig(map[string]any{"uri": "neo4j://db:7687", "password": "p", "connectTimeout": float64(30)})
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.ConnectTimeout != 30*time.Second {
		t.Errorf("ConnectTimeout = %v, want 30s", cfg.ConnectTimeout)
	}
}
