package state

import (
	"database/sql"
	"errors"
	"fmt"
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

	for _, table := range []string{"nodes", "leases", "cache_generations", "job_history"} {
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

// A lease written before the provider split still places after the upgrade.
//
// THE UPGRADE PATH, and the only test that exercises it. Migration 9 splits
// "which backends may this lease run on" from "which one is it on"; a row that
// predates it carries a single provider, and the placement check fails CLOSED on
// a lease whose acceptable backends it cannot read. Get the migration wrong and
// every in-flight job at upgrade time becomes unplaceable — visible only as work
// that stops moving.
//
// Simulated by removing the new columns from a migrated database and replaying,
// which is the closest thing to an old database this repository can construct:
// the schema is defined in code, so there is no historical file to open.
func TestMigrationCarriesASingleProviderForward(t *testing.T) {
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
			id   string
			node any
		}{
			{"unbound-lease", nil},
			{"bound-lease", "epyc-1"},
		} {
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO leases
				   (id, tier, node, macos_slot, guest_os, provider, providers, chosen_provider,
				    phase, vcpu, memory, epoch, created_at, heartbeat_at, expires_at)
				 VALUES (?, 'billet-8vcpu', ?, 0, 'linux', 'firecracker', '', '',
				         'capacity', 8, 34359738368, 0, '2026-01-01T00:00:00Z',
				         '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z')`,
				row.id, row.node); err != nil {
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
