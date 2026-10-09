package alloc

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// otherOrdinal is the ordinal of the row holding the traffic beyond a job's
// named destinations; the named ones take 0 to MaxJobDestinations-1.
const otherOrdinal = MaxJobDestinations

// destinationsVerdict is what job_usage records of a report's destinations:
// NULL when they were not totalled, which a reader must never take for a job
// that sent nothing, and otherwise whether they are complete.
func destinationsVerdict(d *JobDestinations) sql.NullInt64 {
	switch {
	case d == nil:
		return sql.NullInt64{}
	case d.Incomplete:
		return sql.NullInt64{Int64: 1, Valid: true}
	}

	return sql.NullInt64{Int64: 0, Valid: true}
}

// tapTotal is one of the tap's totals as its column holds it: NULL where the
// tap was not read.
func tapTotal(d *JobDestinations, total func(*TapTotals) int64) sql.NullInt64 {
	if d == nil || d.Tap == nil {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: total(d.Tap), Valid: true}
}

// recordDestinations writes a report's destinations, and the row for the
// traffic beyond them even when it is empty, so every job whose flows were
// totalled has that row.
func recordDestinations(ctx context.Context, q state.WriteOps, leaseID string, d *JobDestinations) error {
	if d == nil {
		return nil
	}
	write := func(ordinal int, dest JobDestination) error {
		if err := q.RecordJobDestination(ctx, ledgerdb.RecordJobDestinationParams{
			LeaseID: leaseID, Ordinal: int64(ordinal), Addr: dest.Addr,
			SentBytes: dest.SentBytes, ReceivedBytes: dest.ReceivedBytes, Connections: dest.Connections,
		}); err != nil {
			return fmt.Errorf("alloc: record the destinations of lease %s: %w", leaseID, err)
		}

		return nil
	}
	for i, dest := range d.Destinations {
		if err := write(i, dest); err != nil {
			return err
		}
	}

	return write(otherOrdinal, d.Other)
}

// readDestinations reads back the destinations of a lease whose report
// totalled them. One whose verdict is NULL did not, which is every row from
// before migration 59, and has none to read.
func readDestinations(ctx context.Context, q querier, leaseID string, row ledgerdb.JobUsage,
) (*JobDestinations, error) {
	rows, err := state.ReadQueries(q).ReadJobDestinations(ctx, leaseID)
	if err != nil {
		return nil, fmt.Errorf("alloc: read the destinations of lease %s: %w", leaseID, err)
	}
	out := &JobDestinations{Incomplete: row.DestinationsIncomplete.Int64 == 1}
	other := false
	for _, r := range rows {
		dest := JobDestination{
			Addr: r.Addr, SentBytes: r.SentBytes, ReceivedBytes: r.ReceivedBytes, Connections: r.Connections,
		}
		if r.Ordinal == otherOrdinal {
			out.Other, other = dest, true
			continue
		}
		out.Destinations = append(out.Destinations, dest)
	}
	// A VERDICT WITHOUT ITS OTHER ROW is a ledger that does not hold what was
	// written with it, and reading it as nothing beyond the named destinations
	// would be a guess.
	if !other {
		return nil, fmt.Errorf("alloc: the ledger records destinations for lease %s without the row "+
			"for the traffic beyond them", leaseID)
	}
	if row.TapSentBytes.Valid && row.TapReceivedBytes.Valid {
		out.Tap = &TapTotals{SentBytes: row.TapSentBytes.Int64, ReceivedBytes: row.TapReceivedBytes.Int64}
	}

	return out, nil
}
