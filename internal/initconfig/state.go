package initconfig

import (
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// StateParams selects where a generated control plane keeps its LEDGER.
//
// NIL MEANS THE SHORTHAND, which is what almost every generated file wants:
// `server.state_dir`, one directory holding the ledger, the deployment identity,
// the node-wire CA, the process lock and the maintenance fence. Set it and the
// generation writes `identity_dir` plus an explicit `state:` block instead —
// which is not a decoration, because the two spellings are MUTUALLY EXCLUSIVE at
// load and a file carrying both is refused.
type StateParams struct {
	// Backend is the engine the ledger lives in.
	Backend config.StateBackend
	// DSNEnv names the environment variable holding the PostgreSQL connection
	// string. Required for the postgres backend and refused for any other, because
	// billet reads the DSN from the environment rather than from the file: it
	// carries a password, and a secret written into a config ends up in a backup.
	DSNEnv string
}

var (
	// errStateBackendNeedsDSNEnv is what a postgres generation with nothing to
	// read the connection string from gets.
	errStateBackendNeedsDSNEnv = errors.New(
		"--state-backend postgres needs --state-dsn-env: billet reads the connection " +
			"string from an environment variable rather than from this file, because a DSN " +
			"carries a password and a secret written into a config ends up in a backup, a " +
			"paste buffer and eventually a support thread. Name the variable your service " +
			"unit will export (BILLET_STATE_DSN is the conventional one)")

	// errDSNEnvWithoutPostgres is the mirror image, and it is refused rather than
	// ignored for the reason refuseForeignBackendInputs exists: a value silently
	// discarded is an operator believing they configured something they did not.
	errDSNEnvWithoutPostgres = errors.New(
		"--state-dsn-env names the variable holding a PostgreSQL connection string, so it " +
			"means nothing without --state-backend postgres; nothing would read it")
)

// checkStateParams refuses a ledger selection billet cannot turn into a runnable
// config, by the name of the flag that carried it.
//
// THE FLAG, NOT THE FILE. Every one of these is something `config.Parse` would
// also catch — Generate proves its own output — but the diagnostic it produces
// blames a generated tier or a generated block, and the operator did not write
// either. This is the same reason --org, --runner-group and --image are checked
// up front.
func checkStateParams(s *StateParams) error {
	if s == nil {
		return nil
	}

	switch s.Backend {
	case config.StateSQLite:
		if s.DSNEnv != "" {
			return errDSNEnvWithoutPostgres
		}
	case config.StatePostgres:
		if strings.TrimSpace(s.DSNEnv) == "" {
			// A BLANK-BUT-PRESENT VALUE IS NOT AN ABSENT ONE, and the distinction
			// matters here for the same reason it does for --runner-group: a name of
			// only whitespace is not a legal environment variable, so it would read
			// as empty forever, and the deployment would fail to start complaining
			// about an empty data source rather than about the flag that is wrong.
			return errStateBackendNeedsDSNEnv
		}

		// THE CONFIG LAYER'S OWN RULE, called rather than restated. `9-lives` is a
		// non-empty value that no shell can export, and without this it was
		// written into the file and then refused by config.Parse with a message
		// blaming `server.state.postgres.dsn_env` — a generated block the operator
		// never typed, when what they typed was a flag.
		if err := config.CheckDSNEnv(s.DSNEnv); err != nil {
			return fmt.Errorf("--state-dsn-env: %w", err)
		}
	case "":
		return errors.New(
			"--state-backend is required when a state backend is selected; it is " +
				string(config.StateSQLite) + " or " + string(config.StatePostgres))
	default:
		return fmt.Errorf("--state-backend %q is not a backend billet has; it is %s or %s",
			s.Backend, config.StateSQLite, config.StatePostgres)
	}

	return nil
}

// serverStateYAML renders the `server:` keys that say where the ledger lives.
//
// ONE HELPER FOR EVERY RENDERER, and that is the point rather than tidiness.
// Each backend the generator learns adds a renderer, and a renderer that kept
// emitting `state_dir` while the operator asked for PostgreSQL would produce a
// file that is refused at load — with the refusal naming a key the operator never
// typed. There is one place to get this right.
//
// The returned string always ends with a newline and never begins with one, so a
// caller substitutes it where a single key line stood.
func serverStateYAML(s *StateParams, dir string) string {
	if s == nil || s.Backend == config.StateSQLite {
		// THE SHORTHAND, deliberately, even when a caller asked for sqlite
		// explicitly. `state_dir: X` means exactly `identity_dir: X` plus
		// `state: {backend: sqlite}`, and writing the long form for the default
		// would put a block in every generated file whose only effect is to say
		// what the absence of it already says.
		return fmt.Sprintf("  state_dir: %s\n", yamlScalar(dir))
	}

	var b strings.Builder

	// IDENTITY_DIR IS NOT A RENAMED STATE_DIR, and the comment says so where an
	// operator reads it: the ledger moved and nothing else did. A private key is
	// not rows, and a process lock has nothing to do with SQL.
	b.WriteString("  # THE LEDGER IS IN POSTGRESQL AND EVERYTHING ELSE IS STILL HERE.\n")
	b.WriteString("  #\n")
	b.WriteString("  # This directory holds the deployment identity, the node-wire CA and its\n")
	b.WriteString("  # rotation state, the process lock and the maintenance fence — none of which\n")
	b.WriteString("  # move into a database. Back it up: `billet local backup` archives exactly\n")
	b.WriteString("  # this half and REFUSES the ledger, because a consistent copy of a\n")
	b.WriteString("  # PostgreSQL database is pg_dump or your provider's snapshot.\n")
	fmt.Fprintf(&b, "  identity_dir: %s\n", yamlScalar(dir))
	b.WriteString("\n")
	b.WriteString("  state:\n")
	fmt.Fprintf(&b, "    backend: %s\n", s.Backend)
	b.WriteString("    postgres:\n")
	b.WriteString("      # NAMED, NOT WRITTEN. billet reads the connection string from this\n")
	b.WriteString("      # environment variable; put it in the service's environment (for the\n")
	b.WriteString("      # packaged unit, an EnvironmentFile) and keep the password out of here.\n")
	fmt.Fprintf(&b, "      dsn_env: %s\n", yamlScalar(s.DSNEnv))

	return b.String()
}
