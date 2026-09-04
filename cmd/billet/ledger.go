package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// THE ONE PLACE THAT TURNS A CONFIG INTO AN OPEN LEDGER.
//
// Every command that reaches the control-plane store comes through here, so the
// question "which backend is this deployment on" is answered once. Written as
// eighteen call sites each choosing for themselves, the failure is a command
// that opens SQLite against a deployment whose ledger is in PostgreSQL — which
// on a fresh directory does not fail: it CREATES one, migrates it, and reports
// an empty fleet.

// openState opens the ledger as the CONTROL PLANE, taking the exclusive
// directory lock and migrating.
func openState(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := ledgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Server.LedgerBackend() == config.StatePostgres {
		return state.OpenPostgres(ctx, cfg.Server.IdentityDir, dsn,
			state.WithRunningRelease(version.Version()))
	}

	return state.Open(ctx, cfg.Server.IdentityDir, state.WithRunningRelease(version.Version()))
}

// openStateStandby opens the ledger for a control plane that is WAITING to
// become this deployment's controller.
//
// POSTGRESQL ONLY, and the refusal lives in config rather than here: a SQLite
// ledger is a file a second machine cannot open, so there is nothing to elect
// over. This is the shape of that refusal one layer down — there is no SQLite
// branch to fall through to.
func openStateStandby(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := ledgerDSN(cfg)
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

// errNoLedgerYet means the state directory holds no ledger to read a decision
// from, which on a host the package just installed is the ordinary state.
var errNoLedgerYet = errors.New("no ledger here yet")

// openStateForDecision opens the ledger for the one read that must not be
// refused by the release watermark: the host's own instruction.
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
func openStateForDecision(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := ledgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Server.LedgerBackend() != config.StatePostgres {
		if _, err := os.Lstat(state.LedgerPath(cfg.Server.IdentityDir)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s", errNoLedgerYet, cfg.Server.IdentityDir)
			}

			return nil, fmt.Errorf("look for the ledger: %w", err)
		}
	}

	var db *state.DB

	if cfg.Server.LedgerBackend() == config.StatePostgres {
		db, err = state.OpenPostgresAdmin(ctx, cfg.Server.IdentityDir, dsn)
	} else {
		db, err = state.OpenAdmin(ctx, cfg.Server.IdentityDir)
	}

	if err != nil {
		return nil, err
	}

	if err := verifyLedgerIdentity(ctx, cfg, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}

// openStateAdmin opens the ledger for a ONE-SHOT OPERATOR COMMAND: it proceeds
// without the directory lock when a control plane holds it, and then verifies
// the schema rather than migrating it. See state.OpenAdmin.
func openStateAdmin(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	dsn, err := ledgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	var db *state.DB

	if cfg.Server.LedgerBackend() == config.StatePostgres {
		db, err = state.OpenPostgresAdmin(ctx, cfg.Server.IdentityDir, dsn,
			state.WithRunningRelease(version.Version()))
	} else {
		db, err = state.OpenAdmin(ctx, cfg.Server.IdentityDir,
			state.WithRunningRelease(version.Version()))
	}

	if err != nil {
		return nil, err
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
	if err := verifyLedgerIdentity(ctx, cfg, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}

// verifyLedgerIdentity refuses a ledger that says it belongs to somebody else.
//
// AN ABSENT IDENTITY IS NOT A MISMATCH. A host being prepared has no identity
// file yet — `billet check` is documented as the way to create one — and a
// ledger migrated before the binding existed carries no binding either. Both are
// ordinary and both answer yes; what is refused is two answers that disagree.
func verifyLedgerIdentity(ctx context.Context, cfg *config.Config, db *state.DB) error {
	deployment, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	if err != nil || !ok {
		return err
	}

	return db.VerifyDeploymentBinding(ctx, deployment)
}

// openStateMaintenance opens the ledger for the quiescent upgrade probe, which
// crosses a host-upgrade fence without admitting operator or workload writes.
func openStateMaintenance(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	if cfg.Server.LedgerBackend() == config.StatePostgres {
		// A PROBE, NOT A MAINTENANCE HANDLE. billet copies no PostgreSQL ledger,
		// so the transaction there fences, snapshots and migrates nothing; what
		// the candidate proves is that it could serve what it inherits, which is
		// the standby's question. The handle it gets can write nothing and claim
		// nothing, and the migration waits for the candidate's own claim.
		dsn, err := ledgerDSN(cfg)
		if err != nil {
			return nil, err
		}

		return state.OpenPostgresProbe(ctx, cfg.Server.IdentityDir, dsn,
			state.WithRunningRelease(version.Version()))
	}

	return state.OpenMaintenance(ctx, cfg.Server.IdentityDir,
		state.WithRunningRelease(version.Version()))
}

// ledgerDSN reads the connection string out of the environment.
//
// FROM THE ENVIRONMENT, NEVER FROM THE FILE, and the config only names the
// variable — a DSN carries a password, and a secret written into YAML ends up in
// a backup, a paste buffer and eventually a support thread. The same rule the
// GitHub App private key follows.
//
// AN EMPTY VALUE IS REFUSED HERE rather than passed on, so the diagnostic names
// the variable an operator has to set instead of arriving several layers down as
// a connection failure.
func ledgerDSN(cfg *config.Config) (string, error) {
	if cfg.Server == nil || cfg.Server.LedgerBackend() != config.StatePostgres {
		return "", nil
	}

	name := cfg.Server.LedgerDSNEnv()

	dsn := os.Getenv(name)
	if dsn == "" {
		return "", fmt.Errorf(
			"server.state.postgres.dsn_env names %s and that variable is empty, so billet has "+
				"no connection string. Export it in the service's environment — it is read "+
				"from there rather than from the config file because it carries a password",
			name)
	}

	return dsn, nil
}
