package state

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// WHAT HAD ALREADY FINISHED WAITS FOR A NUMBER, on both engines, and a migration does
// not rewrite it. A ledger past the startup budget would otherwise fail that migration
// on every restart and never start.
func TestTheUsageMigrationLeavesFinishedLeasesWaiting(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		exerciseUsageMigration(t, func(ctx context.Context) (*DB, error) { return Open(ctx, dir) })
	})

	t.Run("postgres", func(t *testing.T) {
		dsn := requirePostgres(t)
		dir := t.TempDir()
		exerciseUsageMigration(t, func(ctx context.Context) (*DB, error) { return OpenPostgres(ctx, dir, dsn) })
	})
}

func exerciseUsageMigration(t *testing.T, open func(context.Context) (*DB, error)) {
	t.Helper()

	ctx := t.Context()

	db, err := open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP INDEX job_history_usage_seq`,
			`DROP INDEX job_history_usage_unnumbered`,
			`DROP TABLE usage_sequence`,
			`ALTER TABLE job_history DROP COLUMN usage_seq`,
			`ALTER TABLE job_history DROP COLUMN usage_mark`,
			`ALTER TABLE job_history DROP COLUMN github_finished_at`,
			`ALTER TABLE pending_completions DROP COLUMN job_runner_assigned_at`,
			`ALTER TABLE pending_completions DROP COLUMN job_finished_at`,
			`ALTER TABLE job_history DROP COLUMN github_target`,
			`DELETE FROM schema_migrations WHERE version = 71`,
			`INSERT INTO job_history (lease_id, tier, queued_at, finished_at) VALUES
			   ('latest', 'small', 't', '2026-01-03T00:00:00.000000000Z'),
			   ('running', 'small', 't', NULL),
			   ('blank', 'small', 't', '')`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("wind the ledger back: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err = open(ctx)
	if err != nil {
		t.Fatalf("reopen, applying the migration against those rows: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.UsageSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the migration numbered %d rows: %+v", len(rows), rows)
	}

	waiting, err := db.UsageStillNumbering(ctx)
	if err != nil || !waiting {
		t.Fatalf("UsageStillNumbering = %v, %v; a finished lease is waiting", waiting, err)
	}

	// ONLY THE FINISHED ONE: a running lease numbered now would be exported with no end.
	var listed []string

	if err := db.View(ctx, func(q Querier) error {
		var err error

		listed, err = ReadQueries(q).ListUnnumberedUsage(ctx, 10)

		return err
	}); err != nil {
		t.Fatalf("ListUnnumberedUsage: %v", err)
	}

	if len(listed) != 1 || listed[0] != "latest" {
		t.Fatalf("waiting for a number: %v, want only the finished lease", listed)
	}

	var first int64

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		var err error

		first, err = WriteQueries(tx).NextUsageSeq(ctx)

		return err
	}); err != nil {
		t.Fatalf("NextUsageSeq: %v", err)
	}

	if first != 1 {
		t.Fatalf("the first number is %d, want 1", first)
	}

	// A SECOND ARCHIVE KEEPS THE FIRST NUMBER: a lease has one place in the order, and
	// an exporter may already have read past it.
	for _, seq := range []int64{1, 2} {
		if err := db.Tx(ctx, func(tx *sql.Tx) error {
			return WriteQueries(tx).ArchiveJobHistory(ctx, ledgerdb.ArchiveJobHistoryParams{
				LeaseID: "running", Tier: "small", QueuedAt: "t", UsageSeq: seq, UsageMark: fmt.Sprintf("mark-%d", seq),
				FinishedAt: sql.NullString{String: "2026-01-04T00:00:00.000000000Z", Valid: true},
			})
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
	}

	closed, err := db.UsageRecordAt(ctx, 1)
	if err != nil || closed.LeaseID != "running" || closed.Mark != "mark-1" {
		t.Fatalf("UsageRecordAt(1) = %+v, %v; a second archive moved the lease's number or its mark", closed, err)
	}
}

// A LEDGER WITH NO HISTORY HOLDS NO COUNTER ROW, because a row in any table is what
// tells a used ledger from a pristine one; and its first close is number 1.
func TestTheUsageCounterWaitsForTheFirstClose(t *testing.T) {
	t.Parallel()

	db := open(t)
	ctx := t.Context()

	var rows int
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_sequence`).Scan(&rows); err != nil {
		t.Fatalf("count the counter's rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a freshly migrated ledger holds %d counter rows, so it reads as used", rows)
	}

	for want := int64(1); want <= 2; want++ {
		var got int64

		if err := db.Tx(ctx, func(tx *sql.Tx) error {
			var err error

			got, err = WriteQueries(tx).NextUsageSeq(ctx)

			return err
		}); err != nil {
			t.Fatalf("NextUsageSeq: %v", err)
		}

		if got != want {
			t.Fatalf("NextUsageSeq = %d, want %d", got, want)
		}
	}
}

