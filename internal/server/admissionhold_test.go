package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
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

	// AND TAKES ITS PLACE AGAIN WHEN IT PROGRESSES: first in the order, it has first pick,
	// and the other cannot buy past it anyway.
	q.launchEnds("stalled")

	if got := holdsOf(q); got["a"] != "stalled" {
		t.Errorf("after it progressed the holds are %v; want it back on a", got)
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
		t.Error("a waiter sharing a host with an older one that has a host was held back")
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

// ONLY A WAITER WITH A HOST LEAVES THE LINE: one with none holds it as under fair,
// whatever its reason for having none, because nothing else keeps smaller work off the
// room it waits for. And a host does not help a waiter of the ceiling or a shared pot.
func TestOnlyAWaiterWithAHostLeavesTheLine(t *testing.T) {
	t.Parallel()

	q := newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", on("host")})

	if q.mayBuy("buyer", on("host")) {
		t.Error("a buyer passed a waiter with no host")
	}

	if got := q.ahead("buyer", on("host")); !slices.Equal(got, []string{"waiter"}) {
		t.Errorf("ahead of the buyer: %v", got)
	}

	q.mayHold("waiter", candidatesOn("host"))

	if !q.mayBuy("buyer", on("host")) {
		t.Error("a waiter with a host still held the line")
	}

	// THE CEILING AND THE POT, WITH A HOST OR WITHOUT.
	ceiling := on("a")
	ceiling.CeilingBound = true

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", ceiling})
	q.mayHold("waiter", candidatesOn("a"))

	if q.mayBuy("buyer", on("a")) {
		t.Error("a buyer passed a ceiling-bound waiter with a host")
	}

	shared := on("a")
	shared.Target, shared.TargetShared = "repo", true

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", shared})
	q.mayHold("waiter", candidatesOn("a"))

	if q.mayBuy("sibling", shared) {
		t.Error("a sibling passed a waiter of its target's shared pot with a host")
	}

	// UNDER WEIGHTS THE LINE IS RANKED, OF ITS OWN TARGET AS OF ANOTHER.
	ranks := map[string]float64{"behind": 0.1, "level": 0.5}

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", ranked("behind", ranks)})

	if q.mayBuy("buyer", ranked("level", ranks)) || q.mayBuy("sibling", ranked("behind", ranks)) {
		t.Error("under weights a buyer passed a waiter with no host, ranked ahead of it")
	}
}

// A PURCHASE GIVES UP THE HOST AS IT GIVES UP THE PLACE, so no host sits idle for a tier
// that just bought elsewhere, and the next waiter has it at once.
func TestAPurchaseGivesUpTheHold(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"owner", []string{"a"}}, wantsHosts{"next", []string{"a"}})

	if got := holdsOf(q); got["a"] != "owner" {
		t.Fatalf("the holds are %v", got)
	}

	q.bought("owner")

	if got := holdsOf(q); got["a"] != "next" {
		t.Errorf("after its owner bought, the host is held for %q; want the next waiter", got["a"])
	}

	// WITH NOBODY NEXT THE HOST IS OPEN, not kept for a tier now behind no one.
	q = holdQueue(wantsHosts{"owner", []string{"a"}})
	q.bought("owner")

	if got := holdsOf(q); len(got) != 0 {
		t.Errorf("after its owner bought, the holds are %v", got)
	}
}

// A HOLD DOES NOT SIT ON A HOST WHOSE READINESS RECEDED: within the slack it stays, past
// it the hold moves, or one job outrunning its kind closes a host for hours.
func TestAHoldMovesWhenItsHostRecedes(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ready := func(host string, after time.Duration) alloc.HoldCandidate {
		return alloc.HoldCandidate{Node: host, ReadyAt: at.Add(after)}
	}

	q := holdQueue(wantsHosts{"waiter", []string{"a"}})

	if got := holdsOf(q); got["a"] != "waiter" {
		t.Fatalf("the holds are %v", got)
	}

	q.mayHold("waiter", []alloc.HoldCandidate{ready("b", 0), ready("a", HoldSlack)})

	if got := holdsOf(q); got["a"] != "waiter" {
		t.Errorf("within the slack the hold moved: %v", got)
	}

	q.mayHold("waiter", []alloc.HoldCandidate{ready("b", 0), ready("a", HoldSlack+time.Second)})

	if got := holdsOf(q); got["b"] != "waiter" {
		t.Errorf("past the slack the hold stayed: %v", got)
	}
}

