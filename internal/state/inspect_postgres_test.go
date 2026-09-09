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
		if err := inspect.View(t.Context(), func(q Querier) error {
			var readOnly, isolation, set string

			if err := q.QueryRowContext(t.Context(), `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
				return err
			}

			if !strings.EqualFold(readOnly, "on") {
				t.Errorf("round %d: transaction_read_only inside View is %q, want on", round, readOnly)
			}

			if err := q.QueryRowContext(t.Context(), `SHOW transaction_isolation`).Scan(&isolation); err != nil {
				return err
			}

			if !strings.EqualFold(isolation, "repeatable read") {
				t.Errorf("round %d: transaction_isolation inside View is %q, want repeatable read", round, isolation)
			}

			if got := providerOf(t, q, "epyc-1"); got != "docker" {
				t.Errorf("round %d: the inspection read provider %q", round, got)
			}

			if err := q.QueryRowContext(t.Context(),
				`SELECT set_config('default_transaction_read_only', 'off', false)`).Scan(&set); err != nil {
				return err
			}

			var name string

			err := q.QueryRowContext(t.Context(), `UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1' RETURNING name`).Scan(&name)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "read-only") {
				t.Errorf("round %d: a write inside View after set_config was not refused: err = %v", round, err)
			}

			return nil
		}); err != nil {
			t.Fatalf("View: %v", err)
		}
	}

	// THE BARE READER IS NOT HANDED OUT, because a bare connection is where
	// set_config would have worked.
	if err := queryOne(t, inspect.Reader()); !errors.Is(err, ErrInspect) {
		t.Errorf("Reader() on a PostgreSQL inspection: err = %v, want ErrInspect", err)
	}

	// The writer pool refuses a statement outright by its DSN's default.
	if _, err := inspect.w.ExecContext(t.Context(), `UPDATE nodes SET provider = 'tart'`); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("the inspection's writer pool accepted a write: err = %v", err)
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
