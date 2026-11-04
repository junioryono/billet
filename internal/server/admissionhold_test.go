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

// sized is an admission of a shape on these hosts.
func sized(vcpu int, hosts ...string) alloc.TierAdmission {
	where := on(hosts...)
	where.VCPU, where.Memory = vcpu, config.ByteSize(vcpu)*config.GiB

	return where
}

// A WAITER WITH A HOST HOLDS NOBODY BACK, and one waiting for a host is no line: its turn
// comes in the order, as the first pick. An admission that could not be read holds the
// line, because an unknown is not a licence.
func TestAWaiterWithAHostOrACandidateHoldsNobodyBack(t *testing.T) {
	t.Parallel()

	q := newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", on("host")})

	if q.mayBuy("buyer", on("host")) {
		t.Error("a buyer passed a waiter whose admission could not be read")
	}

	if got := q.ahead("buyer", on("host")); !slices.Equal(got, []string{"waiter"}) {
		t.Errorf("ahead of the buyer: %v", got)
	}

	q.mayHold("waiter", candidatesOn("host"))

	if !q.mayBuy("buyer", on("host")) {
		t.Error("a waiter with a host still held the line")
	}

	q = holdQueue(wantsHosts{"owner", []string{"a"}}, wantsHosts{"next", []string{"a"}})

	if !q.mayBuy("buyer", on("a", "b", "c")) {
		t.Error("a waiter with a candidate and no host held the line")
	}

	// UNDER WEIGHTS A WAITER WITH NO CANDIDATE IS RANKED, OF ITS OWN TARGET AS OF ANOTHER.
	ranks := map[string]float64{"behind": 0.1, "level": 0.5}

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", ranked("behind", ranks)})

	if q.mayBuy("buyer", ranked("level", ranks)) || q.mayBuy("sibling", ranked("behind", ranks)) {
		t.Error("under weights a buyer passed a waiter ranked ahead of it")
	}
}

// A WAITER WITH NO CANDIDATE HOLDS BACK ONLY A BUYER AT LEAST AS WIDE: that lease ending
// is room it fits in, and only one as wide can put another such lease in its way. The
// ceiling and a shared pot are on no host, and hold back everyone they compete with.
func TestAWaiterWithNoCandidateHoldsBackOnlyAsWide(t *testing.T) {
	t.Parallel()

	q := newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", sized(8, "a")})

	// NO CANDIDATE FOR ANY OTHER REASON IS THE LINE AS UNDER FAIR: a Mac's slot, a remote
	// budget, a lease in custody, or nothing read yet.
	q.mayHold("waiter", nil)

	if q.mayBuy("narrower", sized(2, "a")) {
		t.Error("a narrower buyer passed a waiter not behind a lease as large")
	}

	q.behindAsLarge("waiter")

	if !q.mayBuy("narrower", sized(2, "a")) {
		t.Error("a narrower buyer was held back")
	}

	if q.mayBuy("same", sized(8, "a")) || q.mayBuy("wider", sized(16, "a")) {
		t.Error("a buyer at least as wide passed it")
	}

	// As wide in vCPU and not in memory is not as wide.
	lean := sized(8, "a")
	lean.Memory = config.GiB

	if !q.mayBuy("lean", lean) {
		t.Error("a buyer narrower in memory was held back")
	}

	// A READ THAT FAILED IS AS WIDE AS ANYTHING, on either side.
	if q.mayBuy("unread", on("a")) {
		t.Error("a buyer whose admission was not read passed it")
	}

	ceiling := sized(8, "a")
	ceiling.CeilingBound = true

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", ceiling})

	if q.mayBuy("narrower", sized(2, "b")) {
		t.Error("a narrower buyer passed a ceiling-bound waiter")
	}

	shared, sibling := sized(8, "a"), sized(2, "b")
	shared.Target, shared.TargetShared = "repo", true
	sibling.Target, sibling.TargetShared = "repo", true

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", shared})

	if q.mayBuy("sibling", sibling) {
		t.Error("a narrower sibling passed a waiter of its target's shared pot")
	}

	// WITH A HOST TOO: a sibling taking the pot makes it share-bound at its next read,
	// which drops the host, and another target refills it, so it never has both.
	q.mayHold("waiter", candidatesOn("a"))

	if q.mayBuy("sibling", sibling) {
		t.Error("a sibling passed a waiter of the pot with a host")
	}

	// UNDER WEIGHTS ANOTHER TARGET'S WAITER WITH NO HOST HOLDS THE LINE AS ITS RANK SAYS,
	// narrower buyer or not, and a sibling of its own target is judged by width.
	ranks := map[string]float64{"behind": 0.1, "ahead": 0.5}

	waiter, narrower, near := sized(8, "a"), sized(2, "a"), sized(2, "a")
	waiter.Target, waiter.TargetRank = "behind", ranks
	narrower.Target, narrower.TargetRank = "ahead", ranks
	near.Target, near.TargetRank = "behind", ranks

	q = newAdmissionQueue(config.AdmissionHold)
	waitingInOrder(q, waiterAt{"waiter", waiter})
	q.behindAsLarge("waiter")

	if q.mayBuy("narrower", narrower) {
		t.Error("a narrower buyer of a target ahead passed a waiter ranked before it")
	}

	if !q.mayBuy("near", near) {
		t.Error("a narrower sibling of its own target was held back by rank")
	}
}

