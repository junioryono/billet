package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// postgresDSNEnv names the environment variable that points these tests at a
// real server.
//
// A REAL ONE, ALWAYS. Nothing here is worth asserting against a fake: the whole
// value of this backend's tests is that PostgreSQL answers, because every fact
// this file pins — what an advisory lock does, what a read-only transaction
// refuses, what the catalogue reports — is a fact about the server rather than
// about billet's code.
const postgresDSNEnv = "BILLET_TEST_POSTGRES_DSN"

// requirePostgres returns a DSN pointing at a schema of this test's own, or
// skips.
//
// A SKIP THAT QUIETLY PASSES IS THE FAILURE MODE THIS PACKAGE CARES ABOUT, so
// the skip is refused under CI: a run with no server would otherwise report
// success for a backend nothing exercised, which is exactly the shape of a test
// that exists and checks nothing.
//
// A SCHEMA PER TEST rather than a database per test, for two reasons. It is
// fast, so these can be ordinary tests rather than a suite somebody runs
// separately — and it exercises the property the backend actually needs: every
// catalogue question it asks is scoped to current_schema(), because a deployment
// may share a database with something else entirely. A test that used the public
// schema would pass while that scoping was broken.
func requirePostgres(t *testing.T) string {
	t.Helper()

	return requirePostgresSchema(t, "")
}

// requirePostgresSchema is the same, with a suffix so one test can hold two
// LEDGERS rather than two names for one.
func requirePostgresSchema(t *testing.T, suffix string) string {
	t.Helper()

	dsn := os.Getenv(postgresDSNEnv)
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is unset under CI, so the PostgreSQL backend would go untested "+
				"while the run reported success", postgresDSNEnv)
		}

		t.Skipf("%s is unset; start a PostgreSQL and set it to exercise this backend",
			postgresDSNEnv)
	}

	// LOWERCASE, because search_path folds an unquoted identifier and the CREATE
	// SCHEMA below quotes one. A mixed-case name creates a schema nothing then
	// selects, and the failure is "no schema has been selected to create in",
	// which names neither.
	schema := strings.ToLower("billet_test_" + strings.ReplaceAll(t.Name(), "/", "_") + suffix)
	if len(schema) > 60 {
		schema = schema[:60]
	}

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", postgresDSNEnv, err)
	}

	defer func() { _ = admin.Close() }()

	ctx := t.Context()

	// Quoted, because the schema name is derived from the test's name.
	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + quoted + ` CASCADE`,
		`CREATE SCHEMA ` + quoted,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare the test schema: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}

		defer func() { _ = cleanup.Close() }()

		//nolint:errcheck,noctx // teardown, after the test's context is gone.
		_, _ = cleanup.Exec(`DROP SCHEMA IF EXISTS ` + quoted + ` CASCADE`)
	})

	return withSearchPath(t, dsn, schema)
}

// withSearchPath points a DSN at one schema.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresDSNEnv, err)
	}

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	return u.String()
}

// openPostgres opens a migrated PostgreSQL-backed ledger in its own schema.
func openPostgres(t *testing.T) *DB {
	t.Helper()

	db, err := OpenPostgres(t.Context(), t.TempDir(), requirePostgres(t))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// THE MIGRATIONS APPLY, AND THE LEDGER IS THE ONE THIS BINARY KNOWS.
//
// The first thing worth proving about a second backend, and it covers the
// bootstrap, the migration runner, the timeline and the bookkeeping table in one
// go — the same path Open takes on SQLite, against a server instead of a file.
func TestAPostgresLedgerMigratesToThisBinarysSchema(t *testing.T) {
	db := openPostgres(t)

	applied, err := readAppliedMigrations(t.Context(), ReadQueries(db.Reader()))
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}

	want := loadedTimeline(t, pgTimeline)
	if len(applied) != len(want) {
		t.Fatalf("%d migrations recorded, want %d", len(applied), len(want))
	}

	for _, m := range want {
		rec, ok := applied[m.Version]
		if !ok {
			t.Errorf("migration %d (%s) was not recorded", m.Version, m.Name)

			continue
		}

		if rec.name != m.Name || rec.checksum != m.checksum() {
			t.Errorf("migration %d recorded as (%s, %s), want (%s, %s)",
				m.Version, rec.name, rec.checksum, m.Name, m.checksum())
		}
	}
}

