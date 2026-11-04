package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// holdsOf is who holds which host now.
func holdsOf(q *admissionQueue) map[string]string {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.planHolds()

	out := map[string]string{}

	for label := range q.waiting {
		if held := q.waiting[label].held; held != "" {
			out[held] = label
		}
	}

	return out
}

func candidatesOn(hosts ...string) []alloc.HoldCandidate {
	out := make([]alloc.HoldCandidate, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, alloc.HoldCandidate{Node: host})
	}

	return out
}

// wantsHosts is a waiting tier and the hosts it could be held.
type wantsHosts struct {
	label string
	hosts []string
}

// holdQueue is a queue under hold with each tier waiting a second after the last, each
// able to be held the hosts given.
func holdQueue(waiters ...wantsHosts) *admissionQueue {
	q := newAdmissionQueue(config.AdmissionHold)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	for _, w := range waiters {
		q.waiting[w.label] = waitingTier{since: at, where: on("a", "b", "c"), progress: at, candidates: candidatesOn(w.hosts...)}
		at = at.Add(time.Second)
	}

	q.now = func() time.Time { return at }

	return q
}

// AT MOST ONE HOLD PER HOST AND ONE PER TIER, oldest waiter first, each taking the first
// of its candidates nobody older has: what is predicted soonest for it.
func TestHoldsGoOldestFirstOnePerHostAndTier(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"oldest", []string{"b", "a"}}, wantsHosts{"second", []string{"b", "a", "c"}},
		wantsHosts{"third", []string{"b", "a"}}, wantsHosts{"none", nil})

	got := holdsOf(q)
	want := map[string]string{"b": "oldest", "a": "second"}

	if len(got) != len(want) || got["b"] != want["b"] || got["a"] != want["a"] {
		t.Errorf("the holds are %v; want %v", got, want)
	}
}

// A HOLD STAYS WHERE IT IS WHILE THAT HOST IS STILL A CANDIDATE: room accumulated on one
// host is lost by moving to another, and a prediction moves with every job that ends.
func TestAHoldStaysOnItsHostWhileThatIsACandidate(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"waiter", []string{"a", "b"}})

	if got := holdsOf(q); got["a"] != "waiter" {
		t.Fatalf("the holds are %v; want a", got)
	}

	q.mayHold("waiter", candidatesOn("b", "a"))

	if got := holdsOf(q); got["a"] != "waiter" || got["b"] != "" {
		t.Errorf("a new prediction moved the hold: %v", got)
	}

	// A host no longer a candidate is given up.
	q.mayHold("waiter", candidatesOn("b"))

	if got := holdsOf(q); got["b"] != "waiter" || got["a"] != "" {
		t.Errorf("the hold stayed on a host no longer a candidate: %v", got)
	}

	// And a younger waiter keeps its host though an older one now wants it first.
	q = holdQueue(wantsHosts{"older", []string{"a"}}, wantsHosts{"younger", []string{"b"}})
	holdsOf(q)
	q.mayHold("older", candidatesOn("b", "a"))

	if got := holdsOf(q); got["b"] != "younger" || got["a"] != "older" {
		t.Errorf("an older waiter's new first choice took a younger waiter's host: %v", got)
	}
}

// A FREED HOLD GOES TO THE OLDEST WAITER: when its owner is served, the host passes on
// and the rest are planned again in waiting order.
func TestAFreedHoldGoesToTheOldestWaiter(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"owner", []string{"a"}}, wantsHosts{"older", []string{"a"}},
		wantsHosts{"younger", []string{"a"}})

	if got := holdsOf(q); got["a"] != "owner" {
		t.Fatalf("the holds are %v; want the oldest", got)
	}

	q.served("owner")

	if got := holdsOf(q); got["a"] != "older" {
		t.Errorf("the freed hold went to %q; want the oldest waiter left", got["a"])
	}
}

// A WAITER THAT STALLED LOSES ITS HOLD at the next plan, which bounds what a dead
// listener can cost, and takes it up again when it progresses.
func TestAStalledWaiterLosesItsHold(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"stalled", []string{"a"}}, wantsHosts{"healthy", []string{"a"}})

	if got := holdsOf(q); got["a"] != "stalled" {
		t.Fatalf("the holds are %v", got)
	}

	start := q.now()
	q.now = func() time.Time { return start.Add(WaiterAllowance + time.Second) }

	healthy := q.waiting["healthy"]
	healthy.progress = q.now()
	q.waiting["healthy"] = healthy

	if got := holdsOf(q); got["a"] != "healthy" {
		t.Errorf("the host is held for %q; want the waiter that still progresses", got["a"])
	}
}

