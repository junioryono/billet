package alloc

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

// answerClearance records one answer of node to order pa's clearance, taken against the
// fence the ledger holds now.
func answerClearance(t *testing.T, a *Allocator, node string, empty bool) {
	t.Helper()
	answerClearanceAt(t, a, node, fenceOf(t, a, node), empty)
}

// answerClearanceAt records one answer taken against fence. An empty one reaches the
// node's inventory first, as the plane's ask does (inventoryOf); a non-empty one is
// recorded alone, so what ends the run is the clearance's own reading of it.
func answerClearanceAt(t *testing.T, a *Allocator, node string, fence NodeFence, empty bool) {
	t.Helper()
	if empty {
		if _, err := a.ResolveQuarantineFor(t.Context(), node, nil, fence.Epoch); err != nil {
			t.Fatalf("record the empty inventory: %v", err)
		}
	}
	if err := a.RecordPoolClearance(t.Context(), PoolClearanceObservation{
		Order: "pa", Node: node, Fence: fence, Empty: empty,
	}); err != nil {
		t.Fatalf("record a clearance answer: %v", err)
	}
}

// drainingPoolNode is order pa's node, joined and registered, its arrival reported as
// every registration's is, and its release asked for.
func drainingPoolNode(t *testing.T, a *Allocator) string {
	t.Helper()
	node := joinedPoolNode(t, a)
	if err := a.InvalidateBarrierRun(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	if sealed, err := a.RequestPoolRelease(t.Context(), "pa", "the pool shrinks"); err != nil || sealed {
		t.Fatalf("ask for the release: sealed %v, %v", sealed, err)
	}
	if !readOrder(t, a, "pa").ReleaseContradicted {
		t.Fatal("the node's arrival did not contradict the release, which every registration does")
	}

	return node
}

// A POOL HOST DRAINING ALONE IS SEALED ON TWO EMPTY ANSWERS SPANNING THE GRACE (ADR-030),
// with the deployment open the whole time: one answer proves nothing however long ago it
// was, and the second, at or after the grace, seals the order and revokes its node's
// certificate though its arrival contradicted the release, because that arrival ended
// any run before it. A sealed order is not asked again.
func TestAPoolHostDrainingAloneIsSealedOnTwoAnswers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := drainingPoolNode(t, a)

	if targets, err := a.PoolClearanceTargets(ctx); err != nil ||
		!slices.Equal(targets, []PoolClearanceTarget{{Order: "pa", Node: node}}) {
		t.Fatalf("the nodes releases wait on: %v, %v", targets, err)
	}

	answerClearance(t, a, node, true)
	first := now
	if o := readOrder(t, a, "pa"); !o.ClearanceSince.Equal(first) || !o.ClearanceObserved.Equal(first) {
		t.Fatalf("the run after one answer: since %v, observed %v, want both %v", o.ClearanceSince,
			o.ClearanceObserved, first)
	}
	waitsUnsealed(t, a, "0a", "one empty answer")
	now = now.Add(time.Hour)
	waitsUnsealed(t, a, "0a", "one empty answer and an hour")

	answerClearance(t, a, node, true)
	if o := readOrder(t, a, "pa"); !o.ClearanceSince.Equal(first) || !o.ClearanceObserved.Equal(now) {
		t.Fatalf("the run after two answers: since %v, observed %v, want %v and %v", o.ClearanceSince,
			o.ClearanceObserved, first, now)
	}
	if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
		t.Fatalf("sealing on two empty answers spanning the grace: %v", err)
	}
	if revoked, err := a.CertRevoked(ctx, "0a"); err != nil || !revoked {
		t.Fatalf("the node's certificate was not revoked: %v, %v", revoked, err)
	}
	if _, sealed, err := a.AdmissionGeneration(ctx); err != nil || sealed {
		t.Fatalf("the deployment was sealed for a pool host's release: %v, %v", sealed, err)
	}
	if targets, err := a.PoolClearanceTargets(ctx); err != nil || len(targets) != 0 {
		t.Fatalf("a sealed order is still asked: %v, %v", targets, err)
	}

	// THE NODE LEAVES THE FLEET PROVED, so a later drain does not wait on a host its
	// source takes back.
	c := clearance(t, a)
	if !slices.ContainsFunc(c.Excluded, func(e Exclusion) bool { return e.Node == node && e.Proven }) {
		t.Fatalf("the sealed host's node is not excluded from the fleet as proved: %+v", c.Excluded)
	}

	// AN ANSWER ARRIVING AFTER THE SEAL does not end the run the proof is read from.
	answerClearance(t, a, node, false)
	if _, err := a.PoolReleaseProof(ctx, "pa"); err != nil {
		t.Fatalf("the sealed order's proof after a late answer: %v", err)
	}
}

