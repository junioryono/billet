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

// ErrPoolReleaseContradicted is a sealed release whose host the ledger no longer proves
// runs nothing: an inventory reported compute after the seal, or its node arrived. Its
// source is not asked; a person looks at the host, takes it back by hand and closes the
// order (`billet pool close`).
var ErrPoolReleaseContradicted = errors.New("alloc: the pool host's sealed release is contradicted")

// ErrPoolReleaseAsked is a release asked for again of a joined host already out of
// placement. It wraps ErrPoolOrderMoved; the caller may try the seal itself
// (SealPoolRelease), which is how a person seals a release with no pool configured.
var ErrPoolReleaseAsked = errors.New("alloc: the pool host's release was asked for already")

// RequestPoolRelease asks for a placed host's release on a person's word (ADR-030), and
// answers whether the order was sealed by the asking.
//
// A HOST A NODE JOINED FROM is taken out of placement now and sealed later, once the
// ledger proves it holds nothing (SealPoolRelease): the moment placement stopped is
// recorded, and the node's registration keeps it out while the release stands.
//
// A HOST NOTHING JOINED FROM has no drain to prove, so it is sealed here, in this
// transaction, on the proof that nothing was ever admitted for it: its tokens retired
// and nothing joining from it again. Whether a node joined is read here, so an approval
// that linked one first makes this the joined case.
//
// A HOST SOMETHING WAS ADMITTED FOR WITHOUT A LINK (a certificate `billet ca issue` handed
// out under its name before that name was reserved, or a request approved under it
// outside the pool) is linked to its derived name here and released as a joined host,
// so its node's certificates are revoked at the seal like any other's and a release of
// it is never stranded.
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
			return fmt.Errorf("%w: %w: pool order %s", ErrPoolOrderMoved, ErrPoolReleaseAsked, order)
		}

		if row.NodeName == "" {
			admitted, err := q.CountPoolOrderAdmissions(ctx, ledgerdb.CountPoolOrderAdmissionsParams{
				OrderID: order, NodeName: PoolNodeName(order),
			})
			if err != nil {
				return fmt.Errorf("alloc: read whether a node joined from pool order %s: %w", order, err)
			}

			if admitted == 0 {
				return sealUnjoinedTx(ctx, q, &row, reason, now, &sealed)
			}

			if n, err := q.LinkPoolOrderNode(ctx, ledgerdb.LinkPoolOrderNodeParams{
				NodeName: PoolNodeName(order), UpdatedAt: ts(now), OrderID: order, JoinHold: row.JoinHold,
			}); err != nil {
				return fmt.Errorf("alloc: link pool order %s to the node admitted under its name: %w", order, err)
			} else if n != 1 {
				return fmt.Errorf("%w: pool order %s moved while its release was asked for", ErrPoolOrderMoved, order)
			}

			if err := q.RetirePoolJoinTokens(ctx, order); err != nil {
				return fmt.Errorf("alloc: retire the join tokens of pool order %s: %w", order, err)
			}

			// WHAT THE NODE SAID BEFORE ITS ORDER KNEW IT, under a billet that recorded
			// nothing of it, cannot be read back, so a node linked here is proved again: a
			// decommission on proof taken from now on.
			if _, err := q.ContradictPoolRelease(ctx, PoolNodeName(order)); err != nil {
				return fmt.Errorf("alloc: hold pool order %s's release for a fresh proof: %w", order, err)
			}

			if err := q.DeleteBarrierRun(ctx, PoolNodeName(order)); err != nil {
				return fmt.Errorf("alloc: discard %s's idle run: %w", PoolNodeName(order), err)
			}

			row.NodeName = PoolNodeName(order)
		}

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
	})
	if err != nil {
		return false, err
	}

	return sealed, nil
}

// sealUnjoinedTx seals an order nothing was ever admitted for, on that proof, with its
// tokens retired so nothing joins from it again.
func sealUnjoinedTx(
	ctx context.Context, q state.WriteOps, row *ledgerdb.PoolOrder, reason string, now time.Time, sealed *bool,
) error {
	if _, err := releaseproof.NewUnjoined(releaseproof.Unjoined{
		Host: row.HostID, OrderSealed: now, Admitted: 0,
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrPoolReleaseWaits, err)
	}

	if err := q.RetirePoolJoinTokens(ctx, row.OrderID); err != nil {
		return fmt.Errorf("alloc: retire the join tokens of pool order %s: %w", row.OrderID, err)
	}

	if err := sealPoolOrderTx(ctx, q, row.OrderID, "", ts(now), reason, ts(now)); err != nil {
		return err
	}

	*sealed = true

	return nil
}