// UNDER WEIGHTS A HOLD GOES TO THE WAITER THE ORDER WOULD SERVE, or a host is emptied for
// a tier that then cannot buy it.
func TestUnderWeightsAHoldGoesInTheOrdersOrder(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"behind": 0.1, "ahead": 0.5}

	q := newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"older", ranked("ahead", ranks)}, waiterAt{"younger", ranked("behind", ranks)})
	q.mayHold("older", candidatesOn("a"))
	q.mayHold("younger", candidatesOn("a"))
	q.mayBuy("younger", ranked("behind", ranks))

	if got := holdsOf(q); got["a"] != "younger" {
		t.Errorf("the host is held for %q; want the waiter of the target further behind", got["a"])
	}
}

// A REFUSAL NAMES THE HOST AND ITS OWNER (#248): a tier refused because the only room is
// held reads otherwise as a fleet with room refusing for nothing.
func TestARefusalNamesTheHeldHostAndItsOwner(t *testing.T) {
	t.Parallel()

	r := refusal{room: &alloc.RoomRefusal{}, heldHosts: map[string]string{"b": "wide", "a": "wider"}}

	if protecting := (refusal{room: &alloc.RoomRefusal{}, protecting: []string{"x"}, heldHosts: r.heldHosts}); !strings.Contains(protecting.String(), "buying ahead of its turn") {
		t.Errorf("a purchase refused to protect a waiter reads %q", protecting.String())
	}

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

	// ITS DEMAND MET, THE HOLD IS GONE, and at the small tier's next read neither host is
	// one it holds: each now runs a lease as large as its shape.
	if err := smallL.reconcilePool(t.Context(), 20); err != nil {
		t.Fatalf("small reconcile: %v", err)
	}

	if got := s.order.avoiding("someone-else"); len(got) != 0 {
		t.Errorf("after the wide tier bought, a newcomer avoids %v", got)
	}
}

// A HOLD BINDS ONLY WHO STANDS BEHIND ITS OWNER. An older waiter of a smaller shape has
// no candidate, every host running a lease as large as it; a wider one behind it holds
// the host. It must still get its turn there, or each waits on the other: the wider
// cannot pass it, and if the hold bound it, it could not take the room.
func TestAWaiterAheadTakesRoomOnAHostHeldBehindIt(t *testing.T) {
	t.Parallel()

	busy, older, wider := tierOf("a-busy", 8), tierOf("b-older", 8), tierOf("c-wider", 16)
	busy.Memory, older.Memory, wider.Memory = config.GiB, config.GiB, config.GiB
	tiers := []config.Tier{busy, older, wider}

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if _, err := a.RegisterNode(t.Context(), host("only", 16)); err != nil {
		t.Fatalf("register: %v", err)
	}

	log := &launchLog{}
	s := New(a, nil, tiers, "hold-test", nil, WithNodeRunner(log), WithAdmissionOrder(config.AdmissionHold))
	busyL := NewListener(a, busy.Label, &fakeSession{}, s.listenerOpts(nil)...)
	olderL := NewListener(a, older.Label, &fakeSession{}, s.listenerOpts(nil)...)
	widerL := NewListener(a, wider.Label, &fakeSession{}, s.listenerOpts(nil)...)

	if err := busyL.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("busy reconcile: %v", err)
	}

	if err := olderL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("older reconcile: %v", err)
	}

	if err := widerL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("wider reconcile: %v", err)
	}

	if got := s.order.ownHold(wider.Label); got != "only" {
		t.Fatalf("the wider tier holds %q, and this proves nothing", got)
	}

	if got := s.order.avoiding(older.Label); len(got) != 0 {
		t.Errorf("the waiter ahead avoids %v", got)
	}

	if got := s.order.avoiding("newcomer"); !slices.Equal(got, []string{"only"}) {
		t.Errorf("a newcomer avoids %v; want the held host", got)
	}

	finishOne(t, a, busy.Label)

	if err := olderL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("older reconcile: %v", err)
	}

	if got := startedTiers(log)[older.Label]; got != 1 {
		t.Errorf("the waiter ahead started %d with room for it on a host held behind it", got)
	}
}

// heldScene is two hosts of 8 vCPU, each full of a filler tier's 4 vCPU leases, and a
// listener for every other tier, through the queue a deployment runs.
type heldScene struct {
	a         *alloc.Allocator
	log       *launchLog
	s         *Server
	listeners map[string]*Listener
}