// A CURRENT INVENTORY REPORTING COMPUTE OUTWEIGHS A PROVED RUN, read in the same
// transaction: on PostgreSQL a read sees each statement's own snapshot, so a contradiction
// committed between reading the run and reading the node is seen only as the inventory.
// Staged here by writing the inventory alone, as that read would see it.
func TestACurrentInventoryOutweighsAProvedClearance(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := drainingPoolNode(t, a)
	answerClearance(t, a, node, true)
	now = now.Add(computeAbsenceGrace)
	answerClearance(t, a, node, true)

	reportCompute := func(running int) {
		t.Helper()
		epoch := fenceOf(t, a, node).Epoch
		if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO node_inventory (node, node_epoch, received_at, running)
				VALUES ($1, $2, 'x', $3) ON CONFLICT(node) DO UPDATE SET node_epoch = excluded.node_epoch,
				running = excluded.running`, node, epoch, running)

			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	reportCompute(1)
	waitsUnsealed(t, a, "0a", "a proved run beside an inventory reporting compute")
	reportCompute(0)
	if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
		t.Fatalf("sealing once the inventory is empty: %v", err)
	}
	if refused, err := a.PoolReleaseRefused(ctx, "pa"); err != nil || refused {
		t.Fatalf("a proved seal reads as refused: %v, %v", refused, err)
	}
	reportCompute(1)
	if _, err := a.PoolReleaseProof(ctx, "pa"); !errors.Is(err, ErrPoolReleaseContradicted) {
		t.Fatalf("the proof beside an inventory reporting compute answered %v", err)
	}
	if refused, err := a.PoolReleaseRefused(ctx, "pa"); err != nil || !refused {
		t.Fatalf("PoolReleaseRefused: %v, %v", refused, err)
	}
}

// AN ANSWER THE CLEARANCE CANNOT STAND ON ENDS ITS RUN: a lease on the host, a launch
// dispatched since the question, a registration since it, or an inventory that is not
// empty. The answers after it start again, so the release is sealed only on two that
// span the grace after it.
func TestAnAnswerTheClearanceCannotStandOnEndsTheRun(t *testing.T) {
	t.Parallel()
	for name, interrupt := range map[string]func(t *testing.T, a *Allocator, node string, lease string){
		"a lease on the host": func(t *testing.T, a *Allocator, node, lease string) {
			t.Helper()
			setLeasePhase(t, a, lease, PhaseBusy)
			answerClearance(t, a, node, true)
			setLeasePhase(t, a, lease, PhaseDone)
		},
		"a launch dispatched since the question": func(t *testing.T, a *Allocator, node, _ string) {
			t.Helper()
			fence := fenceOf(t, a, node)
			if _, err := a.BumpDispatch(t.Context(), node); err != nil {
				t.Fatal(err)
			}
			answerClearanceAt(t, a, node, fence, true)
		},
		"a registration since the question": func(t *testing.T, a *Allocator, node, _ string) {
			t.Helper()
			fence := fenceOf(t, a, node)
			registerPoolNode(t, a, node)
			answerClearanceAt(t, a, node, fence, true)
		},
		"an inventory that is not empty": func(t *testing.T, a *Allocator, node, _ string) {
			t.Helper()
			answerClearance(t, a, node, false)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			a := poolAllocator(t, &now)
			ctx := t.Context()
			node := joinedPoolNode(t, a)
			leases, err := a.Escrow(ctx, "intel", 1)
			if err != nil || len(leases) != 1 {
				t.Fatalf("a lease on the host: %v, %v", leases, err)
			}
			if _, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil {
				t.Fatal(err)
			}
			if err := a.Release(ctx, leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
				t.Fatal(err)
			}

			answerClearance(t, a, node, true)
			now = now.Add(computeAbsenceGrace / 2)
			interrupt(t, a, node, leases[0].ID)
			// A FULL GRACE AFTER THE INTERRUPTION, so an interruption recorded as an answer
			// would make this run proved, and only its end keeps the release waiting.
			now = now.Add(computeAbsenceGrace)
			answerClearance(t, a, node, true)
			waitsUnsealed(t, a, "0a", name)

			now = now.Add(computeAbsenceGrace)
			answerClearance(t, a, node, true)
			if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
				t.Fatalf("sealing on two answers after %s: %v", name, err)
			}
		})
	}
}

// AN ANSWER TAKEN BEFORE A LAUNCH OR A REGISTRATION IS NO ANSWER ABOUT THE HOST AFTER IT,
// and a run proved before one is no proof after it: an answer recorded against the fence
// read before the launch was dispatched, or the node registered again, begins no run, so
// the answer after it starts one; and a proved run stops proving once either moves.
func TestAFenceThatMovedOutdatesTheClearance(t *testing.T) {
	t.Parallel()
	for name, move := range map[string]func(t *testing.T, a *Allocator, node string){
		"a launch dispatched": func(t *testing.T, a *Allocator, node string) {
			t.Helper()
			if _, err := a.BumpDispatch(t.Context(), node); err != nil {
				t.Fatal(err)
			}
		},
		"a registration": func(t *testing.T, a *Allocator, node string) {
			t.Helper()
			registerPoolNode(t, a, node)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			a := poolAllocator(t, &now)
			ctx := t.Context()
			node := drainingPoolNode(t, a)

			before := fenceOf(t, a, node)
			move(t, a, node)
			answerClearanceAt(t, a, node, before, true)
			now = now.Add(computeAbsenceGrace)
			answerClearance(t, a, node, true)
			waitsUnsealed(t, a, "0a", "an answer taken before "+name+" and one after it")

			now = now.Add(computeAbsenceGrace)
			answerClearance(t, a, node, true)
			move(t, a, node)
			waitsUnsealed(t, a, "0a", "a run proved before "+name)

			answerClearance(t, a, node, true)
			now = now.Add(computeAbsenceGrace)
			answerClearance(t, a, node, true)
			if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
				t.Fatalf("sealing on a run after %s: %v", name, err)
			}
		})
	}
}

// setLeasePhase moves a lease's phase by hand, which nothing billet does to a lease on a
// host out of placement; it isolates the clearance's own reading of the ledger.
func setLeasePhase(t *testing.T, a *Allocator, lease string, phase Phase) {
	t.Helper()
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE leases SET phase = $1 WHERE id = $2`, string(phase), lease)

		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A CONTRADICTION ENDS THE CLEARANCE RUN IN THE STATEMENT THAT RECORDS IT: an answer
// before it does not count towards the grace after it, before the seal; and after the
// seal the release proof is refused.
func TestAContradictionEndsTheClearanceRun(t *testing.T) {
	t.Parallel()
	for name, contradict := range contradictions {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			a := poolAllocator(t, &now)
			ctx := t.Context()
			node := drainingPoolNode(t, a)

			answerClearance(t, a, node, true)
			now = now.Add(computeAbsenceGrace / 2)
			contradict(t, a, node)
			now = now.Add(computeAbsenceGrace / 2)
			answerClearance(t, a, node, true)
			waitsUnsealed(t, a, "0a", "a run across a contradiction")

			now = now.Add(computeAbsenceGrace)
			answerClearance(t, a, node, true)
			if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
				t.Fatalf("sealing on a run after the contradiction: %v", err)
			}
			contradict(t, a, node)
			if _, err := a.PoolReleaseProof(ctx, "pa"); !errors.Is(err, ErrPoolReleaseContradicted) {
				t.Fatalf("the proof after a contradiction since the seal answered %v", err)
			}
		})
	}
}

