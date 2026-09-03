package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RegisteredNode is the durable placement identity of one compute host.
type RegisteredNode struct {
	Name     string
	Provider config.ProviderKind
	Site     string
	Live     bool
	// Decommissioned is when a person stopped expecting this host to answer, or
	// empty. DecommissionProven says whether anything proved it was running no
	// compute at the time — an unproven exclusion is billet admitting it does not
	// know what is on that machine, and a report that renders the two the same is
	// what the whole membership rule exists to prevent.
	Decommissioned     string
	DecommissionProven bool
	DecommissionedBy   string
}

// RegisteredNodes lists every host the deployment has recorded, including an
// offline host whose placement identity is still part of the ledger.
func (a *Allocator) RegisteredNodes(ctx context.Context) ([]RegisteredNode, error) {
	var out []RegisteredNode
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListRegisteredNodes(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list registered nodes: %w", err)
		}

		for _, row := range rows {
			node := RegisteredNode{
				Name:               row.Name,
				Provider:           config.ProviderKind(row.Provider),
				Site:               row.Site,
				Decommissioned:     row.DecommissionedAt,
				DecommissionProven: row.DecommissionProven == 1,
				DecommissionedBy:   row.DecommissionActor,
			}
			if !node.Provider.Valid() {
				return fmt.Errorf("alloc: registered node %q has unknown provider %q",
					node.Name, row.Provider)
			}
			if row.Live != 0 && row.Live != 1 {
				return fmt.Errorf("alloc: registered node %q has invalid liveness %d",
					node.Name, row.Live)
			}
			node.Live = row.Live == 1
			out = append(out, node)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// NodeWire is what a host's registration said about its build, and the wire
// version this control plane settled on with it.
//
// A VERDICT, WHICH IS WHY IT IS NOT PART OF NodeInventory. That type is one
// host's own account of what it was running — telemetry about a moment that has
// passed. This is a decision this control plane made at registration, and
// putting the two in one struct invites reading either as the other.
type NodeWire struct {
	Name string
	// Live is whether the deployment can reach this host. A host it cannot reach
	// still BLOCKS retirement of the protocol it last spoke: its compute may be
	// running, and it will come back speaking whatever it spoke before.
	Live bool
	// Release is the node binary, or empty for a host that registered before a
	// registration named one.
	Release string
	// Min and Max are the range the node said it speaks, and Negotiated is the
	// version registration chose. Zero means the row predates this being
	// recorded — never that the host speaks version zero.
	Min        int
	Max        int
	Negotiated int
	// Digest is the signed release manifest that produced this host's binary, or
	// empty when nothing on that machine could say.
	//
	// THREE STATES READ OFF TWO FIELDS. Empty with a wire below
	// nodeapi.VersionNodeDigest is a build that has no way to name one; empty at or
	// above it is a host billet did not install, or one whose record no longer
	// describes its binary. Neither is a disagreement, and `billet status` says
	// which is which rather than calling both unverified.
	Digest string
	// Epoch is the fencing token this host's CURRENT registration holds.
	//
	// IT IS THE ONLY CAUSAL EVIDENCE A ROLLOUT HAS about a host coming back.
	// Release and Live are both true of a node that never left and of one that
	// went away and returned on the same binary — so a rollout watching for a
	// rollback cannot tell "still draining" from "restored and running" without
	// something that provably postdates the instruction. A registration bumps
	// this; nothing else does.
	Epoch int64
	// HighestRelease is the newest release tag this host has ever registered
	// with, or empty for a host that has only ever named something that is not
	// one.
	//
	// A REPORT, NOT A RULE. A host whose Release is provably older than this
	// is running something older than it once did, which `billet status` says so
	// a person can decide whether that was a rollout's rollback or somebody's
	// hand. Refusing the registration would break the rollback the coordinator
	// infers from exactly that re-registration.
	HighestRelease string
}

// NodeWireVersions reports which wire version every known host is on.
//
// ON THE READ-ONLY POOL. An operator asks this while the control plane is
// working, and a report must not reserve the single writer slot.
func (a *Allocator) NodeWireVersions(ctx context.Context) ([]NodeWire, error) {
	var out []NodeWire

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListNodeWireVersions(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list which wire version each host speaks: %w", err)
		}

		for _, row := range rows {
			out = append(out, NodeWire{
				Name:           row.Name,
				Live:           row.Live == 1,
				Release:        row.NodeRelease,
				Min:            int(row.WireMin),
				Max:            int(row.WireMax),
				Negotiated:     int(row.WireVersion),
				Digest:         row.NodeDigest,
				Epoch:          row.Epoch,
				HighestRelease: row.HighestRelease,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ErrWithdrawalStale means a withdrawal named a registration this ledger does
// not hold: the host has registered again since, a different process holds its
// name, or it was never registered at all. Nothing was changed.
var ErrWithdrawalStale = errors.New(
	"alloc: that withdrawal names a registration this ledger does not hold")

// NodeWithdrawn records that a host said it is leaving, and takes it out of
// placement at once.
//
// THE OPPOSITE OF NodeGone IN WHAT IT PROVES, and the same in what it writes.
// NodeGone is the control plane giving up on a host it can no longer hear — an
// observation about the jobs on it, which is why it marks them disrupted. This
// is the host itself saying, after it released its last lease, that it will not
// poll again. Nothing is being observed about anybody's job, so nothing is
// marked; and nothing is released, because a withdrawal only stops NEW work
// being aimed here — a lease escrowed against this host in the last instant is
// answered "nothing started" by the plane and handed back by the listener,
// exactly as it is after silence.
//
// FENCED ON THE EPOCH AND THE INCARNATION, and the second is not redundant with
// the first: the epoch proves no registration landed since the plane read it,
// and the incarnation proves the process asking is the one that registration
// recorded. A superseded process still holds the certificate and the name, and
// its withdrawal must not take its replacement out of the fleet.
//
// ONLY `live` MOVES. drained, the decommission columns, the inventory and the
// barrier run are all left alone: a withdrawal is not a decommission and not a
// proof, and the next registration clears what it needs to.
func (a *Allocator) NodeWithdrawn(
	ctx context.Context, name string, epoch int64, incarnation string,
) error {
	if name == "" {
		return errors.New("alloc: a withdrawal must name a host")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		// TRIMMED THE WAY RegisterNode STORED IT, so the comparison is between two
		// values written by the same rule.
		matched, err := state.WriteQueries(tx).WithdrawNode(ctx, ledgerdb.WithdrawNodeParams{
			Name:        name,
			Epoch:       epoch,
			Incarnation: strings.TrimSpace(incarnation),
		})
		if err != nil {
			return fmt.Errorf("alloc: withdraw node %s: %w", name, err)
		}

		// ZERO ROWS IS THE FENCE, not an outage: the row exists with a different
		// epoch or incarnation, or does not exist at all. Either way the process
		// asking is not the one this ledger has registered under that name.
		if matched == 0 {
			return fmt.Errorf("%w: node %s at epoch %d from process %q",
				ErrWithdrawalStale, name, epoch, incarnation)
		}

		return nil
	})
}

// ErrNotDecommissionable means a host may not be removed from the fleet's
// expected set yet, and the message says what would make it removable.
var ErrNotDecommissionable = errors.New("alloc: this host cannot be decommissioned yet")

// DecommissionRequest is one decision to stop expecting a host to answer.
type DecommissionRequest struct {
	Node  string
	Actor string
	// Force skips the checks a person is allowed to override — a host the
	// deployment can still reach, and the absence of proof. It never skips the
	// outstanding-lease check, which is about capacity rather than judgement.
	Force bool
}

// Decommission removes a host from the set a compute barrier expects to hear
// from.
//
// MEMBERSHIP IS NEEDED BECAUSE "EVERY REGISTERED NODE" NEVER CONVERGES: a node
// row is durable and a control-plane start marks the fleet unreachable rather
// than removing anything, so a host retired a year ago would block every drain
// from now on.
//
// IT MUST NOT LAUNDER UNCERTAINTY, which is the whole difficulty. If a silent
// host can simply be excluded, the next drain reports "nothing is running" while
// that host runs somebody's job. So an unproven exclusion is recorded AS
// unproven and stays that way, and every report that consumes it says so.
//
// `drained` is what it writes, reconciling a column the schema has carried since
// migration 1 that no production code has ever written. Both placement queries
// and the floor arithmetic already read `live = 1 AND drained = 0`, so there is
// nothing new to teach them.
//
// IT REPORTS WHETHER THE EXCLUSION WAS PROVED, and derives that ITSELF, inside
// this transaction. A caller cannot supply it: a proof is a statement about an
// incarnation, and a host can re-register between a caller reading its clearance
// and this write — which is exactly the change the epoch fence exists to catch.
// Passing a boolean across that gap would record a machine that had just come
// back, and may be running something, as proved idle.
func (a *Allocator) Decommission(ctx context.Context, req DecommissionRequest) (bool, error) {
	if req.Node == "" {
		return false, errors.New("alloc: a decommission must name a host")
	}

	// A PLAIN LOCAL RATHER THAN A NAMED RETURN, so nothing inside the closure can
	// shadow it with `:=` and leave every caller told the exclusion was unproven.
	// That is not hypothetical — it is what the first version did, and only a test
	// caught it.
	var proven bool

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		proven = false

		q := state.WriteQueries(tx)

		live, err := q.ReadNodeLiveness(ctx, req.Node)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s is not a host this deployment has ever registered",
				ErrNotDecommissionable, req.Node)
		case err != nil:
			return fmt.Errorf("alloc: read node %s: %w", req.Node, err)
		}

		// CAPACITY BEFORE JUDGEMENT, and this one is not overridable. A
		// decommissioned host is excluded from countOpenPerTier, so excluding one
		// that still holds leases silently changes what every tier's floor believes
		// is already met — and the capacity stays charged either way.
		outstanding, err := q.CountOutstandingLeasesOnNode(ctx, req.Node)
		if err != nil {
			return fmt.Errorf("alloc: count the leases on node %s: %w", req.Node, err)
		}

		if outstanding > 0 {
			return fmt.Errorf(
				"%w: %d lease(s) are still outstanding against %s, and their capacity is still "+
					"charged. `billet leases` shows them; `billet leases release --force` is how "+
					"an operator settles one for a machine that is never coming back",
				ErrNotDecommissionable, outstanding, req.Node)
		}

		if live == 1 && !req.Force {
			return fmt.Errorf(
				"%w: %s is still talking to this control plane, so it is not gone. Stop it, or "+
					"pass --force if you mean to exclude a host that is still reachable",
				ErrNotDecommissionable, req.Node)
		}

		// DERIVED HERE, AGAINST THE ROWS THIS TRANSACTION IS ABOUT TO WRITE.
		var proofErr error

		proven, proofErr = provedTx(ctx, tx, req.Node)
		if proofErr != nil {
			return proofErr
		}

		if !proven && !req.Force {
			return fmt.Errorf(
				"%w: nothing has proved %s is running no compute. `billet drain --wait` asks it "+
					"and every other host directly; --force records the exclusion as UNPROVEN "+
					"instead, and every later drain says so",
				ErrNotDecommissionable, req.Node)
		}

		recorded := int64(0)
		if proven {
			recorded = 1
		}

		if err := q.DecommissionNode(ctx, ledgerdb.DecommissionNodeParams{
			DecommissionedAt:   ts(a.now().UTC()),
			DecommissionProven: recorded,
			DecommissionActor:  req.Actor,
			Name:               req.Node,
		}); err != nil {
			return fmt.Errorf("alloc: decommission node %s: %w", req.Node, err)
		}

		// ITS BARRIER RUN GOES WITH IT. A host nobody expects to answer must not
		// leave an observation behind that a later barrier could read as its own.
		if err := q.DeleteBarrierRun(ctx, req.Node); err != nil {
			return fmt.Errorf("alloc: clear the barrier run of node %s: %w", req.Node, err)
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	return proven, nil
}
