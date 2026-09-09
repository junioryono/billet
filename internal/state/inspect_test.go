package state

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// AN INSPECTION VALIDATES EVERYTHING AN OPEN VALIDATES AND MUTATES NOTHING.
//
// The fixtures here are written against the wrong implementation each names,
// because the state afterwards cannot tell an inspection that mutated and
// restored from one that never mutated: the side-effect seam records every
// operation an open can make, and an inspection fires none of them.

// sideEffects records what an open did, through the seam, for the length of
// fn. Not parallel-safe: the seam is package state.
func sideEffects(t *testing.T, fn func()) []string {
	t.Helper()

	var ops []string

	onOpenSideEffect = func(op string) { ops = append(ops, op) }
	t.Cleanup(func() { onOpenSideEffect = nil })

	fn()

	onOpenSideEffect = nil

	return ops
}

// ledgerWithRow is a closed, migrated ledger holding one node row, the sentinel
// every write refusal below is measured against.
func ledgerWithRow(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	seedNode(t, db, "epyc-1", "docker")

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return dir
}

func seedNode(t *testing.T, db *DB, name, provider string) {
	t.Helper()

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES ($1, $2, 't')`, name, provider)

		return err
	}); err != nil {
		t.Fatalf("seed a node: %v", err)
	}
}

// queryOne runs `SELECT 1` through a querier and returns whatever it refused
// with, draining and closing the rows when it did not refuse.
func queryOne(t *testing.T, q Querier) error {
	t.Helper()

	rows, err := q.QueryContext(t.Context(), `SELECT 1`)
	if err != nil {
		return err
	}

	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close rows: %v", err)
		}
	}()

	for rows.Next() {
	}

	return rows.Err()
}

// providerVia reads through View and the generated query, the one read an
// inspection admits.
func providerVia(t *testing.T, db *DB, name string) string {
	t.Helper()

	var provider string

	if err := db.View(t.Context(), func(q Querier) error {
		got, err := ReadQueries(q).ReadNodeProvider(t.Context(), name)
		if err != nil {
			return fmt.Errorf("read the node's provider: %w", err)
		}

		provider = got

		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	return provider
}

// AN INSPECTION OPENS AN EXISTING LEDGER, READS IT, AND DOES NONE OF THE THINGS AN
// OPEN MAY DO: the seam sees no mkdir, chmod, lock, integrity scan, claim,
// migration or watermark write across the open, a read and the close.
func TestAnInspectionMutatesNothingOnTheWayIn(t *testing.T) {
	dir := ledgerWithRow(t)

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("widen the directory: %v", err)
	}

	ops := sideEffects(t, func() {
		db, err := OpenInspect(t.Context(), dir, WithRunningRelease("v0.9.0"))
		if err != nil {
			t.Fatalf("OpenInspect: %v", err)
		}

		if got := providerVia(t, db, "epyc-1"); got != "docker" {
			t.Errorf("the inspection read provider %q, want docker", got)
		}

		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	if len(ops) != 0 {
		t.Errorf("an inspection performed %v", ops)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if info.Mode().Perm() != 0o755 {
		t.Errorf("the directory's mode is %v after an inspection, want 0755 untouched", info.Mode().Perm())
	}
}

// Tx IS REFUSED BEFORE ANY TRANSACTION BEGINS AND BEFORE THE CALLBACK RUNS: an
// implementation that refused after beginning, or after the callback had written,
// would leave the same row count and pass an assertion on it.
func TestAnInspectionRefusesEveryWriteTransaction(t *testing.T) {
	dir := ledgerWithRow(t)

	db, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	ran := false

	err = db.Tx(t.Context(), func(tx *sql.Tx) error {
		ran = true

		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1'`)

		return err
	})
	if !errors.Is(err, ErrInspect) {
		t.Fatalf("Tx on an inspection: err = %v, want ErrInspect", err)
	}

	if ran {
		t.Error("the write callback ran on an inspection")
	}

	// THE BARE READER IS REFUSED, both ways it can be asked, because a pooled
	// connection's read-only default is a session setting a statement can undo
	// on PostgreSQL; and a write inside View is refused by the engine.
	var name string

	if err := queryOne(t, db.Reader()); !errors.Is(err, ErrInspect) {
		t.Errorf("Reader().QueryContext on an inspection: err = %v, want ErrInspect", err)
	}

	if err := db.Reader().QueryRowContext(t.Context(), `SELECT 1`).Scan(&name); err == nil ||
		!strings.Contains(err.Error(), "bare reader is refused") {
		t.Errorf("Reader().QueryRowContext on an inspection: err = %v, want the refusal", err)
	}

	if err := db.View(t.Context(), func(q Querier) error {
		err := q.QueryRowContext(t.Context(),
			`UPDATE nodes SET provider = 'tart' WHERE name = 'epyc-1' RETURNING name`).Scan(&name)
		if err == nil || !strings.Contains(err.Error(), "admits only the named reads") {
			t.Errorf("a write inside View was not refused before the engine: err = %v", err)
		}

		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	if got := providerVia(t, db, "epyc-1"); got != "docker" {
		t.Errorf("the sentinel row reads %q after the refused writes, want docker", got)
	}

	// AND THE CLAIM IS REFUSED BEFORE THE BACKEND IS ASKED: on PostgreSQL the
	// backend's claim is an advisory lock a refused write would only give back.
	ops := sideEffects(t, func() {
		if _, err := db.ClaimController(t.Context(), "inspector", "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrInspect) {
			t.Errorf("ClaimController on an inspection: err = %v, want ErrInspect", err)
		}
	})

	if len(ops) != 0 {
		t.Errorf("a refused claim performed %v", ops)
	}

	if got := countRows(t, dir, "controller_claim"); got != 0 {
		t.Errorf("controller_claim holds %d rows after a refused claim", got)
	}
}

// THE INSPECTION'S CONNECTION IS READ-ONLY AND NEVER IMMUTABLE: immutable=1
// reads a live WAL without its locks and answers with rows that were never
// committed, so the DSN the real open uses is asserted, not a helper nobody
// calls.
func TestAnInspectionOpensReadOnlyAndNeverImmutable(t *testing.T) {
	pools, err := newSQLiteBackend(t.TempDir()).inspectDataSources()
	if err != nil {
		t.Fatalf("inspectDataSources: %v", err)
	}

	for _, dsn := range []string{pools.writer, pools.reader} {
		if !strings.Contains(dsn, "mode=ro") {
			t.Errorf("an inspection pool is not mode=ro: %s", dsn)
		}

		if !strings.Contains(dsn, "query_only%28ON%29") {
			t.Errorf("an inspection pool is not query_only: %s", dsn)
		}

		if strings.Contains(dsn, "immutable") {
			t.Errorf("an inspection pool is immutable, which reads a live WAL unsafely: %s", dsn)
		}
	}

	// AND THE OPEN USES THEM: the writer pool the handle holds refuses a write
	// with the engine's own answer, which the ordinary writer DSN would accept,
	// and so does the read transaction View begins, asked directly.
	dir := ledgerWithRow(t)

	db, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.w.ExecContext(t.Context(), `UPDATE nodes SET provider = 'tart'`); err == nil ||
		!strings.Contains(err.Error(), "readonly") {
		t.Errorf("the inspection's writer pool accepted a write: err = %v", err)
	}

	tx, err := db.r.BeginTx(t.Context(), db.readTxOptions())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tx.ExecContext(t.Context(), `UPDATE nodes SET provider = 'tart'`); err == nil ||
		!strings.Contains(err.Error(), "readonly") {
		t.Errorf("the inspection's read transaction accepted a write: err = %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// AN ABSENT LEDGER IS ErrNoLedger AND NOTHING IS CREATED: not the directory,
// not the ledger, not a sidecar. A read that failed for another reason keeps its
// own diagnostic and is not absence.
func TestAnInspectionCreatesNoLedger(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never")

	ops := sideEffects(t, func() {
		if _, err := OpenInspect(t.Context(), missing); !errors.Is(err, ErrNoLedger) {
			t.Fatalf("an absent state directory: err = %v, want ErrNoLedger", err)
		}
	})

	if len(ops) != 0 {
		t.Errorf("an inspection of an absent directory performed %v", ops)
	}

	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the inspection created the state directory (lstat err = %v)", err)
	}

	empty := t.TempDir()

	if _, err := OpenInspect(t.Context(), empty); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("a state directory without a ledger: err = %v, want ErrNoLedger", err)
	}

	for _, name := range []string{"billet.db", "billet.db-wal", "billet.db-shm", "billet.lock"} {
		if _, err := os.Lstat(filepath.Join(empty, name)); err == nil {
			t.Errorf("the inspection created %s in a directory that held no ledger", name)
		}
	}

	// A path under a regular file fails with ENOTDIR, which is not absence.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := OpenInspectErr(t, filepath.Join(file, "sub"))
	if errors.Is(err, ErrNoLedger) || !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("a failed read was collapsed into absence: err = %v", err)
	}
}

func OpenInspectErr(t *testing.T, dir string) error {
	t.Helper()

	db, err := OpenInspect(t.Context(), dir)
	if err == nil {
		_ = db.Close()
	}

	return err
}

// THE SCHEMA MUST BE EXACTLY THIS BINARY'S. A ledger behind it is refused naming
// the control plane's restart and is not migrated; an interior hole is refused
// naming the missing migration, not only a missing newest one; a version this
// binary does not know is refused as newer, even carrying a known checksum; and
// an edited early migration is refused by name.
func TestAnInspectionVerifiesTheSchemaAndNeverMigrates(t *testing.T) {
	t.Run("behind", func(t *testing.T) {
		dir := t.TempDir()
		behind := latestVersion(t) - 1

		old := openAt(t, dir, behind)
		seedNode(t, old, "epyc-1", "docker")

		if err := old.Close(); err != nil {
			t.Fatal(err)
		}

		var ops []string

		err := error(nil)

		ops = sideEffects(t, func() { err = OpenInspectErr(t, dir) })

		if !errors.Is(err, ErrSchemaBehind) {
			t.Fatalf("a ledger one migration behind: err = %v, want ErrSchemaBehind", err)
		}

		for _, want := range []string{"rollout_last_refusal", "restart the control plane"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q: %v", want, err)
			}
		}

		if len(ops) != 0 {
			t.Errorf("the refused inspection performed %v", ops)
		}

		applied, err := PeekMigrations(t.Context(), LedgerPath(dir))
		if err != nil {
			t.Fatalf("PeekMigrations: %v", err)
		}

		if got := applied[len(applied)-1].Version; got != behind {
			t.Errorf("the ledger is at version %d after a refused inspection, want %d untouched", got, behind)
		}
	})

	t.Run("interior hole", func(t *testing.T) {
		dir := ledgerWithRow(t)
		hole := latestVersion(t) - 1

		plainExec(t, dir, `DELETE FROM schema_migrations WHERE version = ?`, hole)

		err := OpenInspectErr(t, dir)
		if !errors.Is(err, ErrSchemaBehind) || !strings.Contains(err.Error(), "migration "+itoa(hole)+" ") {
			t.Fatalf("a ledger missing migration %d in the middle: err = %v", hole, err)
		}

		if got := countRows(t, dir, "schema_migrations"); got != int64(latestVersion(t)-1) {
			t.Errorf("schema_migrations holds %d rows after the refusal, want %d untouched", got, latestVersion(t)-1)
		}
	})

	t.Run("ahead with a known checksum", func(t *testing.T) {
		dir := ledgerWithRow(t)
		newer := latestVersion(t) + 1

		plainExec(t, dir, `INSERT INTO schema_migrations (version, name, checksum, applied_at) `+
			`SELECT ?, 'from_the_future', checksum, applied_at FROM schema_migrations WHERE version = ?`,
			newer, latestVersion(t))

		err := OpenInspectErr(t, dir)
		if err == nil || !strings.Contains(err.Error(), "newer version") {
			t.Fatalf("a ledger carrying version %d: err = %v, want the newer-version refusal", newer, err)
		}
	})

	t.Run("an edited early migration", func(t *testing.T) {
		dir := ledgerWithRow(t)

		plainExec(t, dir, `UPDATE schema_migrations SET checksum = 'edited' WHERE version = 2`)

		err := OpenInspectErr(t, dir)
		if err == nil || !strings.Contains(err.Error(), "migration 2 ") {
			t.Fatalf("a ledger whose migration 2 was edited: err = %v", err)
		}
	})
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + fmtInt(n)) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}

	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}

	return digits
}

