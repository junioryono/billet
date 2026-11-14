package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// The fixed-width record a job's openings are packed into (migration 83): a
// 3-digit ordinal, then the bucket's start in Unix seconds and the sent,
// received and connection counts, each as 19 zero-padded digits.
const (
	openingOrdinalWidth = 3
	openingCountWidth   = 19 // the digits of the largest int64
	openingRowWidth     = openingOrdinalWidth + 4*openingCountWidth
)

// recordOpenings writes a report's openings, when it split its destinations by
// time; a report that did not writes no row, which a reader says is no split.
func recordOpenings(ctx context.Context, q state.WriteOps, leaseID string, d *JobDestinations) error {
	if d == nil || d.OpeningSeconds == 0 {
		return nil
	}
	packed, err := packOpenings(d.Openings)
	if err != nil {
		return fmt.Errorf("alloc: record the openings of lease %s: %w", leaseID, err)
	}
	if err := q.RecordJobDestinationOpenings(ctx, ledgerdb.RecordJobDestinationOpeningsParams{
		LeaseID: leaseID, WidthSeconds: d.OpeningSeconds, RowCount: int64(len(d.Openings)), Packed: packed,
	}); err != nil {
		return fmt.Errorf("alloc: record the openings of lease %s: %w", leaseID, err)
	}

	return nil
}

// packOpenings is a report's openings as their row holds them. It refuses an
// opening the record cannot hold whole, which a report that validated never has.
func packOpenings(openings []DestinationOpening) (string, error) {
	var b strings.Builder
	b.Grow(len(openings) * openingRowWidth)
	for i, o := range openings {
		if o.Ordinal < 0 || o.Ordinal > MaxJobDestinations || o.Start < 0 || o.SentBytes < 0 ||
			o.ReceivedBytes < 0 || o.Connections < 0 {
			return "", fmt.Errorf("opening %d does not fit the ledger's record", i)
		}
		fmt.Fprintf(&b, "%0*d%0*d%0*d%0*d%0*d", openingOrdinalWidth, o.Ordinal, openingCountWidth, o.Start,
			openingCountWidth, o.SentBytes, openingCountWidth, o.ReceivedBytes, openingCountWidth, o.Connections)
	}

	return b.String(), nil
}

// unpackOpenings reads packed rows back. A row that is not one, or a count of rows
// the packing does not hold, is a ledger that does not hold what was written.
func unpackOpenings(packed string, rows int64) ([]DestinationOpening, error) {
	if rows < 0 || int64(len(packed)) != rows*openingRowWidth {
		return nil, fmt.Errorf("%d bytes do not hold %d openings", len(packed), rows)
	}
	out := make([]DestinationOpening, 0, rows)
	for i := range rows {
		rec := packed[i*openingRowWidth : (i+1)*openingRowWidth]
		field := func(at, width int) (int64, error) {
			s := rec[at : at+width]
			for _, c := range s {
				if c < '0' || c > '9' {
					return 0, fmt.Errorf("opening %d holds %q where a count belongs", i, s)
				}
			}
			return strconv.ParseInt(s, 10, 64)
		}
		var vals [5]int64
		at := 0
		for j, width := range []int{openingOrdinalWidth, openingCountWidth, openingCountWidth, openingCountWidth,
			openingCountWidth} {
			v, err := field(at, width)
			if err != nil {
				return nil, err
			}
			vals[j] = v
			at += width
		}
		out = append(out, DestinationOpening{Ordinal: int(vals[0]), Start: vals[1], SentBytes: vals[2],
			ReceivedBytes: vals[3], Connections: vals[4]})
	}

	return out, nil
}

// readOpenings adds a lease's openings to the destinations read for it, and
// holds them to the same validation a report is held to before it is written. A
// lease with no row has none, which says nothing of when its traffic moved.
func readOpenings(ctx context.Context, q querier, leaseID string, d *JobDestinations) error {
	row, err := state.ReadQueries(q).ReadJobDestinationOpenings(ctx, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("alloc: read the openings of lease %s: %w", leaseID, err)
	}
	openings, err := unpackOpenings(row.Packed, row.RowCount)
	if err != nil {
		return fmt.Errorf("alloc: the ledger's openings for lease %s: %w", leaseID, err)
	}
	d.Openings, d.OpeningSeconds = openings, row.WidthSeconds
	if err := d.Validate(); err != nil {
		return fmt.Errorf("alloc: the ledger's openings for lease %s: %w", leaseID, err)
	}

	return nil
}
