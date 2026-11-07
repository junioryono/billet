package state

import (
	"database/sql"
	"fmt"
	"testing"
)

// ledgerAt75 is a ledger migrated to 75 and no further, so the next open applies
// migration 76 and every one after it. rewindNewest takes back the newest, which is
// the pool release's 79, and then 78, 77 and 76 are undone.
//
// IT REFUSES A NEWER BINARY, as rewindNewest does: a migration 80 would be the one
// rewindNewest took back, and 79 would stay behind on a ledger this test still calls 75.
func ledgerAt75(t *testing.T) string {
	t.Helper()

	if latest := LatestSchemaVersion(); latest != 79 {
		t.Fatalf("this binary's newest migration is %d; ledgerAt75 undoes 79 through 76 and must be "+
			"taught each migration after 75 before it can stand for a ledger at 75", latest)
	}

	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rewindNewest(t, db)

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP INDEX job_history_running_idx`,
			`DROP INDEX job_history_result_idx`,
			`DROP INDEX alert_outbox_due_idx`,
			`DROP INDEX alert_marks_decided_idx`,
			`DROP TABLE alert_outbox`,
			`DROP TABLE alert_streaks`,
			`DROP TABLE alert_marks`,
			`DROP TABLE alert_rules`,
			`DELETE FROM schema_migrations WHERE version = 78`,
			`DROP TABLE lease_holds`,
			`DELETE FROM schema_migrations WHERE version = 77`,
			`ALTER TABLE pool_orders DROP COLUMN join_hold`,
			`ALTER TABLE pool_orders DROP COLUMN node_cert_serial`,
			`ALTER TABLE pool_orders DROP COLUMN node_fingerprint`,
			`ALTER TABLE node_enrollments DROP COLUMN pool_order`,
			`DROP INDEX join_tokens_pool_order_idx`,
			`ALTER TABLE join_tokens DROP COLUMN pool_order`,
			`DELETE FROM schema_migrations WHERE version = 76`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version 75: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close the rewound ledger: %v", err)
	}

	return dir
}

// MIGRATION 76 BINDS WHAT MIGRATION 75 LEFT TO ITS ORDER: a token a pool minted with the
// note `pool order <order>`, and a request named `pool-<order>`, are that order's when
// the order is recorded, and nothing else is: an operator's token, a note or a name
// naming no recorded order. A host already approved from a placed order is linked to it
// with its key and its newest certificate's serial, and the order's tokens are retired;
// one still pending is not linked.
func TestMigration76BindsAnOrdersEarlierTokensAndRequests(t *testing.T) {
	t.Parallel()

	dir := ledgerAt75(t)

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
		 VALUES ('p1', 'aws-metal', 'aws:a:r:s:z', 'm', 'x64', 'placed', 'i-1', 't', 't'),
		        ('p2', 'aws-metal', 'aws:a:r:s:z', 'm', 'x64', 'placed', 'i-2', 't', 't')`,
		`INSERT INTO join_tokens (token_sha256, note, uses_remaining, created_at, expires_at)
		 VALUES ('h-pool', 'pool order p1', 1, 't', 't'), ('h-gone', 'pool order p9', 1, 't', 't'),
		        ('h-op', 'for epyc-1', 1, 't', 't'), ('h-joined', 'pool order p2', 1, 't', 't')`,
		`INSERT INTO node_enrollments (name, fingerprint, csr_pem, state, requested_at)
		 VALUES ('pool-p1', 'SHA256:a', 'csr', 'pending', 't'), ('pool-p9', 'SHA256:b', 'csr', 'pending', 't'),
		        ('epyc-1', 'SHA256:c', 'csr', 'pending', 't'), ('pool-p2', 'SHA256:d', 'csr', 'approved', 't')`,
		`INSERT INTO issued_certs (serial, node, source, not_after, issued_at)
		 VALUES ('0a', 'pool-p2', 'enrolled', 'x', '2026-11-01'), ('0b', 'pool-p2', 'renewed', 'x', '2026-11-05')`,
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

	for name, want := range map[string]string{"pool-p1": "p1", "pool-p9": "", "epyc-1": "", "pool-p2": "p2"} {
		if got := read(`SELECT pool_order FROM node_enrollments WHERE name = ?`, name); got != want {
			t.Errorf("request %s is bound to %q, want %q", name, got, want)
		}
	}

	for order, want := range map[string]string{"p1": "||", "p2": "pool-p2|SHA256:d|0b"} {
		got := read(`SELECT node_name || '|' || node_fingerprint || '|' || node_cert_serial FROM pool_orders WHERE order_id = ?`, order)
		if got != want {
			t.Errorf("order %s is linked as %q, want %q", order, got, want)
		}
	}

	for token, want := range map[string]string{"h-joined": "0", "h-pool": "1"} {
		if got := read(`SELECT CAST(uses_remaining AS TEXT) FROM join_tokens WHERE token_sha256 = ?`, token); got != want {
			t.Errorf("token %s has %s uses, want %s", token, got, want)
		}
	}
}
