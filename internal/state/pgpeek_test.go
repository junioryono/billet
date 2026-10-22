package state

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// pgDeployment is the deployment these tests bind a PostgreSQL ledger to.
const pgDeployment = "abababababababababababababababab"

// schemaFrom is the schema a test DSN's search_path names, which
// requirePostgresSchema lowercases.
func schemaFrom(t *testing.T, name string) string {
	t.Helper()

	s := strings.ToLower("billet_test_" + strings.ReplaceAll(name, "/", "_"))
	if len(s) > 60 {
		s = s[:60]
	}

	return s
}

// AN EMPTY SCHEMA IS SAID TO BE EMPTY, positively: the schema it resolved to,
// the server that answered, and no tables. A restore installs a dump only on
// that answer, so "could not tell" must never produce it.
func TestAPeekOfAnEmptySchemaFindsNothingThere(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)

	led, err := PeekPostgresLedger(t.Context(), dsn)
	if err != nil {
		t.Fatalf("PeekPostgresLedger: %v", err)
	}

	if led.Schema != schemaFrom(t, t.Name()) {
		t.Errorf("Schema = %q, want %q", led.Schema, schemaFrom(t, t.Name()))
	}

	if led.ServerVersion < 100000 {
		t.Errorf("ServerVersion = %d, want a server_version_num", led.ServerVersion)
	}

	if len(led.Tables) != 0 || led.Populated || len(led.Migrations) != 0 || led.Binding != "" {
		t.Errorf("an empty schema reads as %+v", led)
	}
}

// A MIGRATED LEDGER NOBODY HAS USED IS PRISTINE, and one a controller claimed
// and an operator sealed is populated and bound: the two answers a restore
// decides between installing a dump and keeping the database's own rows.
func TestAPeekTellsAPristineLedgerFromABoundOne(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	defer func() { _ = db.Close() }()

	pristine, err := PeekPostgresLedger(t.Context(), dsn)
	if err != nil {
		t.Fatalf("PeekPostgresLedger: %v", err)
	}

	if len(pristine.Tables) == 0 || pristine.Populated || pristine.Binding != "" {
		t.Fatalf("a freshly migrated ledger reads as %+v", pristine)
	}

	if !slices.Equal(pristine.Migrations, knownMigrationsFor(t, db)) {
		t.Errorf("the peek reports %d migrations and the ledger applied %d",
			len(pristine.Migrations), len(knownMigrationsFor(t, db)))
	}

	if _, err := db.ClaimController(t.Context(), "host-a", pgDeployment); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	bound, err := PeekPostgresLedger(t.Context(), dsn)
	if err != nil {
		t.Fatalf("PeekPostgresLedger: %v", err)
	}

	if !bound.Populated || bound.Binding != pgDeployment {
		t.Errorf("a claimed ledger reads as populated=%v binding=%q, want populated and bound to %s",
			bound.Populated, bound.Binding, pgDeployment)
	}
}

// knownMigrationsFor is what an open handle's ledger has applied.
func knownMigrationsFor(t *testing.T, db *DB) []AppliedMigration {
	t.Helper()

	applied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}

	return applied
}

// THE SNAPSHOT IS EXPORTED WITH THE MIGRATIONS AND THE BINDING READ IN IT, and
// it honours the fence of the directory it is told to.
func TestASnapshotCarriesWhatTheLedgerHeldAndHonoursTheFence(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)
	dir := t.TempDir()

	db, err := OpenPostgres(t.Context(), dir, dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	defer func() { _ = db.Close() }()

	if _, err := db.ClaimController(t.Context(), "host-a", pgDeployment); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	var snap LedgerSnapshot

	if err := ExportPostgresSnapshot(t.Context(), dsn, dir, func(s LedgerSnapshot) error {
		snap = s

		return nil
	}); err != nil {
		t.Fatalf("ExportPostgresSnapshot: %v", err)
	}

	if snap.ID == "" || snap.Schema != schemaFrom(t, t.Name()) || snap.Binding != pgDeployment ||
		!slices.Equal(snap.Migrations, knownMigrationsFor(t, db)) {
		t.Errorf("the snapshot is %+v", snap)
	}

	if _, err := WriteMaintenanceFence(dir, "billet local restore"); err != nil {
		t.Fatalf("WriteMaintenanceFence: %v", err)
	}

	called := false

	err = ExportPostgresSnapshot(t.Context(), dsn, dir, func(LedgerSnapshot) error {
		called = true

		return nil
	})
	if !errors.Is(err, ErrMaintenance) || called {
		t.Errorf("a snapshot behind a fence: err = %v, called = %v; want ErrMaintenance and no call",
			err, called)
	}

	// AND THE ONE CALLER THAT HOLDS THE FENCE crosses it by naming no directory.
	if err := ExportPostgresSnapshot(t.Context(), dsn, "", func(LedgerSnapshot) error {
		return nil
	}); err != nil {
		t.Errorf("a snapshot naming no fence: %v", err)
	}
}