// plainExec runs one statement against a closed ledger through the ordinary
// writer DSN, the way another binary would.
func plainExec(t *testing.T, dir, stmt string, args ...any) {
	t.Helper()

	pools, err := newSQLiteBackend(dir).dataSources()
	if err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", pools.writer)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = raw.Close() }()

	if _, err := raw.ExecContext(t.Context(), stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func countRows(t *testing.T, dir, table string) int64 {
	t.Helper()

	pools, err := newSQLiteBackend(dir).dataSources()
	if err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", pools.reader)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = raw.Close() }()

	var n int64
	if err := raw.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

// THE FENCE IS HONOURED AT THE OPEN AND INSIDE EVERY READ: one raised after the
// inspection opened refuses the next View before its callback runs.
func TestAnInspectionHonoursTheMaintenanceFence(t *testing.T) {
	dir := ledgerWithRow(t)

	if _, err := WriteMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatal(err)
	}

	if err := OpenInspectErr(t, dir); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("an inspection crossed the fence: err = %v", err)
	}

	if err := ClearMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatal(err)
	}

	db, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if _, err := WriteMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatal(err)
	}

	ran := false

	err = db.View(t.Context(), func(Querier) error { ran = true; return nil })
	if !errors.Is(err, ErrMaintenance) {
		t.Errorf("a View after the fence was raised: err = %v, want ErrMaintenance", err)
	}

	if ran {
		t.Error("the read callback ran across a fence")
	}
}

