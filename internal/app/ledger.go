package app

import (
	"context"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// LedgerMode is how a control-plane process opens the ledger, which is a
// property of what the process is rather than a choice its caller makes: one
// opener per mode, each naming the running release.
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
	default:
		return nil, fmt.Errorf("app: no ledger mode %d", mode)
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