func newHeldScene(t *testing.T, db *state.DB, tiers ...config.Tier) *heldScene {
	t.Helper()

	fill := tierOf("a-fill", 4)
	all := append([]config.Tier{fill}, tiers...)

	for i := range all {
		all[i].Memory = config.GiB
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, all)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	for _, h := range []alloc.NodeRegistration{host("host-a", 8), host("host-b", 8)} {
		if _, err := a.RegisterNode(t.Context(), h); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	scene := &heldScene{a: a, log: &launchLog{}, listeners: map[string]*Listener{}}
	scene.s = New(a, nil, all, "hold-test", nil, WithNodeRunner(scene.log), WithAdmissionOrder(config.AdmissionHold))

	for i := range all {
		scene.listeners[all[i].Label] = NewListener(a, all[i].Label, &fakeSession{}, scene.s.listenerOpts(nil)...)
	}

	scene.want(t, fill.Label, 4)

	return scene
}

func (h *heldScene) want(t *testing.T, tier string, n int) {
	t.Helper()

	if err := h.listeners[tier].reconcilePool(t.Context(), n); err != nil {
		t.Fatalf("%s reconcile: %v", tier, err)
	}
}

// freeFill ends one of the filler's leases on a host.
func (h *heldScene) freeFill(t *testing.T, onHost string) {
	t.Helper()

	runners, err := h.a.PoolRunners(t.Context(), "a-fill")
	if err != nil {
		t.Fatalf("PoolRunners: %v", err)
	}

	for _, runner := range runners {
		lease, err := h.a.Lease(t.Context(), runner.LeaseID)
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}

		if lease.TargetNode != onHost {
			continue
		}

		if err := h.a.ForgetPoolRunner(t.Context(), lease.ID); err != nil {
			t.Fatalf("ForgetPoolRunner: %v", err)
		}

		if err := h.a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}

		return
	}

	t.Fatalf("the filler has nothing on %s", onHost)
}

// ROOM ON THE HOST IT HOLDS KEEPS THE HOLD: the owner is refused behind an older waiter
// with its host emptied, and the room it accumulated is not handed to the next purchase.
func TestRoomOnItsOwnHostKeepsAWaitersHold(t *testing.T) {
	t.Parallel()

	h := newHeldScene(t, openState(t), tierOf("b-older", 4), tierOf("c-wide", 8))

	h.want(t, "b-older", 1)
	h.want(t, "c-wide", 1)

	if got := h.s.order.ownHold("c-wide"); got != "host-a" {
		t.Fatalf("the wide tier holds %q", got)
	}

	h.freeFill(t, "host-a")
	h.freeFill(t, "host-a")
	h.want(t, "c-wide", 1)

	if got := startedTiers(h.log)["c-wide"]; got != 0 {
		t.Fatalf("the wide tier started %d past an older waiter", got)
	}

	if got := h.s.order.ownHold("c-wide"); got != "host-a" {
		t.Errorf("with its own host empty the wide tier holds %q", got)
	}
}

// ROOM HELD FOR ANOTHER IS NOT ROOM A WAITER HAS: the second wide tier keeps its own host
// when the first's empties, or it would read itself as fitting and give it up.
func TestRoomHeldForAnotherIsNotRoomAWaiterHas(t *testing.T) {
	t.Parallel()

	h := newHeldScene(t, openState(t), tierOf("b-wide", 8), tierOf("c-wide", 8))

	h.want(t, "b-wide", 1)
	h.want(t, "c-wide", 1)

	if got := h.s.order.ownHold("c-wide"); got != "host-b" {
		t.Fatalf("the second wide tier holds %q", got)
	}

	h.freeFill(t, "host-a")
	h.freeFill(t, "host-a")
	h.want(t, "c-wide", 1)

	if got := startedTiers(h.log)["c-wide"]; got != 0 {
		t.Errorf("the second wide tier started %d on the host held for the first", got)
	}

	if got := h.s.order.ownHold("c-wide"); got != "host-b" {
		t.Errorf("with the first's host empty the second holds %q", got)
	}
}

// A PURCHASE PROTECTS A WAITER ON THE HOSTS IT MAY USE: the room left for it on a host
// held ahead of it is no room, so a smaller tier does not take what it has on the other.
func TestAPurchaseThroughAListenerLeavesAWaiterRoomOffTheHeldHosts(t *testing.T) {
	t.Parallel()

	h := newHeldScene(t, openState(t), tierOf("b-wide", 8), tierOf("c-waiter", 4), tierOf("d-small", 2))

	h.want(t, "b-wide", 1)
	h.want(t, "c-waiter", 1)

	if got := h.s.order.ownHold("b-wide"); got != "host-a" {
		t.Fatalf("the wide tier holds %q", got)
	}

	h.freeFill(t, "host-a")
	h.freeFill(t, "host-b")
	h.want(t, "d-small", 1)

	if got := startedTiers(h.log)["d-small"]; got != 0 {
		t.Errorf("the small tier started %d, leaving the waiter ahead only a held host", got)
	}

	h.want(t, "c-waiter", 1)

	if got := startedTiers(h.log)["c-waiter"]; got != 1 {
		t.Errorf("the waiter started %d with room off the held host", got)
	}
}

