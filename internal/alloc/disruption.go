package alloc

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// disruptableLeases is what a disruption may be recorded against, and the guard
// that decides it lives in internal/state/queries/disruption.sql -- where it is
// written out TWICE, once per statement, because SQL has no way to compose a
// predicate the way the Go constant that used to sit here did.
// TestBothDisruptionStatementsPinTheirWholeStatement is what keeps the two copies the
// same, and the reasoning behind each clause is in that file.
type disruptableLeases func(ctx context.Context, q state.ReadOps) ([]string, error)

// byLease and onNode are the two shapes of that question.
func byLease(leaseID string) disruptableLeases {
	return func(ctx context.Context, q state.ReadOps) ([]string, error) {
		return q.DisruptableLease(ctx, leaseID)
	}
}

func onNode(node string) disruptableLeases {
	return func(ctx context.Context, q state.ReadOps) ([]string, error) {
		return q.DisruptableLeasesOnNode(ctx, node)
	}
}

// markLeaseDisruptedTx records a disruption against one lease, and reports
// whether it landed.
func markLeaseDisruptedTx(
	ctx context.Context, tx *sql.Tx, leaseID string, d Disruption, now time.Time,
) (bool, error) {
	ids, err := disruptableTx(ctx, tx, d, byLease(leaseID))
	if err != nil {
		return false, err
	}

	if err := applyDisruptionTx(ctx, tx, ids, d, now); err != nil {
		return false, err
	}

	return len(ids) > 0, nil
}

// markNodeDisruptedTx records a disruption against every lease on one host that
// may still be running a job, and reports how many.
//
// COALESCE(node, target_node), the way the rest of this package attributes a
// lease to a machine: a lease escrowed against a host and launching on it has
// not necessarily been bound yet.
func markNodeDisruptedTx(
	ctx context.Context, tx *sql.Tx, node string, d Disruption, now time.Time,
) (int, error) {
	ids, err := disruptableTx(ctx, tx, d, onNode(node))
	if err != nil {
		return 0, err
	}

	if err := applyDisruptionTx(ctx, tx, ids, d, now); err != nil {
		return 0, err
	}

	return len(ids), nil
}

// disruptableTx reads which leases a disruption may be recorded against.
//
// READ FULLY BEFORE WRITING. Every caller runs inside a transaction and issues
// more statements, so the cursor has to be closed before it continues — which is
// why the ids are collected here rather than iterated in place, the same shape
// readExpiredLeases and quarantinedIDsOn already use.
func disruptableTx(
	ctx context.Context, tx *sql.Tx, d Disruption, which disruptableLeases,
) ([]string, error) {
	if !d.Valid() {
		return nil, fmt.Errorf("alloc: %q is not a disruption billet records", d)
	}

	ids, err := which(ctx, state.WriteQueries(tx))
	if err != nil {
		return nil, fmt.Errorf("alloc: find the leases %s applies to: %w", d, err)
	}

	return ids, nil
}

// applyDisruptionTx writes the observation to BOTH the live lease and its
// history row.
//
// THE HISTORY ROW IS WRITTEN NOW, NOT LEFT TO archive. A lease terminalizes
// whenever its teardown finally succeeds, which can be hours after the job
// ended or never — and a job whose teardown is wedged on a host that vanished is
// exactly the one an operator is looking for. Waiting for the archive would hide
// it for precisely as long as it mattered.
//
// A lease that never reached Assign has no history row and this updates nothing
// there, which is correct: it ran no job.
func applyDisruptionTx(
	ctx context.Context, tx *sql.Tx, ids []string, d Disruption, now time.Time,
) error {
	at := ts(now.UTC())

	q := state.WriteQueries(tx)

	for _, id := range ids {
		if err := q.RecordLeaseDisruption(ctx, ledgerdb.RecordLeaseDisruptionParams{
			Disruption:  string(d),
			DisruptedAt: at,
			ID:          id,
		}); err != nil {
			return fmt.Errorf("alloc: record %s against lease %s: %w", d, id, err)
		}

		if err := q.RecordHistoryDisruption(ctx, ledgerdb.RecordHistoryDisruptionParams{
			Disruption:  string(d),
			DisruptedAt: at,
			LeaseID:     id,
		}); err != nil {
			return fmt.Errorf("alloc: record %s in the history of lease %s: %w", d, id, err)
		}
	}

	return nil
}
