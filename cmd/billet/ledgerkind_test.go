package main

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
)

// THE JOURNAL'S SHAPE FOLLOWS THE LEDGER'S BACKEND. A PostgreSQL controller gets
// the external-ledger transaction, which fences, snapshots and migrates nothing;
// a SQLite controller and a node-only host get the ordinary one. Read from the
// same accessor every other reader of the backend uses, so a config naming the
// database is never handed a snapshot step.
func TestTheJournalShapeFollowsTheLedgerBackend(t *testing.T) {
	t.Parallel()

	postgres := &config.Config{Server: &config.ServerConfig{
		State: &config.StateConfig{Backend: config.StatePostgres},
	}}

	if got := ledgerKindFor(postgres); got != hostupgrade.LedgerExternal {
		t.Errorf("a PostgreSQL controller gets ledger kind %q, want external", got)
	}

	if got := ledgerKindFor(&config.Config{Server: &config.ServerConfig{}}); got != "" {
		t.Errorf("a SQLite controller gets ledger kind %q, want the local shape", got)
	}

	if got := ledgerKindFor(&config.Config{Node: &config.NodeConfig{}}); got != "" {
		t.Errorf("a node-only host gets ledger kind %q, want the local shape", got)
	}
}
