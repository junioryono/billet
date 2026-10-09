package alloc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

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
// totalled has that row. One statement writes them all
// (RecordJobDestinations says why).
func recordDestinations(ctx context.Context, q state.WriteOps, leaseID string, d *JobDestinations) error {
	if d == nil {
		return nil
	}
	packed, rows, err := packDestinations(d)
	if err != nil {
		return fmt.Errorf("alloc: record the destinations of lease %s: %w", leaseID, err)
	}
	if err := q.RecordJobDestinations(ctx, ledgerdb.RecordJobDestinationsParams{
		LeaseID: leaseID, Packed: packed, RowCount: int64(rows),
	}); err != nil {
		return fmt.Errorf("alloc: record the destinations of lease %s: %w", leaseID, err)
	}

	return nil
}

// The fixed-width record RecordJobDestinations cuts apart; the widths there
// are these, and a change to one is a change to both.
const (
	packedOrdinalWidth = 3
	// packedAddrWidth is the longest canonical IPv6 address with no zone.
	packedAddrWidth  = 39
	packedCountWidth = 19 // the digits of the largest int64
	packedRowWidth   = packedOrdinalWidth + packedAddrWidth + 3*packedCountWidth
)

// packDestinations is a report's rows as RecordJobDestinations reads them, and
// how many there are: the named destinations at their ordinals, then the rest
// at otherOrdinal. It refuses a row the record cannot hold whole, which a
// report that validated never has.
func packDestinations(d *JobDestinations) (string, int, error) {
	var b strings.Builder
	b.Grow((len(d.Destinations) + 1) * packedRowWidth)
	row := func(ordinal int, dest JobDestination) error {
		if len(dest.Addr) > packedAddrWidth || strings.ContainsAny(dest.Addr, " \t\r\n") {
			return fmt.Errorf("destination %q does not fit the ledger's record", dest.Addr)
		}
		if dest.SentBytes < 0 || dest.ReceivedBytes < 0 || dest.Connections < 0 {
			return fmt.Errorf("destination %q has a negative total", dest.Addr)
		}
		fmt.Fprintf(&b, "%0*d%-*s%0*d%0*d%0*d", packedOrdinalWidth, ordinal, packedAddrWidth, dest.Addr,
			packedCountWidth, dest.SentBytes, packedCountWidth, dest.ReceivedBytes,
			packedCountWidth, dest.Connections)

		return nil
	}
	for i, dest := range d.Destinations {
		if err := row(i, dest); err != nil {
			return "", 0, err
		}
	}
	if err := row(otherOrdinal, d.Other); err != nil {
		return "", 0, err
	}

	return b.String(), len(d.Destinations) + 1, nil
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
