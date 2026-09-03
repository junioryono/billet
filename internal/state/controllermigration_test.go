package state

import (
	"database/sql"
	"errors"
	"testing"
)

// heldByAnotherHost stands in for a controller on a machine this test cannot
// start, and holds the exclusion the way that controller would.
//
// A RAW CONNECTION RATHER THAN A SECOND OpenPostgres, because opening one would
// MIGRATE — and an absent schema is exactly what the tests below have to
// observe. It is the same reasoning the partition test uses for the claim pool:
// an advisory lock lives in a SESSION, so the connection that takes it is the
// one that has to stay open, which is what MaxOpenConns(1) guarantees.
func heldByAnotherHost(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a connection for the standing-in controller: %v", err)
	}

	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0)
	conn.SetConnMaxIdleTime(0)

	t.Cleanup(func() { _ = conn.Close() })

	key, err := readLockKey(t.Context(), conn)
	if err != nil {
		t.Fatalf("identify the ledger: %v", err)
	}

	var taken bool

	if err := conn.QueryRowContext(t.Context(),
		`SELECT pg_try_advisory_lock($1, $2)`, advisoryClassController, key).Scan(&taken); err != nil {
		t.Fatalf("take the controller exclusion: %v", err)
	}

	if !taken {
		t.Fatal("the controller exclusion was already held on a schema of this test's own")
	}

	return conn
}

// requireNoSchema proves nothing has created the ledger's bookkeeping table.
//
// ASKED THROUGH THE STANDING-IN CONTROLLER'S CONNECTION, which is the one
// connection here that is definitely pointed at this test's own schema.
func requireNoSchema(t *testing.T, conn *sql.DB, what string) {
	t.Helper()

	exists, err := (&postgresBackend{}).bookkeepingTableExists(t.Context(), conn)
	if err != nil {
		t.Fatalf("inspect the ledger's schema: %v", err)
	}

	if exists {
		t.Fatalf("%s migrated a ledger it does not hold the controller exclusion for", what)
	}
}

// A REFUSED CONTROLLER LEAVES THE SCHEMA EXACTLY AS IT FOUND IT.
//
// THE DIRECTORY LOCK IS NOT THE EXCLUSION ONCE THE LEDGER IS SHARED, and this is
// the failure that follows from treating it as one. A second host takes its OWN
// directory's lock happily, so under the old rule — "migrating is the lock
// holder's job" — a server that is about to be told it is not the controller
// would first upgrade the schema underneath the controller that IS, which is the
// one thing the controller election's invariant list says nothing may do.
//
// The exclusion needs no schema to take, which is what makes the ordering
// possible at all: only the RECORD half of a claim needs a table to write to.
func TestARefusedControllerLeavesTheSchemaUntouched(t *testing.T) {
	dsn := requirePostgres(t)
	holder := heldByAnotherHost(t, dsn)

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
		t.Fatal("a second control plane opened a ledger another one holds")
	}

	if !errors.Is(err, ErrControllerHeld) {
		t.Fatalf("the refusal should be ErrControllerHeld; got %v", err)
	}

	requireNoSchema(t, holder, "a refused control plane")
}

// AND NEITHER DOES AN OPERATOR COMMAND, WHICH IS THE HALF THAT IS EASY TO MISS.
//
// OpenAdmin's rule is that it proceeds without the directory lock and then
// VERIFIES rather than migrates. On a shared ledger it GETS that lock, because
// the directory it is locking is its own host's — so the command that exists to
// be harmless against a live deployment would have migrated that deployment's
// schema, from a machine the control plane has never heard of.
//
// It is told to try again rather than refused outright, and the distinction is
// deliberate: an admin handle that CAN take the exclusion still migrates,
// because somebody has to create the schema on a deployment whose server has
// never started and `billet check` runs before the first `billet server`.
func TestAnAdminHandleNeverMigratesASharedLedger(t *testing.T) {
	dsn := requirePostgres(t)
	holder := heldByAnotherHost(t, dsn)

	db, err := OpenPostgresAdmin(t.Context(), t.TempDir(), dsn)
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
		t.Fatal("an operator command opened a ledger whose schema it could not verify")
	}

	if !errors.Is(err, ErrSchemaBehind) {
		t.Fatalf("the refusal should be ErrSchemaBehind; got %v", err)
	}

	requireNoSchema(t, holder, "an operator command")
}

// AN OPERATOR COMMAND ON A DEPLOYMENT WITH NO CONTROLLER STILL CREATES THE
// SCHEMA, AND GIVES THE EXCLUSION STRAIGHT BACK.
//
// The other direction of the rule above, and the one that keeps a fresh install
// working: `billet check` and `billet ca issue` both run before the first
// `billet server`, so a command that could never migrate would leave a
// PostgreSQL deployment with no way to create its schema at all.
//
// Holding the exclusion afterwards was the alternative and is worse: a command
// is not the controller, and `billet local backup` holds its handle across a
// whole ledger snapshot — which would become a window in which no control plane
// can start, refused by an ErrControllerHeld quoting whichever holder the claim
// row last recorded rather than the command actually in the way.
func TestAnAdminHandleMigratesAnUnheldLedgerAndReleasesTheExclusion(t *testing.T) {
	dsn := requirePostgres(t)

	db, err := OpenPostgresAdmin(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgresAdmin on an unheld ledger: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	// IT MIGRATED, which is what a fresh install depends on.
	exists, err := db.backend.bookkeepingTableExists(t.Context(), db.Reader())
	if err != nil {
		t.Fatalf("inspect the ledger's schema: %v", err)
	}

	if !exists {
		t.Fatal("an operator command on an unheld ledger did not create the schema")
	}

	// AND IT IS NOT HOLDING THE EXCLUSION, proved by a control plane taking it
	// while the command's handle is still open. Asserting the backend's own field
	// would test the implementation; opening a controller tests the property.
	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("a control plane could not start while an operator command held a handle: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })
}
