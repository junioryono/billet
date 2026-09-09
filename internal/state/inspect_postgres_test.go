package state

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// THE PROBE OVER A LEDGER ONE MIGRATION BEHIND ITS BINARY: the host transaction's
// candidate, stamped with its release, opens the ledger the old controller still
// serves; its open's watermark check reads through View, and a View that
// re-checked the exact schema refused it with ErrSchemaBehind before the
// candidate could claim and migrate, which made every PostgreSQL upgrade that
// carried a migration roll back. The probe revalidates under the policy it was
// admitted with.
func TestThePostgresProbeReadsALedgerOneMigrationBehind(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := t.Context()

	full := pgTimeline.migrations
	t.Cleanup(func() { pgTimeline.migrations = full })

	behind := full[len(full)-1].Version - 1

	var truncated []migration

	for _, m := range full {
		if m.Version <= behind {
			truncated = append(truncated, m)
		}
	}

	pgTimeline.migrations = truncated

	old, err := OpenPostgres(ctx, t.TempDir(), dsn, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenPostgres at %d: %v", behind, err)
	}

	if _, err := old.ClaimController(ctx, "controller", "deployment-a"); err != nil {
		t.Fatalf("ClaimController at %d: %v", behind, err)
	}

	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	pgTimeline.migrations = full

	probe, err := OpenPostgresProbe(ctx, t.TempDir(), dsn, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("a stamped probe over a ledger one migration behind: %v", err)
	}

	t.Cleanup(func() { _ = probe.Close() })

	release, _, err := probe.ReleaseWatermark(ctx)
	if err != nil {
		t.Fatalf("the probe's read: %v", err)
	}

	if release != "v0.5.0" {
		t.Errorf("the probe read the watermark %q, want v0.5.0", release)
	}

	if err := probe.Tx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, ErrStandby) {
		t.Errorf("the probe's Tx: err = %v, want ErrStandby", err)
	}

	// AND A STANDBY, the same handle without the fence bypass.
	standby, err := OpenPostgresStandby(ctx, t.TempDir(), dsn, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("a stamped standby over a ledger one migration behind: %v", err)
	}

	t.Cleanup(func() { _ = standby.Close() })

	if _, _, err := standby.ReleaseWatermark(ctx); err != nil {
		t.Errorf("the standby's read: %v", err)
	}
}

// A POSTGRESQL INSPECTION RE-READS THE WATERMARK ON EVERY VIEW.
func TestAPostgresInspectionRevalidatesTheWatermark(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := t.Context()

	first, err := OpenPostgres(ctx, t.TempDir(), dsn, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := first.ClaimController(ctx, "controller", "deployment-a"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	inspect, err := OpenPostgresInspect(ctx, t.TempDir(), dsn, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("OpenPostgresInspect: %v", err)
	}

	t.Cleanup(func() { _ = inspect.Close() })

	if err := inspect.View(ctx, func(Querier) error { return nil }); err != nil {
		t.Fatalf("a View before the newer claim: %v", err)
	}

	newer, err := OpenPostgres(ctx, t.TempDir(), dsn, WithRunningRelease("v0.7.0"))
	if err != nil {
		t.Fatalf("OpenPostgres as v0.7.0: %v", err)
	}

	t.Cleanup(func() { _ = newer.Close() })

	if _, err := newer.ClaimController(ctx, "controller-b", "deployment-a"); err != nil {
		t.Fatalf("ClaimController as v0.7.0: %v", err)
	}

	ran := false

	err = inspect.View(ctx, func(Querier) error {
		ran = true

		return nil
	})
	if !errors.Is(err, ErrReleaseBehind) {
		t.Errorf("a View after a newer release claimed: err = %v, want ErrReleaseBehind", err)
	}

	if ran {
		t.Error("the callback ran on a ledger a newer release has served")
	}
}

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

// AN INSPECTION'S VIEW IS ONE SNAPSHOT, PROVED THROUGH THE VIEW ITSELF: a row
// replaced and committed on another connection in the middle of a callback is
// not seen until the next View. Under PostgreSQL's default READ COMMITTED a
// second read inside the callback would see the replacement, so this is what a
// View that began its transaction without the inspection's options would fail;
// the transaction's settings were asked directly above, and this asks the
// transaction View actually made.
func TestAPostgresInspectionsViewIsOneSnapshot(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := t.Context()

	plane, err := OpenPostgres(ctx, t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })

	seedNode(t, plane, "epyc-1", "docker")

	inspect, err := OpenPostgresInspect(ctx, t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgresInspect: %v", err)
	}

	t.Cleanup(func() { _ = inspect.Close() })

	read := func(q Querier) string {
		t.Helper()

		got, err := ReadQueries(q).ReadNodeProvider(ctx, "epyc-1")
		if err != nil {
			t.Fatalf("read the node's provider: %v", err)
		}

		return got
	}

	if err := inspect.View(ctx, func(q Querier) error {
		if got := read(q); got != "docker" {
			t.Fatalf("the first read inside the View saw %q, want docker", got)
		}

		// COMMITTED ON ANOTHER CONNECTION while this View's transaction is open.
		if err := plane.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1'`)

			return err
		}); err != nil {
			t.Fatalf("replace the row on the control plane: %v", err)
		}

		if got := read(q); got != "docker" {
			t.Errorf("the second read inside the same View saw %q, want the snapshot's docker", got)
		}

		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	if got := providerVia(t, inspect, "epyc-1"); got != "tart" {
		t.Errorf("the next View saw %q, want the committed tart", got)
	}
}
