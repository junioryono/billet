package state

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenAppliesMigrations(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	// Assert the recorded versions are exactly the defined ones. Counting rows
	// would pass even if the wrong migrations had been applied.
	rows, err := db.Reader().QueryContext(ctx,
		`SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var v int
		var name, checksum string
		if err := rows.Scan(&v, &name, &checksum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i >= len(migrations) {
			t.Fatalf("recorded more migrations than defined")
		}
		want := migrations[i]
		if v != want.Version || name != want.Name || checksum != want.checksum() {
			t.Errorf("row %d = (%d, %s, %s), want (%d, %s, %s)",
				i, v, name, checksum, want.Version, want.Name, want.checksum())
		}
		i++
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}

	if i != len(migrations) {
		t.Errorf("recorded %d migrations, want %d", i, len(migrations))
	}

	for _, table := range []string{
		"nodes", "leases", "cache_generations", "job_history", "pending_completions",
	} {
		var name string
		if err := db.Reader().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

// The migration list is assembled from a slice literal PLUS appends in init, so
// nothing about the source layout guarantees the versions are sane. A duplicate
// version means one migration's SQL silently never runs while the other's
// checksum is recorded under it; a descending one means the apply loop and the
// ORDER BY version read disagree about which migration is which.
//
// Both failures surface later as a checksum mismatch on an unrelated reopen,
// which is a miserable thing to debug from that symptom.
func TestMigrationVersionsAreUniqueAndAscending(t *testing.T) {
	seen := make(map[int]string, len(migrations))

	for i, m := range migrations {
		if m.Version <= 0 {
			t.Errorf("migrations[%d] (%s) has non-positive version %d", i, m.Name, m.Version)
		}
		if prev, dup := seen[m.Version]; dup {
			t.Errorf("migrations[%d] (%s) reuses version %d, already held by %s", i, m.Name, m.Version, prev)
		}
		seen[m.Version] = m.Name

		if i > 0 && m.Version <= migrations[i-1].Version {
			t.Errorf("migrations[%d] (%s) has version %d, not greater than the preceding %d",
				i, m.Name, m.Version, migrations[i-1].Version)
		}
		if len(m.Stmts) == 0 {
			t.Errorf("migrations[%d] (%s) has no statements", i, m.Name)
		}
	}
}

// Migration 6 backfills guest_os for rows written before the column existed.
// The default has to be a real guest OS rather than empty: Bind compares it
// against a host's allowlist, and an empty value would match nothing and strand
// every pre-existing lease. 'linux' is right because a macOS tier has always
// been required to pin a node.
func TestLeaseGuestOSDefaultsToLinux(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	now := time.Now().UTC().Format(time.RFC3339Nano)

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO leases (id, tier, phase, vcpu, memory, epoch, created_at, heartbeat_at, expires_at)
			 VALUES ('l1', 't', 'capacity', 1, 1, 0, ?, ?, ?)`, now, now, now)

		return err
	}); err != nil {
		t.Fatalf("insert lease without guest_os: %v", err)
	}

	var guestOS string
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT guest_os FROM leases WHERE id = 'l1'`).Scan(&guestOS); err != nil {
		t.Fatalf("read guest_os: %v", err)
	}

	if guestOS != "linux" {
		t.Errorf("guest_os = %q for a row that did not set it, want %q", guestOS, "linux")
	}
}

// openAt opens a database migrated only as far as a given version, so an upgrade
// can be tested as an upgrade.
//
// Swapping the package-level list is what makes this faithful: the real migrate
// runner does the work, in one transaction, with its real bookkeeping. Replaying
// a migration's SQL by hand against an already-current database tests the string
// rather than the runner, and would stay green if the runner mishandled the
// upgrade entirely.
func openAt(t *testing.T, dir string, version int) *DB {
	t.Helper()

	full := migrations

	t.Cleanup(func() { migrations = full })

	var truncated []migration

	for _, m := range full {
		if m.Version <= version {
			truncated = append(truncated, m)
		}
	}

	migrations = truncated

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open at version %d: %v", version, err)
	}

	migrations = full

	return db
}

// Migration 6 backfilled EVERY pre-existing lease to 'linux', macOS ones
// included. That direction is dangerous rather than merely wrong: an unbound
// macOS lease relabelled Linux would be PERMITTED onto a Linux-only host, even
// though its durable macos_slot proves what it is. Migration 7 corrects it from
// macos_slot.
//
// Driven as a real v6 -> v7 upgrade rather than by replaying the UPDATE.
func TestMacOSLeasesAreBackfilledFromTheirSlot(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// A version-6 database holding a macOS lease that migration 6 mislabelled.
	old := openAt(t, dir, 6)

	if err := old.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO leases (id, tier, phase, vcpu, memory, epoch, macos_slot, guest_os,
			                     created_at, heartbeat_at, expires_at)
			 VALUES ('mac', 't', 'capacity', 1, 1, 0, 1, 'linux', ?, ?, ?)`, now, now, now)

		return err
	}); err != nil {
		t.Fatalf("insert mislabelled macOS lease: %v", err)
	}

	if err := old.Close(); err != nil {
		t.Fatalf("close v6 database: %v", err)
	}

	// Reopening with the full list runs migration 7 for real.
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("upgrade to current: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	var guestOS string
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT guest_os FROM leases WHERE id = 'mac'`).Scan(&guestOS); err != nil {
		t.Fatalf("read guest_os: %v", err)
	}

	if guestOS != "macos" {
		t.Errorf("guest_os = %q for a lease holding a macOS slot, want %q", guestOS, "macos")
	}
}

// The upgrade helper is only meaningful if it really stops short. If openAt
// silently applied everything, the test above would be checking a fresh database
// and would pass no matter what migration 7 did.
func TestOpenAtStopsAtTheRequestedVersion(t *testing.T) {
	db := openAt(t, t.TempDir(), 6)

	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read applied versions: %v", err)
	}

	if version != 6 {
		t.Errorf("openAt(6) migrated to %d, want 6", version)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()

	var n int
	if err := second.Reader().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != len(migrations) {
		t.Errorf("reopening applied migrations twice: got %d, want %d", n, len(migrations))
	}
}

// An edited migration means two deployments believe they share a schema they do
// not. The checksum is what turns that into a startup error instead of drift.
func TestEditedMigrationIsRejected(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Corrupt the recorded checksum to simulate the SQL having been edited.
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = ?`,
			migrations[0].Version)
		return err
	}); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Open(t.Context(), dir)
	if err == nil {
		t.Fatal("Open accepted a database whose migration SQL had changed")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("error should explain migrations are append-only, got: %v", err)
	}
}

