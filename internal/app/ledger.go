package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// LedgerMode is how a process opens the ledger, which is a property of what the
// process is rather than a choice its caller makes: one opener per mode, each
// naming the running release but the decision read, which must not.
type LedgerMode int

const (
	// The zero LedgerMode opens nothing, so a mode left unset refuses.
	_ LedgerMode = iota
	// LedgerControlPlane is the controller: the directory lock, the migration.
	LedgerControlPlane
	// LedgerStandby waits to become the controller and can write nothing.
	LedgerStandby
	// LedgerMaintenance is the quiescent upgrade probe, which crosses a
	// host-upgrade fence without admitting operator or workload writes.
	LedgerMaintenance
	// LedgerOperator is a one-shot operator command, under the host's
	// identity exclusion: no directory lock when a control plane holds it, the
	// schema verified rather than migrated.
	LedgerOperator
	// LedgerDecision is the host's read of its own upgrade instruction, which
	// names no release and creates nothing.
	LedgerDecision
	// LedgerInspect is a report: nothing created, locked, claimed or migrated.
	LedgerInspect
)

// OpenLedger opens the ledger in mode.
func OpenLedger(ctx context.Context, cfg *config.Config, mode LedgerMode) (*state.DB, error) {
	switch mode {
	case LedgerControlPlane:
		return openControlPlaneLedger(ctx, cfg)
	case LedgerStandby:
		return openStandbyLedger(ctx, cfg)
	case LedgerMaintenance:
		return openMaintenanceLedger(ctx, cfg)
	case LedgerOperator:
		return openOperatorLedger(ctx, cfg)
	case LedgerDecision:
		return openDecisionLedger(ctx, cfg)
	case LedgerInspect:
		dsn, err := LedgerDSN(cfg)
		if err != nil {
			return nil, err
		}

		return openInspectLedger(ctx, cfg, dsn)
	default:
		return nil, fmt.Errorf("app: no ledger mode %d", mode)
	}
}

// OpenLedgerWith opens the ledger in mode with the caller's connection string,
// for a command handed the environment file the unit names rather than the
// process environment. Only the operator and inspect modes take one.
func OpenLedgerWith(ctx context.Context, cfg *config.Config, mode LedgerMode, dsn state.DSN) (*state.DB, error) {
	switch mode {
	case LedgerOperator:
		return openOperatorLedgerWith(ctx, cfg, dsn)
	case LedgerInspect:
		return openInspectLedger(ctx, cfg, dsn)
	default:
		return nil, fmt.Errorf("app: ledger mode %d takes no connection string of the caller's", mode)
	}
}

// openControlPlaneLedger opens the ledger as the CONTROL PLANE, taking the exclusive
// directory lock and migrating.
func openControlPlaneLedger(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := LedgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Server.LedgerBackend() == config.StatePostgres {
		return state.OpenPostgres(ctx, cfg.Server.IdentityDir, dsn,
			state.WithRunningRelease(version.Version()))
	}

	return state.Open(ctx, cfg.Server.IdentityDir, state.WithRunningRelease(version.Version()))
}

// openStandbyLedger opens the ledger for a control plane that is WAITING to
// become this deployment's controller.
//
// POSTGRESQL ONLY, and the refusal lives in config rather than here: a SQLite
// ledger is a file a second machine cannot open, so there is nothing to elect
// over. This is the shape of that refusal one layer down — there is no SQLite
// branch to fall through to.
func openStandbyLedger(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := LedgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Server.LedgerBackend() != config.StatePostgres {
		return nil, fmt.Errorf(
			"server.controllers is %s but this deployment's ledger is %s; config validation "+
				"should have refused that pairing",
			config.ControllersActivePassive, cfg.Server.LedgerBackend())
	}

	return state.OpenPostgresStandby(ctx, cfg.Server.IdentityDir, dsn,
		state.WithRunningRelease(version.Version()))
}

