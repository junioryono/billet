package state

import (
	"context"
	"database/sql"
	"slices"
	"testing"

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

	// THE FIRST CLOSE ON AN EMPTY LEDGER IS 1, which the counter's seed decides.
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

	lease, err := db.UsageLease(ctx, 4)
	if err != nil || lease != "running" {
		t.Fatalf("UsageLease(4) = %q, %v; a second archive moved the lease's number", lease, err)
	}
}
