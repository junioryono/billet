package initconfig

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func postgresState() *StateParams {
	return &StateParams{Backend: config.StatePostgres, DSNEnv: "BILLET_STATE_DSN"}
}

// A POSTGRESQL GENERATION WRITES THE OTHER SPELLING, AND THE TWO ARE MUTUALLY
// EXCLUSIVE.
//
// This is the assertion the whole feature reduces to: `state_dir` beside `state:`
// is REFUSED at load, so a renderer that emitted the shorthand while the operator
// asked for PostgreSQL would produce a file billet cannot read — with the refusal
// naming a key the operator never typed.
//
// It runs against BOTH renderers, because they are two independent entry points:
// a helper called from one of them and not the other is a backend that silently
// keeps the default.
func TestAPostgresGenerationNamesTheIdentityDirectoryRatherThanTheStateDirectory(t *testing.T) {
	t.Parallel()

	cases := map[string]Params{
		"docker": func() Params { p := dockerParams(); p.State = postgresState(); return p }(),
		"ec2":    func() Params { p := ec2Params(); p.State = postgresState(); return p }(),
	}

	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := mustGenerate(t, p)
			server := serverSection(t, body)

			if strings.Contains(server, "state_dir:") {
				t.Errorf("the server section still writes state_dir beside an explicit state "+
					"backend, which config.Parse refuses:\n%s", server)
			}
			if !strings.Contains(server, "identity_dir: ") {
				t.Errorf("the server section names no identity_dir:\n%s", server)
			}

			// READ BACK THROUGH THE CONFIG LAYER, not by matching more strings: what
			// matters is that the block landed where billet looks for it, and a
			// correctly-spelled block nested one level wrong would satisfy every
			// substring assertion above.
			cfg := loadGenerated(t, body)
			if got := cfg.Server.LedgerBackend(); got != config.StatePostgres {
				t.Errorf("the generated config's ledger backend is %q, want postgres", got)
			}
			if got := cfg.Server.LedgerDSNEnv(); got != "BILLET_STATE_DSN" {
				t.Errorf("the generated config's dsn_env is %q, want BILLET_STATE_DSN", got)
			}
			if cfg.Server.IdentityDir == "" {
				t.Error("the generated config has no identity_dir after loading")
			}
		})
	}
}

// THE NODE'S state_dir IS A DIFFERENT KEY AND MUST SURVIVE.
//
// It is not a variant spelling of the server's — a compute host keeps its custody
// records and its deployment lock there whatever the ledger is doing — and it is
// the exact thing a careless edit to the renderer would take with it, since both
// sections carry a key by that name.
func TestAPostgresGenerationLeavesTheNodeStateDirectoryAlone(t *testing.T) {
	t.Parallel()

	p := dockerParams()
	p.State = postgresState()

	cfg := loadGenerated(t, mustGenerate(t, p))
	if cfg.Node == nil {
		t.Fatal("the generated config has no node section")
	}
	if cfg.Node.StateDir == "" {
		t.Error("the node lost its state_dir; the ledger moved, and the node's custody " +
			"records and deployment lock did not")
	}
	if cfg.Node.StateDir == cfg.Server.IdentityDir {
		t.Errorf("the node and the server now name one directory (%q); the role refuses "+
			"overlapping state directories, and systemd's sandbox is scoped to each",
			cfg.Node.StateDir)
	}
}

// THE DEFAULT IS UNCHANGED, and asserted rather than assumed: every generation
// that says nothing about a backend must keep writing exactly what it always
// wrote, or this feature is a silent rewrite of every existing profile test's
// subject.
func TestAGenerationThatSaysNothingKeepsTheShorthand(t *testing.T) {
	t.Parallel()

	for _, p := range []Params{dockerParams(), ec2Params()} {
		server := serverSection(t, mustGenerate(t, p))

		if !strings.Contains(server, "state_dir: ") {
			t.Errorf("a generation with no state selection stopped writing state_dir:\n%s", server)
		}
		if strings.Contains(server, "identity_dir:") || strings.Contains(server, "state:") {
			t.Errorf("a generation with no state selection wrote the long form:\n%s", server)
		}
	}
}

// AND AN EXPLICIT sqlite IS THE SHORTHAND TOO.
//
// `state_dir: X` means exactly `identity_dir: X` plus `state: {backend: sqlite}`,
// so writing the long form for the default would put a block in every file whose
// only effect is to say what its absence already says.
func TestAnExplicitSQLiteSelectionStillWritesTheShorthand(t *testing.T) {
	t.Parallel()

	p := dockerParams()
	p.State = &StateParams{Backend: config.StateSQLite}

	server := serverSection(t, mustGenerate(t, p))
	if !strings.Contains(server, "state_dir: ") {
		t.Errorf("an explicit sqlite selection did not write the shorthand:\n%s", server)
	}
}

