package wiring

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// LedgerMode is which of the ledger's entry points a process may use.
//
// A PROPERTY OF WHAT THE PROCESS IS, NOT A PARAMETER. Each role's module set
// registers exactly one *state.DB through exactly one mode, so an operator
// command cannot be handed the control plane's handle by accident, and the
// running release is named inside the registration rather than remembered at
// every call site.
type LedgerMode int

const (
	// LedgerControlPlane is `billet server`: the exclusive directory lock and
	// the migration (state.Open, state.OpenPostgres).
	LedgerControlPlane LedgerMode = iota + 1
	// LedgerStandby is a control plane WAITING to become the controller: a
	// handle that refuses every write until the claim (state.OpenPostgresStandby;
	// there is no SQLite form, and config refuses the pairing).
	LedgerStandby
	// LedgerProbe is the host upgrade's quiescent probe, which crosses the
	// maintenance fence and admits no writes (state.OpenMaintenance,
	// state.OpenPostgresProbe).
	LedgerProbe
	// LedgerOperator is a one-shot command: it proceeds without the directory
	// lock when a control plane holds it, verifies rather than migrates, and
	// refuses another deployment's ledger (state.OpenAdmin,
	// state.OpenPostgresAdmin).
	LedgerOperator
	// LedgerDecision is the one open that names no release: the host's own
	// upgrade instruction, read by a standby's timer that may be the older
	// binary the leader's watermark would refuse. It creates nothing.
	LedgerDecision
)

func (m LedgerMode) String() string {
	switch m {
	case LedgerControlPlane:
		return "control plane"
	case LedgerStandby:
		return "standby"
	case LedgerProbe:
		return "probe"
	case LedgerOperator:
		return "operator"
	case LedgerDecision:
		return "decision"
	default:
		return fmt.Sprintf("ledger mode %d", int(m))
	}
}

// ErrNoLedgerYet means the state directory holds no ledger to read a decision
// from, which on a host the package just installed is the ordinary state.
var ErrNoLedgerYet = errors.New("no ledger here yet")

// LedgerModule registers the DSN and one *state.DB opened in the given mode.
func LedgerModule(mode LedgerMode) godi.ModuleOption {
	return godi.NewModule("ledger",
		godi.AddSingleton(newDSN),
		godi.AddSingleton(mode.opener()),
	)
}

// opener is the constructor for this mode's handle, as a closure because a
// constructor cannot take the mode as a parameter without it being a
// registration somebody could vary.
func (m LedgerMode) opener() func(
	ctx context.Context, cfg *config.Config, path ConfigPath, dsn state.DSN, release RunningRelease,
) (*state.DB, error) {
	return func(
		ctx context.Context, cfg *config.Config, path ConfigPath, dsn state.DSN, release RunningRelease,
	) (*state.DB, error) {
		return openLedger(ctx, m, cfg, path, dsn, release)
	}
}