// openMaintenanceLedger opens the ledger for the quiescent upgrade probe, which
// crosses a host-upgrade fence without admitting operator or workload writes.
func openMaintenanceLedger(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	if cfg.Server.LedgerBackend() == config.StatePostgres {
		// A PROBE, NOT A MAINTENANCE HANDLE. billet copies no PostgreSQL ledger,
		// so the transaction there fences, snapshots and migrates nothing; what
		// the candidate proves is that it could serve what it inherits, which is
		// the standby's question. The handle it gets can write nothing and claim
		// nothing, and the migration waits for the candidate's own claim.
		dsn, err := LedgerDSN(cfg)
		if err != nil {
			return nil, err
		}

		return state.OpenPostgresProbe(ctx, cfg.Server.IdentityDir, dsn,
			state.WithRunningRelease(version.Version()))
	}

	return state.OpenMaintenance(ctx, cfg.Server.IdentityDir,
		state.WithRunningRelease(version.Version()))
}

// LedgerDSN reads the connection string out of the environment.
//
// FROM THE ENVIRONMENT, NEVER FROM THE FILE, and the config only names the
// variable — a DSN carries a password, and a secret written into YAML ends up in
// a backup, a paste buffer and eventually a support thread. The same rule the
// GitHub App private key follows.
//
// AN EMPTY VALUE IS REFUSED HERE rather than passed on, so the diagnostic names
// the variable an operator has to set instead of arriving several layers down as
// a connection failure.
func LedgerDSN(cfg *config.Config) (state.DSN, error) {
	if cfg.Server == nil || cfg.Server.LedgerBackend() != config.StatePostgres {
		return "", nil
	}

	name := cfg.Server.LedgerDSNEnv()

	dsn := state.DSN(os.Getenv(name))
	if dsn == "" {
		return "", fmt.Errorf(
			"server.state.postgres.dsn_env names %s and that variable is empty, so billet has "+
				"no connection string. Export it in the service's environment — it is read "+
				"from there rather than from the config file because it carries a password",
			name)
	}

	return dsn, nil
}

// ErrNoLedgerYet means the state directory holds no ledger to read a decision
// from, which on a host the package just installed is the ordinary state.
var ErrNoLedgerYet = errors.New("no ledger here yet")

// openDecisionLedger opens the ledger for the one read that must not be
// refused by the release watermark: the host's own instruction (LedgerDecision).
//
// NAMES NO RELEASE, ON PURPOSE, AND THIS IS THE ONLY OPEN THAT MAY. Every other
// open in this file names the running binary so a proved older one is refused;
// this one exists because `host-upgrade --from-rollout` on a standby is that
// older binary, reading what it should become from a ledger whose leader has
// already recorded the newer release. It is an operator handle otherwise: it
// verifies the schema is one this binary knows, verifies the deployment
// identity, and the caller reads and closes. The structural test on this file
// exempts it by name.
//
// AND IT CREATES NOTHING. The package enables the timer that runs this on every
// host, including one whose server has never run, and an operator open of an
// empty state directory would mint a root-owned ledger there five minutes after
// the install, which the service account then cannot open. A directory with no
// ledger is nothing to decide about. What it still does, like every operator
// open, is migrate an unheld ledger that is behind this binary; that is the
// same act `billet rollout status` performs on such a host.
func openDecisionLedger(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := LedgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Server.LedgerBackend() != config.StatePostgres {
		if _, err := os.Lstat(state.LedgerPath(cfg.Server.IdentityDir)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s", ErrNoLedgerYet, cfg.Server.IdentityDir)
			}

			return nil, fmt.Errorf("look for the ledger: %w", err)
		}
	}

	var db *state.DB

	// UNDER THE IDENTITY EXCLUSION, borrowed from a command that holds it or
	// taken for the open: the opener creates the directory and its lock on
	// first use, and a retirement renames the directory under a global lock
	// this open now waits for rather than racing. THE HAND-BACK BELONGS TO THE
	// ATTEMPT, not to the handle: the opener creates the directory lock before
	// it connects, so a failed open leaves a root-owned file too.
	err = hostauthority.Under(ctx, cfg.Server.IdentityDir, func() error {
		var openErr error

		if cfg.Server.LedgerBackend() == config.StatePostgres {
			db, openErr = state.OpenPostgresAdmin(ctx, cfg.Server.IdentityDir, dsn)
		} else {
			db, openErr = state.OpenAdmin(ctx, cfg.Server.IdentityDir)
		}

		return errors.Join(openErr, hostauthority.HandBackLedger(cfg.Server.IdentityDir))
	})
	if err != nil {
		return nil, errors.Join(err, closeIfOpen(db))
	}

	if err := VerifyLedgerIdentity(ctx, cfg, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}

