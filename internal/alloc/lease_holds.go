package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// What ended a hold.
const (
	// HoldEndedByBound is the hold reaching the time it was opened with.
	HoldEndedByBound = "bound"
	// HoldEndedBySession is a session's shell having exited, and the last session having closed.
	HoldEndedBySession = "session"
	// HoldEndedByOperator is `billet jobs release`.
	HoldEndedByOperator = "operator"
	// HoldEndedByDrain is `billet drain --cut-holds`.
	HoldEndedByDrain = "drain"
)

// LeaseHold is a failed job's VM kept for someone to look at. A HOLD DELAYS A DESTROY THE
// COMPLETION ALREADY ASKED FOR AND NEVER AUTHORISES ONE; its lease stays charged and in its
// phase. Until is fixed when it opens. EndedAt is zero while it holds.
type LeaseHold struct {
	LeaseID string
	Tier    string
	HeldAt  time.Time
	Until   time.Time
	EndedAt time.Time
	EndedBy string
}

// Holding says the hold has not ended. Whether it has passed its bound is the caller's to
// compare against its own clock, which is what ends it.
func (h LeaseHold) Holding() bool { return h.EndedAt.IsZero() }

func leaseHoldOf(row ledgerdb.LeaseHold) LeaseHold {
	return LeaseHold{LeaseID: row.LeaseID, Tier: row.Tier, HeldAt: parseStamp(row.HeldAt), Until: parseStamp(row.UntilAt),
		EndedAt: parseStamp(row.EndedAt), EndedBy: row.EndedBy}
}

// OpenLeaseHold opens a lease's hold until until and answers the row, whatever it was: a
// hold already open keeps the bound it opened with, so nothing can extend one, and one that
// ended stays ended.
//
// A NEW HOLD IS REFUSED ON A SEALED DEPLOYMENT, with ErrAdmissionSealed, and the seal is read in
// the transaction that would write the hold, so a seal committed a moment before cannot be
// missed: someone wants the deployment empty, and a failed job's VM kept for an hour is not what
// their drain is waiting for. A ledger that cannot say is sealed for this. A hold that already
// exists is answered as it is.
func (a *Allocator) OpenLeaseHold(ctx context.Context, leaseID, tier string, until time.Time) (LeaseHold, error) {
	// READ FIRST: the cleanup clock asks on every pass, and only the first needs the writer.
	out, err := a.LeaseHoldOf(ctx, leaseID)
	if !errors.Is(err, ErrLeaseNotFound) {
		return out, err
	}

	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		// AGAIN UNDER THE WRITER, since the read above was outside it: a hold another pass
		// opened meanwhile is the answer, sealed deployment or not.
		row, err := state.ReadQueries(tx).ReadLeaseHold(ctx, leaseID)
		if err == nil {
			out = leaseHoldOf(row)

			return nil
		}

		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: read the hold of lease %s: %w", leaseID, err)
		}

		if err := refuseIfSealed(ctx, tx); err != nil {
			return err
		}

		if err := state.WriteQueries(tx).OpenLeaseHold(ctx, ledgerdb.OpenLeaseHoldParams{
			LeaseID: leaseID, Tier: tier, HeldAt: ts(a.now()), UntilAt: ts(until),
		}); err != nil {
			return fmt.Errorf("alloc: open the hold of lease %s: %w", leaseID, err)
		}

		row, err = state.ReadQueries(tx).ReadLeaseHold(ctx, leaseID)
		if err != nil {
			return fmt.Errorf("alloc: read the hold of lease %s: %w", leaseID, err)
		}

		out = leaseHoldOf(row)

		return nil
	})

	return out, err
}

