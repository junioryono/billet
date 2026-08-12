package state

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// latestVersion is the highest migration this binary carries.
func latestVersion(t *testing.T) int {
	t.Helper()

	highest := 0

	for _, m := range migrations {
		if m.Version > highest {
			highest = m.Version
		}
	}

	if highest == 0 {
		t.Fatal("no migrations are defined, so every version assertion below is vacuous")
	}

	return highest
}

// schemaVersion is the highest migration recorded in a database.
func schemaVersion(t *testing.T, db *DB) int {
	t.Helper()

	var version int

	err := db.Reader().QueryRowContext(t.Context(),
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		t.Fatalf("read the recorded schema version: %v", err)
	}

	return version
}

// THE DEFECT. Every operator command that reaches the ledger — nodes
// pending/approve/revoke, ca token/issue/revoke/revocations, leases
// quarantined/release, and check — opened it through Open, which takes the
// exclusive directory lock a running control plane already holds. So all of them
// failed with ErrLocked against a live deployment.
//
// It is worst for `leases release --force`, whose whole purpose is reclaiming
// capacity a quarantine has stranded on a RUNNING deployment: the documented
// remedy required stopping the thing holding the capacity.
//
// The lock exists to stop TWO CONTROL PLANES writing conflicting scheduling
// decisions. A one-shot command is not one, and SQLite serialises the writes
// themselves, so it may proceed without the lock.
func TestAnOperatorCommandOpensWhileTheServerHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open (the server): %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	// The existing guard still holds: a second CONTROL PLANE is refused.
	if _, err := Open(ctx, dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second Open must still be refused with ErrLocked, got: %v", err)
	}

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin while the server holds the lock: %v", err)
	}

	defer func() { _ = admin.Close() }()

	// AND IT MUST BE USABLE FOR A WRITE, not merely openable. Every command in
	// the list above mutates: approving an enrollment, revoking a certificate,
	// forcing a quarantined lease back. An admin handle that opens and then
	// cannot write would move the failure one line later rather than fix it.
	now := time.Now().UTC().Format(time.RFC3339Nano)

	if err := admin.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES (?, ?, ?)`,
			"epyc-1", "docker", now)

		return err
	}); err != nil {
		t.Fatalf("write through the admin handle: %v", err)
	}

	// READ BACK THROUGH THE SERVER'S OWN HANDLE, because the point is that the
	// running control plane sees it. Reading it back through the admin handle
	// would prove only that SQLite remembers its own transaction.
	var provider string

	if err := server.Reader().QueryRowContext(ctx,
		`SELECT provider FROM nodes WHERE name = ?`, "epyc-1").Scan(&provider); err != nil {
		t.Fatalf("the server should see what the operator command wrote: %v", err)
	}

	if provider != "docker" {
		t.Errorf("provider = %q, want docker", provider)
	}
}

// With nothing else holding the directory there is no reason to behave
// differently from Open, and one good reason not to: on a fresh control plane an
// operator runs `billet ca issue` before the server has ever started, so the
// schema has to be created by whoever gets there first.
func TestAnOperatorCommandMigratesWhenNothingElseHoldsTheDirectory(t *testing.T) {
	ctx := t.Context()

	admin, err := OpenAdmin(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenAdmin on a fresh directory: %v", err)
	}

	defer func() { _ = admin.Close() }()

	if got, want := schemaVersion(t, admin), latestVersion(t); got != want {
		t.Errorf("schema version = %d, want %d; a fresh directory must be migrated", got, want)
	}
}

// A NEWER CLI MUST NOT MIGRATE THE RUNNING SERVER'S DATABASE UNDERNEATH IT.
//
// Open runs migrations, so an operator running a newer binary's `billet nodes
// approve` against a live older control plane would silently upgrade the schema
// that plane is mid-transaction against. Refusing is the only safe answer: the
// running process cannot be asked to re-read it.
func TestAnOperatorCommandRefusesToMigrateUnderARunningServer(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	behind := latestVersion(t) - 1

	// A control plane running an older binary, still holding the directory.
	server := openAt(t, dir, behind)

	t.Cleanup(func() { _ = server.Close() })

	if got := schemaVersion(t, server); got != behind {
		t.Fatalf("staged schema version = %d, want %d", got, behind)
	}

	_, err := OpenAdmin(ctx, dir)
	if err == nil {
		t.Fatal("OpenAdmin must refuse a database whose schema this binary would have to migrate")
	}

	// THE DIAGNOSTIC IS THE POINT. An operator holding two binaries needs to be
	// told which one is ahead, not merely that something is wrong.
	if !errors.Is(err, ErrSchemaBehind) {
		t.Errorf("error should be ErrSchemaBehind, got: %v", err)
	}

	// AND NOTHING WAS MIGRATED. This is the assertion the whole test exists for:
	// refusing after upgrading the schema would be the same defect with a
	// message attached.
	if got := schemaVersion(t, server); got != behind {
		t.Errorf("schema version = %d after a refused admin open, want %d untouched", got, behind)
	}
}
