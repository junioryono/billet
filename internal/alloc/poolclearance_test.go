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

// answerClearanceAt records one answer taken against fence.
func answerClearanceAt(t *testing.T, a *Allocator, node string, fence NodeFence, empty bool) {
	t.Helper()
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
	waitsUnsealed(t, a, "0a", "one empty answer")
	now = now.Add(time.Hour)
	waitsUnsealed(t, a, "0a", "one empty answer and an hour")

	answerClearance(t, a, node, true)
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
	if _, err := a.PoolReleaseProof(ctx, "pa"); err != nil {
		t.Fatalf("the sealed order's proof: %v", err)
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
			now = now.Add(computeAbsenceGrace / 2)
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

	if err := a.RecordPoolClearance(ctx, PoolClearanceObservation{
		Order: "pa", Node: "someone-else", Fence: fenceOf(t, a, node), Empty: true,
	}); err != nil {
		t.Fatal(err)
	}
	if o := readOrder(t, a, "pa"); o.ClearanceSince != (time.Time{}) {
		t.Fatalf("an answer from another node began the run: %+v", o)
	}
}
