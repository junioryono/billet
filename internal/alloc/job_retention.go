package alloc

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RetiredJobDetail is what one batch of RetireJobDetail deleted.
type RetiredJobDetail struct {
	// Jobs is how many jobs the batch marked retired; fewer than the batch's
	// limit means none older is left.
	Jobs         int
	Series       int
	Destinations int
	GuestReports int
}

// RetireJobDetail deletes the series, destinations and guest report of at most
// limit jobs whose detail was recorded before before, oldest first, and marks
// each job's history row retired, in one transaction. A job's usage summary is
// kept.
//
// BY recorded_at, which usage and guest reports write as RFC 3339 with its
// trailing zeros trimmed: compared as text, two times in different seconds
// order correctly, which is the resolution a retention of hours needs.
func (a *Allocator) RetireJobDetail(ctx context.Context, before time.Time, limit int) (RetiredJobDetail, error) {
	var out RetiredJobDetail

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		stamp := ts(a.now())

		jobs, err := q.MarkJobDetailRetired(ctx, ledgerdb.MarkJobDetailRetiredParams{
			Stamp: stamp, Before: before.UTC().Format(time.RFC3339Nano), MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: mark job detail retired: %w", err)
		}
		if jobs == 0 {
			return nil
		}
		series, err := q.DeleteRetiredJobSeries(ctx, stamp)
		if err != nil {
			return fmt.Errorf("alloc: delete retired job series: %w", err)
		}
		destinations, err := q.DeleteRetiredJobDestinations(ctx, stamp)
		if err != nil {
			return fmt.Errorf("alloc: delete retired job destinations: %w", err)
		}
		if err := q.ForgetRetiredJobDestinationTotals(ctx, stamp); err != nil {
			return fmt.Errorf("alloc: forget retired jobs' destination totals: %w", err)
		}
		guest, err := q.DeleteRetiredJobGuestReports(ctx, stamp)
		if err != nil {
			return fmt.Errorf("alloc: delete retired guest reports: %w", err)
		}
		out = RetiredJobDetail{Jobs: int(jobs), Series: int(series), Destinations: int(destinations),
			GuestReports: int(guest)}

		return nil
	})

	return out, err
}
