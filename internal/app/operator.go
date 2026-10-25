package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

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
	return openAllocator(ctx, cfgPath, LedgerOperator)
}

// OpenReport is OpenOperator for a command that only reads: the ledger in
// LedgerInspect mode, which creates, locks, claims and migrates nothing and
// refuses a schema that is not exactly this binary's, so a report run beside
// a control plane of another release can neither move its ledger nor misread
// it. The returned allocator can answer reads only.
//
// ON SQLITE, ONLY AS THE LEDGER'S OWNER. A read-only open of a stopped SQLite
// ledger creates its sidecar files owned by whoever opened it, and a service
// account that cannot write them cannot reopen its own ledger; so a report
// run as anyone but the identity directory's owner, root included, is refused
// before the ledger is opened (the rule `billet rollout status` holds, which
// re-executes itself as the owner instead). A PostgreSQL ledger has no sidecar.
func OpenReport(ctx context.Context, cfgPath string) (*alloc.Allocator, func(), error) {
	a, _, closeDB, err := openAllocator(ctx, cfgPath, LedgerInspect)

	return a, closeDB, err
}

// openAllocator is OpenOperator and OpenReport: the configuration, the ledger
// in mode, and the allocator over it.
func openAllocator(ctx context.Context, cfgPath string, mode LedgerMode,
) (*alloc.Allocator, *state.DB, func(), error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}

	if cfg.Server == nil {
		return nil, nil, nil, errors.New("this command runs on the control plane, and this " +
			"config has no server section")
	}

	if mode == LedgerInspect {
		if err := reportRunsAsOwner(cfg); err != nil {
			return nil, nil, nil, err
		}
	}

	db, err := OpenLedger(ctx, cfg, mode)
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

// reportLedgerDir is the directory a SQLite ledger is opened in, and its
// sidecars created in: the parent of the ledger's joined path. It is not
// always the identity directory as spelled, whose own check (state's
// requireLedgerFile) resolves it as the kernel does: a link followed by `..`
// there resolves beside the link's target, while the join cleans the `..`
// away and opens beside the link.
func reportLedgerDir(cfg *config.Config) string {
	return filepath.Dir(cfg.Server.LedgerPath())
}

// reportEUID is who this process runs as; a test stands another in.
var reportEUID = os.Geteuid

// reportRunsAsOwner refuses a report on a SQLite ledger from any account but
// the owner of the directory the ledger's sidecars would be created in
// (OpenReport). A directory that does not exist is left for the open to refuse
// with its own diagnostic, and an owner that cannot be read refuses, since
// could-not-tell is not the owner.
//
// THE DIRECTORY THE LEDGER IS OPENED IN, NOT THE ONE THE CONFIG SPELLS
// (reportLedgerDir), followed if a link, as the ledger's own open follows it.
func reportRunsAsOwner(cfg *config.Config) error {
	if cfg.Server.LedgerBackend() == config.StatePostgres {
		return nil
	}

	dir := reportLedgerDir(cfg)

	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read who owns %s: %w", dir, err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s carries no owner this platform reports, so this report cannot tell whether "+
			"reading it would leave files its owner cannot write", dir)
	}

	if euid := reportEUID(); int(st.Uid) != euid {
		return fmt.Errorf("%s is owned by uid %d and this process runs as uid %d; a report on a SQLite "+
			"ledger leaves files owned by whoever read it, so run it as the ledger's owner (for example "+
			"sudo -u billet)", dir, st.Uid, euid)
	}

	return nil
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
