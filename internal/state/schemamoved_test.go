package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newestMigration is the version rewindNewest takes back off a ledger.
const newestMigration = 60

// rewindNewest takes the newest migration back off a migrated ledger, so the
// next migration has exactly one version to apply.
//
// IT KNOWS THAT MIGRATION'S STATEMENTS, the way the version tests beside it do,
// and refuses to run once a newer one exists: rewinding 60 under a 61 would
// leave a gap the timeline refuses, and the failure would be about the rewind.
func rewindNewest(t *testing.T, db *DB) {
	t.Helper()

	if latest := LatestSchemaVersion(); latest != newestMigration {
		t.Fatalf("this binary's newest migration is %d; rewindNewest takes back %d and must be "+
			"taught the newest one's statements", latest, newestMigration)
	}

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP TABLE job_guest_reports`,
			`DELETE FROM schema_migrations WHERE version = 60`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version %d: %v", newestMigration-1, err)
	}
}

// rewoundLedger is a SQLite ledger in a fresh directory, migrated and then one
// version behind this binary.
func rewoundLedger(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rewindNewest(t, db)

	if err := db.Close(); err != nil {
		t.Fatalf("close the rewound ledger: %v", err)
	}

	return dir
}

// A MIGRATION SAYS WHAT IT MOVED, AND ONLY WHAT IT MOVED. The report is what a
// caller joins to a failure after it, so a move that went unreported is a
// process that gives up claiming it changed nothing, and a move reported when
// nothing was applied sends an operator after a schema change that never
// happened.
func TestAMigrationReportsWhatItMoved(t *testing.T) {
	t.Parallel()

	dir := rewoundLedger(t)

	// A bare handle over the rewound file, so the migration this test asks for is
	// the first one anything runs against it.
	pools, err := newSQLiteBackend(dir).dataSources()
	if err != nil {
		t.Fatalf("dataSources: %v", err)
	}

	w, err := sql.Open("sqlite", pools.writer)
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	t.Cleanup(func() { _ = w.Close() })

	db := &DB{w: w, backend: newSQLiteBackend(dir)}

	moved, err := db.migrate(t.Context())
	if err != nil {
		t.Fatalf("migrate the rewound ledger: %v", err)
	}

	want := SchemaMoved{From: newestMigration - 1, To: newestMigration}
	if moved == nil || *moved != want {
		t.Fatalf("migrating a ledger one version behind reported %+v, want %+v", moved, want)
	}

	if moved, err := db.migrate(t.Context()); err != nil || moved != nil {
		t.Errorf("migrating a current ledger reported (%+v, %v), want nothing moved", moved, err)
	}
}

// AN OPEN REFUSED AFTER ITS MIGRATION SAYS WHAT IT LEFT BEHIND. The watermark is
// read after the migration, because its table arrived in one, so a binary newer
// in schema and older in release migrates the ledger and is then refused by it:
// the refusal alone would read as "nothing happened" to a ledger that moved.
func TestAnOpenRefusedAfterItsMigrationSaysWhatItMoved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	serveAs(t, dir, "v0.5.0")

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rewindNewest(t, db)

	if err := db.Close(); err != nil {
		t.Fatalf("close the rewound ledger: %v", err)
	}

	_, err = Open(t.Context(), dir, WithRunningRelease("v0.4.9"))
	if !errors.Is(err, ErrReleaseBehind) {
		t.Fatalf("an older release opened a ledger a newer one served: %v", err)
	}

	moved, ok := errors.AsType[*SchemaMoved](err)
	if !ok || moved.From != newestMigration-1 || moved.To != newestMigration || moved.Unknown {
		t.Fatalf("the refusal after a migration does not carry what it moved: %v", err)
	}

	for _, want := range []string{
		fmt.Sprintf("from schema %d to schema %d", newestMigration-1, newestMigration),
		"do not move back",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	// AND A REFUSAL WITH NOTHING MIGRATED SAYS NOTHING MOVED.
	_, err = Open(t.Context(), dir, WithRunningRelease("v0.4.9"))
	if !errors.Is(err, ErrReleaseBehind) {
		t.Fatalf("the second refused open: %v", err)
	}

	if moved, ok := errors.AsType[*SchemaMoved](err); ok {
		t.Errorf("a refused open that migrated nothing reported %+v", moved)
	}
}

// errReplyLost is a COMMIT the database carried out and never confirmed.
var errReplyLost = errors.New("state test: the connection closed before the COMMIT was answered")

// A COMMIT NOBODY ANSWERED IS COULD-NOT-TELL, NEVER "NOTHING MOVED". The driver
// below commits for real and then reports a failure, which is what a reply lost
// on the way back looks like from here; the ledger did move, and a report that
// said otherwise would be read as permission to run the older binary.
func TestAMigrationWhoseCommitWasNotAnsweredIsReportedAsUnknown(t *testing.T) {
	t.Parallel()

	dir := rewoundLedger(t)

	pools, err := newSQLiteBackend(dir).dataSources()
	if err != nil {
		t.Fatalf("dataSources: %v", err)
	}

	probe, err := sql.Open("sqlite", pools.writer)
	if err != nil {
		t.Fatalf("open the driver: %v", err)
	}

	drv := probe.Driver()

	if err := probe.Close(); err != nil {
		t.Fatalf("close the probe: %v", err)
	}

	w := sql.OpenDB(lostReplyConnector{drv: drv, dsn: pools.writer})

	t.Cleanup(func() { _ = w.Close() })

	db := &DB{w: w, backend: newSQLiteBackend(dir)}

	moved, err := db.migrate(t.Context())
	if !errors.Is(err, errReplyLost) {
		t.Fatalf("a commit whose reply was lost reported %v", err)
	}

	want := SchemaMoved{From: newestMigration - 1, To: newestMigration, Unknown: true}
	if moved == nil || *moved != want {
		t.Fatalf("a commit whose reply was lost reported the move as %+v, want %+v", moved, want)
	}

	if !strings.Contains(moved.Error(), "could not tell") {
		t.Errorf("an unknown move does not say it could not tell: %v", moved)
	}

	// THE LEDGER DID MOVE, which is why Unknown can never be read as "no".
	reopened, err := OpenAdmin(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenAdmin over the migrated ledger: %v", err)
	}

	t.Cleanup(func() { _ = reopened.Close() })

	if moved, err := reopened.migrate(t.Context()); err != nil || moved != nil {
		t.Errorf("the ledger whose commit went unanswered was not at the newest schema: (%+v, %v)",
			moved, err)
	}
}

// lostReplyConnector hands out real SQLite connections whose COMMIT succeeds
// and then reports errReplyLost.
type lostReplyConnector struct {
	drv driver.Driver
	dsn string
}

func (c lostReplyConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}

	return lostReplyConn{Conn: conn}, nil
}

func (c lostReplyConnector) Driver() driver.Driver { return c.drv }

type lostReplyConn struct{ driver.Conn }

func (c lostReplyConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	begin, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("state test: the SQLite connection has no BeginTx")
	}

	tx, err := begin.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}

	return lostReplyTx{Tx: tx}, nil
}

type lostReplyTx struct{ driver.Tx }

func (t lostReplyTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}

	return errReplyLost
}

// A PROMOTION REFUSED AFTER ITS MIGRATION SAYS WHAT IT MOVED (#148). The
// migration commits on its own before the claim's transaction, so a standby
// refused there gives back the exclusion and the latch and leaves the ledger on
// the newer schema; the refusal has to say so, and the latch and the exclusion
// still have to come back.
func TestAPromotionRefusedAfterItsMigrationSaysWhatItMoved(t *testing.T) {
	dsn := requirePostgres(t)

	leader, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := leader.ClaimController(t.Context(), "leader", testDeployment); err != nil {
		t.Fatalf("the leader could not claim: %v", err)
	}

	// THE LEADER IS AN OLDER BINARY, one migration behind the standby: the
	// follower-first shape, where the promotion is what migrates.
	rewindNewest(t, leader)

	if err := leader.Close(); err != nil {
		t.Fatalf("closing the leader: %v", err)
	}

	standby := openStandby(t, dsn)

	_, err = standby.ClaimController(t.Context(), "standby", "a-different-deployment")
	if !errors.Is(err, ErrForeignLedger) {
		t.Fatalf("expected ErrForeignLedger past the migration; got %v", err)
	}

	moved, ok := errors.AsType[*SchemaMoved](err)
	if !ok || moved.From != newestMigration-1 || moved.To != newestMigration || moved.Unknown {
		t.Fatalf("a promotion refused after migrating does not say what it moved: %v", err)
	}

	if err := standby.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run for a standby whose claim did not complete")

		return nil
	}); !errors.Is(err, ErrStandby) {
		t.Fatalf("a standby refused after migrating could write; got %v", err)
	}

	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("a controller could not start after a refused promotion released nothing: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })
}

// THE STOP IS THE CONTEXT'S OWN REASON AND NOTHING ELSE. Each case is a way a
// control plane's startup has ended, and the answer is the exit status it earns.
func TestStoppedByIsTheContextsOwnReasonAndNothingElse(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()

	stop := fmt.Errorf("server: reclaim abandoned capacity: begin write tx: %w", context.Canceled)
	fault := errors.New("close the claim's connection: broken pipe")

	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"the stop", cancelled, stop, true},
		{"a stop with a cleanup that succeeded", cancelled, errors.Join(stop, nil), true},
		{"nothing failed", cancelled, nil, false},
		{"a live process's inner cancellation", t.Context(), stop, false},
		{"a fault joined to the stop", cancelled, errors.Join(stop, fault), false},
		{"a schema moved before the stop", cancelled, errors.Join(stop, &SchemaMoved{From: 59, To: 60}), false},
		{"an inner budget under a stop", cancelled, fmt.Errorf("migrate: %w", context.DeadlineExceeded), false},
		{"the caller's own deadline", expired, fmt.Errorf("listener: %w", context.DeadlineExceeded), true},
		{"a cancellation under the caller's deadline", expired, stop, false},
	} {
		if got := StoppedBy(c.ctx, c.err); got != c.want {
			t.Errorf("%s: StoppedBy = %v, want %v (err %v)", c.name, got, c.want, c.err)
		}
	}
}