// Running an older control plane against a schema written by a newer one
// corrupts state slowly and confusingly. Refuse instead.
func TestUnknownFutureMigrationIsRejected(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO schema_migrations (version, name, checksum, applied_at)
			 VALUES (9999, 'from-the-future', 'x', ?)`,
			time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Open(t.Context(), dir)
	if err == nil {
		t.Fatal("Open accepted a database written by a newer billet")
	}
	if !strings.Contains(err.Error(), "newer version") {
		t.Errorf("error should say the database is newer, got: %v", err)
	}
}

// The pragmas are verified on the WRITER connection specifically. Reading them
// back from a reader proves little: journal_mode is a persistent property of the
// file, so a reader reports "wal" regardless of whether the writer's DSN
// configured anything.
func TestWriterDurabilityPragmas(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	var journal string
	if err := db.w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("writer journal_mode = %q, want wal", journal)
	}

	// synchronous is per-connection, so this genuinely proves the writer DSN was
	// applied rather than reflecting persistent file state.
	var syncMode int
	if err := db.w.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&syncMode); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if syncMode != 2 { // 2 == FULL
		t.Errorf("writer synchronous = %d, want 2 (FULL)", syncMode)
	}

	// verifyWriterPragmas is what runs at startup; exercise it directly too.
	if err := db.verifyWriterPragmas(ctx); err != nil {
		t.Errorf("verifyWriterPragmas: %v", err)
	}
}

// The reader pool must not be usable for writes even by a caller who tries.
func TestReaderRejectsWrites(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// QueryRow rather than Query: there is no cursor to leak on the failure path,
	// and the write is refused before any row could be produced either way.
	var scanned string

	err := db.Reader().QueryRowContext(ctx,
		`INSERT INTO nodes (name, provider, last_seen_at) VALUES (?, ?, ?) RETURNING name`,
		"sneaky", "firecracker", now).Scan(&scanned)
	if err == nil {
		t.Fatal("reader pool accepted a write; query_only is not in effect")
	}

	if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("expected a read-only error, got: %v", err)
	}

	// Prove it by absence too: the row must not exist.
	var n int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE name = ?`, "sneaky").Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}

	if n != 0 {
		t.Errorf("reader pool wrote %d rows", n)
	}
}

