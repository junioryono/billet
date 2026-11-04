package state

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// THE BACKFILL NUMBERS WHAT HAD ALREADY FINISHED, on both engines: oldest first,
// the lease breaking a tie of the clock, what has not finished left at zero, and
// the next number following on from the last. A deployment that upgrades exports
// its history, and its next lease cannot collide with it.
func TestTheUsageMigrationNumbersWhatHadAlreadyFinished(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		open := func(ctx context.Context) (*DB, error) { return Open(ctx, dir) }
		exerciseUsageBackfill(t, open)
	})

	t.Run("postgres", func(t *testing.T) {
		dsn := requirePostgres(t)
		dir := t.TempDir()
		open := func(ctx context.Context) (*DB, error) { return OpenPostgres(ctx, dir, dsn) }
		exerciseUsageBackfill(t, open)
	})
}

func exerciseUsageBackfill(t *testing.T, open func(context.Context) (*DB, error)) {
	t.Helper()

	ctx := t.Context()

	db, err := open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP INDEX job_history_usage_seq`,
			`DROP TABLE usage_sequence`,
			`ALTER TABLE job_history DROP COLUMN usage_seq`,
			`ALTER TABLE job_history DROP COLUMN github_target`,
			`DELETE FROM schema_migrations WHERE version = 71`,
			`INSERT INTO job_history (lease_id, tier, queued_at, finished_at) VALUES
			   ('latest', 'small', 't', '2026-01-03T00:00:00.000000000Z'),
			   ('tied-b', 'small', 't', '2026-01-01T00:00:00.000000000Z'),
			   ('tied-a', 'small', 't', '2026-01-01T00:00:00.000000000Z'),
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

	var leases []string
	for _, r := range rows {
		leases = append(leases, r.LeaseID)
	}

	if want := []string{"tied-a", "tied-b", "latest"}; !slices.Equal(leases, want) {
		t.Fatalf("the backfill exports %v, want %v", leases, want)
	}

	for i, r := range rows {
		if r.Sequence != int64(i+1) {
			t.Errorf("%s is at %d, want %d", r.LeaseID, r.Sequence, i+1)
		}
	}

	var next int64

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		var err error

		next, err = WriteQueries(tx).NextUsageSeq(ctx)

		return err
	}); err != nil {
		t.Fatalf("NextUsageSeq: %v", err)
	}

	if next != 4 {
		t.Fatalf("the next lease to finish takes %d, want 4: it follows the backfill or it collides", next)
	}

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		return WriteQueries(tx).ArchiveJobHistory(ctx, ledgerdb.ArchiveJobHistoryParams{
			LeaseID: "running", Tier: "small", QueuedAt: "t", UsageSeq: 4,
			FinishedAt: sql.NullString{String: "2026-01-04T00:00:00.000000000Z", Valid: true},
		})
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	rows, err = db.UsageSince(ctx, 3, 10)
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(rows) != 1 || rows[0].LeaseID != "running" || rows[0].Sequence != 4 {
		t.Fatalf("past the backfill the export is %+v, want the row that just closed", rows)
	}

	// A SECOND ARCHIVE KEEPS THE FIRST NUMBER: a lease has one place in the order,
	// and an exporter may already have read past it.
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		return WriteQueries(tx).ArchiveJobHistory(ctx, ledgerdb.ArchiveJobHistoryParams{
			LeaseID: "running", Tier: "small", QueuedAt: "t", UsageSeq: 99,
			FinishedAt: sql.NullString{String: "2026-01-05T00:00:00.000000000Z", Valid: true},
		})
	}); err != nil {
		t.Fatalf("archive again: %v", err)
	}

	closed, err := db.UsageRecordAt(ctx, 4)
	if err != nil || closed.LeaseID != "running" {
		t.Fatalf("UsageRecordAt(4) = %+v, %v; a second archive moved the lease's number", closed, err)
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

// THE COUNTER'S OWN LOCK ORDERS TWO TAKERS, with nothing else serializing them: a
// second transaction asking for a number waits until the first has committed, sees
// what it committed, and a transaction rolled back gives its number back. A
// PostgreSQL sequence, which hands numbers out beside the transaction, fails each.
func TestTheUsageCounterHoldsASecondTakerUntilTheFirstCommits(t *testing.T) {
	t.Parallel()

	dsn := requirePostgres(t)

	db, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// RAW CONNECTIONS, because a DB.Tx takes the writers' advisory lock first, and
	// that is what this leaves out.
	raw, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	const take = `INSERT INTO usage_sequence (id, last) VALUES (1, 1)
		ON CONFLICT (id) DO UPDATE SET last = usage_sequence.last + 1 RETURNING last`

	begin := func() *sql.Tx {
		t.Helper()

		tx, err := raw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })

		return tx
	}

	number := func(tx *sql.Tx) int64 {
		t.Helper()

		var n int64
		if err := tx.QueryRowContext(ctx, take).Scan(&n); err != nil {
			t.Fatalf("take a number: %v", err)
		}

		return n
	}

	abandoned := begin()
	if got := number(abandoned); got != 1 {
		t.Fatalf("the first number is %d, want 1", got)
	}
	if err := abandoned.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	first := begin()
	if got := number(first); got != 1 {
		t.Fatalf("after a rollback the number is %d, want the 1 it gave back", got)
	}

	second := begin()

	type taken struct {
		n   int64
		err error
	}

	answer := make(chan taken, 1)

	go func() {
		var n int64

		err := second.QueryRowContext(ctx, take).Scan(&n)
		answer <- taken{n, err}
	}()

	// HELD: the server says the second taker is waiting on a lock, which is the
	// fact itself rather than a wait long enough to have missed an answer.
	deadline := time.Now().Add(30 * time.Second)

	for {
		var waiting int
		if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE query LIKE 'INSERT INTO usage_sequence%' AND wait_event_type = 'Lock'
			  AND datname = current_database()`).Scan(&waiting); err != nil {
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
	if got.err != nil || got.n != 2 {
		t.Fatalf("the second taker answered %d, %v; want 2, once the first committed", got.n, got.err)
	}
}
