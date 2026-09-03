package state

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSnapshotIntoIsAConsistentCopyOfALiveLedger proves that snapshot into is a consistent copy of a live ledger.
//
// THE ASSERTION IS THE CONTENT, NEVER THE FILE BYTES. VACUUM INTO repacks pages,
// so a byte-identity check would fail against a correct snapshot — and were it
// ever to pass it would be pinning SQLite's page layout rather than anything
// billet is responsible for.
func TestSnapshotIntoIsAConsistentCopyOfALiveLedger(t *testing.T) {
	db := open(t)

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES ('epyc-1', 'docker', '2026-01-01')`)

		return err
	}); err != nil {
		t.Fatalf("write a row: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "snapshot.db")

	if err := db.SnapshotInto(t.Context(), dest); err != nil {
		t.Fatalf("SnapshotInto: %v", err)
	}

	got, err := PeekMigrations(t.Context(), dest)
	if err != nil {
		t.Fatalf("PeekMigrations: %v", err)
	}

	if !reflect.DeepEqual(got, knownMigrations()) {
		t.Errorf("the snapshot's migration set is not this binary's")
	}

	contents, err := PeekLedger(t.Context(), dest)
	if err != nil {
		t.Fatalf("PeekLedger: %v", err)
	}

	if !contents.Populated {
		t.Error("the snapshot does not carry the row the live ledger held")
	}
}

// TestASnapshotIsWrittenPrivate. SQLite creates the file 0644 under the usual
// umask — measured — and the ledger holds join-token digests and certificate
// serials.
func TestASnapshotIsWrittenPrivate(t *testing.T) {
	db := open(t)
	dest := filepath.Join(t.TempDir(), "snapshot.db")

	if err := db.SnapshotInto(t.Context(), dest); err != nil {
		t.Fatalf("SnapshotInto: %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("the snapshot is mode %04o, want 0600", info.Mode().Perm())
	}
}

// TestSnapshotIntoRefusesARelativePath proves that snapshot into refuses a relative path.
//
// SQLite resolves a relative path against the PROCESS working directory rather
// than against the state directory this handle was opened on, so a caller would
// get a snapshot somewhere neither it nor the operator named and then report the
// path it asked for.
func TestSnapshotIntoRefusesARelativePath(t *testing.T) {
	db := open(t)

	err := db.SnapshotInto(t.Context(), filepath.Join("relative", "snapshot.db"))
	if err == nil {
		t.Fatal("a relative destination was accepted")
	}

	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the refusal does not say the path must be absolute: %v", err)
	}
}

// TestSnapshotIntoRefusesAnOccupiedDestination, and leaves it alone.
func TestSnapshotIntoRefusesAnOccupiedDestination(t *testing.T) {
	db := open(t)
	dest := filepath.Join(t.TempDir(), "occupied.db")

	if err := os.WriteFile(dest, []byte("not a snapshot\n"), 0o600); err != nil {
		t.Fatalf("occupy the destination: %v", err)
	}

	if err := db.SnapshotInto(t.Context(), dest); err == nil {
		t.Fatal("a snapshot over an existing file succeeded")
	}

	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read the destination: %v", err)
	}

	if string(body) != "not a snapshot\n" {
		t.Error("the file already at the destination was modified")
	}
}

// TestSnapshotIntoRefusesAFencedLedger, because a backup is an operator write
// and a host upgrade's fence closes the ledger to those.
func TestSnapshotIntoRefusesAFencedLedger(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if _, err := WriteMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatalf("WriteMaintenanceFence: %v", err)
	}

	err = db.SnapshotInto(t.Context(), filepath.Join(t.TempDir(), "snapshot.db"))
	if !errors.Is(err, ErrMaintenance) {
		t.Errorf("a snapshot of a fenced ledger returned %v, want ErrMaintenance", err)
	}
}

// TestPeekMigrationsWillNotCreateALedger. It is asked about a target directory a
// restore has not committed to, so answering by CREATING one would be the worst
// possible answer.
func TestPeekMigrationsWillNotCreateALedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")

	if _, err := PeekMigrations(t.Context(), path); err == nil {
		t.Fatal("PeekMigrations succeeded against a database that does not exist")
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("PeekMigrations created the database it was asked about: %v", err)
	}
}

// TestPeekRefusesASymlink, for both readers: the path billet was told to read is
// the only one it should read.
func TestPeekRefusesASymlink(t *testing.T) {
	db := open(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "snapshot.db")

	if err := db.SnapshotInto(t.Context(), target); err != nil {
		t.Fatalf("SnapshotInto: %v", err)
	}

	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	if _, err := PeekMigrations(t.Context(), link); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Errorf("PeekMigrations followed a symlink: %v", err)
	}

	if _, err := PeekLedger(t.Context(), link); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Errorf("PeekLedger followed a symlink: %v", err)
	}
}

// TestPeekLedgerTellsAPreflightLedgerFromAUsedOne proves that peek ledger tells a preflight ledger from a used one.
//
// BOTH DIRECTIONS. `billet check` creates a schema-only billet.db on a host
// nobody has commissioned, and a restore is allowed to replace that one and only
// that one — so a PeekLedger that answered "populated" for everything would be
// as wrong as one that answered "empty".
func TestPeekLedgerTellsAPreflightLedgerFromAUsedOne(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	path := filepath.Join(dir, "billet.db")

	contents, err := PeekLedger(t.Context(), path)
	if err != nil {
		t.Fatalf("PeekLedger: %v", err)
	}

	if contents.Populated {
		t.Errorf("a schema-only ledger reads as populated: %v", contents.NonEmpty)
	}

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES ('epyc-1', 'docker', '2026-01-01')`)

		return execErr
	}); err != nil {
		t.Fatalf("write a row: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	contents, err = PeekLedger(t.Context(), path)
	if err != nil {
		t.Fatalf("PeekLedger: %v", err)
	}

	if !contents.Populated {
		t.Fatal("a ledger with a node in it reads as empty")
	}

	if len(contents.NonEmpty) != 1 || !strings.HasPrefix(contents.NonEmpty[0], "nodes") {
		t.Errorf("the report does not name the table that made it populated: %v", contents.NonEmpty)
	}
}

// TestASealedLedgerIsNotPristine proves a ledger somebody sealed does not read
// as an untouched preflight one.
//
// `admission` is a SINGLETON its migration inserts, so a row count cannot tell
// the migration's own row from one an operator changed — and a drain taken on a
// host somebody then restores over is deployment state. Counting to one exempted
// a seal, an advanced generation, and a provenance and reason a person wrote.
func TestASealedLedgerIsNotPristine(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	path := filepath.Join(dir, "billet.db")

	// The other direction first, or a PeekLedger that called everything
	// populated would pass the assertion below.
	contents, err := PeekLedger(t.Context(), path)
	if err != nil {
		t.Fatalf("PeekLedger: %v", err)
	}

	if contents.Populated {
		t.Fatalf("a freshly migrated ledger reads as populated: %v", contents.NonEmpty)
	}

	current, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if _, err := db.Seal(t.Context(), SealRequest{
		Expect:     current.Generation,
		Provenance: ProvenanceOperator,
		Reason:     "maintenance window",
		Actor:      "someone",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	contents, err = PeekLedger(t.Context(), path)
	if err != nil {
		t.Fatalf("PeekLedger: %v", err)
	}

	if !contents.Populated {
		t.Fatal("a ledger somebody sealed reads as an untouched preflight one, so a restore " +
			"would delete it")
	}

	if len(contents.NonEmpty) != 1 || !strings.HasPrefix(contents.NonEmpty[0], "admission") {
		t.Errorf("the report does not name admission as what changed: %v", contents.NonEmpty)
	}
}

// TestRefuseUnknownVersionsIsTheSameRuleTheMigratorApplies proves that refuse unknown versions is the same rule the migrator applies.
func TestRefuseUnknownVersionsIsTheSameRuleTheMigratorApplies(t *testing.T) {
	// NOT DECORATION. knownMigrations() returns an empty slice whenever the
	// embedded set failed to load, and RefuseUnknownVersions of an empty set was
	// then a vacuous pass — the first assertion below agreed with a binary that
	// could not read a single migration.
	loadedMigrations(t)

	if err := RefuseUnknownVersions(knownMigrations()); err != nil {
		t.Errorf("this binary's own migration set was refused: %v", err)
	}

	future := append(knownMigrations(),
		AppliedMigration{Version: 9999, Name: "from_the_future", Checksum: "x"})

	err := RefuseUnknownVersions(future)
	if err == nil {
		t.Fatal("a migration from a newer billet was accepted")
	}

	if !strings.Contains(err.Error(), "newer version") {
		t.Errorf("the refusal does not say a newer billet wrote it: %v", err)
	}

	// AND IT IS NOT ErrSchemaBehind, which means something else entirely: a
	// RUNNING control plane holding a ledger this binary would have to migrate.
	if errors.Is(err, ErrSchemaBehind) {
		t.Error("the refusal reuses ErrSchemaBehind, whose remedy is a restart rather than a " +
			"newer binary")
	}
}