// THE WATERMARK IS CHECKED AND NEVER WRITTEN: an absent mark stays absent under
// a release that would have been recorded, an older binary is refused, and a
// newer one reads without moving the row, timestamp included.
func TestAnInspectionChecksTheWatermarkAndRecordsNothing(t *testing.T) {
	t.Run("absent stays absent", func(t *testing.T) {
		dir := ledgerWithRow(t)

		ops := sideEffects(t, func() {
			db, err := OpenInspect(t.Context(), dir, WithRunningRelease("v0.9.0"))
			if err != nil {
				t.Fatalf("OpenInspect: %v", err)
			}

			_ = db.Close()
		})

		for _, op := range ops {
			if op == "watermark" {
				t.Error("an inspection wrote the watermark")
			}
		}

		if got := countRows(t, dir, "release_watermark"); got != 0 {
			t.Errorf("release_watermark holds %d rows after an inspection under a release, want 0", got)
		}
	})

	t.Run("served by a newer release", func(t *testing.T) {
		dir := t.TempDir()
		serveAs(t, dir, "v0.5.0")

		before := watermarkRow(t, dir)

		if err := OpenInspectErr(t, dir); err != nil {
			t.Fatalf("an inspection naming no release: %v", err)
		}

		if _, err := OpenInspect(t.Context(), dir, WithRunningRelease("v0.4.9")); !errors.Is(err, ErrReleaseBehind) {
			t.Errorf("an older inspection was admitted: err = %v", err)
		}

		db, err := OpenInspect(t.Context(), dir, WithRunningRelease("v0.6.0"))
		if err != nil {
			t.Fatalf("a newer inspection was refused: %v", err)
		}

		_ = db.Close()

		if after := watermarkRow(t, dir); after != before {
			t.Errorf("the watermark row moved from %v to %v under inspections", before, after)
		}
	})
}

