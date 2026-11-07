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
			if n, err := q.RequestPoolOrderRelease(ctx, ledgerdb.RequestPoolOrderReleaseParams{
				Now: ts(now), Reason: reason, OrderID: order,
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

// SealPoolRelease seals a joined host whose release was asked for, once the ledger shows
// it holds nothing, and returns the proof its source is asked with (ADR-030).
//
// ONE TRANSACTION, so nothing is placed between the reading and the seal: the node is
// live, it holds no lease, no quarantine and no custody, and its last inventory, from
// its current registration and received after placement stopped, reported nothing
// running. Then its certificates are revoked, first, as the release requires, and the
// order is sealed against everything. A proof that does not hold writes nothing and
// answers ErrPoolReleaseWaits.
func (a *Allocator) SealPoolRelease(ctx context.Context, order string) (releaseproof.Proof, error) {
	var proof releaseproof.Proof

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		proof = releaseproof.Proof{}
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

		holdings, err := nodeHoldingsTx(ctx, q, row.NodeName, requested)
		if err != nil {
			return err
		}

		holdings.Host = row.HostID

		p, err := releaseproof.NewDrained(holdings)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrPoolReleaseWaits, err)
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
	if err != nil {
		return releaseproof.Proof{}, err
	}

	return proof, nil
}

// nodeHoldingsTx reads what a node holds for its release proof. An inventory that is
// not from its current registration, or arrived before placement stopped, or a node
// that is not live, cannot say what the host runs now, so it reads as unreadable.
func nodeHoldingsTx(ctx context.Context, q state.WriteOps, node string, stopped time.Time) (releaseproof.Holdings,
	error,
) {
	h := releaseproof.Holdings{Sealed: stopped}

	counts, err := q.CountNodeHoldings(ctx, node)
	if err != nil {
		return h, fmt.Errorf("alloc: count what %s holds: %w", node, err)
	}

	h.Leases, h.Quarantine, h.Custody = int(counts.Outstanding), int(counts.Quarantine), int(counts.Custody)

	facts, err := q.ReadNodeReleaseFacts(ctx, node)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		h.Inventory = -1

		return h, nil
	case err != nil:
		return h, fmt.Errorf("alloc: read %s's inventory: %w", node, err)
	}

	h.Inventory = -1

	if facts.Live != 1 || !facts.NodeEpoch.Valid || facts.NodeEpoch.Int64 != facts.Epoch ||
		!facts.Running.Valid || !facts.ReceivedAt.Valid {
		return h, nil
	}

	received, err := time.Parse(timestampFormat, facts.ReceivedAt.String)
	if err != nil || !received.After(stopped) {
		return h, nil
	}

	h.Inventory = int(facts.Running.Int64)

	return h, nil
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
		return w.MarkPoolOrderGone(ctx, ledgerdb.MarkPoolOrderGoneParams{OrderID: id, UpdatedAt: now})
	})
}
