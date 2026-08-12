package state

import (
	"database/sql"
	"errors"
	"strings"
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

// THE OPEN-TIME CHECK IS A TIME-OF-CHECK-TO-TIME-OF-USE ON ITS OWN.
//
// An admin handle verifies the schema against the control plane it found. That
// plane can then exit and a NEWER one acquire the lock and migrate, leaving the
// still-running command writing against a schema it never checked — which is
// precisely the restart boundary refuseUnknownVersions exists to guard. So every
// transaction on an unlocked handle re-checks.
//
// The migration is staged by writing the bookkeeping row a newer billet would
// have written, which is what an older binary actually sees afterwards.
func TestAnOperatorTransactionRechecksTheSchemaItIsWritingAgainst(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open (the server): %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	// It verified cleanly at open, which is what makes this a TOCTOU rather than
	// an ordinary refusal.
	if err := admin.Tx(ctx, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("the admin handle should be usable before anything changes: %v", err)
	}

	// A NEWER BILLET MIGRATES. From this binary's side that is a recorded
	// migration it has never heard of.
	if err := server.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			9999, "from_a_newer_billet", "whatever", time.Now().UTC().Format(time.RFC3339Nano))

		return err
	}); err != nil {
		t.Fatalf("stage a newer billet's migration: %v", err)
	}

	err = admin.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES (?, ?, ?)`,
			"epyc-1", "docker", time.Now().UTC().Format(time.RFC3339Nano))

		return err
	})
	if err == nil {
		t.Fatal("a transaction on an unlocked handle must re-check the schema; the database was " +
			"migrated by a newer billet after this handle verified it")
	}

	// AND THE WRITE DID NOT LAND. Refusing after committing would be the defect
	// with a message attached.
	var nodes int

	if err := server.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE name = ?`, "epyc-1").Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}

	if nodes != 0 {
		t.Errorf("the refused transaction still wrote %d node row(s)", nodes)
	}
}

// AN OPERATOR COMMAND DOES NOT RE-SCAN THE WHOLE LEDGER.
//
// quick_check reads the entire file and job_history is unbounded, so its cost
// grows with the deployment. Running it at every open put that growing scan in
// front of `nodes approve`, `leases release --force` and `check`, under the same
// thirty-second startup budget — so a large or loaded deployment could lose every
// live administration command, the emergency one included.
//
// The control plane still scans: it is about to schedule against this ledger.
func TestOnlyTheControlPlaneScansTheLedgerAtOpen(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open (the server): %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	if !server.scanned {
		t.Error("a control plane must verify the ledger's integrity before scheduling against it")
	}

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if admin.scanned {
		t.Error("an operator command re-scanned the whole ledger; that cost grows with " +
			"job_history and sits in front of the command an operator runs in an emergency")
	}

	// AND IT IS STILL AVAILABLE ON DEMAND, which is what `billet check` uses.
	if err := admin.IntegrityCheck(ctx); err != nil {
		t.Errorf("IntegrityCheck on a healthy ledger: %v", err)
	}
}

// AND A READ RE-CHECKS TOO, for the same reason a write does.
//
// Separate from the transaction test above because View has its own code path:
// deleting its verification left every other test green, so the guard was
// decorative. A read against a schema a newer billet has rebuilt would report
// rows that no longer mean what this binary thinks they mean.
func TestAnOperatorReadRechecksTheSchemaItIsReadingFrom(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open (the server): %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if err := admin.View(ctx, func(Querier) error { return nil }); err != nil {
		t.Fatalf("the admin handle should read cleanly before anything changes: %v", err)
	}

	if err := server.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			9999, "from_a_newer_billet", "whatever", time.Now().UTC().Format(time.RFC3339Nano))

		return err
	}); err != nil {
		t.Fatalf("stage a newer billet's migration: %v", err)
	}

	called := false

	err = admin.View(ctx, func(Querier) error {
		called = true

		return nil
	})
	if err == nil {
		t.Fatal("a read on an unlocked handle must re-check the schema")
	}

	// REFUSED BEFORE THE CALLBACK RAN, not after. Reporting an error having
	// already handed the caller rows from a schema it does not understand would
	// be the defect with a message attached.
	if called {
		t.Error("the callback ran despite the schema check failing")
	}
}

// TWO OPERATOR COMMANDS ON A FRESH INSTALL RACE HERE, and the answer has to be
// honest about which situation it is.
//
// The first takes the free lock and creates the schema; the second finds the
// lock held, which is indistinguishable from a running control plane, and finds
// no schema at all. Reporting that as "the plane holding this directory is older
// than you" would send an operator looking for a control plane that does not
// exist.
func TestAnOperatorCommandSaysTheDirectoryIsStillBeingInitialised(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	// Somebody else holds the directory and has not created the schema yet,
	// which is what the middle of a first-run migration looks like from here.
	held, err := lockDir(dir)
	if err != nil {
		t.Fatalf("take the directory lock: %v", err)
	}

	t.Cleanup(func() {
		if err := held.release(); err != nil {
			t.Errorf("release the directory lock: %v", err)
		}
	})

	_, err = OpenAdmin(ctx, dir)
	if err == nil {
		t.Fatal("OpenAdmin must refuse a directory whose schema does not exist yet")
	}

	if !errors.Is(err, ErrSchemaBehind) {
		t.Fatalf("error should be ErrSchemaBehind, got: %v", err)
	}

	// THE REMEDY IS THE POINT, not the sentinel. "Restart the older control
	// plane" is the wrong advice here and is what this asserts against.
	if got := err.Error(); !strings.Contains(got, "initialising") {
		t.Errorf("the diagnostic should say the directory is being initialised, got: %v", got)
	}
}
