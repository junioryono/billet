package server

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// These are #157: the admission order must never let a tier that room cannot
// help hold the line, and every purchase must ask it.

// fairListeners builds a control plane under `fair` over the given hosts, the
// way the CLI does, and returns its listeners in tier order.
func fairListeners(t *testing.T, tiers []config.Tier, limits alloc.Limits,
	hosts []alloc.NodeRegistration,
) (*launchLog, []*Listener) {
	t.Helper()

	a := newBareAllocator(t, limits, tiers)
	for i := range hosts {
		if _, err := a.RegisterNode(t.Context(), hosts[i]); err != nil {
			t.Fatalf("register %s: %v", hosts[i].Name, err)
		}
	}

	log := &launchLog{}
	s := New(a, nil, tiers, "eligibility-test", nil,
		WithNodeRunner(log), WithAdmissionOrder(config.AdmissionFair))

	listeners := make([]*Listener, 0, len(tiers))
	for i := range tiers {
		listeners = append(listeners, NewListener(a, tiers[i].Label, &fakeSession{}, s.listenerOpts(nil)...))
	}

	return log, listeners
}

// host is one Firecracker machine of the given size.
func host(name string, vcpu int) alloc.NodeRegistration {
	return alloc.NodeRegistration{
		Name: name, Provider: config.ProviderFirecracker,
		VCPU: vcpu, Memory: 64 * config.GiB,
	}
}

func startedTiers(log *launchLog) map[string]int {
	out := map[string]int{}
	for _, tier := range log.tiers() {
		out[tier]++
	}

	return out
}

// A TIER AT ITS OWN max_concurrent DOES NOT HOLD THE LINE.
//
// Its refusal is its own cap, which room freed anywhere else will never cure.
// Recorded as the longest waiter it would stop every other tier buying room that
// is free, for as long as its demand outran its cap — which is exactly when a
// busy repository's CI is capped to leave the rest of the fleet room.
func TestATierAtItsOwnCapDoesNotHoldTheLine(t *testing.T) {
	t.Parallel()

	capped := tierOf("a-capped", 4)
	capped.MaxConcurrent = 1
	other := tierOf("b-other", 4)

	log, listeners := fairListeners(t, []config.Tier{capped, other},
		alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]alloc.NodeRegistration{host("only", 16)})

	// Two jobs assigned, room for four, a cap of one: one starts, one cannot.
	if err := listeners[0].reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("capped tier reconcile: %v", err)
	}

	if err := listeners[1].reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("other tier reconcile: %v", err)
	}

	got := startedTiers(log)
	if got["b-other"] != 1 {
		t.Errorf("started %v: a tier waiting on its OWN cap held the line, and a tier with "+
			"12 vCPU free to use started nothing", got)
	}
}

// A TIER PINNED TO A HOST THAT IS GONE DOES NOT HOLD THE LINE.
//
// No room anywhere can place it until that host returns, so it must not stop a
// tier that the hosts which ARE here can serve. The shape this takes in real
// life is a macOS tier pinned to a Mac that is off.
func TestATierPinnedToAMissingHostDoesNotHoldTheLine(t *testing.T) {
	t.Parallel()

	pinned := tierOf("a-pinned", 4)
	pinned.Node = "gone"
	other := tierOf("b-other", 4)

	log, listeners := fairListeners(t, []config.Tier{pinned, other},
		alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]alloc.NodeRegistration{host("here", 16)})

	if err := listeners[0].reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("pinned tier reconcile: %v", err)
	}

	if err := listeners[1].reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("other tier reconcile: %v", err)
	}

	if got := startedTiers(log); got["b-other"] != 1 {
		t.Errorf("started %v: a tier pinned to a host that is gone held the line", got)
	}
}

