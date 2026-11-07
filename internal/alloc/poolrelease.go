package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/releaseproof"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ErrPoolReleaseWaits is a release asked for that cannot be sealed yet: its host still
// holds something, or what it holds could not be read. It wraps releaseproof.ErrNoProof;
// nothing was written, and the pool asks again at its next step.
var ErrPoolReleaseWaits = errors.New("alloc: the pool host's release waits for its proof")

// RequestPoolRelease asks for a placed host's release on a person's word (ADR-030), and
// answers whether the order was sealed by the asking.
//
// A HOST A NODE JOINED FROM is taken out of placement now and sealed later, once the
// ledger shows it holds nothing (SealPoolRelease): the moment placement stopped is
// recorded, and the node's registration keeps it out while the release stands.
//
// A HOST NOTHING JOINED FROM has no drain to prove, so it is sealed here, in this
// transaction, on the proof that nothing was ever admitted for it: its tokens retired
// and nothing joining from it again. Whether a node joined is read here, so an approval
// that linked one first makes this the joined case.
func (a *Allocator) RequestPoolRelease(ctx context.Context, order, reason string) (bool, error) {
	if reason == "" {
		return false, fmt.Errorf("alloc: releasing pool order %s needs the reason a person gave", order)
	}

	var sealed bool

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		sealed = false
		q := state.WriteQueries(tx)
		now := a.now().UTC()

		row, err := q.ReadPoolOrder(ctx, order)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderMoved, order)
		case err != nil:
			return fmt.Errorf("alloc: read pool order %s: %w", order, err)
		case row.State != PoolPlaced:
			return fmt.Errorf("%w: pool order %s is %s; only a placed host is released", ErrPoolOrderMoved,
				order, row.State)
		case row.ReleaseRequestedAt != "":
			return fmt.Errorf("%w: pool order %s's release was asked for already", ErrPoolOrderMoved, order)
		}

		if row.NodeName != "" {
			// THE LAUNCHES DISPATCHED SO FAR, so one dispatched since restarts the proof.
			// A node that never registered has dispatched nothing.
			var dispatched int64

			switch fence, err := q.ReadNodeFence(ctx, row.NodeName); {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("alloc: read %s's dispatch fence: %w", row.NodeName, err)
			default:
				dispatched = fence.DispatchGeneration
			}

			if n, err := q.RequestPoolOrderRelease(ctx, ledgerdb.RequestPoolOrderReleaseParams{
				Now: ts(now), Reason: reason, Dispatch: dispatched, OrderID: order,
			}); err != nil {
				return fmt.Errorf("alloc: ask for the release of pool order %s: %w", order, err)
			} else if n != 1 {
				return fmt.Errorf("%w: pool order %s moved while its release was asked for", ErrPoolOrderMoved, order)
			}

			if err := q.MarkNodeDrainedForRelease(ctx, row.NodeName); err != nil {
				return fmt.Errorf("alloc: take %s out of placement: %w", row.NodeName, err)
			}

			return nil
		}

		admitted, err := q.CountPoolOrderAdmissions(ctx, ledgerdb.CountPoolOrderAdmissionsParams{
			OrderID: order, NodeName: PoolNodeName(order),
		})
		if err != nil {
			return fmt.Errorf("alloc: read whether a node joined from pool order %s: %w", order, err)
		}

		if _, err := releaseproof.NewUnjoined(releaseproof.Unjoined{
			Host: row.HostID, OrderSealed: now, Admitted: int(admitted),
		}); err != nil {
			return fmt.Errorf("%w: %w", ErrPoolReleaseWaits, err)
		}

		if err := q.RetirePoolJoinTokens(ctx, order); err != nil {
			return fmt.Errorf("alloc: retire the join tokens of pool order %s: %w", order, err)
		}

		if err := sealPoolOrderTx(ctx, q, order, "", ts(now), reason, ts(now)); err != nil {
			return err
		}

		sealed = true

		return nil
	})
	if err != nil {
		return false, err
	}

	return sealed, nil
}

// poolReleaseQuiet is how long after a release is asked for before its node's
// inventory counts towards the proof: longer than the ten minutes a node's command
// queue keeps a launch (internal/nodeplane), so a launch queued before the release has
// run, and shows in what the node lists, or been dropped by then.
const poolReleaseQuiet = 15 * time.Minute