// THE COUNTER'S OWN LOCK ORDERS TWO TAKERS, with nothing else serializing them, and a
// run of numbers holds it as one number does: a second transaction asking waits until
// the first has committed and sees what it committed, and a transaction rolled back
// gives its numbers back. A PostgreSQL sequence, which hands numbers out beside the
// transaction, fails each.
func TestTheUsageCounterHoldsASecondTaker(t *testing.T) {
	t.Parallel()

	one := func(ctx context.Context, tx *sql.Tx) (int64, error) { return WriteQueries(tx).NextUsageSeq(ctx) }
	run := func(ctx context.Context, tx *sql.Tx) (int64, error) { return WriteQueries(tx).TakeUsageSeqs(ctx, 500) }

	for name, tc := range map[string]struct {
		first, second         func(context.Context, *sql.Tx) (int64, error)
		wantFirst, wantSecond int64
	}{
		"oneone": {one, one, 1, 2},
		"runone": {run, one, 500, 501},
		"onerun": {one, run, 1, 501},
		"runrun": {run, run, 500, 1000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exerciseUsageCounterLock(t, name, tc.first, tc.second, tc.wantFirst, tc.wantSecond)
		})
	}
}

func exerciseUsageCounterLock(
	t *testing.T, name string, takeFirst, takeSecond func(context.Context, *sql.Tx) (int64, error),
	wantFirst, wantSecond int64,
) {
	t.Helper()

	dsn := requirePostgres(t)

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// RAW CONNECTIONS, because a DB.Tx takes the writers' advisory lock first, and
	// that is what this leaves out. The statements are the generated ones.
	raw, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	begin := func() *sql.Tx {
		t.Helper()

		tx, err := raw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })

		return tx
	}

	abandoned := begin()
	if got, err := takeFirst(ctx, abandoned); err != nil || got != wantFirst {
		t.Fatalf("the first take answered %d, %v; want %d", got, err, wantFirst)
	}
	if err := abandoned.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	first := begin()
	if got, err := takeFirst(ctx, first); err != nil || got != wantFirst {
		t.Fatalf("after a rollback the take answered %d, %v; want the %d it gave back", got, err, wantFirst)
	}

	second := begin()

	// NAMED, because the suite's schemas share one database: another subtest's waiter would
	// otherwise satisfy this one's wait.
	waiter := "usage-counter-" + name
	if _, err := second.ExecContext(ctx, "SET LOCAL application_name = '"+waiter+"'"); err != nil {
		t.Fatalf("name the second taker: %v", err)
	}

	type taken struct {
		n   int64
		err error
	}

	answer := make(chan taken, 1)

	go func() {
		n, err := takeSecond(ctx, second)
		answer <- taken{n, err}
	}()

	// HELD: the server says the second taker is waiting on a lock, which is the
	// fact itself rather than a wait long enough to have missed an answer.
	deadline := time.Now().Add(30 * time.Second)

	for {
		var waiting int
		if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock'
			  AND datname = current_database()`, waiter).Scan(&waiting); err != nil {
			t.Fatalf("read the server's locks: %v", err)
		}

		if waiting > 0 {
			break
		}

		select {
		case got := <-answer:
			t.Fatalf("the second taker answered %d, %v while the first had not committed", got.n, got.err)
		default:
		}

		if time.Now().After(deadline) {
			t.Fatal("the second taker neither answered nor waited on a lock within 30s")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if err := first.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got := <-answer
	if got.err != nil || got.n != wantSecond {
		t.Fatalf("the second taker answered %d, %v; want %d, once the first committed", got.n, got.err, wantSecond)
	}
}