// A TIER WAITING FOR ONE HOST DOES NOT BLOCK A TIER THAT RUNS ON ANOTHER.
//
// Fairness is about room two tiers compete for. A tier waiting for room on host
// A is not helped by making a tier that can only run on host B wait: nothing B
// frees can ever reach it. Both hosts are real and live here; only their
// eligibility differs.
func TestAWaiterOnOneHostDoesNotBlockATierOnAnother(t *testing.T) {
	t.Parallel()

	onA := tierOf("a-on-a", 8)
	onA.Node = "host-a"
	fillsA := tierOf("b-fills-a", 8)
	fillsA.Node = "host-a"
	onB := tierOf("c-on-b", 4)
	onB.Node = "host-b"

	// THE CEILING IS ABOVE WHAT THE HOSTS HOLD, so the hosts are the only thing
	// these tiers could share: a ceiling smaller than the fleet is itself a pot
	// they all buy from, and every tier would compete for it wherever it runs.
	log, listeners := fairListeners(t, []config.Tier{onA, fillsA, onB},
		alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]alloc.NodeRegistration{host("host-a", 8), host("host-b", 8)})

	// host-a is full, so a-on-a waits for room there.
	if err := listeners[1].reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("fill host-a: %v", err)
	}

	if err := listeners[0].reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("a-on-a reconcile: %v", err)
	}

	// c-on-b can only ever run on host-b, which has room.
	if err := listeners[2].reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("c-on-b reconcile: %v", err)
	}

	if got := startedTiers(log); got["c-on-b"] != 1 {
		t.Errorf("started %v: a tier waiting for room on host-a blocked a tier that can only "+
			"run on host-b", got)
	}
}

// AND A WAITER THAT SHARES A HOST STILL HOLDS THE LINE, or the three tests above
// would pass against an order that had simply stopped being fair.
func TestAWaiterThatCanGrowStillHoldsTheLine(t *testing.T) {
	t.Parallel()

	a, log, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	freeOneLease(t, a)

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile after a slot freed: %v", err)
	}

	if got := startedTiers(log); got["a-small"] != 2 || got["b-large"] != 0 {
		t.Errorf("started %v: the large tier can grow once the small jobs end, so it must "+
			"still hold the freed room", got)
	}
}

// on is a tier that could grow on these hosts, with room to spare under the
// deployment ceiling.
func on(hosts ...string) alloc.TierAdmission {
	return alloc.TierAdmission{CanGrow: true, Nodes: hosts}
}

// TIERS ON DIFFERENT HOSTS STILL COMPETE FOR THE DEPLOYMENT CEILING.
//
// A ceiling smaller than the fleet is one pot both tiers buy from: a tier pinned
// to one host and a tier pinned to another share nothing but that number, and a
// rule that looked only at hosts let a stream of small jobs on one host starve a
// large tier waiting on the other.
func TestTiersSharingTheDeploymentCeilingCompeteWhereverTheyRun(t *testing.T) {
	t.Parallel()

	shared := func(hosts ...string) alloc.TierAdmission {
		return alloc.TierAdmission{CanGrow: true, Nodes: hosts, CeilingShared: true}
	}

	q := newAdmissionQueue(config.AdmissionFair)
	q.waits("a-large", shared("host-a"))

	if q.mayBuy("b-small", shared("host-b")) {
		t.Error("a tier on another host bought room the ceiling was holding for the waiter")
	}

	// On a deployment whose ceiling is above what its hosts could hold, the hosts
	// are the only thing two tiers can share.
	if !q.mayBuy("c-elsewhere", on("host-c")) {
		t.Error("a tier on a fleet its ceiling does not cap was held behind a waiter it shares no host with")
	}
}

// A WAITER'S GROUND IS REFRESHED AND ITS PLACE IS NOT.
//
// Hosts come and go under a waiting tier. Judged by the set it had when it first
// waited, it would block tiers it no longer competes with and miss ones it now
// does; re-dated at every refusal, it would never reach the front.
func TestAWaitersHostsAreRefreshedAndItsPlaceIsKept(t *testing.T) {
	t.Parallel()

	q := newAdmissionQueue(config.AdmissionFair)
	q.waits("a-waiting", on("host-a"))

	first, ok := q.waitingSince("a-waiting")
	if !ok {
		t.Fatal("the tier was not recorded as waiting")
	}

	// It waited again, now that only host-b could serve it.
	q.now = func() time.Time { return first.Add(time.Hour) }
	q.waits("a-waiting", on("host-b"))

	if again, _ := q.waitingSince("a-waiting"); !again.Equal(first) {
		t.Errorf("the wait was re-dated to %s from %s; a tier refused on every poll would never reach the front", again, first)
	}

	if q.mayBuy("b-on-b", on("host-b")) {
		t.Error("a tier on host-b bought ahead of the waiter, which now needs host-b")
	}

	if !q.mayBuy("c-on-a", on("host-a")) {
		t.Error("a tier on host-a was still blocked by a waiter that no longer competes for it")
	}
}