// REOPENING APPLIES NOTHING AND CHANGES NOTHING.
//
// The migrator's idempotence is what makes a restart safe, and on this backend
// it is also what proves the checksum comparison reads the same bytes back that
// it wrote.
func TestReopeningAPostgresLedgerAppliesNothingTwice(t *testing.T) {
	dsn := requirePostgres(t)
	dir := t.TempDir()

	first, err := OpenPostgres(t.Context(), dir, dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := OpenPostgres(t.Context(), dir, dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	applied, err := readAppliedMigrations(t.Context(), ReadQueries(second.Reader()))
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}

	if got, want := len(applied), len(loadedTimeline(t, pgTimeline)); got != want {
		t.Errorf("reopening recorded %d migrations, want %d", got, want)
	}
}

// THE READER CANNOT WRITE, AND THE ENGINE IS WHAT REFUSES.
//
// The counterpart of the SQLite query_only assertion. Querier being narrow stops
// a caller writing by accident; this is the layer underneath, and it is the one
// that still holds when a mutation reaches the handle some other way — an
// `UPDATE … RETURNING` declared `:one`, which is dispatched as a query.
func TestThePostgresReaderRefusesAWrite(t *testing.T) {
	db := openPostgres(t)

	err := db.View(t.Context(), func(q Querier) error {
		var n int

		return q.QueryRowContext(t.Context(),
			`INSERT INTO admission (id, mode, generation, provenance, reason, actor, changed_at) `+
				`VALUES (2, 'open', 0, '', '', '', '') RETURNING id`).Scan(&n)
	})
	if err == nil {
		t.Fatal("a write through the read-only pool was accepted")
	}

	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("want the server's read-only refusal, got: %v", err)
	}
}

// ONE WRITER AT A TIME ACROSS PROCESSES, WHICH IS WHAT MAKES Tx MEAN ON
// POSTGRESQL WHAT IT MEANS ON SQLITE.
//
// Every allocation decision is read-current, decide, record. SQLite guarantees
// that with BEGIN IMMEDIATE, which is a lock on the FILE and so excludes the
// operator command in the other process as well; this backend guarantees it with
// a transaction-scoped advisory lock, which is the only part of that the engine
// does not give for free.
//
// TWO HANDLES, NOT TWO GOROUTINES, AND THAT DISTINCTION IS THE WHOLE TEST. The
// writer pool is a single connection, so two goroutines sharing one DB are
// serialized by database/sql before the ledger is involved at all — an overlap
// test written that way passes with no lock whatsoever, which is a test that
// exists and checks nothing. What the lock is actually for is the SECOND
// PROCESS: an operator command opening the same ledger while the control plane
// is running, which is ordinary and which SQLite's file lock already covers.
func TestASecondHandleCannotWriteWhileTheFirstHoldsTheLedger(t *testing.T) {
	dsn := requirePostgres(t)

	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })

	// A DIFFERENT STATE DIRECTORY, because the two handles stand for two
	// processes on two machines: the directory lock is per host and is not what
	// is under test here.
	command, err := OpenPostgresAdmin(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgresAdmin: %v", err)
	}

	t.Cleanup(func() { _ = command.Close() })

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- plane.Tx(t.Context(), func(*sql.Tx) error {
			close(held)
			<-release

			return nil
		})
	}()

	<-held

	// SHORT, AND A DEADLINE RATHER THAN AN ATTEMPT COUNT. The second handle is
	// expected to wait, so what is asserted is that it does NOT get in — and the
	// only way to observe "did not get in" is to stop waiting.
	blocked, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err = command.Tx(blocked, func(*sql.Tx) error { return nil })
	if err == nil {
		close(release)
		<-done

		t.Fatal("a second handle wrote while the first held the ledger; without the advisory " +
			"lock in beginWrite, a read-current-decide-record sequence in one process can " +
			"interleave with one in another and double-charge a lease")
	}

	// AND IT IS WAITING RATHER THAN FAILING. A refusal would be a different bug
	// with the same symptom, and contention is a race rather than a verdict.
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-done

		t.Fatalf("the second handle should have been WAITING for the lock; got %v", err)
	}

	close(release)

	if err := <-done; err != nil {
		t.Fatalf("the first handle's transaction: %v", err)
	}

	// AND IT GETS IN ONCE THE FIRST COMMITS, which is what says the lock is
	// transaction-scoped rather than leaked for the connection's lifetime.
	after, cancelAfter := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelAfter()

	if err := command.Tx(after, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("the second handle could not write after the first committed: %v", err)
	}
}