// Two control planes against one ledger produce double-admitted jobs, and the
// failure is quiet. The second process must not start.
func TestSecondOpenIsRefused(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer first.Close()

	second, err := Open(t.Context(), dir)
	if err == nil {
		second.Close()
		t.Fatal("a second Open on the same state directory succeeded")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("error should be ErrLocked, got: %v", err)
	}
}

// Releasing the lock must make the directory usable again, or a clean restart
// would need manual cleanup.
func TestLockReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open after Close was refused: %v", err)
	}
	_ = second.Close()
}

func TestTxRollsBackOnError(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	sentinel := errors.New("boom")

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES (?, ?, ?)`,
			"ghost", "firecracker", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx error = %v, want sentinel", err)
	}

	var n int
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if n != 0 {
		t.Errorf("rolled-back insert persisted: %d rows", n)
	}
}

// Concurrent writers must serialize rather than lose updates. A capacity ledger
// that intermittently drops a reservation double-admits jobs onto a machine that
// cannot hold them.
func TestConcurrentWritesSerialize(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (name, provider, last_seen_at, total_vcpu) VALUES (?, ?, ?, 0)`,
			"epyc-1", "firecracker", time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	const writers, each = 8, 25
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				err := db.Tx(ctx, func(tx *sql.Tx) error {
					// Read-modify-write inside one transaction: exactly the shape an
					// allocation decision takes.
					var cur int
					if err := tx.QueryRowContext(ctx,
						`SELECT total_vcpu FROM nodes WHERE name = ?`, "epyc-1").Scan(&cur); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx,
						`UPDATE nodes SET total_vcpu = ? WHERE name = ?`, cur+1, "epyc-1")
					return err
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent write failed: %v", err)
	}

	var total int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT total_vcpu FROM nodes WHERE name = ?`, "epyc-1").Scan(&total); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if want := writers * each; total != want {
		t.Errorf("total_vcpu = %d, want %d — increments were lost, so writes did not serialize", total, want)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO leases (id, tier, node, phase, vcpu, memory, created_at, heartbeat_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"lease-1", "billet-4vcpu", "no-such-node", "capacity", 4, 1<<34, now, now, now)
		return err
	})
	if err == nil {
		t.Fatal("insert referencing a nonexistent node succeeded; foreign_keys is not on")
	}
}

// The schema must protect its own invariants: a typo'd phase would otherwise sit
// in the partial "open leases" index forever, invisible to every reaper.
func TestLeasePhaseIsConstrained(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO leases (id, tier, phase, vcpu, memory, created_at, heartbeat_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"lease-typo", "billet-4vcpu", "Done", 4, 1<<34, now, now, now)
		return err
	})
	if err == nil {
		t.Fatal("insert with phase 'Done' succeeded; the CHECK constraint is missing")
	}
}

func TestLeaseCapacityMustBePositive(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO leases (id, tier, phase, vcpu, memory, created_at, heartbeat_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"lease-zero", "billet-4vcpu", "capacity", 0, 1<<34, now, now, now)
		return err
	})
	if err == nil {
		t.Fatal("insert with vcpu=0 succeeded; the CHECK constraint is missing")
	}
}

// A lease written before the provider split is BACKFILLED correctly.
//
// THE UPGRADE PATH. Migration 9 splits "which backends may this lease run on"
// from "which one is it on"; a row that predates it carries a single provider,
// and the placement check fails CLOSED on a lease whose acceptable backends it
// cannot read. Get the migration wrong and every in-flight job at upgrade time
// becomes unplaceable — visible only as work that stops moving.
//
// Simulated by removing the new columns from a migrated database and replaying,
// which is the closest thing to an old database this repository can construct:
// the schema is defined in code, so there is no historical file to open.
//
// SCOPED TO THE COLUMNS, deliberately, and named for that. This package cannot
// import internal/alloc — alloc depends on it — so proving the migrated rows
// actually PLACE belongs in alloc's own suite, where TestAnUpgradedLeaseStillBinds
// does it by binding one.
//
// A running-phase row whose node was deleted is covered too: the foreign key is
// ON DELETE SET NULL, so it migrates with no chosen backend, and every path that
// could double-place it refuses.
func TestMigrationBackfillsASingleProvider(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Two rows: one never placed, one already bound.
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		// The bound lease references a node, and leases.node is a foreign key.
		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at)
			 VALUES ('epyc-1', 'firecracker', '2026-01-01T00:00:00Z')`); err != nil {
			return fmt.Errorf("register the node: %w", err)
		}

		for _, row := range []struct {
			id    string
			node  any
			phase string
		}{
			{"unbound-lease", nil, "capacity"},
			{"bound-lease", "epyc-1", "capacity"},
			// The case the foreign key produces: a lease that WAS running when its
			// node was deleted.
			{"orphaned-running-lease", nil, "launching"},
		} {
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO leases
				   (id, tier, node, macos_slot, guest_os, provider, providers, chosen_provider,
				    phase, vcpu, memory, epoch, created_at, heartbeat_at, expires_at)
				 VALUES (?, 'billet-8vcpu', ?, 0, 'linux', 'firecracker', '', '',
				         ?, 8, 34359738368, 0, '2026-01-01T00:00:00Z',
				         '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z')`,
				row.id, row.node, row.phase); err != nil {
				return err
			}
		}

		// Now make the database look pre-9.
		for _, stmt := range []string{
			`ALTER TABLE leases DROP COLUMN providers`,
			`ALTER TABLE leases DROP COLUMN chosen_provider`,
			`DELETE FROM schema_migrations WHERE version = 9`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("build a pre-migration database: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening replays migration 9 over the old rows.
	db, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("reopen and migrate: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	for _, want := range []struct {
		id        string
		providers string
		chosen    string
	}{
		// A running lease whose node was deleted: ON DELETE SET NULL blanked the
		// column, so it looks unbound and gets no chosen backend. Conservative and
		// correct — Bind refuses it because its phase already requires placement,
		// and Advance refuses because it names no node, so it cannot be adopted
		// onto a second host. Release and Reap remain available.
		{"orphaned-running-lease", "firecracker", ""},
		// An unbound row has not chosen anything. Writing a choice for it would
		// make the column mean "was reserved for" again, which is the exact
		// conflation the migration exists to undo.
		{"unbound-lease", "firecracker", ""},
		// A bound row is running on the backend it was reserved for; that is the
		// honest reading, and without it the row is indistinguishable from one
		// that never placed.
		{"bound-lease", "firecracker", "firecracker"},
	} {
		var providers, chosen string

		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT providers, chosen_provider FROM leases WHERE id = ?`, want.id).
				Scan(&providers, &chosen)
		}); err != nil {
			t.Fatalf("read %s: %v", want.id, err)
		}

		if providers != want.providers {
			t.Errorf("%s: providers = %q, want %q — this lease can no longer be placed",
				want.id, providers, want.providers)
		}

		if chosen != want.chosen {
			t.Errorf("%s: chosen_provider = %q, want %q", want.id, chosen, want.chosen)
		}
	}
}

