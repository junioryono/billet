package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// CredentialSweepRecord is what one pass of the control plane's sweep over one
// Parameter Store path found and did.
//
// A RECORD, NOT A DECISION. Nothing reads it to decide whether a parameter may be
// deleted; it exists because `billet status` runs in another process and a count
// held in the control plane's memory would be invisible to it.
type CredentialSweepRecord struct {
	Region string
	Path   string
	// SweptAt is when the pass ran.
	SweptAt time.Time
	// Removed is what the pass deleted; RemovedTotal accumulates across passes and
	// is only ever read back, never written by a caller.
	Removed      int
	RemovedTotal int
	// Kept is what is waiting on a lease that is open or too recently closed.
	Kept int
	// Unaccounted names the ledger has never heard of, which a person has to look
	// at: a ledger restored from an older backup, or a foreign writer on the path.
	Unaccounted int
	// ForeignNames are entries under the path that are not billet's at all.
	ForeignNames int
	// Error is why the pass stopped, or empty for one that completed. A pass that
	// could not read the ledger keeps everything and says so here.
	Error string
}

// RecordCredentialSweep records one pass over one path, accumulating what it
// removed onto what earlier passes removed.
func (db *DB) RecordCredentialSweep(ctx context.Context, rec CredentialSweepRecord) error {
	if rec.Region == "" || rec.Path == "" {
		return errors.New("state: record credential sweep: a region and a path are required")
	}

	if rec.Removed < 0 || rec.Kept < 0 || rec.Unaccounted < 0 || rec.ForeignNames < 0 {
		return fmt.Errorf("state: record credential sweep for %s: a count is negative", rec.Path)
	}

	if rec.SweptAt.IsZero() {
		return fmt.Errorf("state: record credential sweep for %s: no time", rec.Path)
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		return WriteQueries(tx).RecordCredentialSweep(ctx, ledgerdb.RecordCredentialSweepParams{
			Region:       rec.Region,
			Path:         rec.Path,
			SweptAt:      rec.SweptAt.UTC().Format(time.RFC3339Nano),
			Removed:      int64(rec.Removed),
			Kept:         int64(rec.Kept),
			Unaccounted:  int64(rec.Unaccounted),
			ForeignNames: int64(rec.ForeignNames),
			Error:        rec.Error,
		})
	})
}

// CredentialSweeps lists every path the sweep has recorded a pass over, with its
// most recent pass.
//
// On the read-only pool: a read routed through Tx would reserve the single writer
// slot while it scans.
func (db *DB) CredentialSweeps(ctx context.Context) ([]CredentialSweepRecord, error) {
	var out []CredentialSweepRecord

	err := db.View(ctx, func(q Querier) error {
		rows, err := ReadQueries(q).ListCredentialSweeps(ctx)
		if err != nil {
			return fmt.Errorf("state: list credential sweeps: %w", err)
		}

		for _, row := range rows {
			// A TIMESTAMP THAT WILL NOT PARSE IS REPORTED AS ZERO rather than
			// failing the listing: this is a diagnostic, and a status line with an
			// unknown time beats no status line at all.
			sweptAt, _ := time.Parse(time.RFC3339Nano, row.SweptAt) //nolint:errcheck // a zero time is the honest rendering of an unparseable one

			out = append(out, CredentialSweepRecord{
				Region:       row.Region,
				Path:         row.Path,
				SweptAt:      sweptAt,
				Removed:      int(row.Removed),
				RemovedTotal: int(row.RemovedTotal),
				Kept:         int(row.Kept),
				Unaccounted:  int(row.Unaccounted),
				ForeignNames: int(row.ForeignNames),
				Error:        row.Error,
			})
		}

		return nil
	})

	return out, err
}