// TWO LEDGERS IN ONE DATABASE DO NOT SERIALIZE AGAINST EACH OTHER.
//
// The reason the writer's lock is keyed on the LEDGER rather than taken
// globally: sharing a PostgreSQL server is one of the reasons to run one, and a
// deployment whose writes queued behind a neighbour's would have bought a
// bottleneck nobody asked for.
//
// TWO SCHEMAS, WHICH IS WHAT TWO DEPLOYMENTS ACTUALLY ARE. An earlier version of
// this test used one schema and two deployment names, and it was asserting a bug
// as a feature: the key was derived from the local identity then, so two
// configurations pointing at the SAME rows took different locks and could write
// concurrently. Keying on current_database() and current_schema() is what fixed
// it, and this test only means anything against two real ledgers.
func TestTwoLedgersDoNotBlockEachOther(t *testing.T) {
	first, err := OpenPostgres(t.Context(), t.TempDir(), requirePostgresSchema(t, "_one"))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = first.Close() })

	second, err := OpenPostgres(t.Context(), t.TempDir(), requirePostgresSchema(t, "_two"))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- first.Tx(t.Context(), func(*sql.Tx) error {
			close(held)
			<-release

			return nil
		})
	}()

	<-held

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := second.Tx(ctx, func(*sql.Tx) error { return nil }); err != nil {
		close(release)
		<-done

		t.Fatalf("a second ledger could not be written while the first was held: %v", err)
	}

	close(release)

	if err := <-done; err != nil {
		t.Fatalf("the first ledger's transaction: %v", err)
	}
}

// AND TWO PROCESSES POINTED AT ONE LEDGER TAKE THE SAME LOCK, WHICH IS THE HALF
// THAT MATTERS.
//
// The pair to the test above, and the one that would have caught the defect it
// describes: two handles built from two different state directories, against one
// schema, must serialize. Keyed on anything local they would not.
func TestTwoHandlesOnOneLedgerShareItsLockKey(t *testing.T) {
	dsn := requirePostgres(t)

	first, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = first.Close() })

	second, err := OpenPostgresAdmin(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgresAdmin: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	firstKey := postgresLockKey(t, first)
	secondKey := postgresLockKey(t, second)

	if firstKey == 0 || secondKey == 0 {
		t.Fatalf("a lock key is zero (%d, %d); prepare did not run, and every writer would "+
			"serialize on the same key whatever ledger it is on", firstKey, secondKey)
	}

	if firstKey != secondKey {
		t.Errorf("two handles on one ledger have lock keys %d and %d, so they would take "+
			"different advisory locks and write concurrently", firstKey, secondKey)
	}
}