// ONLY A DRAINING ORDER'S NODE IS ASKED, AND ONLY WHILE NOTHING HOLDS IT: an order no
// release was asked of, one whose node holds a lease, and an answer recorded against an
// order that is not this node's are none of the clearance's.
func TestOnlyADrainingOrdersIdleNodeIsAsked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := joinedPoolNode(t, a)
	if targets, err := a.PoolClearanceTargets(ctx); err != nil || len(targets) != 0 {
		t.Fatalf("an order no release was asked of is asked: %v, %v", targets, err)
	}
	answerClearance(t, a, node, true)
	now = now.Add(computeAbsenceGrace)
	answerClearance(t, a, node, true)
	if o := readOrder(t, a, "pa"); !o.ClearanceSince.IsZero() {
		t.Fatalf("answers before any release was asked for began a run: %+v", o)
	}

	leases, err := a.Escrow(ctx, "intel", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("a lease on the host: %v, %v", leases, err)
	}
	if _, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil {
		t.Fatal(err)
	}
	if targets, err := a.PoolClearanceTargets(ctx); err != nil || len(targets) != 0 {
		t.Fatalf("a node holding a lease is asked: %v, %v", targets, err)
	}

	// AN ANSWER UNDER ANOTHER NODE'S NAME neither begins nor ends this order's run.
	if err := a.Release(ctx, leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
		t.Fatal(err)
	}
	answerClearance(t, a, node, true)
	standing := readOrder(t, a, "pa").ClearanceSince
	if standing.IsZero() {
		t.Fatal("the order's own node's answer began no run")
	}
	if err := a.RecordPoolClearance(ctx, PoolClearanceObservation{
		Order: "pa", Node: "someone-else", Fence: fenceOf(t, a, node), Empty: false,
	}); err != nil {
		t.Fatal(err)
	}
	if o := readOrder(t, a, "pa"); !o.ClearanceSince.Equal(standing) {
		t.Fatalf("an answer from another node moved the run: %+v", o)
	}
}