// EVERY TABLE IS STRICT, and the ones holding credentials most of all.
//
// SQLite's default typing accepts a string where an integer belongs and stores
// it as one, so a bug that writes the wrong type is found by a later reader
// rather than by the write that caused it. Three tables added during the trust
// work were declared without it.
func TestEveryTableIsStrict(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer db.Close()

	rows, err := db.Reader().QueryContext(t.Context(),
		`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}

	defer rows.Close()

	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan: %v", err)
		}

		// schema_migrations is the bootstrap table, created before the migration
		// machinery it records exists.
		if name == "schema_migrations" {
			continue
		}

		if !strings.Contains(strings.ToUpper(ddl), "STRICT") {
			t.Errorf("table %s is not STRICT, so a value of the wrong type is stored rather "+
				"than refused", name)
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// A REBUILDING MIGRATION COPIES EVERY COLUMN IT DECLARES.
//
// Migrations 16, 17 and 20 rebuild tables, because CHECK constraints and STRICT
// are properties of the declaration and SQLite cannot alter either. A rebuild
// is the one migration shape that silently loses things: a column left out of
// the copy list keeps its DEFAULT in the new table and nothing complains.
//
// IT HAS TO BE CHECKED STRUCTURALLY, and the first version of this test is why.
// It opened a database, inserted rows, and read them back — but Open runs every
// migration before returning, so the rebuild had already happened against empty
// tables and the rows were written to the table that came out of it. Dropping a
// column from the copy list did not fail it. The migration only ever runs on
// data that predates it, which a test starting from Open can never produce.
//
// So the statements themselves are read: every column the new table declares
// must appear in both halves of the INSERT ... SELECT that fills it.
func TestRebuildingMigrationsCopyEveryColumn(t *testing.T) {
	for _, m := range []migration{
		quarantineMigration, strictTrustTablesMigration, custodyVisibilityMigration,
	} {
		declared := map[string][]string{}
		inserted := map[string][]string{}
		selected := map[string][]string{}

		for _, stmt := range m.Stmts {
			switch {
			case strings.HasPrefix(stmt, "CREATE TABLE "):
				table, cols := parseCreate(t, stmt)
				declared[table] = cols
			case strings.HasPrefix(stmt, "INSERT INTO "):
				table, into, from := parseInsertSelect(t, stmt)
				inserted[table], selected[table] = into, from
			}
		}

		for table, cols := range declared {
			into, from := inserted[table], selected[table]

			// COVERAGE is a question about sets: every declared column has to be
			// filled by something, in whatever order the statement lists them.
			declaredSorted := slices.Sorted(slices.Values(cols))
			intoSorted := slices.Sorted(slices.Values(into))

			if !slices.Equal(declaredSorted, intoSorted) {
				t.Errorf("migration %d rebuilds %s with columns %v but only fills %v; the "+
					"difference keeps its default and nothing says so",
					m.Version, table, declaredSorted, intoSorted)
			}

			// CORRESPONDENCE is a question about ORDER, and must not be sorted
			// away. INSERT INTO t (a, b) SELECT b, a is valid SQL that swaps two
			// values, and SQLite accepts it silently whenever the types agree —
			// which for a table of TEXT columns is always.
			if !slices.Equal(into, from) {
				t.Errorf("migration %d writes %v into %s but reads %v in that order; the two "+
					"lists are positional, so this shuffles values between columns",
					m.Version, into, table, from)
			}
		}

		if len(declared) == 0 {
			t.Errorf("migration %d declares no rebuilt table, so this test checked nothing",
				m.Version)
		}
	}
}

// indexNamesInMigrations is every index the migration set creates, deduplicated.
//
// A rebuild recreates the ones it names; anything created earlier and not named
// again is gone. Reading them out of the statements is what makes "every index
// survives" a property rather than a list somebody has to remember to update.
func indexNamesInMigrations() []string {
	seen := map[string]bool{}

	var names []string

	for _, m := range migrations {
		for _, stmt := range m.Stmts {
			const prefix = "CREATE INDEX "
			if !strings.HasPrefix(stmt, prefix) {
				continue
			}

			name := strings.Fields(strings.TrimPrefix(stmt, prefix))[0]
			if !seen[name] {
				seen[name] = true

				names = append(names, name)
			}
		}
	}

	return names
}

// parseCreate reads a table name and its column names out of a CREATE TABLE.
func parseCreate(t *testing.T, stmt string) (string, []string) {
	t.Helper()

	name := strings.TrimSuffix(strings.Fields(strings.TrimPrefix(stmt, "CREATE TABLE "))[0], "_new")
	body := stmt[strings.Index(stmt, "(")+1 : strings.LastIndex(stmt, ")")]

	var cols []string

	depth := 0
	current := strings.Builder{}

	flush := func() {
		line := strings.TrimSpace(current.String())
		current.Reset()

		if line == "" {
			return
		}

		first := strings.Fields(line)[0]
		// Table-level constraints are not columns.
		if strings.EqualFold(first, "PRIMARY") || strings.EqualFold(first, "CHECK") ||
			strings.EqualFold(first, "FOREIGN") || strings.EqualFold(first, "UNIQUE") {
			return
		}

		cols = append(cols, first)
	}

	for _, r := range body {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			flush()

			continue
		}

		current.WriteRune(r)
	}

	flush()

	return name, cols
}

// parseInsertSelect reads the two column lists out of an INSERT ... SELECT.
func parseInsertSelect(t *testing.T, stmt string) (table string, into, from []string) {
	t.Helper()

	table = strings.TrimSuffix(strings.Fields(strings.TrimPrefix(stmt, "INSERT INTO "))[0], "_new")

	openAt := strings.Index(stmt, "(")
	closeAt := strings.Index(stmt, ")")
	selectAt := strings.Index(stmt, "SELECT")
	fromAt := strings.LastIndex(stmt, "FROM")

	if openAt < 0 || closeAt < 0 || selectAt < 0 || fromAt < 0 {
		t.Fatalf("cannot read the column lists out of: %s", stmt)
	}

	split := func(s string) []string {
		var out []string

		for _, f := range strings.Split(s, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}

		return out
	}

	return table, split(stmt[openAt+1 : closeAt]), split(stmt[selectAt+len("SELECT") : fromAt])
}

// AND THE SHAPE THAT COMES OUT IS THE ONE THE CODE EXPECTS.
//
// The indexes a rebuild drops with the table have to come back, the columns have
// to round-trip their values, and the widened CHECK has to accept the phase it
// was widened for. Unlike the copy lists above, all of that is observable from a
// database Open has already migrated.
func TestRebuildingMigrationsKeepRowsIndexesAndKeys(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer db.Close()

	// Rows written the way the previous version would have.
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at)
			 VALUES ('epyc-1', 'docker', '2026-01-01T00:00:00Z')`); err != nil {
			return err
		}

		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO leases
			   (id, tier, node, phase, vcpu, memory, epoch, created_at, heartbeat_at,
			    expires_at, target_node, macos_slot, guest_os, provider, providers,
			    chosen_provider, run_id, request_id)
			 VALUES ('l1','small','epyc-1','busy',2,8589934592,3,'t','t','t','epyc-1',
			         1,'macos','tart','tart,ec2','tart',77,88)`); err != nil {
			return err
		}

		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO join_tokens (token_sha256, note, uses_remaining, created_at, expires_at)
			 VALUES ('abc','a note',2,'t','t')`)

		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// EVERY COLUMN SURVIVES. A copy list that forgets one loses it silently.
	var (
		tier, node, phase, target, guestOS, provider, providers, chosen string
		vcpu, epoch, macos, runID, requestID                            int64
		memory                                                          int64
	)

	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT tier, node, phase, target_node, guest_os, provider, providers, chosen_provider,
		        vcpu, memory, epoch, macos_slot, run_id, request_id
		   FROM leases WHERE id = 'l1'`).
		Scan(&tier, &node, &phase, &target, &guestOS, &provider, &providers, &chosen,
			&vcpu, &memory, &epoch, &macos, &runID, &requestID); err != nil {
		t.Fatalf("read the lease back: %v", err)
	}

	for _, tc := range []struct{ name, got, want string }{
		{"tier", tier, "small"}, {"node", node, "epyc-1"}, {"phase", phase, "busy"},
		{"target_node", target, "epyc-1"}, {"guest_os", guestOS, "macos"},
		{"provider", provider, "tart"}, {"providers", providers, "tart,ec2"},
		{"chosen_provider", chosen, "tart"},
	} {
		if tc.got != tc.want {
			t.Errorf("leases.%s is %q after the rebuild, was %q", tc.name, tc.got, tc.want)
		}
	}

	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"vcpu", vcpu, 2}, {"memory", memory, 8589934592}, {"epoch", epoch, 3},
		{"macos_slot", macos, 1}, {"run_id", runID, 77}, {"request_id", requestID, 88},
	} {
		if tc.got != tc.want {
			t.Errorf("leases.%s is %d after the rebuild, was %d", tc.name, tc.got, tc.want)
		}
	}

	var note string

	var uses int

	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT note, uses_remaining FROM join_tokens WHERE token_sha256 = 'abc'`).
		Scan(&note, &uses); err != nil {
		t.Fatalf("read the join token back: %v", err)
	}

	if note != "a note" || uses != 2 {
		t.Errorf("join_tokens holds %q/%d after the rebuild, was \"a note\"/2", note, uses)
	}

	// EVERY INDEX ANY MIGRATION EVER CREATED IS STILL THERE.
	//
	// Derived rather than listed, because listing them is the bug: the first
	// version of this test named the three indexes I happened to remember and
	// missed leases_expiry_idx, which migration 5 added and the rebuild dropped —
	// the one that keeps the reaper from scanning the whole lease history on the
	// single writer connection.
	for _, want := range indexNamesInMigrations() {
		var name string

		err := db.Reader().QueryRowContext(t.Context(),
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, want).Scan(&name)
		if err != nil {
			t.Errorf("index %s did not survive the rebuild: %v", want, err)
		}
	}

	// AND EVERY PHASE A REBUILD ADDED IS ACCEPTED.
	for _, phase := range []string{"quarantine", "custody", "teardown"} {
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(),
				`UPDATE leases SET phase = ? WHERE id = 'l1'`, phase)

			return err
		}); err != nil {
			t.Errorf("the rebuilt table refuses the %s phase: %v", phase, err)
		}
	}
}