// THE CATALOGUE QUESTIONS ARE SCOPED TO THIS DEPLOYMENT'S SCHEMA.
//
// PeekLedger reads "no rows anywhere" as permission to replace a ledger, so
// userTables missing one of billet's own tables is the dangerous direction. A
// neighbour's table appearing in the list is the safe one, and this asserts
// neither happens: the list is exactly what the migrations created.
func TestThePostgresCatalogueSeesOnlyThisSchema(t *testing.T) {
	db := openPostgres(t)

	tables, err := db.backend.userTables(t.Context(), db.Reader())
	if err != nil {
		t.Fatalf("userTables: %v", err)
	}

	found := make(map[string]bool, len(tables))
	for _, name := range tables {
		found[name] = true
	}

	for _, want := range []string{"nodes", "leases", "schema_migrations", "admission"} {
		if !found[want] {
			t.Errorf("userTables did not report %s; PeekLedger counts rows per table, so a "+
				"table it misses is one whose rows are never counted", want)
		}
	}

	if n, err := db.backend.countRows(t.Context(), db.Reader(), "admission"); err != nil {
		t.Errorf("countRows: %v", err)
	} else if n != 1 {
		t.Errorf("admission has %d rows, want the 1 its migration inserts", n)
	}
}

// A POSTGRESQL LEDGER IS NOT SNAPSHOTTED INTO A FILE, AND THE REFUSAL SAYS SO.
//
// The absence is the design. A consistent copy of a PostgreSQL database is the
// operator's own backup, and a half-measure that copied rows through this
// connection would produce an archive that LOOKS like a backup and is not.
func TestAPostgresLedgerRefusesToBeSnapshotted(t *testing.T) {
	db := openPostgres(t)

	err := db.SnapshotInto(t.Context(), fmt.Sprintf("%s/ledger.db", t.TempDir()))
	if err == nil {
		t.Fatal("SnapshotInto claimed to have copied a PostgreSQL ledger")
	}

	for _, want := range []string{"pg_dump", "external"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q so an operator knows what to run instead; "+
				"got: %v", want, err)
		}
	}
}

// postgresLockKey reads the key a handle will serialize its writers on.
//
// CHECKED RATHER THAN ASSERTED BLIND: a handle that is not the PostgreSQL
// backend would panic, and a panic in the middle of a test that is about lock
// keys reads as a crash rather than as the wrong backend being under test.
func postgresLockKey(t *testing.T, db *DB) int64 {
	t.Helper()

	be, ok := db.backend.(*postgresBackend)
	if !ok {
		t.Fatalf("this handle is on the %s backend, so it has no PostgreSQL lock key",
			db.backend.engine())
	}

	return be.lockKey
}