func watermarkRow(t *testing.T, dir string) [2]string {
	t.Helper()

	pools, err := newSQLiteBackend(dir).dataSources()
	if err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", pools.reader)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = raw.Close() }()

	var row [2]string
	if err := raw.QueryRowContext(t.Context(),
		`SELECT release, recorded_at FROM release_watermark WHERE id = 1`).Scan(&row[0], &row[1]); err != nil {
		t.Fatalf("read the watermark row: %v", err)
	}

	return row
}

// THE CLAIM IS NEITHER TAKEN NOR WRITTEN: an absent claim stays absent and a
// present one keeps every field.
func TestAnInspectionTakesNoControllerClaim(t *testing.T) {
	dir := ledgerWithRow(t)

	db, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	claim, err := db.ControllerHolder(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_ = db.Close()

	if claim.Epoch != 0 {
		t.Errorf("an inspection of a never-claimed ledger found a claim at epoch %d", claim.Epoch)
	}

	if got := countRows(t, dir, "controller_claim"); got != 0 {
		t.Errorf("controller_claim holds %d rows after an inspection, want 0", got)
	}

	serveAs(t, dir, "v0.5.0")
	before := claimRow(t, dir)

	db, err = OpenInspect(t.Context(), dir, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	_ = db.Close()

	if after := claimRow(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("the claim row moved from %v to %v under an inspection", before, after)
	}
}

func claimRow(t *testing.T, dir string) []string {
	t.Helper()

	pools, err := newSQLiteBackend(dir).dataSources()
	if err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", pools.reader)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = raw.Close() }()

	rows, err := raw.QueryContext(t.Context(), `SELECT * FROM controller_claim`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}

	var out []string

	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))

		for i := range values {
			ptrs[i] = &values[i]
		}

		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}

		for _, v := range values {
			out = append(out, fmtAny(v))
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return out
}