// LeaseHoldOf reads a lease's hold. A lease with none is ErrLeaseNotFound, which is never a
// hold that ended.
func (a *Allocator) LeaseHoldOf(ctx context.Context, leaseID string) (LeaseHold, error) {
	var out LeaseHold

	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadLeaseHold(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: lease %s has no hold", ErrLeaseNotFound, leaseID)
		}

		if err != nil {
			return fmt.Errorf("alloc: read the hold of lease %s: %w", leaseID, err)
		}

		out = leaseHoldOf(row)

		return nil
	})

	return out, err
}

// EndLeaseHold ends a lease's hold if it is holding, and says whether it did.
func (a *Allocator) EndLeaseHold(ctx context.Context, leaseID, by string) (bool, error) {
	var ended bool

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := state.WriteQueries(tx).EndLeaseHold(ctx, ledgerdb.EndLeaseHoldParams{
			LeaseID: leaseID, EndedAt: ts(a.now()), EndedBy: by,
		})
		if err != nil {
			return fmt.Errorf("alloc: end the hold of lease %s: %w", leaseID, err)
		}

		ended = n > 0

		return nil
	})

	return ended, err
}

// EndHoldingLeaseHolds ends every hold that is holding, and says how many.
func (a *Allocator) EndHoldingLeaseHolds(ctx context.Context, by string) (int, error) {
	var ended int64

	// READ FIRST: a drain asks on every poll, and nearly always there are none, which needs no writer.
	holds, err := a.unendedLeaseHolds(ctx)
	if err != nil {
		return 0, err
	}

	if len(holds) == 0 {
		return 0, nil
	}

	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := state.WriteQueries(tx).EndHoldingLeaseHolds(ctx, ledgerdb.EndHoldingLeaseHoldsParams{
			EndedAt: ts(a.now()), EndedBy: by,
		})
		if err != nil {
			return fmt.Errorf("alloc: end the holds: %w", err)
		}

		ended = n

		return nil
	})

	return int(ended), err
}

// unendedLeaseHolds is every hold nothing has ended, the soonest bound first.
func (a *Allocator) unendedLeaseHolds(ctx context.Context) ([]LeaseHold, error) {
	var out []LeaseHold

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListHoldingLeaseHolds(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list the holds: %w", err)
		}

		out = make([]LeaseHold, 0, len(rows))
		for i := range rows {
			out = append(out, leaseHoldOf(rows[i]))
		}

		return nil
	})

	return out, err
}

// HoldingLeaseHolds is every hold still holding, the soonest to end first. One past its bound has
// stopped, whether or not anything has written that yet.
func (a *Allocator) HoldingLeaseHolds(ctx context.Context) ([]LeaseHold, error) {
	holds, err := a.unendedLeaseHolds(ctx)
	if err != nil {
		return nil, err
	}

	now := a.now()

	return slices.DeleteFunc(holds, func(h LeaseHold) bool { return !now.Before(h.Until) }), nil
}

// EndLapsedLeaseHolds writes the end of every hold past its bound, and says how many. A hold whose
// VM went some other way, by a force operation or a listener's teardown, reaches here and no other
// end, and this is what lets its row be forgotten.
func (a *Allocator) EndLapsedLeaseHolds(ctx context.Context) (int, error) {
	holds, err := a.unendedLeaseHolds(ctx)
	if err != nil {
		return 0, err
	}

	now, ended := a.now(), 0

	for i := range holds {
		if now.Before(holds[i].Until) {
			break
		}

		did, err := a.EndLeaseHold(ctx, holds[i].LeaseID, HoldEndedByBound)
		if err != nil {
			return ended, err
		}

		if did {
			ended++
		}
	}

	return ended, nil
}

// ForgetEndedLeaseHolds deletes holds that ended before before.
func (a *Allocator) ForgetEndedLeaseHolds(ctx context.Context, before time.Time) (int, error) {
	var deleted int64

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := state.WriteQueries(tx).DeleteEndedLeaseHolds(ctx, ts(before))
		if err != nil {
			return fmt.Errorf("alloc: forget ended holds: %w", err)
		}

		deleted = n

		return nil
	})

	return int(deleted), err
}