// A HOLD ON A HOST WHOSE JOB RUNS LONG STAYS, AND ITS OWNER ALSO HOLDS THE LINE, whose bound
// is the first host that fits. One job's six hours must not be the wide shape's wait.
func TestALateHostAlsoHoldsTheLine(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"waiter", []string{"a"}})

	if !q.mayBuy("buyer", sized(2, "a", "b")) {
		t.Fatal("a buyer was held back by a waiter with a host in time")
	}

	q.mayHold("waiter", []alloc.HoldCandidate{{Node: "a", Late: true}, {Node: "b"}})

	if got := holdsOf(q); got["a"] != "waiter" {
		t.Errorf("a late host lost the hold: %v", got)
	}

	if q.mayBuy("buyer", sized(2, "a", "b")) {
		t.Error("a buyer passed a waiter whose host runs late")
	}

	q.mayHold("waiter", []alloc.HoldCandidate{{Node: "a"}, {Node: "b", Late: true}})

	if !q.mayBuy("buyer", sized(2, "a", "b")) {
		t.Error("a late host it does not hold held the buyer back")
	}

	q.mayHold("waiter", []alloc.HoldCandidate{{Node: "b", Late: true}, {Node: "a"}})

	if !q.mayBuy("buyer", sized(2, "a", "b")) {
		t.Error("a late host listed first and not held held the buyer back")
	}
}

// A BUYER WITH A HOST THE WAITER CANNOT USE takes nothing the waiter waits for, as wide or not.
func TestABuyerWithAHostAWaiterCannotUseBuysThere(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"waiter", nil}, wantsHosts{"owner", []string{"h"}})

	q.mu.Lock()
	waiter := q.waiting["waiter"]
	waiter.where = sized(4, "g")
	q.waiting["waiter"] = waiter
	q.mu.Unlock()

	q.behindAsLarge("waiter")

	if !q.mayBuy("owner", sized(16, "h", "g")) {
		t.Error("the owner of a host the waiter cannot use was held back")
	}

	if q.mayBuy("no-host", sized(16, "h", "g")) {
		t.Error("a buyer as wide with no host passed the waiter")
	}

	// A HOST THE WAITER CAN USE IS NO SUCH LICENCE.
	q = holdQueue(wantsHosts{"waiter", nil}, wantsHosts{"owner", []string{"g"}})

	q.mu.Lock()
	waiter = q.waiting["waiter"]
	waiter.where = sized(4, "g")
	q.waiting["waiter"] = waiter
	q.mu.Unlock()

	q.behindAsLarge("waiter")

	if q.mayBuy("owner", sized(16, "g", "h")) {
		t.Error("the owner of a host the waiter can use passed it")
	}
}