// openOperatorLedger opens the ledger for a ONE-SHOT OPERATOR COMMAND
// (LedgerOperator): it proceeds without the directory lock when a control plane
// holds it, and then verifies the schema rather than migrating it. See
// state.OpenAdmin.
func openOperatorLedger(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := LedgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	return openOperatorLedgerWith(ctx, cfg, dsn)
}

// openOperatorLedgerWith is openOperatorLedger with the caller's connection string,
// for a command handed the environment file the unit names rather than the
// process environment.
func openOperatorLedgerWith(ctx context.Context, cfg *config.Config, dsn state.DSN) (*state.DB, error) {
	var (
		db  *state.DB
		err error
	)

	// Under the identity exclusion and with the hand-back on the attempt, as in
	// openDecisionLedger.
	err = hostauthority.Under(ctx, cfg.Server.IdentityDir, func() error {
		var openErr error

		if cfg.Server.LedgerBackend() == config.StatePostgres {
			db, openErr = state.OpenPostgresAdmin(ctx, cfg.Server.IdentityDir, dsn,
				state.WithRunningRelease(version.Version()))
		} else {
			db, openErr = state.OpenAdmin(ctx, cfg.Server.IdentityDir,
				state.WithRunningRelease(version.Version()))
		}

		return errors.Join(openErr, hostauthority.HandBackLedger(cfg.Server.IdentityDir))
	})
	if err != nil {
		return nil, errors.Join(err, closeIfOpen(db))
	}

	// AND IT IS THIS DEPLOYMENT'S LEDGER, ASKED ONCE FOR EVERY OPERATOR COMMAND.
	//
	// A command binds nothing — it is not the authority for what these rows are —
	// but pointing one at another deployment's ledger is exactly as wrong as
	// pointing a control plane at them, and one wrong DSN reaches it. `billet ca
	// issue` would record an admission in a fleet it has never met.
	//
	// PEEKED RATHER THAN READ, because state.DeploymentID MINTS one when the
	// directory has none: a status command that created an identity as a side
	// effect of looking would be the thing that makes the next start read a
	// deployment as day one.
	if err := VerifyLedgerIdentity(ctx, cfg, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}

// VerifyLedgerIdentity refuses a ledger that says it belongs to somebody else.
//
// AN ABSENT IDENTITY IS NOT A MISMATCH. A host being prepared has no identity
// file yet — `billet check` is documented as the way to create one — and a
// ledger migrated before the binding existed carries no binding either. Both are
// ordinary and both answer yes; what is refused is two answers that disagree.
func VerifyLedgerIdentity(ctx context.Context, cfg *config.Config, db *state.DB) error {
	deployment, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	if err != nil || !ok {
		return err
	}

	return db.VerifyDeploymentBinding(ctx, deployment)
}

// openInspectLedger opens the ledger for a REPORT (LedgerInspect), through
// state.OpenInspect: an
// existing ledger only, nothing created, locked, claimed, migrated or recorded,
// the schema exactly this binary's and the identity verified. The DSN is the
// caller's, because a report may be handed the environment file the unit
// names rather than the process environment.
func openInspectLedger(ctx context.Context, cfg *config.Config, dsn state.DSN) (*state.DB, error) {
	var (
		db  *state.DB
		err error
	)

	if cfg.Server.LedgerBackend() == config.StatePostgres {
		db, err = state.OpenPostgresInspect(ctx, cfg.Server.IdentityDir, dsn,
			state.WithRunningRelease(version.Version()))
	} else {
		db, err = state.OpenInspect(ctx, cfg.Server.IdentityDir,
			state.WithRunningRelease(version.Version()))
	}

	if err != nil {
		return nil, err
	}

	if err := VerifyLedgerIdentity(ctx, cfg, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}