// openLedger turns a config into an open handle, once, for the mode the
// process is.
//
// Written as eighteen call sites each choosing for themselves, the failure is a
// command that opens SQLite against a deployment whose ledger is in PostgreSQL,
// which on a fresh directory does not fail: it CREATES one, migrates it, and
// reports an empty fleet.
func openLedger(
	ctx context.Context,
	mode LedgerMode,
	cfg *config.Config,
	path ConfigPath,
	dsn state.DSN,
	release RunningRelease,
) (*state.DB, error) {
	if cfg.Server == nil {
		if mode == LedgerControlPlane || mode == LedgerStandby || mode == LedgerProbe {
			return nil, fmt.Errorf("%s has no server section", path)
		}

		return nil, errors.New("this command runs on the control plane, and this " +
			"config has no server section")
	}

	postgres := cfg.Server.LedgerBackend() == config.StatePostgres
	dir := cfg.Server.IdentityDir
	named := state.WithRunningRelease(string(release))

	var (
		db  *state.DB
		err error
	)

	switch mode {
	case LedgerControlPlane:
		if postgres {
			db, err = state.OpenPostgres(ctx, dir, dsn, named)
		} else {
			db, err = state.Open(ctx, dir, named)
		}

		if err != nil {
			return nil, fmt.Errorf("server state: %w", err)
		}

		return db, nil

	case LedgerStandby:
		// POSTGRESQL ONLY, and the refusal lives in config rather than here: a
		// SQLite ledger is a file a second machine cannot open, so there is
		// nothing to elect over. This is the shape of that refusal one layer
		// down: there is no SQLite branch to fall through to.
		if !postgres {
			return nil, fmt.Errorf(
				"server.controllers is %s but this deployment's ledger is %s; config validation "+
					"should have refused that pairing",
				config.ControllersActivePassive, cfg.Server.LedgerBackend())
		}

		db, err = state.OpenPostgresStandby(ctx, dir, dsn, named)
		if err != nil {
			return nil, fmt.Errorf("server state: %w", err)
		}

		return db, nil

	case LedgerProbe:
		if postgres {
			// A PROBE, NOT A MAINTENANCE HANDLE. billet copies no PostgreSQL
			// ledger, so the transaction there fences, snapshots and migrates
			// nothing; what the candidate proves is that it could serve what it
			// inherits, which is the standby's question. The handle it gets can
			// write nothing and claim nothing, and the migration waits for the
			// candidate's own claim.
			return state.OpenPostgresProbe(ctx, dir, dsn, named)
		}

		return state.OpenMaintenance(ctx, dir, named)

	case LedgerOperator:
		if postgres {
			db, err = state.OpenPostgresAdmin(ctx, dir, dsn, named)
		} else {
			db, err = state.OpenAdmin(ctx, dir, named)
		}

		if err != nil {
			return nil, err
		}

		// AND IT IS THIS DEPLOYMENT'S LEDGER, ASKED ONCE FOR EVERY OPERATOR
		// COMMAND. A command binds nothing, but pointing one at another
		// deployment's ledger is exactly as wrong as pointing a control plane at
		// them, and one wrong DSN reaches it: `billet ca issue` would record an
		// admission in a fleet it has never met.
		if err := verifyLedgerIdentity(ctx, cfg, db); err != nil {
			return nil, errors.Join(err, db.Close())
		}

		return db, nil

	case LedgerDecision:
		// NAMES NO RELEASE, ON PURPOSE, AND THIS IS THE ONLY OPEN THAT MAY.
		// `host-upgrade --from-rollout` on a standby is the older binary reading
		// what it should become from a ledger whose leader has already recorded
		// the newer release. AND IT CREATES NOTHING: the package enables the
		// timer that runs this on every host, including one whose server has
		// never run, and an operator open of an empty state directory would mint
		// a root-owned ledger there five minutes after the install, which the
		// service account then cannot open.
		if !postgres {
			if _, err := os.Lstat(state.LedgerPath(dir)); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("%w: %s", ErrNoLedgerYet, dir)
				}

				return nil, fmt.Errorf("look for the ledger: %w", err)
			}
		}

		if postgres {
			db, err = state.OpenPostgresAdmin(ctx, dir, dsn)
		} else {
			db, err = state.OpenAdmin(ctx, dir)
		}

		if err != nil {
			return nil, err
		}

		if err := verifyLedgerIdentity(ctx, cfg, db); err != nil {
			return nil, errors.Join(err, db.Close())
		}

		return db, nil

	default:
		return nil, fmt.Errorf("wiring: unknown ledger mode %d", int(mode))
	}
}

// verifyLedgerIdentity refuses a ledger that says it belongs to somebody else.
//
// AN ABSENT IDENTITY IS NOT A MISMATCH. A host being prepared has no identity
// file yet (`billet check` is documented as the way to create one) and a ledger
// migrated before the binding existed carries no binding either. Both are
// ordinary and both answer yes; what is refused is two answers that disagree.
// PEEKED RATHER THAN READ, because state.DeploymentID MINTS one when the
// directory has none: a status command that created an identity as a side
// effect of looking would be the thing that makes the next start read a
// deployment as day one.
func verifyLedgerIdentity(ctx context.Context, cfg *config.Config, db *state.DB) error {
	deployment, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	if err != nil || !ok {
		return err
	}

	return db.VerifyDeploymentBinding(ctx, deployment)
}

// newDSN reads the connection string out of the environment.
//
// FROM THE ENVIRONMENT, NEVER FROM THE FILE, and the config only names the
// variable: a DSN carries a password, and a secret written into YAML ends up in
// a backup, a paste buffer and eventually a support thread. The same rule the
// GitHub App private key follows. An empty value is refused here rather than
// passed on, so the diagnostic names the variable an operator has to set instead
// of arriving several layers down as a connection failure. A SQLite deployment
// has no DSN and gets the empty value.
func newDSN(cfg *config.Config) (state.DSN, error) {
	if cfg.Server == nil || cfg.Server.LedgerBackend() != config.StatePostgres {
		return "", nil
	}

	name := cfg.Server.LedgerDSNEnv()

	dsn := os.Getenv(name)
	if dsn == "" {
		return "", fmt.Errorf(
			"server.state.postgres.dsn_env names %s and that variable is empty, so billet has "+
				"no connection string. Export it in the service's environment; it is read "+
				"from there rather than from the config file because it carries a password",
			name)
	}

	return state.DSN(dsn), nil
}
