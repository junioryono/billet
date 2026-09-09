package state

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// A POSTGRESQL INSPECTION OPENS UNDER A READ-ONLY DEFAULT ON EVERY CONNECTION,
// TAKES NO ADVISORY EXCLUSION, AND IS REFUSED NOTHING BY ITS OWN READ-ONLY
// WRITER: the durability check that refuses a read-only writer on a control
// plane does not apply to it, and that is asserted by the open succeeding first.
func TestAPostgresInspectionReadsUnderAReadOnlyDefaultAndClaimsNothing(t *testing.T) {
	dsn := requirePostgres(t)

	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })

	seedNode(t, plane, "epyc-1", "docker")

	var inspect *DB

	ops := sideEffects(t, func() {
		inspect, err = OpenPostgresInspect(t.Context(), t.TempDir(), dsn)
		if err != nil {
			t.Fatalf("OpenPostgresInspect beside a live control plane: %v", err)
		}
	})

	t.Cleanup(func() { _ = inspect.Close() })

	if len(ops) != 0 {
		t.Errorf("a PostgreSQL inspection performed %v", ops)
	}

	// THE TRANSACTION ITSELF IS READ ONLY AND REPEATABLE READ, and the session
	// default is beside the point: set_config turns the default off on this very
	// connection (measured: on a bare pooled connection the UPDATE after it wrote
	// a row), and the UPDATE in the same View is still refused, as is one in the
	// next View on whichever connection the pool hands out.
	for round := range 3 {
		// The transaction View begins, asked directly what it is.
		tx, err := inspect.r.BeginTx(t.Context(), inspect.readTxOptions())
		if err != nil {
			t.Fatal(err)
		}

		var readOnly, isolation string

		if err := tx.QueryRowContext(t.Context(), `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			t.Fatal(err)
		}

		if !strings.EqualFold(readOnly, "on") {
			t.Errorf("round %d: transaction_read_only in the read transaction is %q, want on", round, readOnly)
		}

		if err := tx.QueryRowContext(t.Context(), `SHOW transaction_isolation`).Scan(&isolation); err != nil {
			t.Fatal(err)
		}

		if !strings.EqualFold(isolation, "repeatable read") {
			t.Errorf("round %d: transaction_isolation in the read transaction is %q, want repeatable read", round, isolation)
		}

		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}

		if err := inspect.View(t.Context(), func(q Querier) error {
			var set string

			if got, err := ReadQueries(q).ReadNodeProvider(t.Context(), "epyc-1"); err != nil || got != "docker" {
				t.Errorf("round %d: the inspection read provider %q (err %v)", round, got, err)
			}

			// set_config is refused before the engine; the UPDATE after it would be
			// too, and inside this transaction the engine refuses it as well.
			if err := q.QueryRowContext(t.Context(),
				`SELECT set_config('default_transaction_read_only', 'off', false)`).Scan(&set); err == nil ||
				!strings.Contains(err.Error(), "admits only the named reads") {
				t.Errorf("round %d: set_config inside View: err = %v, want the refusal", round, err)
			}

			var name string

			err := q.QueryRowContext(t.Context(), `UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1' RETURNING name`).Scan(&name)
			if err == nil || !strings.Contains(err.Error(), "admits only the named reads") {
				t.Errorf("round %d: a write inside View was not refused: err = %v", round, err)
			}

			return nil
		}); err != nil {
			t.Fatalf("View: %v", err)
		}
	}

	// THE TRANSACTION CANNOT BE ENDED OR THE SESSION CHANGED FROM INSIDE VIEW:
	// COMMIT, set_config and an advisory lock are refused before the engine sees
	// them, and the querier still answers the generated read afterwards.
	if err := inspect.View(t.Context(), func(q Querier) error {
		for _, stmt := range []string{"COMMIT", "SELECT set_config('default_transaction_read_only', 'off', false)",
			"SELECT pg_try_advisory_lock(424242)", "UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1' RETURNING name"} {
			if err := queryOne(t, querierWith(q, stmt)); !errors.Is(err, ErrInspect) {
				t.Errorf("%q through an inspection's View: err = %v, want ErrInspect", stmt, err)
			}
		}

		if _, err := ReadQueries(q).ReadDeploymentBinding(t.Context()); err != nil && !strings.Contains(err.Error(), "no rows") {
			t.Errorf("a generated read after the refusals: %v", err)
		}

		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	var held bool
	if err := plane.View(t.Context(), func(q Querier) error {
		return q.QueryRowContext(t.Context(),
			`SELECT COUNT(*) > 0 FROM pg_locks WHERE locktype = 'advisory' AND objid = 424242`).Scan(&held)
	}); err != nil {
		t.Fatal(err)
	}

	if held {
		t.Error("the refused advisory lock is held")
	}

	// THE BARE READER IS NOT HANDED OUT, because a bare connection is where
	// set_config would have worked.
	if err := queryOne(t, inspect.Reader()); !errors.Is(err, ErrInspect) {
		t.Errorf("Reader() on a PostgreSQL inspection: err = %v, want ErrInspect", err)
	}

	// The writer pool refuses a statement outright by its DSN's default, and the
	// read transaction View begins refuses one whatever the session default
	// says: set_config on the transaction's own connection, then the UPDATE, is
	// SQLSTATE 25006 (measured 2026-09-09, the reason the transaction is READ ONLY).
	if _, err := inspect.w.ExecContext(t.Context(), `UPDATE nodes SET provider = 'tart'`); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("the inspection's writer pool accepted a write: err = %v", err)
	}

	tx, err := inspect.r.BeginTx(t.Context(), inspect.readTxOptions())
	if err != nil {
		t.Fatal(err)
	}

	var set string
	if err := tx.QueryRowContext(t.Context(), `SELECT set_config('default_transaction_read_only', 'off', false)`).Scan(&set); err != nil {
		t.Fatal(err)
	}

	var name string
	if err := tx.QueryRowContext(t.Context(), `UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1' RETURNING name`).Scan(&name); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("an UPDATE inside the read transaction after set_config: err = %v, want SQLSTATE 25006", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	if err := inspect.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the write callback ran on a PostgreSQL inspection")

		return nil
	}); !errors.Is(err, ErrInspect) {
		t.Errorf("Tx on an inspection: err = %v, want ErrInspect", err)
	}

	if got := providerVia(t, inspect, "epyc-1"); got != "docker" {
		t.Errorf("the sentinel row reads %q after the refused writes", got)
	}

	// THE EXCLUSION IS NEITHER TAKEN NOR DISTURBED: a second control plane is
	// still refused by the first while the inspection is open; once the first
	// closes, a claim through the inspection is refused before the advisory lock
	// is asked for, and a control plane then claims while the inspection stays
	// open.
	if _, err := OpenPostgres(t.Context(), t.TempDir(), dsn); !errors.Is(err, ErrControllerHeld) {
		t.Errorf("a second control plane beside the inspection: err = %v, want ErrControllerHeld", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := inspect.ClaimController(t.Context(), "inspector", "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrInspect) {
		t.Errorf("ClaimController on a PostgreSQL inspection: err = %v, want ErrInspect", err)
	}

	successor, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("a control plane could not claim while an inspection was open: %v", err)
	}

	_ = successor.Close()
}