// AN OLDER DATABASE UPGRADES, WITH ITS ROWS INTACT.
//
// Everything else about migrations is checked on a database this binary just
// created, which is the one case that cannot go wrong: an empty table survives
// any rebuild. The interesting case is a database with DATA written by an
// earlier billet, and CI never produces one — it starts from an empty directory
// every time.
//
// Staged by removing the newest migrations' bookkeeping and reopening, which is
// exactly the state an upgrade meets: tables in their old shape, rows in them,
// and the runner about to rebuild the ones whose declaration changed.
func TestADatabaseWrittenByAnEarlierBilletUpgrades(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Rows an earlier billet would have left behind, in every table the newest
	// migrations rebuild.
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at)
			 VALUES ('epyc-1', 'docker', '2026-01-01T00:00:00Z')`); err != nil {
			return err
		}

		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO leases
			   (id, tier, node, phase, vcpu, memory, epoch, created_at, heartbeat_at,
			    expires_at, target_node, macos_slot, guest_os, provider, providers,
			    chosen_provider, run_id, request_id)
			 VALUES ('l1','small','epyc-1','busy',2,8589934592,3,'t','t','t','epyc-1',
			         1,'macos','tart','tart,ec2','tart',77,88)`); err != nil {
			return err
		}

		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO join_tokens (token_sha256, note, uses_remaining, created_at, expires_at)
			 VALUES ('abc','a note',2,'t','t')`); err != nil {
			return err
		}

		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO node_enrollments (name, fingerprint, csr_pem, state, requested_at)
			 VALUES ('epyc-1','SHA256:x','csr','approved','t')`)

		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// WOUND BACK TO WHAT v14 ACTUALLY LOOKED LIKE: the bookkeeping forgotten AND
	// the tables the newest migrations introduce dropped, because a database from
	// before them does not have those tables at all.
	//
	// What this does not reproduce is the OLD shape of the tables 16 and 17
	// rebuild — they are already rebuilt here. The valuable half survives: the
	// copy lists run again, against rows, which is the part that silently loses
	// data. The declarations themselves are checked structurally elsewhere.
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP TABLE issued_certs`,
			`DROP TABLE node_revocations`,
			`DROP TABLE pending_completions`,
			`ALTER TABLE nodes DROP COLUMN ec2_shapes`,
			`ALTER TABLE leases DROP COLUMN force_release`,
			`ALTER TABLE leases DROP COLUMN held_at`,
			`ALTER TABLE leases DROP COLUMN requested_vcpu`,
			`ALTER TABLE leases DROP COLUMN requested_memory`,
			`ALTER TABLE leases DROP COLUMN instance_type`,
			`ALTER TABLE leases DROP COLUMN failure_reason`,
			`ALTER TABLE job_history DROP COLUMN failure_reason`,
			`DELETE FROM schema_migrations WHERE version >= 15`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	upgraded, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("an upgrade of a database with rows in it failed: %v", err)
	}

	defer upgraded.Close()

	// EVERY ROW SURVIVED, in every rebuilt table.
	for _, tc := range []struct{ what, query, want string }{
		{"the lease's provider list", `SELECT providers FROM leases WHERE id = 'l1'`, "tart,ec2"},
		{"the join token's note", `SELECT note FROM join_tokens WHERE token_sha256 = 'abc'`, "a note"},
		{"the enrollment's fingerprint",
			`SELECT fingerprint FROM node_enrollments WHERE name = 'epyc-1'`, "SHA256:x"},
	} {
		var got string
		if err := upgraded.Reader().QueryRowContext(t.Context(), tc.query).Scan(&got); err != nil {
			t.Errorf("%s did not survive the upgrade: %v", tc.what, err)

			continue
		}

		if got != tc.want {
			t.Errorf("%s is %q after the upgrade, was %q", tc.what, got, tc.want)
		}
	}

	// And the schema the upgrade was for actually arrived.
	if err := upgraded.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE leases SET phase = 'quarantine' WHERE id = 'l1'`)

		return err
	}); err != nil {
		t.Errorf("the upgraded database refuses the quarantine phase: %v", err)
	}
}

