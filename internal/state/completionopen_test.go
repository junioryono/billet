package state

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// UNREACHABILITY IS A PROPERTY OF THE ERROR, NOT OF THE STEP THAT PRODUCED IT.
// A caller that waits out an outage rather than refusing needs to know the
// database could not be reached; a ping that failed because the file is not a
// database is the database answering, and reading that as unreachability would
// have such a caller wait out a corrupt ledger for ever.
func TestUnreachableIsAskedOfTheErrorAndNotOfTheStep(t *testing.T) {
	// A LEDGER THAT IS NOT ONE: the ping fails, and it fails with positive
	// evidence about the file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "billet.db"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}

	_, err := OpenAdmin(t.Context(), dir)
	if err == nil {
		t.Fatal("a file that is not a database opened")
	}

	if Unreachable(err) {
		t.Fatalf("a corrupt ledger reads as unreachable: %v", err)
	}

	// A POSTGRESQL DSN NOTHING LISTENS ON: the connection cannot be
	// established, which is what the word means.
	_, err = OpenPostgresCompletion(t.Context(), t.TempDir(),
		"postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable&connect_timeout=2")
	if err == nil {
		t.Fatal("a port nothing listens on opened")
	}

	if !Unreachable(err) {
		t.Fatalf("a refused connection does not read as unreachable: %v", err)
	}

	if Unreachable(nil) {
		t.Fatal("no error reads as unreachable")
	}

	// AND A REFUSAL THE LEDGER ITSELF COMPOSED IS NOT ONE.
	if Unreachable(ErrSchemaAhead) || Unreachable(ErrForeignLedger) {
		t.Fatal("a refusal the ledger answered with reads as unreachable")
	}
}

// A COMPLETION WRITES ONE ROW AND CLAIMS NOTHING. Its caller is a host whose
// controller has been retired: an admin open would take the deployment's
// controller exclusion and migrate the shared schema whenever the survivor
// happened to be down, which is the one moment a retired host must not become
// this deployment's control plane.
func TestOpenPostgresCompletionClaimsNothingAndMigratesNothing(t *testing.T) {
	dsn := requirePostgres(t)

	// A ledger a control plane has already migrated and left.
	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := plane.ClaimController(t.Context(), "control-a", "dddddddddddddddddddddddddddddddd"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatalf("close the plane: %v", err)
	}

	// The retired host's own directory is the archive: nothing else locks it,
	// so the controller exclusion is free for the taking — and is not taken.
	var db *DB

	ops := sideEffects(t, func() {
		db, err = OpenPostgresCompletion(t.Context(), t.TempDir(), dsn)
	})
	if err != nil {
		t.Fatalf("OpenPostgresCompletion: %v", err)
	}

	defer func() { _ = db.Close() }()

	if slices.Contains(ops, "claim") || slices.Contains(ops, "migrate") || slices.Contains(ops, "watermark") {
		t.Fatalf("a completion claimed or migrated: %v", ops)
	}

	// AND IT CAN WRITE, which is the whole reason it is not an inspection.
	if _, _, err := db.CompleteRetirement(t.Context(), RetirementCompletion{
		Deployment: "dddddddddddddddddddddddddddddddd", Retiring: "control-a", Survivor: "control-b",
		TransitionID: "0123456789abcdef0123456789abcdef", ReservedAt: "2026-09-11T10:00:00Z",
		CompletedBy: "control-a", At: time.Now(),
	}); err != nil {
		t.Fatalf("complete the row through a completion handle: %v", err)
	}
}

// AND IT REFUSES A SCHEMA THAT IS NOT EXACTLY THIS BINARY'S rather than
// migrating it, which is what lets the caller hand the write to the survivor:
// a ledger the survivor has already migrated past is ErrSchemaAhead, and one
// behind this binary is ErrSchemaBehind — neither is this host's to move.
func TestOpenPostgresCompletionRefusesASchemaItWouldHaveToMove(t *testing.T) {
	dsn := requirePostgresSchema(t, "completion_behind")

	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatalf("close the plane: %v", err)
	}

	// ONE APPLIED VERSION REMOVED FROM THE RECORD is a ledger behind this
	// binary without touching a table: what the open compares is the recorded
	// set against its own.
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a connection: %v", err)
	}

	//billet:ignore rawsql // the fixture rewinds the bookkeeping table this open reads
	if _, err := conn.ExecContext(t.Context(),
		`DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatalf("rewind the recorded schema: %v", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close the connection: %v", err)
	}

	_, err = OpenPostgresCompletion(t.Context(), t.TempDir(), dsn)
	if !errors.Is(err, ErrSchemaBehind) {
		t.Fatalf("a completion over a ledger behind this binary: %v", err)
	}
}