// A MIGRATION WAITS FOR ITS LOCKS, WHICH THE WRITER'S OWN TIMEOUT WOULD REFUSE.
//
// The writer pool's lock_timeout is fifty milliseconds so contention comes back
// to beginWrite's loop, and that loop covers the BEGIN and the advisory lock
// only. A migration's statements take locks the advisory lock says nothing about
// — DDL on the tables, reads of the bookkeeping table — so under that timeout
// the open failed fifty milliseconds behind any other session, measured in CI
// under the alloc suite's parallel schema builds. beginMigration is what lets
// this one transaction wait instead; without it this test fails at the assertion
// with SQLSTATE 55P03.
//
// THE HOLDER IS A SESSION OF ITS OWN, and the open is observed WAITING before
// the lock is released: a release that came first would be a wait nothing
// satisfied, and a test that passes whether or not the migration waited.
func TestAMigrationWaitsOutAnotherSessionsLock(t *testing.T) {
	dsn := requirePostgres(t)
	dir := t.TempDir()

	// Migrated once so the table the holder locks exists. The second open's
	// migration transaction then reads schema_migrations first, and that read
	// queues behind ACCESS EXCLUSIVE exactly as a CREATE TABLE queues behind a
	// catalogue lock.
	first, err := OpenPostgres(t.Context(), dir, dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	release := holdSchemaMigrations(t, dsn)

	opened := make(chan error, 1)

	go func() {
		db, err := OpenPostgres(t.Context(), dir, dsn)
		if err == nil {
			err = db.Close()
		}

		opened <- err
	}()

	awaitAWaiterOnSchemaMigrations(t, dsn)

	// LONGER THAN THE WRITER'S lock_timeout, MEASURED FROM THE MOMENT THE OPEN
	// WAS SEEN WAITING. Released any sooner, the fifty-millisecond timeout might
	// not have fired yet and the old code would pass this test some of the time.
	time.Sleep(250 * time.Millisecond)

	release()

	if err := <-opened; err != nil {
		t.Fatalf("the open should have waited out the held lock and succeeded; got: %v", err)
	}
}

// A MIGRATION CUT OFF AT ITS DEADLINE SAYS WHAT THE BUDGET WAS AND WHERE TO LOOK.
//
// Waiting is bounded by the context, and the bound arrives as the server
// cancelling the statement (57014), which Tx translates into the deadline. That
// left "canceling statement due to user request: context deadline exceeded" —
// true, and naming no lock, no holder and no advice. The refusal has to carry
// the migration's account, the budget and, for this engine, the catalogue view
// that names the session in the way.
func TestAMigrationCutOffAtItsDeadlineSaysWhatToLookAt(t *testing.T) {
	dsn := requirePostgres(t)
	dir := t.TempDir()

	first, err := OpenPostgres(t.Context(), dir, dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	holdSchemaMigrations(t, dsn)

	// SHORT, so the open's own thirty-second budget is not what this waits for:
	// openDir derives its deadline from the caller's and keeps the earlier one.
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	db, err := OpenPostgres(ctx, dir, dsn)
	if err == nil {
		_ = db.Close()

		t.Fatal("the open succeeded while another session held the ledger's bookkeeping " +
			"table; either the holder was not holding or the migration did not wait")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a migration stopped by its deadline must carry the deadline's identity, "+
			"so a caller can tell a stall from a fault; got: %v", err)
	}

	for _, want := range []string{"startup budget", "pg_stat_activity", "schema_migrations"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q so an operator knows what happened and "+
				"where to look; got: %v", want, err)
		}
	}
}

// holdSchemaMigrations takes ACCESS EXCLUSIVE on the bookkeeping table from a
// session of its own and returns the release. The release is also registered as
// a cleanup, after the schema's own, so the schema can still be dropped.
func holdSchemaMigrations(t *testing.T, dsn string) (release func()) {
	t.Helper()

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a holder session: %v", err)
	}

	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}

	if _, err := tx.ExecContext(t.Context(),
		`LOCK TABLE schema_migrations IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock schema_migrations: %v", err)
	}

	var once sync.Once

	release = func() {
		once.Do(func() {
			// A ROLLBACK THAT FAILED IS A LOCK STILL HELD, and the test would then
			// hang or fail somewhere that names neither; say so here.
			if err := tx.Rollback(); err != nil {
				t.Errorf("release the holder's lock on schema_migrations: %v", err)
			}

			_ = conn.Close()
		})
	}

	t.Cleanup(release)

	return release
}

// awaitAWaiterOnSchemaMigrations blocks until some session is waiting for a lock
// on this schema's bookkeeping table, which is the proof that the open under
// test is blocked by the holder rather than by nothing.
func awaitAWaiterOnSchemaMigrations(t *testing.T, dsn string) {
	t.Helper()

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open an observer session: %v", err)
	}

	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(10 * time.Second)

	for {
		var waiting int

		// to_regclass resolves through this DSN's search_path, so the count is
		// scoped to this test's schema and not to every ledger in the database.
		if err := conn.QueryRowContext(t.Context(),
			`SELECT count(*) FROM pg_locks WHERE relation = to_regclass('schema_migrations') AND NOT granted`,
		).Scan(&waiting); err != nil {
			t.Fatalf("inspect pg_locks: %v", err)
		}

		if waiting > 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("no session ever waited on schema_migrations, so the open was not " +
				"blocked by the held lock and releasing it would prove nothing")
		}

		time.Sleep(10 * time.Millisecond)
	}
}