// SealPoolRelease seals a joined host whose release was asked for, once the ledger
// proves it holds nothing, and returns the proof its source is asked with (ADR-030).
//
// ONE TRANSACTION, so nothing is placed between the reading and the seal: the host holds
// no lease, quarantine or custody, and runs nothing by a proof that is not an inventory
// (releaseInventory). Then its certificates are revoked, first, as the release requires,
// and the order is sealed against everything. A proof that does not hold writes nothing
// and answers ErrPoolReleaseWaits.
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

		holdings, err := releaseHoldingsTx(ctx, q, row.NodeName, row.HostID, requested, row.ReleaseContradicted == 1)
		if err != nil {
			return err
		}

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

// releaseHoldingsTx reads what still holds a joined pool host for its release: its
// leases, quarantine and custody, and what it is proved to run. A registered node that
// has contradicted the order's release since its last proved decommission
// (release_contradicted) is could-not-tell; one that never registered held nothing
// whatever it said arriving, because nothing places on a node the ledger has no row for.
func releaseHoldingsTx(
	ctx context.Context, q state.ReadOps, node, host string, sealed time.Time, contradicted bool,
) (releaseproof.Holdings, error) {
	counts, err := q.CountNodeHoldings(ctx, node)
	if err != nil {
		return releaseproof.Holdings{}, fmt.Errorf("alloc: count what %s holds: %w", node, err)
	}

	holdings := releaseproof.Holdings{
		Host: host, Sealed: sealed, Leases: int(counts.Outstanding),
		Quarantine: int(counts.Quarantine), Custody: int(counts.Custody), Inventory: -1,
	}

	switch facts, err := q.ReadNodeReleaseFacts(ctx, node); {
	case errors.Is(err, sql.ErrNoRows):
		// NEVER REGISTERED, SO NEVER PLACEABLE: placement reads the node table, and a first
		// registration is held out of placement by the release and, once sealed, refused by
		// the certificates the seal revokes.
		holdings.Inventory = 0
	case err != nil:
		return releaseproof.Holdings{}, fmt.Errorf("alloc: read %s's release facts: %w", node, err)
	case contradicted:
		holdings.Inventory = -1
	default:
		holdings.Inventory = releaseInventory(&facts)
	}

	return holdings, nil
}

