package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// OpenOperator opens what a one-shot operator command on the control plane works
// with: the configuration at cfgPath, the ledger in LedgerOperator mode, and the
// capacity allocator over it. The returned func closes the ledger.
//
// THE OPERATOR MODE RATHER THAN THE CONTROL PLANE'S, because these commands run
// WHILE the control plane is running — which is the only time most of them are
// any use — and the control plane's open takes the exclusive directory lock the
// server holds for its whole life.
//
// ONE IMPLEMENTATION, for every command that reads or changes capacity: a second
// copy of this open is how two commands end up disagreeing about which limits a
// deployment has, and the limits are the control plane's own (allocatorLimits).
func OpenOperator(ctx context.Context, cfgPath string) (*alloc.Allocator, *state.DB, func(), error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}

	if cfg.Server == nil {
		return nil, nil, nil, errors.New("this command runs on the control plane, and this " +
			"config has no server section")
	}

	db, err := OpenLedger(ctx, cfg, LedgerOperator)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("server state: %w", err)
	}

	a, err := NewAllocator(cfg, db)
	if err != nil {
		db.Close()

		return nil, nil, nil, fmt.Errorf("capacity allocator: %w", err)
	}

	return a, db, func() { db.Close() }, nil
}

// NewAllocator is the capacity allocator an operator command works with over a
// ledger it opened: the control plane's own limits and tiers, so a command and
// the plane it runs beside never disagree about what the deployment may hold.
// cfg must have a server section.
func NewAllocator(cfg *config.Config, db *state.DB) (*alloc.Allocator, error) {
	return alloc.New(db, allocatorLimits(cfg), cfg.Tiers)
}

// allocatorLimits is what a deployment's configuration lets the allocator
// charge: the ceilings, the per-host policies and each target's share.
func allocatorLimits(cfg *config.Config) alloc.Limits {
	return alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
		Shares:    cfg.TargetShares(),
	}
}