func TestAPendingCompletionWrittenAtVersion22SurvivesVersion23(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`ALTER TABLE pending_completions DROP COLUMN release_only`,
			`ALTER TABLE pending_completions DROP COLUMN outcome`,
			`ALTER TABLE pending_completions DROP COLUMN lease_epoch`,
			`ALTER TABLE pending_completions DROP COLUMN lease_id`,
			`DELETE FROM schema_migrations WHERE version = 23`,
			`INSERT INTO pending_completions (tier, request_id, run_id, result)
			 VALUES ('linux', 91, 101, 'Succeeded')`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version 22: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version 22 database: %v", err)
	}

	upgraded, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open version 22 database with version 23: %v", err)
	}
	defer upgraded.Close()
	got, err := upgraded.PendingCompletions(t.Context(), "linux")
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	want := PendingCompletion{Tier: "linux", RequestID: 91, RunID: 101, Result: "Succeeded"}
	if !slices.Equal(got, []PendingCompletion{want}) {
		t.Fatalf("upgraded pending completions = %+v, want %+v", got, want)
	}
}

func TestAPendingCompletionWrittenAtVersion23SurvivesVersion24(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`ALTER TABLE pending_completions DROP COLUMN retired`,
			`ALTER TABLE pending_completions DROP COLUMN message_id`,
			`ALTER TABLE pending_completions DROP COLUMN lease_node`,
			`DELETE FROM schema_migrations WHERE version = 24`,
			`INSERT INTO leases
			 (id, tier, phase, vcpu, memory, created_at, heartbeat_at, expires_at, target_node)
			 VALUES ('lease-92', 'linux', 'busy', 4, 4294967296,
			         '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z',
			         '2026-01-01T01:00:00Z', 'holder')`,
			`INSERT INTO pending_completions
			 (tier, request_id, run_id, result, lease_id, lease_epoch, outcome, release_only)
			 VALUES ('linux', 92, 102, 'Succeeded', 'lease-92', 4, 'done', 1)`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version 23: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version 23 database: %v", err)
	}

	upgraded, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open version 23 database with version 24: %v", err)
	}
	defer upgraded.Close()
	got, err := upgraded.PendingCompletions(t.Context(), "linux")
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	want := PendingCompletion{Tier: "linux", RequestID: 92, RunID: 102, Result: "Succeeded",
		LeaseID: "lease-92", LeaseEpoch: 4, LeaseNode: "holder", Outcome: "done", ReleaseOnly: true}
	if !slices.Equal(got, []PendingCompletion{want}) {
		t.Fatalf("upgraded pending completions = %+v, want %+v", got, want)
	}
}

func TestAPendingCompletionWrittenAtVersion24SurvivesVersion25(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`ALTER TABLE pending_completions DROP COLUMN acknowledged`,
			`DELETE FROM schema_migrations WHERE version = 25`,
			`INSERT INTO pending_completions
			 (tier, request_id, run_id, result, message_id, retired)
			 VALUES ('linux', 93, 103, 'Succeeded', 7, 1)`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version 24: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version 24 database: %v", err)
	}

	upgraded, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open version 24 database with version 25: %v", err)
	}
	defer upgraded.Close()
	got, err := upgraded.PendingCompletions(t.Context(), "linux")
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	want := PendingCompletion{Tier: "linux", RequestID: 93, RunID: 103, Result: "Succeeded",
		MessageID: 7, Retired: true}
	if !slices.Equal(got, []PendingCompletion{want}) {
		t.Fatalf("upgraded pending completions = %+v, want %+v", got, want)
	}
}
