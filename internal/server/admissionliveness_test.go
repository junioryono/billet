package server

import (
	"context"
	"sync/atomic"
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
}

// A STALLED WAITER IS NEVER LET PAST ITSELF. The oldest waiter's listener comes
// back after its allowance, while a younger waiter is fresh: the oldest buys
// first, and the younger one buys past nothing that is still holding the line.
func TestAStalledOldestWaiterStillBuysAheadOfAYoungerOne(t *testing.T) {
	t.Parallel()

	clock := time.Date(2026, 9, 23, 15, 48, 0, 0, time.UTC)
	q := newAdmissionQueue(config.AdmissionFair)
	q.now = func() time.Time { return clock }

	shared := alloc.TierAdmission{CanGrow: true, Nodes: []string{"ubuntu-01"}}

	q.waits("b-oldest", shared)
	clock = clock.Add(time.Minute)
	q.waits("a-younger", shared)
	clock = clock.Add(WaiterAllowance - 30*time.Second)

	// b-oldest last progressed an allowance and a half-minute ago; a-younger
	// half a minute inside it.
	if !q.mayBuy("b-oldest", shared) {
		t.Fatal("the oldest waiter was held behind a younger one because its own record was skipped as stalled")
	}
	if !q.mayBuy("a-younger", shared) {
		t.Fatal("the younger waiter was held behind an oldest waiter whose listener has stalled")
	}
}

// slowLauncher holds the launch of one tier until released, and says when it
// has begun, so a test can move the clock while a waiter is mid-launch.
type slowLauncher struct {
	launchLog

	tier    string
	began   chan struct{}
	release chan struct{}
}

func (s *slowLauncher) Launch(ctx context.Context, lease *alloc.Lease, job Job) error {
	if lease.Tier == s.tier {
		close(s.began)
		<-s.release
	}

	return s.launchLog.Launch(ctx, lease, job)
}

// A WAITER IN THE MIDDLE OF A LAUNCH HOLDS THE LINE, THROUGH THE LISTENER. A
// launch can take the node's whole command timeout; the listener's own launch
// path is what marks it in flight, so a waiter doing one is not treated as
// stalled however long it takes.
func TestAWaiterMidLaunchHoldsTheLineThroughTheListener(t *testing.T) {
	t.Parallel()

	tiers := contenders()
	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: 12, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	registerHost(t, a)

	runner := &slowLauncher{tier: "b-large", began: make(chan struct{}), release: make(chan struct{})}
	s := New(a, nil, tiers, "order-test", nil, WithNodeRunner(runner), WithAdmissionOrder(config.AdmissionFair))
	small := NewListener(a, tiers[0].Label, &fakeSession{}, s.listenerOpts(nil)...)
	large := NewListener(a, tiers[1].Label, &fakeSession{}, s.listenerOpts(nil)...)

	var clock atomic.Int64
	clock.Store(time.Date(2026, 9, 23, 15, 48, 0, 0, time.UTC).UnixNano())
	small.order.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }

	// Two small runners take 8 of 12 vCPU; the large tier finds 4 and waits.
	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}
	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}
	if _, _, waiting := large.order.waitingSince("b-large"); !waiting {
		t.Fatal("the large tier found no room and is not recorded as waiting")
	}

	// A small job ends; the large tier buys the 8 vCPU and starts a launch that
	// takes ten allowances.
	freeOneLease(t, a)
	done := make(chan error, 1)
	go func() { done <- large.reconcilePool(t.Context(), 1) }()
	<-runner.began

	clock.Add(int64(10 * WaiterAllowance))
	freeOneLease(t, a)
	if small.order.mayBuy("a-small", small.admission(t.Context())) {
		t.Error("a waiter in the middle of a launch was treated as stalled and let a competing tier past it")
	}

	close(runner.release)
	if err := <-done; err != nil {
		t.Fatalf("large tier reconcile through its slow launch: %v", err)
	}
	if got := runner.tiers(); len(got) != 3 || got[2] != "b-large" {
		t.Fatalf("started %v, want the large tier's launch to have completed", got)
	}
}