// releaseInventory is what a registered node is proved to run for its release: nothing
// when a person decommissioned it on the compute barrier's proof, unless an inventory of
// its current registration since reports compute, which outweighs it; otherwise
// could-not-tell (-1).
//
// AN INVENTORY IS NEVER THE PROOF (Codex, 2026-11-07): it is stamped when it arrives, not
// when the node listed it, a launch still running is in no inventory, and the barrier
// writes inventories of its own out of order with the node's sweeps. An acknowledgement
// a node gives only after its launches settle, fenced by its epoch and its dispatch
// generation, is the proof a host draining alone needs; until it exists, a joined host is
// released after `billet drain --wait` and `billet nodes decommission`.
func releaseInventory(f *ledgerdb.ReadNodeReleaseFactsRow) int {
	if f.DecommissionProven != 1 {
		return -1
	}

	if f.NodeEpoch.Valid && f.NodeEpoch.Int64 == f.Epoch && f.Running.Valid && f.Running.Int64 > 0 {
		return int(f.Running.Int64)
	}

	return 0
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
//
// AND STILL PROVES (Codex, 2026-11-07): a joined host's holdings are read again at every
// ask, because an inventory already in flight when the seal committed can report compute
// after it, and a node arriving withdraws its decommission's proof. Both are durable in
// decommission_proven, so a contradicted seal answers ErrPoolReleaseContradicted at every
// ask until a person takes the host back and closes the order.
func (a *Allocator) PoolReleaseProof(ctx context.Context, order string) (releaseproof.Proof, error) {
	var proof releaseproof.Proof

	err := a.db.View(ctx, func(tx querier) error {
		q := state.ReadQueries(tx)
		row, err := q.ReadPoolOrder(ctx, order)

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

		if row.NodeName != "" {
			holdings, err := releaseHoldingsTx(ctx, q, row.NodeName, row.HostID, sealed, row.ReleaseContradicted == 1)
			if err != nil {
				return err
			}

			if _, err := releaseproof.NewDrained(holdings); err != nil {
				return fmt.Errorf("%w: pool order %s: %w", ErrPoolReleaseContradicted, order, err)
			}
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

// ForceClosePoolOrder closes a joined host's release that was never proved, on a person's
// word that they took the host back by hand (ADR-030): the way out for a node that died, or
// lost the credential it would prove itself with, which no decommission on proof can reach
// again. It is `billet nodes decommission --force` for a pool order, and records itself as
// unproven in the order's reason.
//
// WHAT THE LEDGER CAN STILL HOLD IS HELD: a host that holds a lease, quarantine or custody
// is refused, so its capacity is settled by the force paths that exist for it
// (`billet leases release --force`, `billet force-destroy`), each its own decision. In one
// transaction, the node's certificates are revoked and it is taken out of placement before
// the order closes, as a seal would.
func (a *Allocator) ForceClosePoolOrder(ctx context.Context, id, reason string) error {
	if reason == "" {
		return fmt.Errorf("alloc: closing pool order %s without proof needs the reason a person gave", id)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		now := a.now().UTC()

		row, err := q.ReadPoolOrder(ctx, id)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderMoved, id)
		case err != nil:
			return fmt.Errorf("alloc: read pool order %s: %w", id, err)
		case row.State != PoolPlaced || row.ReleaseRequestedAt == "" || row.NodeName == "":
			return fmt.Errorf("%w: pool order %s is %s with no release of a joined host asked for", ErrPoolOrderMoved,
				id, row.State)
		}

		counts, err := q.CountNodeHoldings(ctx, row.NodeName)
		if err != nil {
			return fmt.Errorf("alloc: count what %s holds: %w", row.NodeName, err)
		}

		if counts.Outstanding > 0 {
			return fmt.Errorf("%w: %s still holds %d lease(s), %d in quarantine and %d in custody; settle them "+
				"first (`billet leases`, `billet leases release --force`, `billet force-destroy`)", ErrPoolReleaseWaits,
				row.NodeName, counts.Outstanding, counts.Quarantine, counts.Custody)
		}

		if err := revokeNodeTx(ctx, q, row.NodeName, "its pool host was closed by a person without proof: "+reason,
			now); err != nil {
			return err
		}

		if err := q.MarkNodeDrainedForRelease(ctx, row.NodeName); err != nil {
			return fmt.Errorf("alloc: take %s out of placement: %w", row.NodeName, err)
		}

		n, err := q.ForceClosePoolOrder(ctx, ledgerdb.ForceClosePoolOrderParams{
			Reason: "closed by a person without proof: " + reason, UpdatedAt: ts(now), OrderID: id,
			NodeName: row.NodeName,
		})
		if err != nil {
			return fmt.Errorf("alloc: close pool order %s: %w", id, err)
		}

		if n != 1 {
			return fmt.Errorf("%w: pool order %s moved while it was closed", ErrPoolOrderMoved, id)
		}

		return nil
	})
}

// ClosePoolOrder closes a sealed or released order on a person's word, after they have
// looked at the source themselves: the way a release closes on a source that never says
// gone of a host it took back (phoenixNAP), on one no longer configured or ordering
// elsewhere, and on a sealed host a person took back by hand. The seal, its proof and its
// revocation come first; the reason records that it was a person, not the source.
func (a *Allocator) ClosePoolOrder(ctx context.Context, id, reason string) error {
	if reason == "" {
		return fmt.Errorf("alloc: closing pool order %s needs the reason a person gave", id)
	}

	return a.movePoolOrder(ctx, id, PoolSealed+" or "+PoolReleased, func(w state.WriteOps, now string) (int64, error) {
		return w.ClosePoolOrder(ctx, ledgerdb.ClosePoolOrderParams{
			Reason: "closed by a person: " + reason, OrderID: id, UpdatedAt: now,
		})
	})
}