// A TARGET NOBODY RANKED FALLS BACK TO OLDEST FIRST IN THE PLAN, as it does for a purchase:
// read as rank zero it would stand first and take a host from the waiter ahead of it.
func TestAWaiterOfNoTargetDoesNotJumpThePlan(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"known": 0.5}

	q := newAdmissionQueue(config.AdmissionHold)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return at }
	q.lastRanks = ranks
	q.waiting["known"] = waitingTier{since: at, progress: at, where: ranked("known", ranks), candidates: candidatesOn("a")}
	q.waiting["unread"] = waitingTier{since: at.Add(time.Second), progress: at, where: on("a"), candidates: candidatesOn("a")}

	if got := holdsOf(q); got["a"] != "known" {
		t.Errorf("the host is held for %q; want the older waiter", got["a"])
	}

	// AND A FAILED READ DOES NOT OVERWRITE A BUYER'S RECORD.
	q.mayBuy("known", alloc.TierAdmission{})

	if got := q.waiting["known"].where.Target; got != "known" {
		t.Errorf("a failed read left its record saying target %q", got)
	}
}

// ROOM THAT APPEARS BEFORE THE READ KEEPS THE WAITER IN THE LINE: its purchase was refused, a
// lease ended, and the read found it fits. A narrower buyer waking first must not take what
// it was refused for, again and again, with every read fresh and every refusal progress.
func TestRoomAppearingBeforeTheReadKeepsTheLine(t *testing.T) {
	t.Parallel()

	wide, small := tierOf("a-wide", 8), tierOf("b-small", 2)
	wide.Memory, small.Memory = config.GiB, config.GiB
	tiers := []config.Tier{wide, small}

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)
	if _, err := a.RegisterNode(t.Context(), host("only", 8)); err != nil {
		t.Fatalf("register: %v", err)
	}

	log := &launchLog{}
	s := New(a, nil, tiers, "hold-test", nil, WithNodeRunner(log), WithAdmissionOrder(config.AdmissionHold))
	wideL := NewListener(a, wide.Label, &fakeSession{}, s.listenerOpts(nil)...)
	smallL := NewListener(a, small.Label, &fakeSession{}, s.listenerOpts(nil)...)

	if err := smallL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("small reconcile: %v", err)
	}

	if err := wideL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("wide reconcile: %v", err)
	}

	finishOne(t, a, small.Label)
	wideL.askForAHold(t.Context(), alloc.TierAdmission{})

	if err := smallL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("small reconcile: %v", err)
	}

	if got := startedTiers(log)[small.Label]; got != 1 {
		t.Errorf("the narrower tier started %d, the second on room the wide one was about to buy", got)
	}

	if err := wideL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("wide reconcile: %v", err)
	}

	if got := startedTiers(log)[wide.Label]; got != 1 {
		t.Errorf("the wide tier started %d with the room free", got)
	}
}