// A HOST SET THAT COULD NOT BE READ COMPETES WITH EVERYONE.
//
// The scope rule is what lets a tier bypass a waiter, so an unknown must not be
// the way past it: a ledger read that failed admits the purchase only because
// the allocator still decides whether there is room, never because the tier was
// judged not to compete.
func TestAnUnknownHostSetCompetesWithEveryWaiter(t *testing.T) {
	t.Parallel()

	q := newAdmissionQueue(config.AdmissionFair)
	q.waits("a-waiting", on("host-a"))

	if q.mayBuy("b-unknown", alloc.TierAdmission{CanGrow: true}) {
		t.Error("a tier whose hosts could not be read bought ahead of a waiting tier")
	}

	if !q.mayBuy("a-waiting", alloc.TierAdmission{CanGrow: true}) {
		t.Error("the waiting tier was held behind itself")
	}

	// And a waiter whose own hosts are unknown holds every tier.
	q = newAdmissionQueue(config.AdmissionFair)
	q.waits("a-waiting", alloc.TierAdmission{CanGrow: true})

	if q.mayBuy("b-elsewhere", on("host-b")) {
		t.Error("a waiter whose hosts could not be read let another tier buy ahead of it")
	}
}

// AND A TIER SERVED RELEASES THE LINE, including one that was recorded before it
// reached its cap.
func TestAServedTierReleasesTheLine(t *testing.T) {
	t.Parallel()

	q := newAdmissionQueue(config.AdmissionFair)
	q.waits("a-waiting", on("only"))

	if q.mayBuy("b-other", on("only")) {
		t.Fatal("a tier bought ahead of the waiter")
	}

	q.served("a-waiting")

	if !q.mayBuy("b-other", on("only")) {
		t.Error("the line was still held after the waiter was served")
	}
}

// offerListeners builds the contending control plane orderedListeners builds,
// keeping each session so a test can read what its listener asked GitHub to
// claim: an offer is acquired rather than launched, so a launch log cannot see
// this path at all.
func offerListeners(t *testing.T) (*alloc.Allocator, []*fakeSession, []*Listener) {
	t.Helper()

	tiers := contenders()

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	registerHost(t, a)

	s := New(a, nil, tiers, "offer-test", nil,
		WithNodeRunner(&launchLog{}), WithAdmissionOrder(config.AdmissionFair))

	sessions := make([]*fakeSession, 0, len(tiers))
	listeners := make([]*Listener, 0, len(tiers))

	for i := range tiers {
		session := &fakeSession{}
		sessions = append(sessions, session)
		listeners = append(listeners, NewListener(a, tiers[i].Label, session, s.listenerOpts(nil)...))
	}

	return a, sessions, listeners
}

// AN OFFER ASKS THE ORDER TOO.
//
// The offer path buys escrow for exactly the offers in hand, and on a fleet with
// no room to spare that escrow is the room a waiting tier was accumulating.
// Gated on the pool and the assignment paths alone, a tier that is offered work
// on every poll took each fragment as it was freed and the waiter never reached
// a shape that fits — the same starvation as #157's other half, through the
// message GitHub sends most often.
func TestAnOfferWaitsItsTurn(t *testing.T) {
	t.Parallel()

	a, sessions, listeners := offerListeners(t)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	freeOneLease(t, a)

	// A fresh offer for the small tier, which fits the freed room exactly.
	if err := small.handle(t.Context(),
		&Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}); err != nil {
		t.Fatalf("small tier handle: %v", err)
	}

	if got := sessions[0].acquiredIDs(); len(got) > 0 {
		t.Errorf("the small tier acquired %v: an offer bought the room the longest-waiting "+
			"tier is holding", got)
	}
}

// A DIRECT ASSIGNMENT ASKS THE ORDER TOO.
//
// On GitHub.com a job arrives as an assignment with request id 0, gets an
// identity, and was bought for in the assignment loop without asking anyone. So
// a stream of small direct assignments took every fragment of freed room while
// a large waiting tier accumulated nothing: fairness protected the pool path and
// not the path that carries the work.
func TestADirectAssignmentWaitsItsTurn(t *testing.T) {
	t.Parallel()

	a, log, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	freeOneLease(t, a)

	// A fresh job for the small tier, delivered the way GitHub.com delivers it.
	if err := small.handle(t.Context(), directlyAssigned(1, "J-small", 3)); err != nil {
		t.Fatalf("small tier handle: %v", err)
	}

	if got := startedTiers(log); got["a-small"] != 2 {
		t.Errorf("started %v: a direct assignment bought the room the longest-waiting tier "+
			"is holding", got)
	}
}