// SealPoolRelease seals a joined host whose release was asked for, once the ledger
// proves it holds nothing, and returns the proof its source is asked with (ADR-030).
//
// ONE TRANSACTION, so nothing is placed between the reading and the seal: the node
// holds no lease, quarantine or custody, no launch was dispatched to it since the
// release was asked for, and its inventory proves it runs nothing (releaseInventory).
// Then its certificates are revoked, first, as the release requires, and the order is
// sealed against everything. A proof that does not hold seals nothing and answers
// ErrPoolReleaseWaits, after recording where the proof stands.
func (a *Allocator) SealPoolRelease(ctx context.Context, order string) (releaseproof.Proof, error) {
	var (
		proof releaseproof.Proof
		waits error
	)

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		proof, waits = releaseproof.Proof{}, nil
		q := state.WriteQueries(tx)
		now := a.now().UTC()

		row, err := q.ReadPoolOrder(ctx, order)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderMoved, order)
		case err != nil:
			return fmt.Errorf("alloc: read pool order %s: %w", order, err)
		case row.State != PoolPlaced || row.NodeName == "" || row.ReleaseRequestedAt == "":
			return fmt.Errorf("%w: pool order %s is %s with no release of a joined host asked for", ErrPoolOrderMoved,
				order, row.State)
		}

		requested, err := time.Parse(timestampFormat, row.ReleaseRequestedAt)
		if err != nil {
			return fmt.Errorf("alloc: pool order %s: release asked for at %q: %w", order, row.ReleaseRequestedAt, err)
		}

		counts, err := q.CountNodeHoldings(ctx, row.NodeName)
		if err != nil {
			return fmt.Errorf("alloc: count what %s holds: %w", row.NodeName, err)
		}

		holdings := releaseproof.Holdings{
			Host: row.HostID, Sealed: requested, Leases: int(counts.Outstanding),
			Quarantine: int(counts.Quarantine), Custody: int(counts.Custody), Inventory: -1,
		}

		switch facts, err := q.ReadNodeReleaseFacts(ctx, row.NodeName); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("alloc: read %s's inventory: %w", row.NodeName, err)
		default:
			inventory, dispatch, reportAt, err := releaseInventory(&row, &facts, requested)
			if err != nil {
				return fmt.Errorf("alloc: %s's release proof: %w", row.NodeName, err)
			}

			holdings.Inventory = inventory

			if dispatch != row.ReleaseDispatch || reportAt != row.ReleaseReportAt {
				if _, err := q.NotePoolReleaseReport(ctx, ledgerdb.NotePoolReleaseReportParams{
					Dispatch: dispatch, ReportAt: reportAt, OrderID: order,
				}); err != nil {
					return fmt.Errorf("alloc: record where pool order %s's release stands: %w", order, err)
				}
			}
		}

		p, err := releaseproof.NewDrained(holdings)
		if err != nil {
			waits = fmt.Errorf("%w: %w", ErrPoolReleaseWaits, err)

			return nil
		}

		// THE CERTIFICATES BEFORE THE SOURCE: once this commits, nothing the host holds
		// authenticates it again, so a host the source has not yet taken back cannot
		// register into placement or ask for work.
		if err := revokeNodeTx(ctx, q, row.NodeName, "its pool host is being released: "+row.ReleaseReason,
			a.now().UTC()); err != nil {
			return err
		}

		if err := q.MarkNodeDrainedForRelease(ctx, row.NodeName); err != nil {
			return fmt.Errorf("alloc: take %s out of placement: %w", row.NodeName, err)
		}

		if err := sealPoolOrderTx(ctx, q, order, row.NodeName, row.ReleaseRequestedAt, row.ReleaseReason,
			ts(now)); err != nil {
			return err
		}

		proof = p

		return nil
	})

	switch {
	case err != nil:
		return releaseproof.Proof{}, err
	case waits != nil:
		return releaseproof.Proof{}, waits
	}

	return proof, nil
}

