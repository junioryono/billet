package state

import (
	"database/sql"
	"testing"
)

// MIGRATION 78 STARTS A LINKED ORDER CONTRADICTED: a billet before it recorded nothing
// durable when a decommissioned node came back or reported compute, so an order a release
// could still seal is held for a decommission proved after the upgrade, and its node's
// idle run goes with it. An order no node was linked to, one already gone, and any other
// host's run are left as they were.
func TestMigration78HoldsALinkedOrderForAFreshProof(t *testing.T) {
	t.Parallel()

	dir := rewoundLedger(t)

	pools, err := newSQLiteBackend(dir).dataSources()
	if err != nil {
		t.Fatalf("dataSources: %v", err)
	}

	w, err := sql.Open("sqlite", pools.writer)
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	for _, stmt := range []string{
		`INSERT INTO pool_orders (order_id, source, scope, shape, arch, state, host_id, node_name, first_attempt_at, updated_at)
		 VALUES ('p1', 'aws-metal', 'aws:a:r:s:z', 'm', 'x64', 'placed', 'i-1', 'pool-p1', 't', 't'),
		        ('p2', 'aws-metal', 'aws:a:r:s:z', 'm', 'x64', 'placed', 'i-2', '', 't', 't'),
		        ('p3', 'aws-metal', 'aws:a:r:s:z', 'm', 'x64', 'gone', 'i-3', 'pool-p3', 't', 't')`,
		`INSERT INTO compute_barrier_nodes (node, barrier_id, node_epoch, dispatch_generation, empty_since, observed_at)
		 VALUES ('pool-p1', 'b', 1, 0, 't', 't'), ('pool-p3', 'b', 1, 0, 't', 't'), ('epyc-1', 'b', 1, 0, 't', 't')`,
	} {
		if _, err := w.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	for order, want := range map[string]int64{"p1": 1, "p2": 0, "p3": 0} {
		var got int64
		if err := db.w.QueryRowContext(t.Context(),
			`SELECT release_contradicted FROM pool_orders WHERE order_id = ?`, order).Scan(&got); err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Errorf("order %s is contradicted %d, want %d", order, got, want)
		}
	}

	for node, want := range map[string]bool{"pool-p1": false, "pool-p3": true, "epyc-1": true} {
		var n int
		if err := db.w.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM compute_barrier_nodes WHERE node = ?`, node).Scan(&n); err != nil {
			t.Fatal(err)
		}

		if got := n == 1; got != want {
			t.Errorf("node %s's idle run kept %v, want %v", node, got, want)
		}
	}
}
