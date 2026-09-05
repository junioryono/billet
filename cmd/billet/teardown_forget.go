package main

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/config"
)

// forgetScaleSet drops billet's record of having created a scale set.
//
// Opened through OpenAdmin because a control plane may be running: this is a
// bounded write by a command with a person waiting on it, not a scheduling
// decision, and refusing to run against a live deployment would make teardown
// unusable exactly when it is needed.
//
// TAKES THE ALREADY-LOADED CONFIG. Reloading it here would give a second place
// for the path to be wrong and a second failure to swallow.
//
// The ONE thing it treats as nothing to do is a config that names no state
// directory, which cannot hold a record. Every other failure is returned: a
// ledger that will not open is a row left behind, and the caller says so rather
// than printing Done over it.
func forgetScaleSet(ctx context.Context, cfg *config.Config, target, group, label string) error {
	if cfg == nil || cfg.Server == nil || cfg.Server.IdentityDir == "" {
		return nil
	}

	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open the ledger at %s: %w", cfg.Server.IdentityDir, err)
	}
	defer func() { _ = db.Close() }()

	return db.ForgetScaleSet(ctx, target, group, label)
}
