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
	// Jobs is how many of the jobs it was handed the batch marked retired.
	Jobs         int
	Series       int
	Destinations int
	GuestReports int
}

// JobsWithRetirableDetail lists at most limit finished, unretired jobs all of
// whose series, destinations and guest report were recorded before before,
// oldest first. It is a read: listing them reads every unretired job's detail,
// and that work stays outside the writer's transaction.
//
// BY recorded_at, which usage and guest reports write as RFC 3339 with its
// trailing zeros trimmed: compared as text, two times in different seconds
// order correctly, which is the resolution a retention of hours needs.
func (a *Allocator) JobsWithRetirableDetail(ctx context.Context, before time.Time, limit int) ([]string, error) {
	var out []string

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListRetirableJobDetail(ctx, ledgerdb.ListRetirableJobDetailParams{
			Before: before.UTC().Format(time.RFC3339Nano), MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list jobs whose detail is past its retention: %w", err)
		}
		out = rows

		return nil
	})

	return out, err
}

// RetireJobDetail deletes the series, destinations and guest report of the
// jobs leaseIDs names that are still unretired and finished, and marks each
// one's history row retired, in one transaction. A job's usage summary is
// kept, its destinations verdict cleared with its destinations.
func (a *Allocator) RetireJobDetail(ctx context.Context, leaseIDs []string) (RetiredJobDetail, error) {
	var out RetiredJobDetail
	if len(leaseIDs) == 0 {
		return out, nil
	}

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		stamp := ts(a.now())

		jobs := 0
		for _, id := range leaseIDs {
			marked, err := q.MarkJobDetailRetired(ctx, ledgerdb.MarkJobDetailRetiredParams{Stamp: stamp, LeaseID: id})
			if err != nil {
				return fmt.Errorf("alloc: mark the detail of lease %s retired: %w", id, err)
			}
			jobs += int(marked)
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
		out = RetiredJobDetail{Jobs: jobs, Series: int(series), Destinations: int(destinations),
			GuestReports: int(guest)}

		return nil
	})

	return out, err
}
