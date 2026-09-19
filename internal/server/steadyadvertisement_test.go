package server

import (
	"fmt"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// catalogue is a fleet-shaped set of tiers whose shapes sum past the ceiling,
// which is the point of a catalogue: any one of them fits, and they cannot all
// run at once. A catalogue whose entries all fit together would not exercise
// what this file is about.
func catalogue() []config.Tier {
	sizes := []int{2, 8, 16, 32, 48, 64}
	tiers := make([]config.Tier, 0, len(sizes))

	for _, vcpu := range sizes {
		entry := tier(fmt.Sprintf("t-%02dvcpu", vcpu))
		entry.VCPU = vcpu
		tiers = append(tiers, entry)
	}

	return tiers
}

// EVERY TIER IS AVAILABLE AT ONCE, AND NOTHING IS RESERVED FOR IT WHILE IDLE.
//
// GitHub.com assigns work only to a scale set that is advertising capacity, and
// tells a scale set advertising zero nothing at all — so a tier that is not
// advertising is a tier that cannot be given a job. v0.11.0 advertised one
// idle tier at a time, holding a reservation for it, and on a real fleet
// (2026-09-19, nine tiers) a job queued against an idle tier went unassigned
// for over four hours. #140.
//
// The ceiling is configuration only — the most of this shape the deployment
// could run at once — so it is steady: it does not flicker with live headroom,
// and a job that arrives when there is no room is assigned anyway and waits
// for a runner, which is GitHub's own model and the one its reference
// controller uses. Here 120 vCPU and 64 GiB against 4-GiB shapes: the memory
// ceiling binds the small tiers at 16, and the vCPU ceiling the large ones.
func TestEveryTierAdvertisesAtOnceWithNothingReserved(t *testing.T) {
	t.Parallel()

	_, listeners := wiredListeners(t, catalogue(), 120)

	want := map[string]int{
		"t-02vcpu": 16, "t-08vcpu": 15, "t-16vcpu": 7,
		"t-32vcpu": 3, "t-48vcpu": 2, "t-64vcpu": 1,
	}

	// ALL OF THEM, READ TOGETHER, because in production they are polling at the
	// same time: a design that advertises only while it holds something is
	// exactly the one that fails when every tier looks at once.
	for _, l := range listeners {
		advertised := l.steadyAdvertisement()
		if advertised != want[l.tier] {
			t.Errorf("%s advertises %d, want its ceiling %d: a tier advertising less than "+
				"it could run is a tier GitHub will not give its jobs to", l.tier, advertised,
				want[l.tier])
		}

		if held := l.idleEscrow(); held != 0 {
			t.Errorf("%s holds %d idle reservations; nothing should be reserved for a tier "+
				"that is running nothing (#116)", l.tier, held)
		}
	}
}

// directlyAssigned is the message GitHub.com sends: the job is assigned to the
// scale set outright, with no offer before it and no acquisition, and the
// statistics count it. It carries no request id of the scale set's choosing.
func directlyAssigned(id int64, job string, assigned int) *Message {
	return &Message{
		MessageID:  id,
		Statistics: &Statistics{TotalAssignedJobs: assigned},
		Assigned:   []Job{{JobID: job}},
	}
}

// A JOB GITHUB ASSIGNS STARTS A RUNNER WHEN THERE IS ROOM, and buys its capacity
// at that moment rather than finding it already set aside.
//
// With nothing reserved in advance this is the path every job takes. The
// capacity is bought atomically in the ledger, so every guarantee the
// allocator enforces — ceiling, per-node fit, floors, max_concurrent — still
// holds; only the moment of the purchase moves.
func TestADirectAssignmentStartsARunnerWhenThereIsRoom(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("a-work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)

	var launched []int64

	work := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(requestID int64) error {
			launched = append(launched, requestID)

			return nil
		},
	}))

	if err := work.handle(t.Context(), directlyAssigned(1, "J", 1)); err != nil {
		t.Fatalf("handle a direct assignment: %v", err)
	}

	if err := work.reconcilePool(t.Context(), work.observed.TotalAssignedJobs); err != nil {
		t.Fatalf("reconcile the pool against GitHub's count: %v", err)
	}

	if len(launched) != 1 {
		t.Fatalf("a job assigned with room to run it launched %d runners, want 1: with "+
			"nothing reserved in advance, the pool has to buy its capacity when it launches",
			len(launched))
	}
}