// A REFUSAL NAMES ONLY A HOLD THE TIER COULD HAVE USED: one pinned elsewhere was not kept
// off that host by the hold.
func TestARefusalNamesOnlyAHoldOnAHostTheTierCanUse(t *testing.T) {
	t.Parallel()

	pinned := tierOf("c-pinned", 2)
	pinned.Node = "host-b"

	h := newHeldScene(t, openState(t), tierOf("b-wide", 8), pinned, tierOf("d-free", 2))

	h.want(t, "b-wide", 1)

	if got := h.listeners["d-free"].heldAgainst(t.Context()); got["host-a"] != "b-wide" {
		t.Errorf("a tier that can use the host reads the holds as %v", got)
	}

	if got := h.listeners["c-pinned"].heldAgainst(t.Context()); len(got) != 0 {
		t.Errorf("a tier pinned elsewhere reads the holds as %v", got)
	}
}

// A FAILED READ HOLDS NOTHING: what keeps room from others is what a read that could not
// tell must not keep doing.
func TestAFailedReadHoldsNothing(t *testing.T) {
	t.Parallel()

	db := openState(t)
	h := newHeldScene(t, db, tierOf("b-wide", 8))

	h.want(t, "b-wide", 1)

	if got := h.s.order.ownHold("b-wide"); got == "" {
		t.Fatal("the wide tier holds nothing, and this proves nothing")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h.listeners["b-wide"].askForAHold(t.Context(), alloc.TierAdmission{})

	if got := h.s.order.ownHold("b-wide"); got != "" {
		t.Errorf("after a failed read the wide tier holds %q", got)
	}
}

// A RESTART REBUILDS THE HOLD THROUGH THE LEDGER: a new control plane over the same
// ledger, with nothing carried over, holds the same host at the first refusal.
func TestAControlPlaneOverTheSameLedgerHoldsTheSameHost(t *testing.T) {
	t.Parallel()

	small, wide := tierOf("a-small", 2), tierOf("b-wide", 8)
	small.Memory, wide.Memory = config.GiB, config.GiB
	tiers := []config.Tier{small, wide}

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	for _, h := range []alloc.NodeRegistration{host("host-a", 8), host("host-b", 8)} {
		if _, err := a.RegisterNode(t.Context(), h); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	held := func() string {
		t.Helper()

		s := New(a, nil, tiers, "hold-test", nil, WithNodeRunner(&launchLog{}), WithAdmissionOrder(config.AdmissionHold))
		l := NewListener(a, wide.Label, &fakeSession{}, s.listenerOpts(nil)...)

		if err := l.reconcilePool(t.Context(), 1); err != nil {
			t.Fatalf("wide reconcile: %v", err)
		}

		return s.order.ownHold(wide.Label)
	}

	first := New(a, nil, tiers, "hold-test", nil, WithNodeRunner(&launchLog{}), WithAdmissionOrder(config.AdmissionHold))
	smallL := NewListener(a, small.Label, &fakeSession{}, first.listenerOpts(nil)...)

	if err := smallL.reconcilePool(t.Context(), 5); err != nil {
		t.Fatalf("small reconcile: %v", err)
	}

	before := held()
	if before == "" {
		t.Fatal("the wide tier holds nothing, so this proves nothing")
	}

	if after := held(); after != before {
		t.Errorf("a new control plane holds %q; the last held %q", after, before)
	}
}

// A WAITER WITH NO CANDIDATE STILL GETS ITS TURN: both hosts run a lease as large as its
// shape, so it holds no host, and a sibling of its shape with a backlog must not take
// every lease as it ends.
func TestAWaiterWithNoHostGetsItsTurnAgainstABacklog(t *testing.T) {
	t.Parallel()

	busy, quiet := tierOf("a-busy", 8), tierOf("b-quiet", 8)
	busy.Memory, quiet.Memory = config.GiB, config.GiB
	tiers := []config.Tier{busy, quiet}

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	for _, h := range []alloc.NodeRegistration{host("host-a", 8), host("host-b", 8)} {
		if _, err := a.RegisterNode(t.Context(), h); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	log := &launchLog{}
	s := New(a, nil, tiers, "hold-test", nil, WithNodeRunner(log), WithAdmissionOrder(config.AdmissionHold))
	busyL := NewListener(a, busy.Label, &fakeSession{}, s.listenerOpts(nil)...)
	quietL := NewListener(a, quiet.Label, &fakeSession{}, s.listenerOpts(nil)...)

	if err := busyL.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("busy reconcile: %v", err)
	}

	if err := quietL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("quiet reconcile: %v", err)
	}

	if got := s.order.avoiding("newcomer"); len(got) != 0 {
		t.Fatalf("a host is held (%v) though each runs a lease as large as the shape", got)
	}

	// One of the busy tier's leases ends, and it hears first.
	finishOne(t, a, busy.Label)

	if err := busyL.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("busy reconcile: %v", err)
	}

	if err := quietL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("quiet reconcile: %v", err)
	}

	if got := startedTiers(log)[quiet.Label]; got != 1 {
		t.Errorf("the waiter with no host started %d; the busy tier's backlog took the room", got)
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
