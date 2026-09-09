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

	if err := inspect.View(t.Context(), func(q Querier) error {
		var readOnly string
		if err := q.QueryRowContext(t.Context(), `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			return err
		}

		if !strings.EqualFold(readOnly, "on") {
			t.Errorf("transaction_read_only inside View is %q, want on", readOnly)
		}

		if got := providerOf(t, q, "epyc-1"); got != "docker" {
			t.Errorf("the inspection read provider %q", got)
		}

		var name string

		err := q.QueryRowContext(t.Context(), `UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1' RETURNING name`).Scan(&name)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "read-only") {
			t.Errorf("a write inside View was not refused by the server: err = %v", err)
		}

		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	// EVERY POOL, fresh connections included: the writer pool is read-only by
	// its DSN and refuses a statement outright.
	if _, err := inspect.w.ExecContext(t.Context(), `UPDATE nodes SET provider = 'tart'`); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("the inspection's writer pool accepted a write: err = %v", err)
	}

	// A SESSION CANNOT TALK ITS WAY OUT: the default is a connection parameter,
	// and a transaction that asks for read-write is still refused a write, or the
	// pool's next connection would be one statement away from writing.
	if _, err := inspect.w.ExecContext(t.Context(), `SET default_transaction_read_only = off`); err == nil {
		var readOnly string
		if err := inspect.w.QueryRowContext(t.Context(), `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil {
			t.Fatalf("SHOW default_transaction_read_only: %v", err)
		}

		t.Logf("the session accepted SET default_transaction_read_only = off (now %q); "+
			"the refusal below is what holds", readOnly)
	}

	if err := inspect.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the write callback ran on a PostgreSQL inspection")

		return nil
	}); !errors.Is(err, ErrInspect) {
		t.Errorf("Tx on an inspection: err = %v, want ErrInspect", err)
	}

	if got := providerOf(t, inspect.Reader(), "epyc-1"); got != "docker" {
		t.Errorf("the sentinel row reads %q after the refused writes", got)
	}

	// THE EXCLUSION IS NEITHER TAKEN NOR DISTURBED: a second control plane is
	// still refused by the first while the inspection is open, and once the
	// first closes a control plane claims while the inspection stays open.
	if _, err := OpenPostgres(t.Context(), t.TempDir(), dsn); !errors.Is(err, ErrControllerHeld) {
		t.Errorf("a second control plane beside the inspection: err = %v, want ErrControllerHeld", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatal(err)
	}

	successor, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("a control plane could not claim while an inspection was open: %v", err)
	}

	_ = successor.Close()
}