// EVERY REFUSAL NAMES THE FLAG THAT CAUSED IT.
//
// All of these are things config.Parse would also catch — Generate proves its own
// output — but its diagnostic blames a generated block, and the operator did not
// write one. The assertion is on the flag name appearing, because that is the
// whole difference between the two.
func TestAnUnusableLedgerSelectionIsRefusedByItsFlag(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		state *StateParams
		want  string
	}{
		"postgres with nothing to read the DSN from": {
			state: &StateParams{Backend: config.StatePostgres},
			want:  "--state-dsn-env",
		},
		"postgres whose DSN variable is only whitespace": {
			state: &StateParams{Backend: config.StatePostgres, DSNEnv: "   "},
			want:  "--state-dsn-env",
		},
		"a DSN variable with no postgres backend": {
			state: &StateParams{Backend: config.StateSQLite, DSNEnv: "BILLET_STATE_DSN"},
			want:  "--state-backend postgres",
		},
		"a backend selected and not named": {
			state: &StateParams{},
			want:  "--state-backend is required",
		},
		"a backend billet does not have": {
			state: &StateParams{Backend: config.StateBackend("mysql")},
			want:  `--state-backend "mysql"`,
		},
		// A NAME NO SHELL COULD EXPORT is not caught by "is it blank", and the
		// config layer's own rule is what says so — called rather than restated,
		// because a rule enforced at only one of two entry points is an entry
		// point that does not enforce it. Without this the value was written into
		// the file and then refused by config.Parse, blaming a generated block
		// the operator never typed.
		"a DSN variable name nothing could export": {
			state: &StateParams{Backend: config.StatePostgres, DSNEnv: "9-lives"},
			want:  "--state-dsn-env",
		},
		"a DSN variable that is the connection string itself": {
			state: &StateParams{
				Backend: config.StatePostgres,
				DSNEnv:  "postgres://billet:x@ledger.internal:5432/billet",
			},
			want: "--state-dsn-env",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p := dockerParams()
			p.State = tc.state

			_, _, err := Generate(p)
			if err == nil {
				t.Fatal("Generate accepted a ledger selection billet cannot run")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Generate: %v\nwant it to name %q", err, tc.want)
			}

			// AND NOT AS A CONFIG-LOAD FAILURE. `initconfig: the generated config is
			// not valid` means the refusal came from Parse rather than from the
			// check in front of it — which is the state this whole family of
			// up-front checks exists to avoid, and a substring match on the flag
			// name alone cannot tell the two apart.
			if strings.Contains(err.Error(), "the generated config is not valid") {
				t.Errorf("the refusal came from config.Parse rather than from the flag "+
					"check ahead of it: %v", err)
			}
		})
	}
}

// The two refusals a caller may want to distinguish are sentinels, so a wrapper
// can match them rather than the sentence.
func TestTheLedgerRefusalsAreMatchable(t *testing.T) {
	t.Parallel()

	if err := checkStateParams(&StateParams{Backend: config.StatePostgres}); !errors.Is(err, errStateBackendNeedsDSNEnv) {
		t.Errorf("postgres with no DSN variable = %v, want errStateBackendNeedsDSNEnv", err)
	}
	if err := checkStateParams(&StateParams{Backend: config.StateSQLite, DSNEnv: "X"}); !errors.Is(err, errDSNEnvWithoutPostgres) {
		t.Errorf("a DSN variable under sqlite = %v, want errDSNEnvWithoutPostgres", err)
	}
	if err := checkStateParams(nil); err != nil {
		t.Errorf("no selection at all = %v, want nil", err)
	}
}

// serverSection returns the generated `server:` block alone.
//
// SO AN ASSERTION ABOUT state_dir CANNOT BE SATISFIED BY THE NODE'S. Both
// sections carry a key by that name, and a whole-document `strings.Contains` for
// "state_dir:" is true of every postgres generation ever produced — a test that
// would pass however wrong the server block was.
func serverSection(t *testing.T, body string) string {
	t.Helper()

	const marker = "\nserver:\n"

	_, rest, ok := strings.Cut(body, marker)
	if !ok {
		t.Fatalf("the generated config has no server section:\n%s", body)

		return ""
	}

	// The next line that begins in column zero and is not a comment ends the
	// section. Comments are skipped because the generated file carries top-level
	// commentary between sections. The offset is accumulated rather than searched
	// for, because an identical line can occur twice and a search would return the
	// first one.
	end := 0
	for _, line := range strings.SplitAfter(rest, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		if trimmed != "" && !strings.HasPrefix(trimmed, " ") && !strings.HasPrefix(trimmed, "#") {
			break
		}

		end += len(line)
	}

	return rest[:end]
}