// A RESTART REBUILDS THE HOLD: nothing is kept but the waiting order, and a new queue
// over the same reads picks the same host at the first refusal.
func TestARestartRebuildsTheHold(t *testing.T) {
	t.Parallel()

	first := newAdmissionQueue(config.AdmissionHold)
	again := newAdmissionQueue(config.AdmissionHold)

	for _, q := range []*admissionQueue{first, again} {
		q.waits("wide", on("a", "b"))
		q.mayHold("wide", candidatesOn("b", "a"))
	}

	if got, want := holdsOf(again), holdsOf(first); got["b"] != "wide" || got["b"] != want["b"] {
		t.Errorf("after a restart the holds are %v; before it %v", got, want)
	}
}

// THE OWNER OF A HOLD MAY USE IT AND NOBODY ELSE MAY; every other host stays open, and
// waiters that merely share a host no longer hold each other back.
func TestAHeldHostIsAvoidedByEveryoneButItsOwner(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"owner", []string{"a"}}, wantsHosts{"waiter", nil})

	if got := q.avoiding("owner"); len(got) != 0 {
		t.Errorf("the owner avoids %v", got)
	}

	if got := q.avoiding("waiter"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("a waiter avoids %v; want the held host", got)
	}

	if got := q.avoiding("nobody"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("a buyer with no wait avoids %v; want the held host", got)
	}

	if !q.mayBuy("waiter", on("a", "b", "c")) {
		t.Error("a waiter sharing a host with an older one was held back, under hold")
	}

	// Under fair the same waiters hold each other, and nothing is held.
	fair := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(fair, waiterAt{"older", on("a")}, waiterAt{"younger", on("a")})
	fair.mayHold("older", candidatesOn("a"))

	if fair.mayBuy("younger", on("a")) {
		t.Error("under fair the younger waiter passed the older")
	}

	if got := fair.avoiding("younger"); len(got) != 0 {
		t.Errorf("under fair a purchase avoids %v", got)
	}
}

// WHAT A HOLD CANNOT HELP STILL HOLDS ITS LINE: a ceiling-bound waiter waits for room
// any purchase spends, and a target's shared pot for room any sibling does. Under
// weights, a waiter of another target with no host held is still ranked.
func TestWhatAHoldCannotHelpStillHoldsTheLine(t *testing.T) {
	t.Parallel()

	ceiling := on("a")
	ceiling.CeilingBound = true

	q := newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", ceiling})

	if q.mayBuy("buyer", on("b")) {
		t.Error("a buyer passed a ceiling-bound waiter")
	}

	if got := q.ahead("buyer", on("b")); !slices.Equal(got, []string{"waiter"}) {
		t.Errorf("ahead of the buyer: %v", got)
	}

	shared := on("a")
	shared.Target, shared.TargetShared = "repo", true

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", shared})

	if q.mayBuy("sibling", shared) {
		t.Error("a sibling passed a waiter of its target's shared pot")
	}

	ranks := map[string]float64{"behind": 0.1, "level": 0.5}

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", ranked("behind", ranks)})

	if q.mayBuy("buyer", ranked("level", ranks)) {
		t.Error("under weights a buyer passed a waiter of a target further behind")
	}

	if !q.mayBuy("sibling", ranked("behind", ranks)) {
		t.Error("under weights a waiter of its own target held a buyer back, under hold")
	}

	// With a host held, it has what it needs from the order.
	waiter := q.waiting["waiter"]
	waiter.candidates = candidatesOn("host")
	q.waiting["waiter"] = waiter
	holdsOf(q)

	if !q.mayBuy("buyer", ranked("level", ranks)) {
		t.Error("a waiter with a host held still ranked ahead on every other host")
	}
}

// A REFUSAL NAMES THE HOST AND ITS OWNER (#248): a tier refused because the only room is
// held reads otherwise as a fleet with room refusing for nothing.
func TestARefusalNamesTheHeldHostAndItsOwner(t *testing.T) {
	t.Parallel()

	r := refusal{room: &alloc.RoomRefusal{}, heldHosts: map[string]string{"b": "wide", "a": "wider"}}

	got := r.String()
	if want := "the only room is on hosts held for tiers whose shape fits on none: a for wider, b for wide"; got != want {
		t.Errorf("the refusal reads %q; want %q", got, want)
	}

	if !r.explained() || !r.byOrder() {
		t.Errorf("a refusal for a held host: explained %v, by the order %v; want both", r.explained(), r.byOrder())
	}

	if bound := (refusal{room: &alloc.RoomRefusal{AtCap: 1}, heldHosts: map[string]string{"b": "wide"}}); strings.Contains(bound.String(), "held") {
		t.Errorf("a limit that binds gave way to a hold: %q", bound.String())
	}
}