// AND WAITS WHEN THERE IS NONE, then starts when room frees. Nothing is
// overcommitted in between: the purchase is the allocator's ordinary atomic
// check, so a full fleet refuses it and the job stays assigned at GitHub until
// a later reconciliation can buy the room.
func TestADirectAssignmentWaitsWhenTheFleetIsFullAndStartsWhenRoomFrees(t *testing.T) {
	t.Parallel()

	busy, work := tier("a-busy"), tier("b-work")
	tiers := []config.Tier{busy, work}

	// ROOM FOR EXACTLY ONE of these shapes.
	a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)

	occupying, err := a.Escrow(t.Context(), busy.Label, 1)
	if err != nil || len(occupying) != 1 {
		t.Fatalf("occupy the only slot: %v, %v", occupying, err)
	}

	var launched []int64

	l := NewListener(a, work.Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(requestID int64) error {
			launched = append(launched, requestID)

			return nil
		},
	}))

	if err := l.handle(t.Context(), directlyAssigned(1, "J", 1)); err != nil {
		t.Fatalf("handle a direct assignment: %v", err)
	}

	if err := l.reconcilePool(t.Context(), l.observed.TotalAssignedJobs); err != nil {
		t.Fatalf("reconcile with the fleet full: %v", err)
	}

	if len(launched) != 0 {
		t.Fatalf("launched %d runners with the fleet full; the job must wait, not overcommit",
			len(launched))
	}

	if err := a.Release(t.Context(), occupying[0].ID, occupying[0].Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("free the slot: %v", err)
	}

	if err := l.reconcilePool(t.Context(), l.observed.TotalAssignedJobs); err != nil {
		t.Fatalf("reconcile once room is free: %v", err)
	}

	if len(launched) != 1 {
		t.Fatalf("launched %d runners once room freed, want 1: a waiting job has to start "+
			"when there is room for it", len(launched))
	}
}

// TWO TIERS ASSIGNED THE LAST SLOT START EXACTLY ONE RUNNER BETWEEN THEM.
//
// Every tier advertises its own ceiling, so GitHub can assign more work across
// tiers than the fleet can run at once. That is the design, not a fault: the
// allocator's atomic purchase decides which one runs, and the other waits.
// What must never happen is both running on room for one.
func TestTwoTiersAssignedTheLastSlotStartExactlyOne(t *testing.T) {
	t.Parallel()

	first, second := tier("a-first"), tier("b-second")
	tiers := []config.Tier{first, second}
	a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)

	var launched []string

	listen := func(label string) *Listener {
		return NewListener(a, label, &fakeSession{}, WithRunner(&fakeRunner{
			onLaunch: func(int64) error {
				launched = append(launched, label)

				return nil
			},
		}))
	}

	listeners := []*Listener{listen(first.Label), listen(second.Label)}

	for i, l := range listeners {
		if err := l.handle(t.Context(), directlyAssigned(int64(i+1), "J-"+l.tier, 1)); err != nil {
			t.Fatalf("%s: handle a direct assignment: %v", l.tier, err)
		}
	}

	for _, l := range listeners {
		if err := l.reconcilePool(t.Context(), l.observed.TotalAssignedJobs); err != nil {
			t.Fatalf("%s: reconcile: %v", l.tier, err)
		}
	}

	if len(launched) != 1 {
		t.Fatalf("started %v on room for one; exactly one tier must run and the other wait",
			launched)
	}
}

// AN ASSIGNMENT THIS LISTENER NEVER ACQUIRED BUYS ITS OWN CAPACITY TOO.
//
// GitHub can assign a request this process holds no promise for: after a
// restart, or when the offer was acquired by a control plane that is gone. It
// used to run on the reservation held in advance; with nothing held in advance,
// the assignment has to buy its lease, or a fleet with room declines it.
func TestAnUnpromisedAssignmentBuysItsCapacityWhenThereIsRoom(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("a-work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)

	var launched []int64

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(requestID int64) error {
			launched = append(launched, requestID)

			return nil
		},
	}))

	msg := &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}
	if err := l.handle(t.Context(), msg); err != nil {
		t.Fatalf("handle an unpromised assignment: %v", err)
	}

	if len(launched) != 1 || launched[0] != 11 {
		t.Fatalf("launched %v for an assignment with room to run it, want [11]", launched)
	}

	if held := l.idleEscrow(); held != 0 {
		t.Errorf("holds %d idle reservations after the launch, want 0", held)
	}
}

// AND STARTS NOTHING WHEN THERE IS NO ROOM, rather than overcommitting.
func TestAnUnpromisedAssignmentOnAFullFleetStartsNothing(t *testing.T) {
	t.Parallel()

	busy, work := tier("a-busy"), tier("b-work")
	tiers := []config.Tier{busy, work}
	a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)

	if occupying, err := a.Escrow(t.Context(), busy.Label, 1); err != nil || len(occupying) != 1 {
		t.Fatalf("occupy the only slot: %v, %v", occupying, err)
	}

	var launched []int64

	l := NewListener(a, work.Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(requestID int64) error {
			launched = append(launched, requestID)

			return nil
		},
	}))

	msg := &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}
	if err := l.handle(t.Context(), msg); err != nil {
		t.Fatalf("handle an unpromised assignment on a full fleet: %v", err)
	}

	if len(launched) != 0 {
		t.Fatalf("launched %v with the fleet full; nothing may run without capacity", launched)
	}
}
