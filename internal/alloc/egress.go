package alloc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RecordLeaseEgress keeps what the host saw of a lease's network: the names its
// guest resolved and what a tier's allowlist refused (ADR-024).
//
// FENCED ON THE EPOCH, in the same transaction, as RecordLeaseUsage is: a
// superseded holder cannot report, and a lease that has ended is refused
// (ErrLeaseNotFound), because the node reports before the plane releases it. The
// first report is kept and a later one is ignored.
func (a *Allocator) RecordLeaseEgress(ctx context.Context, leaseID string, epoch int64, report JobEgress) error {
	if err := report.Validate(); err != nil {
		return err
	}

	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("alloc: encode the egress report of lease %s: %w", leaseID, err)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if _, err := state.WriteQueries(tx).RecordJobEgress(ctx, ledgerdb.RecordJobEgressParams{
			LeaseID: lease.ID, Node: lease.Node, RecordedAt: nowStamp(), Listed: flag(report.Listed),
			Report: string(data),
		}); err != nil {
			return fmt.Errorf("alloc: record the egress report of lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// RecordedEgress is a lease's egress report as the ledger kept it, with the host
// it came from and when the control plane kept it.
type RecordedEgress struct {
	JobEgress
	Node       string
	RecordedAt string
}

// LeaseEgress reads what the host saw of a lease's network. A lease with no report
// is ErrLeaseNotFound, which says nothing was kept and is never an empty report; a
// row the ledger cannot read back is an error of its own, never either of those.
func (a *Allocator) LeaseEgress(ctx context.Context, leaseID string) (RecordedEgress, error) {
	var out RecordedEgress

	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJobEgress(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no egress report was recorded for lease %s", ErrLeaseNotFound, leaseID)
		}

		if err != nil {
			return fmt.Errorf("alloc: read the egress report of lease %s: %w", leaseID, err)
		}

		var report JobEgress
		if err := json.Unmarshal([]byte(row.Report), &report); err != nil {
			return fmt.Errorf("alloc: the egress report of lease %s cannot be read: %w", leaseID, err)
		}

		if err := report.Validate(); err != nil {
			return fmt.Errorf("alloc: the ledger holds an egress report for lease %s it would refuse: %w",
				leaseID, err)
		}

		out = RecordedEgress{JobEgress: report, Node: row.Node, RecordedAt: row.RecordedAt}

		return nil
	})

	return out, err
}