// THE CENTRAL WALK, THROUGH LISTENERS. The widest shape waits behind a lease as large. A
// narrower tier passes it. The lease ends and the narrower takes part of the room; the host
// now has no lease as large, so the widest is given it, nothing else lands, and it buys.
func TestAWaiterBehindALeaseAsLargeGetsItsTurnAgainstANarrowerStream(t *testing.T) {
	t.Parallel()

	big, wide, small := tierOf("a-big", 16), tierOf("b-wide", 16), tierOf("c-small", 2)
	big.Memory, wide.Memory, small.Memory = config.GiB, config.GiB, config.GiB
	tiers := []config.Tier{big, wide, small}

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if _, err := a.RegisterNode(t.Context(), host("only", 16)); err != nil {
		t.Fatalf("register: %v", err)
	}

	log := &launchLog{}
	s := New(a, nil, tiers, "hold-test", nil, WithNodeRunner(log), WithAdmissionOrder(config.AdmissionHold))
	bigL := NewListener(a, big.Label, &fakeSession{}, s.listenerOpts(nil)...)
	wideL := NewListener(a, wide.Label, &fakeSession{}, s.listenerOpts(nil)...)
	smallL := NewListener(a, small.Label, &fakeSession{}, s.listenerOpts(nil)...)

	want := func(l *Listener, n int) {
		t.Helper()

		if err := l.reconcilePool(t.Context(), n); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	want(bigL, 1)
	want(wideL, 1)

	if got := s.order.ownHold(wide.Label); got != "" {
		t.Fatalf("the wide tier holds %q behind a lease as large", got)
	}

	if !s.order.waiting[wide.Label].behindAsLarge {
		t.Fatal("the wide tier does not read as behind a lease as large")
	}

	finishOne(t, a, big.Label)
	want(smallL, 1)

	if got := startedTiers(log)[small.Label]; got != 1 {
		t.Fatalf("the narrower tier started %d past the wide one", got)
	}

	want(wideL, 1)

	if got := s.order.ownHold(wide.Label); got != "only" {
		t.Fatalf("with no lease as large left the wide tier holds %q", got)
	}

	want(smallL, 4)

	if got := startedTiers(log)[small.Label]; got != 1 {
		t.Errorf("the narrower tier started %d on the host held", got)
	}

	finishOne(t, a, small.Label)
	want(wideL, 1)

	if got := startedTiers(log)[wide.Label]; got != 1 {
		t.Errorf("the wide tier started %d with the host emptied", got)
	}
}

// A HOLD BINDS ONLY WHO STANDS BEHIND ITS OWNER.
func TestAHoldBindsOnlyWhoStandsBehindItsOwner(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"first", []string{"a"}}, wantsHosts{"second", []string{"b"}})

	if got := q.avoiding("first"); len(got) != 0 {
		t.Errorf("the first avoids %v", got)
	}

	if got := q.avoiding("second"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("the second avoids %v; want the first's", got)
	}
}

// A POOL PASS GIVES UP THE HOST AT ITS FIRST LEASE AND KEEPS OFF EVERY OTHER HOLD: it keeps
// its place until the pass ends, or batches shrink to one, and without this that place
// sweeps a younger waiter's host.
func TestAPoolPassKeepsOffEveryOtherHold(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"first", []string{"a"}}, wantsHosts{"second", []string{"b"}})

	if got := q.avoiding("first"); len(got) != 0 {
		t.Fatalf("before it bought the first avoids %v", got)
	}

	q.boughtOne("first")

	if got := q.avoiding("first"); !slices.Equal(got, []string{"b"}) {
		t.Errorf("mid-pass the first avoids %v; want the second's", got)
	}

	if got := holdsOf(q); got["a"] != "" || got["b"] != "second" {
		t.Errorf("mid-pass the holds are %v; want its own given up, the second's kept", got)
	}

	q.bought("first")

	if q.waiting["first"].spent {
		t.Error("the end of the pass left it marked")
	}
}