func fmtAny(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return fmtInt(int(x))
	default:
		return "?"
	}
}

// A SCHEMA CHANGE UNDER AN OPEN INSPECTION IS SEEN BY ITS NEXT READ: the
// read-transaction revalidation an unlocked handle gets is kept, so a bookkeeping
// row this binary does not know, written after the open, refuses the next View
// with the unknown-version diagnostic before its callback runs.
func TestAnInspectionRevalidatesInsideEveryRead(t *testing.T) {
	dir := ledgerWithRow(t)

	db, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if err := db.View(t.Context(), func(Querier) error { return nil }); err != nil {
		t.Fatalf("a View before the change: %v", err)
	}

	plainExec(t, dir, `INSERT INTO schema_migrations (version, name, checksum, applied_at) `+
		`VALUES (?, 'from_the_future', 'x', 't')`, latestVersion(t)+1)

	ran := false

	err = db.View(t.Context(), func(Querier) error { ran = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Errorf("a View after a newer version appeared: err = %v, want the newer-version refusal", err)
	}

	if ran {
		t.Error("the read callback ran against a schema this binary does not know")
	}
}

// EVERY READ AN INSPECTION OFFERS GOES THROUGH VIEW, ScaleSets included: a
// fence raised after the open and a version this binary does not know each
// refuse it, as they refuse a View, because a read outside View would skip
// both.
func TestAnInspectionsScaleSetsHonourTheFenceAndTheSchema(t *testing.T) {
	dir := ledgerWithRow(t)

	db, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ScaleSets(t.Context(), "acme"); err != nil {
		t.Fatalf("ScaleSets before any change: %v", err)
	}

	if _, err := WriteMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ScaleSets(t.Context(), "acme"); !errors.Is(err, ErrMaintenance) {
		t.Errorf("ScaleSets across a fence: err = %v, want ErrMaintenance", err)
	}

	if err := ClearMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatal(err)
	}

	plainExec(t, dir, `INSERT INTO schema_migrations (version, name, checksum, applied_at) `+
		`VALUES (?, 'from_the_future', 'x', 't')`, latestVersion(t)+1)

	if _, err := db.ScaleSets(t.Context(), "acme"); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Errorf("ScaleSets against a newer version: err = %v, want the newer-version refusal", err)
	}
}

// A BINARY WHOSE MIGRATION SET CANNOT BE READ REFUSES TO INSPECT, FIRST: before
// any operation, with the reason.
func TestAnInspectionRefusesAnUnreadableMigrationSetFirst(t *testing.T) {
	dir := ledgerWithRow(t)

	fullSet, fullErr := sqliteTimeline.migrations, sqliteTimeline.loadErr

	t.Cleanup(func() { sqliteTimeline.migrations, sqliteTimeline.loadErr = fullSet, fullErr })

	sqliteTimeline.migrations, sqliteTimeline.loadErr = nil, errors.New("0007_x.sql: pretend the embed broke")

	var err error

	ops := sideEffects(t, func() { err = OpenInspectErr(t, dir) })

	if !errors.Is(err, errMigrationsUnavailable) || !strings.Contains(err.Error(), "pretend the embed broke") {
		t.Fatalf("an inspection with an unreadable migration set: err = %v", err)
	}

	if len(ops) != 0 {
		t.Errorf("the refused inspection performed %v", ops)
	}

	// FIRST MEANS BEFORE THE PATHNAME: with no ledger at all the answer is still
	// the broken binary, never "no ledger", which would send an operator to the
	// wrong machine.
	missing := filepath.Join(t.TempDir(), "never")

	err = OpenInspectErr(t, missing)
	if !errors.Is(err, errMigrationsUnavailable) || errors.Is(err, ErrNoLedger) {
		t.Errorf("an unreadable migration set beside an absent ledger: err = %v, want the migration error first", err)
	}
}

