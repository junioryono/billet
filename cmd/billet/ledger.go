package main

import (
	"context"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// The ledger opens are internal/app's modes; these name them for the commands
// that choose an opener as a value and for the tests that hold which one a
// command uses.

// errNoLedgerYet means the state directory holds no ledger to read a decision
// from, which on a host the package just installed is the ordinary state.
var errNoLedgerYet = app.ErrNoLedgerYet

func openStateForDecision(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	return app.OpenLedger(ctx, cfg, app.LedgerDecision)
}

func openStateAdmin(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	return app.OpenLedger(ctx, cfg, app.LedgerOperator)
}

func openStateInspect(ctx context.Context, cfg *config.Config, dsn state.DSN) (*state.DB, error) {
	return app.OpenLedgerWith(ctx, cfg, app.LedgerInspect, dsn)
}

func openStateMaintenance(ctx context.Context, cfg *config.Config) (*state.DB, error) {
	return app.OpenLedger(ctx, cfg, app.LedgerMaintenance)
}

func verifyLedgerIdentity(ctx context.Context, cfg *config.Config, db *state.DB) error {
	return app.VerifyLedgerIdentity(ctx, cfg, db)
}

// closeIfOpen closes a handle an open may or may not have produced, so an error
// joined onto a failed open can also carry a cleanup error rather than drop it.
func closeIfOpen(db *state.DB) error {
	if db == nil {
		return nil
	}

	return db.Close()
}