// A WAITER IN A LAUNCH'S BACKOFF HOLDS NOTHING, as it holds no line.
func TestAWaiterBackingOffALaunchHoldsNoHost(t *testing.T) {
	t.Parallel()

	q := holdQueue(wantsHosts{"failing", []string{"a"}}, wantsHosts{"healthy", []string{"a"}})
	q.lastRanks = map[string]float64{}
	q.launchResult("failing", true)

	if got := holdsOf(q); got["a"] != "healthy" {
		t.Errorf("the host is held for %q; want the waiter whose launches work", got["a"])
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

// A HELD HOST ONLY EMPTIES, SO A HOLD STAYS HOWEVER LATE ITS HOST RUNS: moving throws away
// the room accumulated, and it costs the wide shape its turn. It moves only when the host
// stops being a candidate, and buying starts it over.
func TestAHoldStaysOnAHostThatRecedes(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ready := func(host string, after time.Duration) alloc.HoldCandidate {
		return alloc.HoldCandidate{Node: host, ReadyAt: at.Add(after)}
	}

	q := holdQueue(wantsHosts{"waiter", []string{"a"}})

	if got := holdsOf(q); got["a"] != "waiter" {
		t.Fatalf("the holds are %v", got)
	}

	q.mayHold("waiter", []alloc.HoldCandidate{ready("b", 0), ready("a", 6*time.Hour)})

	if got := holdsOf(q); got["a"] != "waiter" {
		t.Errorf("the hold left a host that receded: %v", got)
	}

	q.mayHold("waiter", []alloc.HoldCandidate{ready("b", 0)})

	if got := holdsOf(q); got["b"] != "waiter" {
		t.Errorf("the hold stayed on a host that is no candidate: %v", got)
	}

	q.mayHold("waiter", nil)

	if got := holdsOf(q); len(got) != 0 {
		t.Errorf("with no candidate the holds are %v", got)
	}
}

// UNDER WEIGHTS A HOLD GOES TO THE WAITER THE ORDER WOULD SERVE, or a host is emptied for
// a tier that then cannot buy it.
func TestUnderWeightsAHoldGoesInTheOrdersOrder(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"behind": 0.1, "level": 0.3, "ahead": 0.5}

	q := newAdmissionQueue(config.AdmissionHold)

	// The first refused keeps the line; those refused behind it are ranked when it goes.
	q.waits("first", ranked("level", ranks))
	q.waits("older", ranked("ahead", ranks))
	q.waits("younger", ranked("behind", ranks))
	q.mayHold("older", candidatesOn("a"))
	q.mayHold("younger", candidatesOn("a"))
	q.served("first")
	q.mayBuy("younger", ranked("behind", ranks))

	if got := holdsOf(q); got["a"] != "younger" {
		t.Errorf("the host is held for %q; want the waiter of the target further behind", got["a"])
	}
}

// A WAITER WITH A HOST IS STILL AHEAD OF WHO IS BEHIND IT when the line is first judged, or
// everyone behind one reads themselves as first and keeps the line against the ranks.
func TestBehindAWaiterWithAHostNobodyIsFirst(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"behind": 0.1, "ahead": 0.5}

	q := newAdmissionQueue(config.AdmissionHold)
	q.waits("owner", ranked("behind", ranks))
	q.mayHold("owner", candidatesOn("a"))
	q.waits("next", ranked("ahead", ranks))

	if q.waiting["next"].head {
		t.Error("a waiter behind one with a host took the line for its own")
	}
}

// A BUYER'S OWN STANDING IS WHAT THIS READ SAYS: its record may hold that it was below its
// floor, and ahead of a hold that a fresh read puts it behind.
func TestABuyerStandsWhereItsReadSays(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"behind": 0.1, "ahead": 0.5}

	below := ranked("ahead", ranks)
	below.BelowFloor = true

	q := newAdmissionQueue(config.AdmissionHold)
	q.waits("owner", ranked("behind", ranks))
	q.mayHold("owner", candidatesOn("a"))
	q.waits("was-below", below)
	q.bought("was-below")

	if got := q.avoiding("was-below"); len(got) != 0 {
		t.Fatalf("on what its record says it avoids %v, and this proves nothing", got)
	}

	q.mayBuy("was-below", ranked("ahead", ranks))

	if got := q.avoiding("was-below"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("at its floor it avoids %v; want the hold ahead of it", got)
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

	if got := s.order.ownHold("a-small") + s.order.ownHold("b-wide"); got != "" {
		t.Errorf("after the wide tier bought, %q is held", got)
	}
}

// AN OLDER WAITER OF A SMALLER SHAPE WITH NO CANDIDATE TAKES ROOM ON A HOST HELD BEHIND IT.
// Every host runs a lease as large as it, so it holds none; a wider one behind it holds
// the host. Bound by that hold it could not take the room, and the wider one cannot pass
// it, so each would wait on the other.
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

	finishOne(t, a, busy.Label)

	if err := olderL.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("older reconcile: %v", err)
	}

	if got := startedTiers(log)[older.Label]; got != 1 {
		t.Errorf("the waiter ahead started %d with room for it", got)
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

	scene := newHeldSceneOn(t, db, []alloc.NodeRegistration{host("host-a", 8), host("host-b", 8)}, tiers...)
	scene.want(t, "a-fill", 4)

	return scene
}