// releaseInventory says what a joined host's node proves it runs for its release (-1 is
// could-not-tell), and where the proof stands after this reading: the launch-dispatch
// generation it is measured from, and when the first inventory counted towards it
// arrived.
//
// AN INVENTORY IS EVIDENCE, NOT PROOF, on its own: it is stamped when it ARRIVES, and the
// node listed its compute before that. So it counts only from the node's current
// registration while it is live, only once it arrived after the quiet period that lets
// a launch queued before the release run or be dropped, and only the SECOND such report
// is read: a node lists its next report only after its previous one arrived (its
// sweeps are serial), so the second was listed after the first arrived, which was after
// the quiet period. A launch dispatched since the release was asked for starts the
// proof again.
//
// A NODE A PERSON DECOMMISSIONED ON THE COMPUTE BARRIER'S PROOF runs nothing, which is
// what that proof is, and is how a host that stopped reporting is released: a
// registration clears the mark.
func releaseInventory(row *ledgerdb.PoolOrder, f *ledgerdb.ReadNodeReleaseFactsRow, requested time.Time) (int, int64,
	string, error,
) {
	dispatch, reportAt := row.ReleaseDispatch, row.ReleaseReportAt

	switch {
	case f.DecommissionProven == 1:
		return 0, dispatch, reportAt, nil
	case f.DispatchGeneration != dispatch:
		return -1, f.DispatchGeneration, "", nil
	case f.Live != 1 || !f.NodeEpoch.Valid || f.NodeEpoch.Int64 != f.Epoch || !f.Running.Valid || !f.ReceivedAt.Valid:
		return -1, dispatch, reportAt, nil
	}

	received, err := time.Parse(timestampFormat, f.ReceivedAt.String)
	if err != nil {
		return -1, dispatch, reportAt, fmt.Errorf("its inventory was received at %q: %w", f.ReceivedAt.String, err)
	}

	if !received.After(requested.Add(poolReleaseQuiet)) {
		return -1, dispatch, reportAt, nil
	}

	// THE FIRST REPORT COUNTED is this one when none was, and proves nothing on its own.
	if reportAt == "" {
		reportAt = f.ReceivedAt.String
	}

	first, err := time.Parse(timestampFormat, reportAt)
	if err != nil {
		return -1, dispatch, reportAt, fmt.Errorf("its first counted inventory arrived at %q: %w", reportAt, err)
	}

	if !received.After(first) {
		return -1, dispatch, reportAt, nil
	}

	return int(f.Running.Int64), dispatch, reportAt, nil
}

func sealPoolOrderTx(ctx context.Context, q state.WriteOps, order, node, requested, reason, now string) error {
	n, err := q.SealPoolOrder(ctx, ledgerdb.SealPoolOrderParams{
		RequestedAt: requested, Reason: reason, Now: now, OrderID: order, NodeName: node,
	})
	if err != nil {
		return fmt.Errorf("alloc: seal pool order %s: %w", order, err)
	}

	if n != 1 {
		return fmt.Errorf("%w: pool order %s moved while it was sealed", ErrPoolOrderMoved, order)
	}

	return nil
}

// PoolReleaseProof is the proof a sealed order's host is released on, from the seal the
// ledger recorded: a release asked again, after a failed attempt or a restart, is the
// release the seal proved.
func (a *Allocator) PoolReleaseProof(ctx context.Context, order string) (releaseproof.Proof, error) {
	var proof releaseproof.Proof

	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadPoolOrder(ctx, order)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderMoved, order)
		case err != nil:
			return fmt.Errorf("alloc: read pool order %s: %w", order, err)
		case row.State != PoolSealed:
			return fmt.Errorf("%w: pool order %s is %s, not sealed", ErrPoolOrderMoved, order, row.State)
		}

		sealed, err := time.Parse(timestampFormat, row.UpdatedAt)
		if err != nil {
			return fmt.Errorf("alloc: pool order %s: sealed at %q: %w", order, row.UpdatedAt, err)
		}

		proof, err = releaseproof.Sealed(row.HostID, sealed)

		return err
	})

	return proof, err
}

// PoolOrderReleased records that the source took a sealed order's host back.
func (a *Allocator) PoolOrderReleased(ctx context.Context, id string) error {
	return a.movePoolOrder(ctx, id, PoolSealed, func(w state.WriteOps, now string) (int64, error) {
		return w.MarkPoolOrderReleased(ctx, ledgerdb.MarkPoolOrderReleasedParams{OrderID: id, UpdatedAt: now})
	})
}

// PoolOrderGone records the source's own terminal answer that a released host is gone.
func (a *Allocator) PoolOrderGone(ctx context.Context, id string) error {
	return a.movePoolOrder(ctx, id, PoolReleased, func(w state.WriteOps, now string) (int64, error) {
		return w.MarkPoolOrderGone(ctx, ledgerdb.MarkPoolOrderGoneParams{
			Reason: "its source said the host is gone", OrderID: id, UpdatedAt: now,
		})
	})
}

// ClosePoolOrder closes a released order on a person's word, after they have looked at
// the source themselves: the way a release closes on a source that never says gone of a
// host it took back (phoenixNAP), and on one no longer configured. The reason records
// that it was a person, not the source.
func (a *Allocator) ClosePoolOrder(ctx context.Context, id, reason string) error {
	if reason == "" {
		return fmt.Errorf("alloc: closing pool order %s needs the reason a person gave", id)
	}

	return a.movePoolOrder(ctx, id, PoolReleased, func(w state.WriteOps, now string) (int64, error) {
		return w.MarkPoolOrderGone(ctx, ledgerdb.MarkPoolOrderGoneParams{
			Reason: "closed by a person: " + reason, OrderID: id, UpdatedAt: now,
		})
	})
}