// A BARRIER ANSWER LISTING COMPUTE CONTRADICTS A RELEASE, whatever became of the barrier
// and though the inventory it carried was never recorded: after the seal the proof is
// refused, and the contradiction is the same one a clearance answer records.
func TestABarrierAnswerListingComputeContradictsARelease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := drainingPoolNode(t, a)
	answerClearance(t, a, node, true)
	now = now.Add(computeAbsenceGrace)
	answerClearance(t, a, node, true)
	if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
		t.Fatalf("seal: %v", err)
	}

	if err := a.RecordBarrierObservation(ctx, BarrierObservation{
		Node: node, BarrierID: "no-barrier-in-force", Fence: fenceOf(t, a, node), Running: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PoolReleaseProof(ctx, "pa"); !errors.Is(err, ErrPoolReleaseContradicted) {
		t.Fatalf("the proof after a barrier answer listing compute answered %v", err)
	}
}

// A CLEARANCE ANSWER LISTING COMPUTE ENDS THE NODE'S BARRIER RUN TOO, so a decommission
// after it is not proved by a run taken before the compute listed, and the contradiction
// stands.
func TestAClearanceListingComputeLeavesNoOlderRunToDecommissionOn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := drainingPoolNode(t, a)
	barrier := requestBarrier(t, a)
	observeNode(t, a, barrier, node, true)
	now = now.Add(computeAbsenceGrace)
	observeNode(t, a, barrier, node, true)

	if err := a.RecordPoolClearance(ctx, PoolClearanceObservation{
		Order: "pa", Node: node, Fence: fenceOf(t, a, node), Running: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.NodeGone(ctx, node, fenceOf(t, a, node).Epoch); err != nil {
		t.Fatal(err)
	}
	if proven, err := a.Decommission(ctx, DecommissionRequest{Node: node, Actor: "ops"}); err == nil || proven {
		t.Fatalf("a decommission proved by a run older than the compute listed: proved %v, %v", proven, err)
	}
	waitsUnsealed(t, a, "0a", "compute listed after a barrier run")
}

// COMPUTE A NODE LISTED IS A CONTRADICTION WHATEVER THE ORDER BECAME SINCE IT WAS ASKED:
// an answer of its current registration listing compute, recorded after the seal, refuses
// the release proof, though the inventory the same answer carried was never recorded;
// one of an earlier registration says nothing about the host now. A listing read after
// the order moved on is neither a refusal nor an error.
func TestComputeListedAfterTheSealContradictsTheRelease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := drainingPoolNode(t, a)
	answerClearance(t, a, node, true)
	now = now.Add(computeAbsenceGrace)
	answerClearance(t, a, node, true)
	if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
		t.Fatalf("seal: %v", err)
	}

	listed := func(fence NodeFence) {
		t.Helper()
		if err := a.RecordPoolClearance(ctx, PoolClearanceObservation{
			Order: "pa", Node: node, Fence: fence, Running: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	earlier := fenceOf(t, a, node)
	earlier.Epoch--
	listed(earlier)
	if _, err := a.PoolReleaseProof(ctx, "pa"); err != nil {
		t.Fatalf("an earlier registration's listing refused the proof: %v", err)
	}

	listed(fenceOf(t, a, node))
	if _, err := a.PoolReleaseProof(ctx, "pa"); !errors.Is(err, ErrPoolReleaseContradicted) {
		t.Fatalf("the proof after compute listed since the seal answered %v", err)
	}

	if err := a.ClosePoolOrder(ctx, "pa", "took it back by hand"); err != nil {
		t.Fatal(err)
	}
	if refused, err := a.PoolReleaseRefused(ctx, "pa"); err != nil || refused {
		t.Fatalf("a closed order's refusal read: %v, %v", refused, err)
	}
}