// newHeldSceneOn is heldScene on these hosts, with the filler not yet bought.
func newHeldSceneOn(t *testing.T, db *state.DB, hosts []alloc.NodeRegistration, tiers ...config.Tier) *heldScene {
	t.Helper()

	all := append([]config.Tier{tierOf("a-fill", 4)}, tiers...)

	for i := range all {
		all[i].Memory = config.GiB
	}

	limits := alloc.Limits{}

	for i := range hosts {
		limits.MaxVCPU += hosts[i].VCPU
		limits.MaxMemory += hosts[i].Memory
	}

	a, err := alloc.New(db, limits, all)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	for i := range hosts {
		if _, err := a.RegisterNode(t.Context(), hosts[i]); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	scene := &heldScene{a: a, log: &launchLog{}, listeners: map[string]*Listener{}}
	scene.s = New(a, nil, all, "hold-test", nil, WithNodeRunner(scene.log), WithAdmissionOrder(config.AdmissionHold))

	for i := range all {
		scene.listeners[all[i].Label] = NewListener(a, all[i].Label, &fakeSession{}, scene.s.listenerOpts(nil)...)
	}

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

	for i := range runners {
		lease, err := h.a.Lease(t.Context(), runners[i].LeaseID)
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

// ROOM ON THE HOST IT HOLDS KEEPS THE HOLD: a job ending between its refusal and its read
// must not hand the room it accumulated to the next purchase.
func TestRoomOnItsOwnHostKeepsAWaitersHold(t *testing.T) {
	t.Parallel()

	h := newHeldScene(t, openState(t), tierOf("c-wide", 8))

	h.want(t, "c-wide", 1)

	if got := h.s.order.ownHold("c-wide"); got != "host-a" {
		t.Fatalf("the wide tier holds %q", got)
	}

	h.freeFill(t, "host-a")
	h.freeFill(t, "host-a")
	h.listeners["c-wide"].askForAHold(t.Context(), alloc.TierAdmission{})

	if got := h.s.order.ownHold("c-wide"); got != "host-a" {
		t.Errorf("with its own host empty the wide tier holds %q", got)
	}
}

// ON HOSTS OF UNEQUAL SIZE THE WIDEST SHAPE STOPS NOTHING NARROWER. It fits one host, which
// runs a lease as large; the next is given the smaller host, and buys there when it
// empties, rather than wait out a job on the other. When the large lease ends the widest
// has its turn: what the narrower took leaves a host it has a claim on.
func TestAWaiterWithNoCandidateDoesNotStopANarrowerOne(t *testing.T) {
	t.Parallel()

	pinned := tierOf("b-big", 16)
	pinned.Node = "host-c"

	small := tierOf("e-small", 2)
	small.Node = "host-a"

	h := newHeldSceneOn(t, openState(t), []alloc.NodeRegistration{host("host-a", 8), host("host-c", 16)},
		pinned, tierOf("c-widest", 16), tierOf("d-next", 8), small)

	h.want(t, "b-big", 1)
	h.want(t, "a-fill", 2)
	h.want(t, "c-widest", 1)
	h.want(t, "d-next", 1)

	if got := h.s.order.ownHold("c-widest"); got != "" {
		t.Fatalf("the widest tier holds %q; its one host runs a lease as large", got)
	}

	if got := h.s.order.ownHold("d-next"); got != "host-a" {
		t.Fatalf("the next tier holds %q", got)
	}

	h.freeFill(t, "host-a")
	h.freeFill(t, "host-a")

	h.want(t, "e-small", 1)

	if got := startedTiers(h.log)["e-small"]; got != 0 {
		t.Errorf("a tier behind the hold started %d on the host held", got)
	}

	h.want(t, "d-next", 1)

	if got := startedTiers(h.log)["d-next"]; got != 1 {
		t.Errorf("the next tier started %d with its held host empty", got)
	}
}

// A POOL PASS THROUGH A LISTENER: the oldest tier with a backlog buys its own host and an
// open one, and leaves the younger waiter's alone.
func TestAPoolPassLeavesAYoungerWaitersHost(t *testing.T) {
	t.Parallel()

	h := newHeldSceneOn(t, openState(t),
		[]alloc.NodeRegistration{host("host-a", 8), host("host-b", 8), host("host-c", 8)},
		tierOf("b-older", 8), tierOf("c-younger", 8))

	h.listeners["b-older"] = NewListener(h.a, "b-older", &fakeSession{},
		append(h.s.listenerOpts(nil), WithPoolLaunchBatch(4))...)

	h.want(t, "a-fill", 6)
	h.want(t, "b-older", 3)
	h.want(t, "c-younger", 1)

	younger := h.s.order.ownHold("c-younger")
	if h.s.order.ownHold("b-older") == "" || younger == "" {
		t.Fatalf("the holds are %v and %v", h.s.order.ownHold("b-older"), younger)
	}

	for _, onHost := range []string{"host-a", "host-b", "host-c"} {
		h.freeFill(t, onHost)
		h.freeFill(t, onHost)
	}

	h.want(t, "b-older", 3)

	if got := startedTiers(h.log)["b-older"]; got != 2 {
		t.Errorf("the older tier started %d in one pass; want its own host and the open one", got)
	}

	h.want(t, "c-younger", 1)

	if got := startedTiers(h.log)["c-younger"]; got != 1 {
		t.Errorf("the younger tier started %d with the host held for it empty", got)
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
// held ahead of it is no room, so a tier as wide does not take what it has on the other.
func TestAPurchaseThroughAListenerLeavesAWaiterRoomOffTheHeldHosts(t *testing.T) {
	t.Parallel()

	h := newHeldScene(t, openState(t), tierOf("b-wide", 8), tierOf("c-waiter", 4), tierOf("d-same", 4))

	h.want(t, "b-wide", 1)
	h.want(t, "c-waiter", 1)

	if got := h.s.order.ownHold("b-wide"); got != "host-a" {
		t.Fatalf("the wide tier holds %q", got)
	}

	h.freeFill(t, "host-a")
	h.freeFill(t, "host-b")
	h.want(t, "d-same", 1)

	if got := startedTiers(h.log)["d-same"]; got != 0 {
		t.Errorf("the tier behind started %d, leaving the waiter ahead only a held host", got)
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
// ledger, with nothing carried over, holds the same host at the first refusal, the one
// the leases' ends say and not the first by name.
func TestAControlPlaneOverTheSameLedgerHoldsTheSameHost(t *testing.T) {
	t.Parallel()

	onA, onB, wide := tierOf("a-small", 2), tierOf("b-small", 2), tierOf("c-wide", 8)
	onA.Node, onB.Node = "host-a", "host-b"
	onA.Memory, onB.Memory, wide.Memory = config.GiB, config.GiB, config.GiB
	tiers := []config.Tier{onA, onB, wide}

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithClock(func() time.Time { return now }))
	for _, h := range []alloc.NodeRegistration{host("host-a", 8), host("host-b", 8)} {
		if _, err := a.RegisterNode(t.Context(), h); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	run := func(label string, job int64) {
		t.Helper()

		lease, err := a.Reserve(t.Context(), label)
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}

		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
			t.Fatalf("Bind: %v", err)
		}

		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, job); err != nil {
			t.Fatalf("Assign: %v", err)
		}

		for _, phase := range []alloc.Phase{alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy} {
			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
				t.Fatalf("Advance: %v", err)
			}
		}
	}

	// host-b's job started first, so it ends first.
	run(onB.Label, 1)

	now = now.Add(9 * time.Minute)

	run(onA.Label, 2)

	held := func() string {
		t.Helper()

		s := New(a, nil, tiers, "hold-test", nil, WithNodeRunner(&launchLog{}), WithAdmissionOrder(config.AdmissionHold))
		l := NewListener(a, wide.Label, &fakeSession{}, s.listenerOpts(nil)...)

		if err := l.reconcilePool(t.Context(), 1); err != nil {
			t.Fatalf("wide reconcile: %v", err)
		}

		return s.order.ownHold(wide.Label)
	}

	if got := held(); got != "host-b" {
		t.Fatalf("the wide tier holds %q; want the host predicted free first", got)
	}

	if got := held(); got != "host-b" {
		t.Errorf("a new control plane holds %q", got)
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

	if got := s.order.ownHold(quiet.Label); got != "" || !s.order.waiting[quiet.Label].behindAsLarge {
		t.Fatalf("%q is held though each host runs a lease as large as the shape", got)
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