// THE BARRIER WAITS OUT A WRITE IN FLIGHT AND THEN SAYS IT COULD NOT PROVE
// QUIET, rather than proving it: the writer's advisory lock held by another
// session is what every billet write transaction holds from its first
// statement. Released, the barrier passes.
func TestTheWriterBarrierWaitsOutAWriteInFlight(t *testing.T) {
	dsn := requirePostgres(t)

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	defer func() { _ = db.Close() }()

	prev := writerBarrierLimit
	writerBarrierLimit = 300 * time.Millisecond

	t.Cleanup(func() { writerBarrierLimit = prev })

	holder, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open a second session: %v", err)
	}

	defer func() { _ = holder.Close() }()

	key, err := readLockKey(t.Context(), holder)
	if err != nil {
		t.Fatalf("readLockKey: %v", err)
	}

	tx, err := holder.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if _, err := tx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock($1, $2)`,
		advisoryClassWriter, key); err != nil {
		t.Fatalf("hold the writer lock: %v", err)
	}

	err = PostgresWriterBarrier(t.Context(), dsn)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("the barrier with a write in flight = %v, want the refusal that names it", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// THE PRODUCTION BOUND FOR THE PASS, so a loaded machine slow to take a free
	// lock is not read as a writer still in flight.
	writerBarrierLimit = prev

	if err := PostgresWriterBarrier(t.Context(), dsn); err != nil {
		t.Fatalf("the barrier with nothing in flight: %v", err)
	}
}

// THE RECOVERY OPEN CROSSES THE FENCE AND WRITES, and the operator open beside
// it does not: a recovery's seal is the one write behind its own fence.
func TestARecoveryOpenCrossesTheFenceThatStopsAnOperatorOpen(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)
	dir := t.TempDir()

	seed, err := OpenPostgres(t.Context(), dir, dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := seed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := WriteMaintenanceFence(dir, "billet local recover"); err != nil {
		t.Fatalf("WriteMaintenanceFence: %v", err)
	}

	if db, err := OpenPostgresAdmin(t.Context(), dir, dsn); !errors.Is(err, ErrMaintenance) {
		if db != nil {
			_ = db.Close()
		}

		t.Fatalf("an operator open behind the fence = %v, want ErrMaintenance", err)
	}

	db, err := OpenPostgresRecovery(t.Context(), dir, dsn)
	if err != nil {
		t.Fatalf("OpenPostgresRecovery: %v", err)
	}

	defer func() { _ = db.Close() }()

	if _, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceOperator, Reason: "recovered", Actor: "test",
	}); err != nil {
		t.Fatalf("Seal through the recovery open: %v", err)
	}

	a, err := PeekPostgresAdmission(t.Context(), dsn)
	if err != nil {
		t.Fatalf("PeekPostgresAdmission: %v", err)
	}

	if a.Mode != AdmissionSealed || a.Provenance != ProvenanceOperator {
		t.Errorf("the admission read back is %+v, want an operator's seal", a)
	}
}

// A RESTORE'S CLEANUP DROPS EXACTLY THE TABLES THE DUMP DOES NOT NAME, in the
// schema the connection resolves to and nowhere else, and refuses a schema it
// was not told: a manifest whose schema disagrees with the connection would
// otherwise empty one schema and restore into another.
func TestDropTablesOutsideDropsOnlyWhatTheDumpDoesNotName(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)
	schema := schemaFrom(t, t.Name())

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	defer func() { _ = db.Close() }()

	raw, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = raw.Close() }()

	if _, err := raw.ExecContext(t.Context(), `CREATE TABLE "`+schema+`".a_later_migration (x int)`); err != nil {
		t.Fatalf("create a later migration's table: %v", err)
	}

	before, err := PeekPostgresLedger(t.Context(), dsn)
	if err != nil {
		t.Fatalf("PeekPostgresLedger: %v", err)
	}

	keep := slices.DeleteFunc(slices.Clone(before.Tables), func(s string) bool { return s == "a_later_migration" })
	into := RestoreInto{Database: before.Database, Schema: schema}

	// A TARGET THE CONNECTION DOES NOT REACH IS REFUSED BEFORE ANYTHING IS
	// DROPPED, on the drop's own connection: another schema, another database,
	// or a ledger bound otherwise than the caller proved.
	for name, wrong := range map[string]RestoreInto{
		"another schema":   {Database: into.Database, Schema: schema + "_other"},
		"another database": {Database: into.Database + "_other", Schema: schema},
		"another binding":  {Database: into.Database, Schema: schema, Binding: pgDeployment},
	} {
		if _, err := DropTablesOutside(t.Context(), dsn, wrong, keep); err == nil ||
			!strings.Contains(err.Error(), "drops nothing") {
			t.Fatalf("DropTablesOutside into %s = %v, want a refusal", name, err)
		}
	}

	if still, err := PeekPostgresLedger(t.Context(), dsn); err != nil || !slices.Equal(still.Tables, before.Tables) {
		t.Fatalf("a refused cleanup changed the schema: %v (%v)", still.Tables, err)
	}

	dropped, err := DropTablesOutside(t.Context(), dsn, into, keep)
	if err != nil {
		t.Fatalf("DropTablesOutside: %v", err)
	}

	if !slices.Equal(dropped, []string{"a_later_migration"}) {
		t.Errorf("dropped %v, want only the table the dump does not name", dropped)
	}

	after, err := PeekPostgresLedger(t.Context(), dsn)
	if err != nil {
		t.Fatalf("PeekPostgresLedger: %v", err)
	}

	if !slices.Equal(after.Tables, keep) {
		t.Errorf("after the cleanup the schema holds %v, want %v", after.Tables, keep)
	}
}

// NOTHING OUTSIDE THE DROPPED TABLES GOES WITH THEM: a view in another schema
// that depends on a table the dump does not name refuses the whole drop, and so
// does a partitioned table, whose partitions a drop takes wherever they are.
// The copies a caller holds are of this schema alone and could not bring either
// back. A table in another schema that depends on nothing stays as it was.
func TestDropTablesOutsideTakesNothingOutsideThem(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)
	schema := schemaFrom(t, t.Name())
	elsewhere := schema + "_elsewhere"

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	defer func() { _ = db.Close() }()

	raw, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = raw.Close() }()

	run := func(stmt string) {
		t.Helper()

		if _, err := raw.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// ITS OWN CONNECTION, because raw is closed by the time a cleanup runs, and
	// a schema left from a run that died is cleared first.
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", string(dsn))
		if err != nil {
			return
		}

		defer func() { _ = cleanup.Close() }()

		//nolint:errcheck,noctx // teardown, after the test's context is gone.
		_, _ = cleanup.Exec(`DROP SCHEMA IF EXISTS "` + elsewhere + `" CASCADE`)
	})

	run(`DROP SCHEMA IF EXISTS "` + elsewhere + `" CASCADE`)
	run(`CREATE SCHEMA "` + elsewhere + `"`)
	run(`CREATE TABLE "` + elsewhere + `".neighbour (x int)`)
	run(`CREATE TABLE "` + schema + `".a_later_migration (x int)`)
	run(`CREATE VIEW "` + elsewhere + `".report AS SELECT x FROM "` + schema + `".a_later_migration`)

	before, err := PeekPostgresLedger(t.Context(), dsn)
	if err != nil {
		t.Fatalf("PeekPostgresLedger: %v", err)
	}

	keep := slices.DeleteFunc(slices.Clone(before.Tables), func(s string) bool { return s == "a_later_migration" })
	into := RestoreInto{Database: before.Database, Schema: schema}

	// REFUSED BY THE SERVER'S OWN DEPENDENCY CHECK, which is what RESTRICT asks
	// for; a CASCADE would have taken the view.
	if _, err := DropTablesOutside(t.Context(), dsn, into, keep); err == nil ||
		!strings.Contains(err.Error(), "other objects depend on it") {
		t.Fatalf("DropTablesOutside with a view elsewhere depending on it = %v, want a refusal", err)
	}

	if still, err := PeekPostgresLedger(t.Context(), dsn); err != nil || !slices.Equal(still.Tables, before.Tables) {
		t.Fatalf("a refused drop changed the schema: %v (%v)", still.Tables, err)
	}

	run(`SELECT x FROM "` + elsewhere + `".report`)

	// PARTITIONED, with no dependent at all: still refused.
	run(`DROP VIEW "` + elsewhere + `".report`)
	run(`DROP TABLE "` + schema + `".a_later_migration`)
	run(`CREATE TABLE "` + schema + `".a_later_migration (x int) PARTITION BY RANGE (x)`)

	// THE PEEK A PLAN READS REPORTS IT, so a recovery is refused before it
	// seals, fences or copies anything.
	if led, err := PeekPostgresLedger(t.Context(), dsn); err != nil ||
		!slices.Equal(led.Hierarchy, []string{"a_later_migration"}) {
		t.Fatalf("PeekPostgresLedger reports hierarchy %v (%v), want [a_later_migration]", led.Hierarchy, err)
	}

	if _, err := DropTablesOutside(t.Context(), dsn, into, keep); err == nil ||
		!strings.Contains(err.Error(), "partitioned, a partition or an inheriting") {
		t.Fatalf("DropTablesOutside over a partitioned table = %v, want a refusal", err)
	}

	// AN INHERITING CHILD OF A PARENT ELSEWHERE, with nothing depending on it:
	// still refused, because its rows are the parent's too.
	run(`DROP TABLE "` + schema + `".a_later_migration`)
	run(`CREATE TABLE "` + elsewhere + `".parent (x int)`)
	run(`CREATE TABLE "` + schema + `".a_later_migration (x int) INHERITS ("` + elsewhere + `".parent)`)

	if _, err := DropTablesOutside(t.Context(), dsn, into, keep); err == nil ||
		!strings.Contains(err.Error(), "partitioned, a partition or an inheriting") {
		t.Fatalf("DropTablesOutside over an inheriting table = %v, want a refusal", err)
	}

	// AND KEPT BY NAME, still refused: pg_restore --clean drops a kept table
	// itself, which takes its rows out of the parent elsewhere just the same.
	if _, err := DropTablesOutside(t.Context(), dsn, into, append(slices.Clone(keep), "a_later_migration")); err == nil ||
		!strings.Contains(err.Error(), "partitioned, a partition or an inheriting") {
		t.Fatalf("DropTablesOutside keeping an inheriting table = %v, want a refusal", err)
	}

	// A TABLE WITH WHITESPACE IN ITS NAME, which a dump's list could not name.
	run(`DROP TABLE "` + schema + `".a_later_migration`)
	run(`CREATE TABLE "` + schema + `"."a later migration" (x int)`)

	if _, err := DropTablesOutside(t.Context(), dsn, into, keep); err == nil ||
		!strings.Contains(err.Error(), "whitespace in its name") {
		t.Fatalf("DropTablesOutside over a table with a space in its name = %v, want a refusal", err)
	}

	// AN ORDINARY TABLE WITH NOTHING DEPENDING ON IT GOES, and the neighbour
	// elsewhere stays.
	run(`DROP TABLE "` + schema + `"."a later migration"`)
	run(`CREATE TABLE "` + schema + `".a_later_migration (x int)`)

	dropped, err := DropTablesOutside(t.Context(), dsn, into, keep)
	if err != nil {
		t.Fatalf("DropTablesOutside: %v", err)
	}

	if !slices.Equal(dropped, []string{"a_later_migration"}) {
		t.Errorf("dropped %v, want only the table the dump does not name", dropped)
	}

	run(`SELECT x FROM "` + elsewhere + `".neighbour`)
}

// A SNAPSHOT OF A LEDGER SCHEMA HOLDING A TABLE WITH WHITESPACE IN ITS NAME IS
// REFUSED AT BACKUP, where the operator can move it, rather than discovered by a
// restore that cannot read the dump.
func TestASnapshotRefusesATableItsDumpCouldNotName(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)
	schema := schemaFrom(t, t.Name())

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	defer func() { _ = db.Close() }()

	raw, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = raw.Close() }()

	if _, err := raw.ExecContext(t.Context(), `CREATE TABLE "`+schema+`"."operator notes" (x int)`); err != nil {
		t.Fatalf("create the table: %v", err)
	}

	called := false

	err = ExportPostgresSnapshot(t.Context(), dsn, "", func(LedgerSnapshot) error {
		called = true

		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "whitespace in its name") {
		t.Fatalf("ExportPostgresSnapshot = %v, want a refusal naming the table", err)
	}

	if called {
		t.Error("the snapshot was handed to its caller although the dump could not be restored")
	}
}
