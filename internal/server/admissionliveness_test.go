package server

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// A WAITER WHOSE LISTENER STALLS STOPS HOLDING THE LINE, AND TAKES IT BACK FROM
// ITS ORIGINAL PLACE WHEN IT PROGRESSES.
//
// On 2026-09-23 the longest waiter's listener sat on a dead connection to GitHub
// for 18 minutes. It could neither buy nor say its demand had gone, and every
// tier sharing its host declined every assignment with 78 of 136 vCPU free.
// Here the large tier waits and then its listener goes silent: the small tier
// is held while the waiter is fresh, buys once the waiter has made no progress
// for the allowance, and is held again the moment the waiter progresses, which
// finds it still first in line.
func TestAStalledWaiterStopsHoldingTheLineAndKeepsItsPlace(t *testing.T) {
	t.Parallel()

	a, log, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	clock := time.Date(2026, 9, 23, 15, 48, 0, 0, time.UTC)
	small.order.now = func() time.Time { return clock }

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}
	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}
	since, _, waiting := large.order.waitingSince("b-large")
	if !waiting {
		t.Fatal("the large tier found no room and is not recorded as waiting")
	}

	// A small job finishes. The large tier is fresh, so it holds the line.
	freeOneLease(t, a)
	clock = clock.Add(WaiterAllowance - time.Second)
	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile beside a fresh waiter: %v", err)
	}
	if got := log.tiers(); len(got) != 2 {
		t.Fatalf("started %v: the small tier bought past a waiter still inside its allowance", got)
	}

	// The large tier's listener has now made no progress for longer than the
	// allowance. The small tier may buy the freed room.
	clock = clock.Add(2 * time.Second)
	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile beside a stalled waiter: %v", err)
	}
	if got := log.tiers(); len(got) != 3 || got[2] != "a-small" {
		t.Fatalf("started %v: a waiter whose listener has stalled still held every tier sharing its host", got)
	}

	// The large tier's listener comes back. No room, so it waits again, from the
	// place it first took.
	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile after its stall: %v", err)
	}
	again, _, waiting := large.order.waitingSince("b-large")
	if !waiting || !again.Equal(since) {
		t.Fatalf("the returning waiter's place is %v (waiting %v), want its first refusal %v", again, waiting, since)
	}

	freeOneLease(t, a)
	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile after the waiter progressed: %v", err)
	}
	if got := log.tiers(); len(got) != 3 {
		t.Fatalf("started %v: the small tier bought past a waiter that is progressing again", got)
	}

	freeOneLease(t, a)
	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile once its shape fits: %v", err)
	}
	if got := log.tiers(); len(got) != 4 || got[3] != "b-large" {
		t.Fatalf("started %v, want the large tier to run once its shape fit", got)
	}
}

// A LAUNCH IN FLIGHT IS PROGRESS, HOWEVER LONG THE NODE TAKES. A launch can run
// for the node's whole command timeout, far past the allowance, and a waiter
// doing one is not stalled.
func TestALaunchInFlightKeepsAWaiterHoldingTheLine(t *testing.T) {
	t.Parallel()

	clock := time.Date(2026, 9, 23, 15, 48, 0, 0, time.UTC)
	q := newAdmissionQueue(config.AdmissionFair)
	q.now = func() time.Time { return clock }

	shared := alloc.TierAdmission{CanGrow: true, Nodes: []string{"ubuntu-01"}}

	q.waits("b-large", shared)
	q.launchBegins("b-large")

	clock = clock.Add(10 * WaiterAllowance)
	if q.mayBuy("a-small", shared) {
		t.Fatal("a waiter with a launch in flight was treated as stalled")
	}

	// The launch finishing is progress: the allowance starts again from it.
	q.launchEnds("b-large")
	clock = clock.Add(WaiterAllowance - time.Second)
	if q.mayBuy("a-small", shared) {
		t.Fatal("the allowance did not restart when the waiter's launch finished")
	}

	clock = clock.Add(2 * time.Second)
	if !q.mayBuy("a-small", shared) {
		t.Fatal("a waiter with nothing in flight and no progress for the allowance still held the line")
	}

	// A stalled waiter is still first for itself: it is never let past itself.
	if !q.mayBuy("b-large", shared) {
		t.Fatal("a stalled waiter was refused its own purchase")
	}
}