// WHAT A READ-ONLY OPEN SEES AND LEAVES, MEASURED WITH THE BUNDLED DRIVER: a
// row committed to the WAL of a crashed ledger (no -shm) is visible; a commit
// made under a live control plane after the inspection opened is visible to a
// later View; and an inspection of a cleanly stopped ledger CREATES the -shm
// and -wal sidecars beside it, owned by the account that ran it, which is why
// `billet rollout status` runs the report as the ledger's owner.
func TestAnInspectionReadsTheWALAndLeavesSidecarsItOwns(t *testing.T) {
	t.Run("a crashed ledger's WAL row is visible", func(t *testing.T) {
		live := t.TempDir()

		db, err := Open(t.Context(), live)
		if err != nil {
			t.Fatal(err)
		}

		// THE SCHEMA CHECKPOINTED INTO THE MAIN FILE FIRST, then no automatic
		// checkpoint, so the row below is provably in the WAL and not in the main
		// file: the copy of the main file alone is a migrated ledger that lacks it.
		if _, err := db.w.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}

		if _, err := db.w.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint = 0`); err != nil {
			t.Fatal(err)
		}

		seedNode(t, db, "wal-row", "docker")

		copyLedger := func(names ...string) string {
			dir := t.TempDir()

			for _, name := range names {
				body, err := os.ReadFile(filepath.Join(live, name))
				if err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			return dir
		}

		mainOnly := copyLedger("billet.db")
		crashed := copyLedger("billet.db", "billet.db-wal")

		_ = db.Close()

		// The main file alone does not hold the row: proof the row is WAL-resident.
		mainInspect, err := OpenInspect(t.Context(), mainOnly)
		if err != nil {
			t.Fatalf("OpenInspect on the main file alone: %v", err)
		}

		t.Cleanup(func() { _ = mainInspect.Close() })

		if err := mainInspect.View(t.Context(), func(q Querier) error {
			got, err := ReadQueries(q).ReadNodeProvider(t.Context(), "wal-row")
			if !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("the main file alone answers %q (err %v) for the row, so the fixture proves nothing about the WAL", got, err)
			}

			return nil
		}); err != nil {
			t.Fatal(err)
		}

		inspect, err := OpenInspect(t.Context(), crashed)
		if err != nil {
			t.Fatalf("OpenInspect on a crashed ledger: %v", err)
		}

		t.Cleanup(func() { _ = inspect.Close() })

		if got := providerVia(t, inspect, "wal-row"); got != "docker" {
			t.Errorf("the WAL-resident row reads %q", got)
		}
	})

	t.Run("a later commit under a live control plane is visible", func(t *testing.T) {
		dir := t.TempDir()

		plane, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = plane.Close() })

		inspect, err := OpenInspect(t.Context(), dir)
		if err != nil {
			t.Fatalf("OpenInspect beside a live control plane: %v", err)
		}

		t.Cleanup(func() { _ = inspect.Close() })

		seedNode(t, plane, "later", "tart")

		if got := providerVia(t, inspect, "later"); got != "tart" {
			t.Errorf("a commit made after the inspection opened reads %q", got)
		}
	})

	t.Run("a stopped ledger gains sidecars the inspector owns", func(t *testing.T) {
		dir := ledgerWithRow(t)

		for _, name := range []string{"billet.db-wal", "billet.db-shm"} {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				t.Fatalf("the stopped ledger already has %s", name)
			}
		}

		db, err := OpenInspect(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}

		_ = providerVia(t, db, "epyc-1")

		// WHILE THE HANDLE IS OPEN the sidecars exist (the measured behaviour the
		// re-execution answers) and are owned by whoever opened; a lookup that
		// fails for any reason is a failed fixture, never a skipped one.
		for _, name := range []string{"billet.db-wal", "billet.db-shm"} {
			info, err := os.Lstat(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("%s while the inspection is open: %v", name, err)
			}

			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				t.Fatalf("%s carries no owner", name)
			}

			if int(st.Uid) != os.Geteuid() {
				t.Errorf("%s is owned by uid %d, not the inspector's %d", name, st.Uid, os.Geteuid())
			}
		}

		_ = db.Close()

		// After the close the driver may have removed them; what is permitted is
		// absence or a file this account owns, nothing else.
		for _, name := range []string{"billet.db-wal", "billet.db-shm"} {
			info, err := os.Lstat(filepath.Join(dir, name))

			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil:
				t.Fatalf("%s after the close: %v", name, err)
			}

			if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
				t.Errorf("%s left behind is not the inspector's", name)
			}
		}
	})
}

// The helper the process-level fixtures run: a control plane holding the
// directory for as long as its stdin is open.
const (
	inspectHelperEnv   = "BILLET_TEST_INSPECT_HELPER_DIR"
	inspectHelperReady = "HELPER-IS-THE-CONTROL-PLANE"
)

func TestInspectHelperProcess(t *testing.T) {
	dir := os.Getenv(inspectHelperEnv)
	if dir == "" {
		t.Skip("not the helper process")
	}

	// THE PARENT'S OWN TEMPORARY DIRECTORY, and nothing else: an environment
	// value is an input the parent test wrote, so it is held to its shape here
	// rather than handed to the open as it came.
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) || !strings.HasPrefix(dir, filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatalf("the helper was pointed outside the temporary directory: %s", dir)
	}

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("helper could not open the ledger: %v", err)
	}

	defer func() { _ = db.Close() }()

	if _, err := os.Stdout.WriteString(inspectHelperReady + "\n"); err != nil {
		t.Fatalf("helper could not announce: %v", err)
	}

	if _, err := os.Stdin.Read(make([]byte, 1)); err != nil {
		return
	}
}

// startControlPlaneProcess runs a real second process holding the directory as
// the control plane, and returns a function that ends it.
func startControlPlaneProcess(t *testing.T, dir string) func() {
	t.Helper()

	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestInspectHelperProcess", "-test.v")
	helper.Env = append(os.Environ(), inspectHelperEnv+"="+dir)

	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := helper.Start(); err != nil {
		t.Fatalf("start the helper: %v", err)
	}

	ended := false
	end := func() {
		if ended {
			return
		}

		ended = true

		if err := stdin.Close(); err != nil {
			t.Logf("closing the helper's stdin: %v", err)
		}

		if err := helper.Process.Kill(); err != nil {
			t.Logf("killing the helper: %v", err)
		}

		// The helper ends by the kill, and its status is not the test's.
		if err := helper.Wait(); err != nil {
			t.Logf("reaping the helper: %v", err)
		}
	}

	t.Cleanup(end)

	ready := make(chan bool, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), inspectHelperReady) {
				ready <- true

				return
			}
		}

		ready <- false
	}()

	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the helper exited without becoming the control plane")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the helper never announced itself")
	}

	return end
}

// THE DIRECTORY LOCK IS NEITHER TAKEN NOR RELEASED, MEASURED AGAINST REAL
// PROCESSES: with a control plane in another process, an inspection opens and
// a second control plane is still refused while the inspection is open and
// after it closes; with nobody holding the ledger, an inspection stays open
// while a control plane acquires the directory.
func TestAnInspectionHoldsNoDirectoryLock(t *testing.T) {
	t.Run("beside a live control plane", func(t *testing.T) {
		dir := ledgerWithRow(t)
		end := startControlPlaneProcess(t, dir)

		inspect, err := OpenInspect(t.Context(), dir)
		if err != nil {
			t.Fatalf("OpenInspect beside a control plane: %v", err)
		}

		if _, err := Open(t.Context(), dir); !errors.Is(err, ErrLocked) {
			t.Errorf("a second control plane beside the inspection: err = %v, want ErrLocked", err)
		}

		if err := inspect.Close(); err != nil {
			t.Fatal(err)
		}

		if _, err := Open(t.Context(), dir); !errors.Is(err, ErrLocked) {
			t.Errorf("a second control plane after the inspection closed: err = %v, want ErrLocked; "+
				"the inspection released a lock it never held", err)
		}

		end()

		plane, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatalf("a control plane after the helper ended: %v", err)
		}

		_ = plane.Close()
	})

	t.Run("with nobody holding the ledger", func(t *testing.T) {
		dir := ledgerWithRow(t)

		inspect, err := OpenInspect(t.Context(), dir)
		if err != nil {
			t.Fatalf("OpenInspect: %v", err)
		}

		t.Cleanup(func() { _ = inspect.Close() })

		plane, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatalf("a control plane could not acquire the directory while an inspection was open: %v", err)
		}

		_ = plane.Close()
	})
}
