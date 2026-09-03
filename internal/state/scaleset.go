package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ScaleSetRecord is a scale set billet created, and where.
type ScaleSetRecord struct {
	Org         string
	RunnerGroup string
	Label       string
	ID          int
}

// RecordScaleSet remembers that billet created this scale set.
//
// Idempotent by (runner_group, label): the server reconciles every tier on every
// start, so this runs constantly against rows that already exist. The id is
// refreshed rather than kept, because a scale set deleted and recreated outside
// billet keeps its name and takes a new id, and the stale one would send an
// operator looking for an object that is gone.
func (db *DB) RecordScaleSet(ctx context.Context, rec ScaleSetRecord) error {
	if rec.Org == "" {
		return fmt.Errorf("state: record scale set %q: no organization", rec.Label)
	}

	if rec.Label == "" {
		return fmt.Errorf("state: record scale set: no label")
	}

	if rec.ID <= 0 {
		return fmt.Errorf("state: record scale set %q: id %d is not one GitHub issued", rec.Label, rec.ID)
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		q := WriteQueries(tx)

		if err := q.DeleteMovedScaleSet(ctx, ledgerdb.DeleteMovedScaleSetParams{
			Org:         rec.Org,
			ScaleSetID:  int64(rec.ID),
			RunnerGroup: rec.RunnerGroup,
			Label:       rec.Label,
		}); err != nil {
			return err
		}

		return q.UpsertScaleSet(ctx, ledgerdb.UpsertScaleSetParams{
			Org:         rec.Org,
			RunnerGroup: rec.RunnerGroup,
			Label:       rec.Label,
			ScaleSetID:  int64(rec.ID),
			CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		})
	})
}

// ForgetScaleSet drops the record for a scale set that is gone.
//
// Called when billet deletes one, so the orphan report stops naming something an
// operator has already cleaned up. Removing a record billet never had is not an
// error: teardown may be run against a deployment whose ledger predates this.
func (db *DB) ForgetScaleSet(ctx context.Context, org, group, label string) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		return WriteQueries(tx).DeleteScaleSet(ctx, ledgerdb.DeleteScaleSetParams{
			Org:         org,
			RunnerGroup: group,
			Label:       label,
		})
	})
}

// ScaleSets returns every scale set billet recorded creating for one organization.
//
// On the read-only pool: a read routed through Tx would reserve the single
// writer slot while it scans. One statement needs no snapshot, so it does not go
// through View either — which is exactly why it has to translate a cancellation
// ITSELF. Server.Run calls this before any listener starts and returns what comes
// back, so a stop landing in that window used to leave the unit `failed` over a
// read the shutdown had interrupted. See asCancellation.
func (db *DB) ScaleSets(ctx context.Context, org string) ([]ScaleSetRecord, error) {
	rows, err := ReadQueries(db.Reader()).ListScaleSets(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("state: list scale sets: %w", db.asCancellation(ctx, err))
	}

	var out []ScaleSetRecord

	for _, r := range rows {
		out = append(out, ScaleSetRecord{
			Org:         r.Org,
			RunnerGroup: r.RunnerGroup,
			Label:       r.Label,
			ID:          int(r.ScaleSetID),
		})
	}

	return out, nil
}
