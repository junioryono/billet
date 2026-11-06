package state

import (
	"database/sql"
	"testing"
)

// MIGRATION 75 BINDS WHAT MIGRATION 74 LEFT TO ITS ORDER: a token a pool minted with the
// note `pool order <order>`, and a request named `pool-<order>`, are that order's when
// the order is recorded, and nothing else is: an operator's token, a note or a name
// naming no recorded order.
func TestMigration75BindsAnOrdersEarlierTokensAndRequests(t *testing.T) {
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
		`INSERT INTO pool_orders (order_id, source, scope, shape, arch, state, host_id, first_attempt_at, updated_at)
		 VALUES ('p1', 'aws-metal', 'aws:a:r:s:z', 'm', 'x64', 'placed', 'i-1', 't', 't')`,
		`INSERT INTO join_tokens (token_sha256, note, uses_remaining, created_at, expires_at)
		 VALUES ('h-pool', 'pool order p1', 1, 't', 't'), ('h-gone', 'pool order p9', 1, 't', 't'),
		        ('h-op', 'for epyc-1', 1, 't', 't')`,
		`INSERT INTO node_enrollments (name, fingerprint, csr_pem, state, requested_at)
		 VALUES ('pool-p1', 'SHA256:a', 'csr', 'pending', 't'), ('pool-p9', 'SHA256:b', 'csr', 'pending', 't'),
		        ('epyc-1', 'SHA256:c', 'csr', 'pending', 't')`,
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

	read := func(query, key string) string {
		t.Helper()

		var order string
		if err := db.w.QueryRowContext(t.Context(), query, key).Scan(&order); err != nil {
			t.Fatalf("%s %s: %v", query, key, err)
		}

		return order
	}

	for token, want := range map[string]string{"h-pool": "p1", "h-gone": "", "h-op": ""} {
		if got := read(`SELECT pool_order FROM join_tokens WHERE token_sha256 = ?`, token); got != want {
			t.Errorf("token %s is bound to %q, want %q", token, got, want)
		}
	}

	for name, want := range map[string]string{"pool-p1": "p1", "pool-p9": "", "epyc-1": ""} {
		if got := read(`SELECT pool_order FROM node_enrollments WHERE name = ?`, name); got != want {
			t.Errorf("request %s is bound to %q, want %q", name, got, want)
		}
	}
}
