package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// serverWith builds a minimal loadable config with the given server block.
//
// The rest is the smallest thing validation accepts, so a failure names the rule
// under test rather than something incidental.
func serverWith(server string) string {
	return "github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n" +
		"  private_key_path: /tmp/key.pem\nserver:\n" + server +
		"  max_vcpu: 4\n  max_memory: 8GiB\n"
}

func loadServer(t *testing.T, body string) (*Config, error) {
	t.Helper()

	return Load(writeConfig(t, body))
}

// THE SHORTHAND STILL MEANS WHAT IT ALWAYS MEANT, which is the compatibility
// half of this change: every existing config says state_dir and nothing else,
// and it has to keep resolving to the same SQLite ledger in the same directory.
func TestStateDirIsTheSQLiteShorthand(t *testing.T) {
	dir := t.TempDir()

	cfg, err := loadServer(t, serverWith("  state_dir: "+dir+"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.LedgerBackend(); got != StateSQLite {
		t.Errorf("backend = %q, want %q", got, StateSQLite)
	}

	if got := cfg.Server.IdentityDir; got != dir {
		t.Errorf("identity_dir = %q, want the state_dir %q", got, dir)
	}

	if got, want := cfg.Server.LedgerPath(), filepath.Join(dir, "billet.db"); got != want {
		t.Errorf("ledger path = %q, want %q", got, want)
	}

	if got := cfg.Server.LedgerDSNEnv(); got != "" {
		t.Errorf("a SQLite deployment named a DSN variable: %q", got)
	}
}

// AN OMITTED state_dir STILL RESOLVES, and to the same place it always did.
func TestAnOmittedStateDirKeepsItsDefault(t *testing.T) {
	cfg, err := loadServer(t, serverWith(""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got, want := cfg.Server.IdentityDir, DefaultServerStateDir(); got != want {
		t.Errorf("identity_dir = %q, want the default %q", got, want)
	}
}

// TWO SPELLINGS OF ONE VALUE ARE REFUSED RATHER THAN MERGED.
//
// Merging means guessing which the operator meant when they disagree, and the
// guess is invisible — which is exactly how internal/config has been wrong three
// times before.
func TestStateDirAndStateTogetherAreRefused(t *testing.T) {
	_, err := loadServer(t, serverWith(
		"  state_dir: /srv/billet\n  identity_dir: /srv/billet\n"+
			"  state:\n    backend: sqlite\n"))
	if err == nil {
		t.Fatal("a config writing both state_dir and state was accepted")
	}

	if !strings.Contains(err.Error(), "only\none may be written") &&
		!strings.Contains(err.Error(), "only one may be written") {
		t.Errorf("the refusal should say only one may be written; got: %v", err)
	}
}

// THE EXPLICIT FORM RESOLVES TO THE SAME THINGS.
func TestTheExplicitSQLiteFormResolves(t *testing.T) {
	cfg, err := loadServer(t, serverWith(
		"  identity_dir: /srv/billet\n  state:\n    backend: sqlite\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.LedgerBackend(); got != StateSQLite {
		t.Errorf("backend = %q, want %q", got, StateSQLite)
	}

	if got, want := cfg.Server.LedgerPath(), "/srv/billet/billet.db"; got != want {
		t.Errorf("ledger path = %q, want %q", got, want)
	}
}

// THE LEDGER PATH IS DERIVED AND CANNOT BE CONFIGURED, which is the property
// that stops a configured-but-unread path from starting a deployment against a
// freshly created empty ledger.
func TestTheSQLiteLedgerPathIsAlwaysInsideTheIdentityDirectory(t *testing.T) {
	for _, server := range []string{
		"  state_dir: /srv/billet\n",
		"  identity_dir: /srv/billet\n  state:\n    backend: sqlite\n",
	} {
		cfg, err := loadServer(t, serverWith(server))
		if err != nil {
			t.Fatalf("load %q: %v", server, err)
		}

		if got, want := cfg.Server.LedgerPath(), "/srv/billet/billet.db"; got != want {
			t.Errorf("ledger path = %q, want %q", got, want)
		}
	}
}

func TestAPostgresLedgerResolvesItsDSNVariable(t *testing.T) {
	cfg, err := loadServer(t, serverWith(
		"  identity_dir: /srv/billet\n  state:\n    backend: postgres\n"+
			"    postgres:\n      dsn_env: BILLET_STATE_DSN\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.LedgerBackend(); got != StatePostgres {
		t.Errorf("backend = %q, want %q", got, StatePostgres)
	}

	if got := cfg.Server.LedgerDSNEnv(); got != "BILLET_STATE_DSN" {
		t.Errorf("dsn_env = %q", got)
	}

	// AND IT NAMES NO LEDGER FILE. A path here would be a file some command
	// later creates, migrates and reports an empty fleet from.
	if got := cfg.Server.LedgerPath(); got != "" {
		t.Errorf("a PostgreSQL deployment named a ledger file: %q", got)
	}
}

// EACH OF THESE IS A CONFIG THAT WOULD OTHERWISE LOAD AND MEAN SOMETHING ELSE.
func TestTheStateBlockRefusesWhatItCannotHonour(t *testing.T) {
	cases := []struct {
		name   string
		server string
		want   string
	}{
		{
			name: "a block for the backend that is not selected",
			server: "  identity_dir: /srv/billet\n  state:\n    backend: sqlite\n" +
				"    postgres:\n      dsn_env: BILLET_STATE_DSN\n",
			want: "nothing would read it",
		},
		{
			// THERE IS NO sqlite: BLOCK, so this is refused by the decoder rather
			// than by a rule of billet's — and that is the right layer. A key
			// billet accepted and did not use would be a deployment started
			// against a freshly created empty ledger, which is not a failure
			// anybody sees until the fleet comes back empty.
			name: "a sqlite block, which configures nothing and so does not exist",
			server: "  identity_dir: /srv/billet\n  state:\n    backend: sqlite\n" +
				"    sqlite:\n      path: /srv/other.db\n",
			want: "sqlite",
		},
		{
			name:   "a state block naming no backend",
			server: "  identity_dir: /srv/billet\n  state:\n    postgres:\n      dsn_env: X\n",
			want:   "server.state.backend is required",
		},
		{
			name:   "a backend billet does not have",
			server: "  identity_dir: /srv/billet\n  state:\n    backend: mysql\n",
			want:   `server.state.backend is "mysql"`,
		},
		{
			name: "PostgreSQL with no DSN variable",
			server: "  identity_dir: /srv/billet\n  state:\n    backend: postgres\n" +
				"    postgres: {}\n",
			want: "dsn_env is required",
		},
		{
			name:   "PostgreSQL with no postgres block at all",
			server: "  identity_dir: /srv/billet\n  state:\n    backend: postgres\n",
			want:   "dsn_env is required",
		},
		{
			// THE MOST LIKELY MISTAKE, and it deserves its own sentence: os.Getenv
			// answers "" for a connection string, so the deployment would fail to
			// start complaining about an empty DSN rather than about this line.
			name: "the DSN written where its variable's name belongs",
			server: "  identity_dir: /srv/billet\n  state:\n    backend: postgres\n" +
				"    postgres:\n      dsn_env: postgres://billet@db/billet\n",
			want: "looks like a connection string",
		},
		{
			name: "a variable name no shell could export",
			server: "  identity_dir: /srv/billet\n  state:\n    backend: postgres\n" +
				"    postgres:\n      dsn_env: 9-lives\n",
			want: "not a legal environment variable name",
		},
		{
			name:   "a state block with no identity directory",
			server: "  state:\n    backend: sqlite\n",
			want:   "server.identity_dir is required",
		},
		{
			name:   "an identity directory whose padding changes which one it names",
			server: "  identity_dir: \" /srv/billet \"\n  state:\n    backend: sqlite\n",
			want:   "begins or ends with whitespace",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadServer(t, serverWith(tc.server))
			if err == nil {
				t.Fatal("the config was accepted")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a refusal naming %q, got: %v", tc.want, err)
			}
		})
	}
}

// THE BUDGET IS THE ARITHMETIC THE ERROR STATES: the writer, the claim and at
// least one reader, with the pool getting what those leave and never more than
// the default it always had.
func TestAPostgresConnectionBudgetSizesTheReaderPool(t *testing.T) {
	block := func(budget string) string {
		return serverWith("  identity_dir: /srv/billet\n  state:\n    backend: postgres\n" +
			"    postgres:\n      dsn_env: BILLET_STATE_DSN\n" + budget)
	}

	for name, tc := range map[string]struct {
		budget       string
		readers, all int
	}{
		"no budget keeps the default": {"", DefaultLedgerReaderConnections, 6},
		"the smallest budget":         {"      max_connections: 3\n", 1, 3},
		"a budget inside the default": {"      max_connections: 4\n", 2, 4},
		"exactly the default":         {"      max_connections: 6\n", 4, 6},
		"more than a plane can use":   {"      max_connections: 50\n", 4, 6},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadServer(t, block(tc.budget))
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			if got := cfg.Server.LedgerReaderConnections(); got != tc.readers {
				t.Errorf("readers = %d, want %d", got, tc.readers)
			}

			if got := cfg.Server.LedgerConnections(); got != tc.all {
				t.Errorf("connections = %d, want %d", got, tc.all)
			}
		})
	}

	for _, budget := range []string{"0", "2", "1", "-1"} {
		t.Run("refuses "+budget, func(t *testing.T) {
			_, err := loadServer(t, block("      max_connections: "+budget+"\n"))
			if err == nil || !strings.Contains(err.Error(), "needs at least 3: the writer, the controller "+
				"claim's session and one reader") {
				t.Fatalf("max_connections %s answered %v, want the arithmetic", budget, err)
			}
		})
	}
}

// A SQLITE LEDGER IS A FILE: it has no connections to budget, and no block to put
// a budget in.
func TestASQLiteLedgerCountsNoServerConnections(t *testing.T) {
	cfg, err := loadServer(t, serverWith("  state_dir: "+t.TempDir()+"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.LedgerConnections(); got != 0 {
		t.Errorf("connections = %d on a SQLite ledger, want 0", got)
	}

	if got := cfg.Server.LedgerReaderConnections(); got != DefaultLedgerReaderConnections {
		t.Errorf("readers = %d, want the default", got)
	}
}

// BOTH DIRECTIONS OF THE BROKER'S BLOCK: with it a target names no key and the file
// loads; a key beside it is a key this host was told it does not hold.
func TestAConfigOnABrokerNamesNoKey(t *testing.T) {
	base := func(key, block string) string {
		return "github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n" + key + "server:\n" +
			"  state_dir: /srv/billet\n  max_vcpu: 4\n  max_memory: 8GiB\n" + block
	}

	const broker = "  token_broker:\n    url: https://broker.internal:8443\n" +
		"    client_key_path: /etc/billet/broker-client.key\n"

	cfg, err := loadServer(t, base("", broker))
	if err != nil {
		t.Fatalf("a deployment on a broker with no key: %v", err)
	}
	if cfg.Server.TokenBroker == nil || cfg.Server.TokenBroker.URL != "https://broker.internal:8443" {
		t.Fatalf("the block did not reach the config: %+v", cfg.Server.TokenBroker)
	}

	if _, err := loadServer(t, base("  private_key_path: /tmp/key.pem\n", broker)); err == nil ||
		!strings.Contains(err.Error(), "holds no App key") {
		t.Fatalf("a key beside a broker answered %v", err)
	}

	if _, err := loadServer(t, base("", "")); err == nil || !strings.Contains(err.Error(), "private_key_path is required") {
		t.Fatalf("no key and no broker answered %v", err)
	}

	for name, tc := range map[string]struct{ block, want string }{
		"no url": {"  token_broker:\n    client_key_path: /etc/k\n", "url is required"},
		"http":   {"  token_broker:\n    url: http://broker\n    client_key_path: /etc/k\n", "must be https"},
		"credentials in the url": {
			"  token_broker:\n    url: https://u:p@broker\n    client_key_path: /etc/k\n", "only a scheme and a host",
		},
		"a path in the url": {
			"  token_broker:\n    url: https://broker/v1\n    client_key_path: /etc/k\n", "only a scheme and a host",
		},
		"no key path": {"  token_broker:\n    url: https://broker\n", "client_key_path is required"},
		"a relative key": {
			"  token_broker:\n    url: https://broker\n    client_key_path: k\n", "must be absolute",
		},
		"inside the identity directory": {
			"  token_broker:\n    url: https://broker\n    client_key_path: /srv/billet/k\n", "inside identity_dir",
		},
		"a relative ca": {
			"  token_broker:\n    url: https://broker\n    client_key_path: /etc/k\n    ca_file: ca.pem\n",
			"ca_file",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadServer(t, base("", tc.block)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// A BROKER BLOCK STOPS THE APP'S CREATION WHETHER OR NOT IT IS COMPLETE: a half-written
// one still says this host is not to hold a key, and the spend cannot be taken back.
func TestAnyBrokerBlockIsSeenBeforeAnAppIsCreated(t *testing.T) {
	t.Parallel()

	for body, want := range map[string]bool{
		"server:\n  token_broker:\n    url: https://broker\n":     true,
		"server:\n  token_broker:\n    client_key_path: /etc/k\n": true,
		"server:\n  token_broker: {}\n":                           true,
		"server:\n  listen: 127.0.0.1:7717\n":                     false,
		"github:\n  org: acme\n":                                  false,
		"not yaml: [":                                             false,
	} {
		if got := PeekTokenBroker([]byte(body)); got != want {
			t.Errorf("PeekTokenBroker(%q) = %v, want %v", body, got, want)
		}
	}
}