// THROUGH THE LISTENERS: a wide tier that fits nowhere is held the host its work leaves
// first, a small tier places elsewhere, and the held host empties for it.
func TestAWideTierIsHeldAHostAndGetsIt(t *testing.T) {
	t.Parallel()

	small := tierOf("a-small", 2)
	small.Memory = config.GiB

	wide := tierOf("b-wide", 8)
	wide.Memory = config.GiB

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{small, wide})
	for _, h := range []alloc.NodeRegistration{host("host-a", 8), host("host-b", 8)} {
		if _, err := a.RegisterNode(t.Context(), h); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	log := &launchLog{}
	s := New(a, nil, []config.Tier{small, wide}, "hold-test", nil,
		WithNodeRunner(log), WithAdmissionOrder(config.AdmissionHold))
	smallL := NewListener(a, small.Label, &fakeSession{}, s.listenerOpts(nil)...)
	wideL := NewListener(a, wide.Label, &fakeSession{}, s.listenerOpts(nil)...)

	// A job on each host; neither has the 8 vCPU the wide tier needs.
	if err := smallL.reconcilePool(t.Context(), 5); err != nil {
		t.Fatalf("small reconcile: %v", err)
	}

	if err := wideL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("wide reconcile: %v", err)
	}

	if got := startedTiers(log)["b-wide"]; got != 0 {
		t.Fatalf("the wide tier started %d with no host empty", got)
	}

	held := s.order.avoiding("a-small")
	if len(held) != 1 {
		t.Fatalf("the small tier avoids %v; want the one host held for the wide tier", held)
	}

	// While it is held, the small tier takes the other host's room and none of this.
	if err := smallL.reconcilePool(t.Context(), 20); err != nil {
		t.Fatalf("small reconcile: %v", err)
	}

	runners, err := a.PoolRunners(t.Context(), "a-small")
	if err != nil {
		t.Fatalf("PoolRunners: %v", err)
	}

	open := 0

	for _, runner := range runners {
		lease, err := a.Lease(t.Context(), runner.LeaseID)
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}

		if lease.TargetNode != held[0] {
			open++

			continue
		}

		// Its leases on the held host end, and nothing replaces them.
		if err := a.ForgetPoolRunner(t.Context(), lease.ID); err != nil {
			t.Fatalf("ForgetPoolRunner: %v", err)
		}

		if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}

	if open != 4 {
		t.Errorf("the small tier holds %d leases on the open host; want it full, 4", open)
	}

	// THE ONLY ROOM NOW IS THE HELD HOST'S, AND A REFUSAL SAYS SO.
	if got := smallL.roomRefusal(t.Context(), nil).String(); !strings.Contains(got, held[0]+" for b-wide") {
		t.Errorf("the small tier's refusal reads %q; want the held host and its owner", got)
	}

	if err := smallL.reconcilePool(t.Context(), 20); err != nil {
		t.Fatalf("small reconcile: %v", err)
	}

	if err := wideL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("wide reconcile: %v", err)
	}

	if got := startedTiers(log)["b-wide"]; got != 1 {
		t.Errorf("the wide tier started %d once its held host emptied", got)
	}
}

// NO HOLD EVER CHANGES AN ADVERTISEMENT: it gates purchases and nothing else, and a tier
// that advertised less for one would not be given the work it waits to buy for.
func TestAHoldChangesNoAdvertisement(t *testing.T) {
	t.Parallel()

	_, fair := wiredListeners(t, catalogue(), 120)

	want := map[string]int{}
	for _, l := range fair {
		want[l.tier] = l.steadyAdvertisement()
	}

	_, held := wiredListeners(t, catalogue(), 120)

	q := newAdmissionQueue(config.AdmissionHold)
	q.waits("t-64vcpu", on("host"))
	q.mayHold("t-64vcpu", candidatesOn("host"))

	for _, l := range held {
		l.order = q

		if got := l.steadyAdvertisement(); got != want[l.tier] {
			t.Errorf("%s advertises %d with a host held, and %d without", l.tier, got, want[l.tier])
		}
	}

	if got := q.avoiding("t-02vcpu"); len(got) != 1 {
		t.Fatalf("no host is held (%v), so this proves nothing", got)
	}
}
