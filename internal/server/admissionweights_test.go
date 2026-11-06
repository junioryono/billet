package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

// ranked is a tier's admission on one shared host under ranks.
func ranked(target string, ranks map[string]float64) alloc.TierAdmission {
	where := on("host")
	where.VCPU, where.Target, where.TargetRank = 2, target, ranks

	return where
}

// waiterAt is a tier and the ground it waits on.
type waiterAt struct {
	label string
	where alloc.TierAdmission
}

// waitingInOrder records each label as waiting a minute after the last, the first
// named the oldest, each as a waiter that has not yet been refused at the head.
func waitingInOrder(q *admissionQueue, waiters ...waiterAt) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	for _, w := range waiters {
		q.waiting[w.label] = waitingTier{since: at, where: w.where, progress: at}
		at = at.Add(time.Minute)
	}

	q.now = func() time.Time { return at }
}

// WITH NO WEIGHT THE ORDER IS OLDEST FIRST, whatever targets the tiers are on: a
// deployment that never writes a weight changes in no way.
func TestWithNoWeightTheOrderIsOldestFirst(t *testing.T) {
	t.Parallel()

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"older", ranked("heavy", nil)}, waiterAt{"younger", ranked("light", nil)})

	if q.mayBuy("younger", ranked("light", nil)) {
		t.Error("the younger waiter bought ahead of the older with no weight written")
	}

	if !q.mayBuy("older", ranked("heavy", nil)) {
		t.Error("the older waiter could not buy with no weight written")
	}
}

// WEIGHTED, THE TARGET HOLDING LEAST PER UNIT OF WEIGHT GOES FIRST, however long
// the other has waited, and every question the order answers says so.
func TestTheTargetHoldingLeastPerWeightGoesFirst(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"heavy": 0.5, "light": 0.1}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"older", ranked("heavy", ranks)}, waiterAt{"younger", ranked("light", ranks)})

	if !q.mayBuy("younger", ranked("light", ranks)) {
		t.Error("the waiter of the target furthest behind could not buy")
	}

	if q.mayBuy("older", ranked("heavy", ranks)) {
		t.Error("the older waiter bought ahead of a target further behind")
	}

	if holder, _, held := q.heldBy("older", ranked("heavy", ranks)); !held || holder != "younger" {
		t.Errorf("the older waiter is held by %q (%v); want the younger", holder, held)
	}

	if got := q.ahead("older", ranked("heavy", ranks)); !slices.Equal(got, []string{"younger"}) {
		t.Errorf("ahead of the older waiter: %v; want the younger", got)
	}

	if got := q.ahead("younger", ranked("light", ranks)); len(got) != 0 {
		t.Errorf("ahead of the younger waiter: %v; want nobody", got)
	}
}

// WITHIN A TARGET IT STAYS OLDEST FIRST, and so does a tie between targets, each
// with the other target's tier ranked where it belongs.
func TestWithinATargetAndOnATieTheOrderIsOldestFirst(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"a": 0.1, "b": 0.5}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"b-oldest", ranked("b", ranks)}, waiterAt{"a-older", ranked("a", ranks)},
		waiterAt{"a-younger", ranked("a", ranks)})

	if !q.mayBuy("a-older", ranked("a", ranks)) || q.mayBuy("a-younger", ranked("a", ranks)) {
		t.Error("within one target the older tier did not go first")
	}

	if got := q.ahead("a-younger", ranked("a", ranks)); !slices.Equal(got, []string{"a-older"}) {
		t.Errorf("ahead of the younger tier of its target: %v; want its older sibling alone", got)
	}

	tied := map[string]float64{"a": 0.3, "b": 0.3}

	q = newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"older", ranked("a", tied)}, waiterAt{"younger", ranked("b", tied)})

	if !q.mayBuy("older", ranked("a", tied)) || q.mayBuy("younger", ranked("b", tied)) {
		t.Error("between targets that rank alike the older tier did not go first")
	}
}

// AN IDLE TARGET'S WEIGHT CLAIMS NOTHING: a target far behind with nobody waiting
// and nobody buying holds no room for itself.
func TestAnIdleTargetClaimsNothing(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"busy": 0.9, "idle": 0}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"waiter", ranked("busy", ranks)})

	if !q.mayBuy("waiter", ranked("busy", ranks)) {
		t.Error("a waiter was held back by a target with nothing waiting")
	}

	if _, _, held := q.heldBy("waiter", ranked("busy", ranks)); held {
		t.Error("a waiter was said to be held by a target with nothing waiting")
	}
}

// A BUYER WITH NO WAIT ON RECORD IS RANKED: ahead of waiters of targets it is
// behind, and behind any waiter of a target it is not behind, ties included,
// since it has waited for nothing.
func TestABuyerWithNoWaitIsRanked(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"held": 0.5, "behind": 0.1, "level": 0.5, "ahead": 0.7}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"waiter", ranked("held", ranks)})

	for _, c := range []struct {
		target string
		may    bool
	}{{"behind", true}, {"level", false}, {"ahead", false}, {"held", false}} {
		if got := q.mayBuy("buyer", ranked(c.target, ranks)); got != c.may {
			t.Errorf("a buyer of target %s: mayBuy %v; want %v", c.target, got, c.may)
		}

		want := []string{"waiter"}
		if c.may {
			want = nil
		}

		if got := q.ahead("buyer", ranked(c.target, ranks)); !slices.Equal(got, want) {
			t.Errorf("a buyer of target %s: ahead %v; want %v", c.target, got, want)
		}
	}
}

// A WAITER THAT DOES NOT COMPETE IS NOT RANKED AGAINST: a target further behind on
// another host takes nothing from this one.
func TestAWaiterOnAnotherHostIsNotRankedAgainst(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"here": 0.9, "there": 0.1}

	there := on("other-host")
	there.Target, there.TargetRank = "there", ranks

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"elsewhere", there}, waiterAt{"waiter", ranked("here", ranks)})

	if !q.mayBuy("waiter", ranked("here", ranks)) {
		t.Error("a waiter was held by a target further behind on a host it does not share")
	}
}

// A STALLED WAITER STEPS ASIDE, THOUGH ITS TARGET IS FURTHEST BEHIND: a rank never
// lets a listener with no progress hold the line, and it keeps its place.
func TestAStalledWaiterStepsAsideWhateverItsRank(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"behind": 0.1, "ahead": 0.5}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"stalled", ranked("behind", ranks)})

	at := q.now()
	q.now = func() time.Time { return at.Add(WaiterAllowance + time.Second) }

	if !q.mayBuy("buyer", ranked("ahead", ranks)) {
		t.Error("a stalled waiter held a buyer back because of its rank")
	}

	if got := q.ahead("buyer", ranked("ahead", ranks)); len(got) != 0 {
		t.Errorf("a stalled waiter is listed ahead: %v", got)
	}
}

// A WAITER IT DOES NOT COMPETE WITH CANNOT TURN THE RANKS OFF: an unread target on
// a host this buyer does not share is no reason to fall back to oldest first.
func TestAnUnknownWaiterElsewhereLeavesTheRanksOn(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"heavy": 0.5, "light": 0.1}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"elsewhere", on("other-host")}, waiterAt{"older", ranked("heavy", ranks)},
		waiterAt{"younger", ranked("light", ranks)})

	if !q.mayBuy("younger", ranked("light", ranks)) || q.mayBuy("older", ranked("heavy", ranks)) {
		t.Error("an unknown waiter on another host turned the ranks off")
	}
}

// ANY TARGET THE RANKS CANNOT PLACE LEAVES THE ORDER OLDEST FIRST: a failed read
// is not a way past it, whichever side it failed on.
func TestATargetTheRanksCannotPlaceLeavesTheOrderOldestFirst(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"a": 0.5, "b": 0.1}

	// The older waiter's own read failed, so its target is unknown.
	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"older", on("host")}, waiterAt{"younger", ranked("b", ranks)})

	if q.mayBuy("younger", ranked("b", ranks)) {
		t.Error("a waiter passed an older one whose target could not be read")
	}

	if got := q.ahead("younger", ranked("b", ranks)); !slices.Equal(got, []string{"older"}) {
		t.Errorf("ahead of the younger waiter: %v; want the older", got)
	}

	// A younger waiter whose read failed does not rank as if it held nothing: the
	// order falls back, and the older goes first though its target holds more.
	q = newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"older", ranked("a", ranks)}, waiterAt{"younger", on("host")})

	if !q.mayBuy("older", ranked("a", ranks)) {
		t.Error("an older waiter was held by a younger one whose target could not be read")
	}

	// The buyer's target is one its own ranks do not list.
	q = newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"waiter", ranked("a", ranks)})

	if q.mayBuy("buyer", ranked("departed", ranks)) {
		t.Error("a buyer the ranks cannot place passed a waiter")
	}

	// The buyer's own read failed: no ranks at all.
	if q.mayBuy("buyer", on("host")) {
		t.Error("a buyer whose read failed passed a waiter")
	}
}

// EVERY ANSWER THE ORDER GIVES AGREES: a buyer may buy exactly when nobody is
// ahead of it and nobody holds it, whatever the ranks and the waits, so a refused
// purchase and the room it must leave are judged by one order.
func TestMayBuyHeldByAndAheadAgree(t *testing.T) {
	t.Parallel()

	targets := []string{"a", "b", "c"}
	labels := []string{"one", "two", "three"}

	for _, ranks := range []map[string]float64{
		{"a": 0.1, "b": 0.2, "c": 0.3}, {"a": 0.3, "b": 0.2, "c": 0.1},
		{"a": 0.2, "b": 0.2, "c": 0.2}, {"a": 0.1, "b": 0.1, "c": 0.3},
	} {
		for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {0, 0, 1}, {1, 1, 1}} {
			q := newAdmissionQueue(config.AdmissionFair)

			var waiters []waiterAt
			for i, target := range order {
				waiters = append(waiters, waiterAt{labels[i], ranked(targets[target], ranks)})
			}

			waitingInOrder(q, waiters...)

			buyers := 0

			for _, w := range waiters {
				may := q.mayBuy(w.label, w.where)
				ahead := q.ahead(w.label, w.where)
				_, _, held := q.heldBy(w.label, w.where)

				if may != (len(ahead) == 0) || may == held {
					t.Errorf("ranks %v order %v: %s may buy %v, ahead %v, held %v", ranks, order, w.label, may,
						ahead, held)
				}

				if may {
					buyers++
				}
			}

			if buyers != 1 {
				t.Errorf("ranks %v order %v: %d waiters may buy; want exactly one", ranks, order, buyers)
			}
		}
	}
}

// A POOL'S PURCHASES ARE JUDGED ON WHAT THE LAST ONE HELD: a target behind buys
// its backlog only until it is no longer behind, and then only out of turn,
// leaving the waiter ahead of it room for one lease.
func TestAWeightedPoolStopsBuyingAheadOnceItIsNotBehind(t *testing.T) {
	t.Parallel()

	first, second := tierOf("first-tier", 2), tierOf("second-tier", 2)
	first.Memory, second.Memory = config.GiB, config.GiB
	first.Target, second.Target = "first", "second"

	log, listeners := fairListeners(t, []config.Tier{first, second}, alloc.Limits{
		MaxVCPU: 32, MaxMemory: 64 * config.GiB, Weights: map[string]int{"first": 2, "second": 1},
	}, []alloc.NodeRegistration{host("host", 32)})
	x, y := listeners[0], listeners[1]

	// second holds 8 of 32 vCPU, and waits for more.
	if err := y.reconcilePool(t.Context(), 4); err != nil {
		t.Fatalf("second tier reconcile: %v", err)
	}

	waitingInOrder(y.order, waiterAt{"second-tier", y.admission(t.Context())})

	// first holds nothing and wants more than there is. Its rank passes second's
	// at 8 leases (16 of 32 vCPU over a weight of 2); from there it buys only while
	// that leaves second room for one lease: 11 leases, 22 + 8 of 32 vCPU. Judged
	// on its rank before it bought, it would have taken the last room too.
	if err := x.reconcilePool(t.Context(), 20); err != nil {
		t.Fatalf("first tier reconcile: %v", err)
	}

	if got := startedTiers(log)["first-tier"]; got != 11 {
		t.Errorf("the first target started %d runners; want 11, leaving the second room for one lease", got)
	}
}

// A WAITER THAT WAS HEAD WHEN IT WAS REFUSED KEEPS THE LINE until it buys: a rival's
// job ending lowers that rival's rank, and that must not sell the room the head is
// accumulating. Fresh ranks then choose only the next head.
func TestAHeadKeepsTheLineUntilItBuys(t *testing.T) {
	t.Parallel()

	level := map[string]float64{"wide": 0.5, "stream": 0.5}

	q := newAdmissionQueue(config.AdmissionFair)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return at }

	// The wide tier is refused first, level with the stream, so it is head.
	q.waits("wide", ranked("wide", level))
	at = at.Add(time.Minute)
	q.waits("stream", ranked("stream", level))

	// One of the stream's jobs ends, and its target is now behind.
	behind := map[string]float64{"wide": 0.5, "stream": 0.4}

	if q.mayBuy("stream", ranked("stream", behind)) {
		t.Error("a rival's job ending took the line from the head")
	}

	if got := q.ahead("stream", ranked("stream", behind)); !slices.Equal(got, []string{"wide"}) {
		t.Errorf("ahead of the rival: %v; want the head", got)
	}

	if !q.mayBuy("wide", ranked("wide", behind)) {
		t.Error("the head could not buy")
	}

	// It buys, and the ranks choose the next head.
	at = at.Add(time.Minute)
	q.bought("wide")

	if !q.mayBuy("stream", ranked("stream", behind)) || q.mayBuy("wide", ranked("wide", behind)) {
		t.Error("after the head bought, the target behind did not go next")
	}

	// A tier refused while another is ahead of it is not made head by the refusal.
	q.waits("wide", ranked("wide", behind))

	if q.waiting["wide"].head {
		t.Error("a waiter refused behind another was recorded as head")
	}

	// With no ranks there is no head to keep: the wait is all there is.
	plain := newAdmissionQueue(config.AdmissionFair)
	plain.waits("only", on("host"))

	if plain.waiting["only"].head {
		t.Error("a waiter was recorded as head with no ranks")
	}
}

// A TIER BELOW ITS FLOOR GOES FIRST: room is held for it from everyone else, and a
// waiter its floor keeps short could otherwise stand ahead of it forever, with the
// reserved room idle.
func TestATierBelowItsFloorGoesFirst(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"low-weight": 0.5, "high-weight": 0.1}

	floor := ranked("low-weight", ranks)
	floor.BelowFloor = true

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"older", ranked("high-weight", ranks)}, waiterAt{"reserved", floor})

	// Even over a head.
	older := q.waiting["older"]
	older.head = true
	q.waiting["older"] = older

	if !q.mayBuy("reserved", floor) {
		t.Error("a tier below its floor was held back")
	}

	if q.mayBuy("older", ranked("high-weight", ranks)) {
		t.Error("a head bought ahead of a tier below its floor")
	}

	// On the ground it reads now: once at its floor it is ranked again.
	floor.BelowFloor = false
	if q.mayBuy("reserved", floor) {
		t.Error("a tier at its floor kept its place above the head")
	}

	// And a buyer with no wait on record goes first too.
	q = newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"waiter", ranked("high-weight", ranks)})

	floor.BelowFloor = true
	if !q.mayBuy("reserved", floor) {
		t.Error("a buyer below its floor was held back by a waiter")
	}
}

// A RANK CANNOT STARVE: a tier that has gone WeightPatience without buying stands
// above every rank, though its target holds more than any rival, and it is dated by
// its last purchase, so buying makes it wait its turn again.
func TestAWaiterPastItsPatienceStandsAboveEveryRank(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"over": 0.9, "under": 0.1}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"starving", ranked("over", ranks)}, waiterAt{"rival", ranked("under", ranks)})

	start := q.now()

	progressAll := func() {
		for label, w := range q.waiting {
			w.progress = q.now()
			q.waiting[label] = w
		}
	}

	if q.mayBuy("starving", ranked("over", ranks)) {
		t.Error("a waiter inside its patience passed a target further behind")
	}

	// The starving waiter began two minutes before start: a minute short of its
	// patience it is ranked, and on reaching it it is not.
	q.now = func() time.Time { return start.Add(WeightPatience - 3*time.Minute) }
	progressAll()

	if q.mayBuy("starving", ranked("over", ranks)) {
		t.Error("a waiter passed a target further behind before its patience ran out")
	}

	q.now = func() time.Time { return start.Add(WeightPatience) }
	progressAll()

	if !q.mayBuy("starving", ranked("over", ranks)) || q.mayBuy("rival", ranked("under", ranks)) {
		t.Error("a waiter past its patience did not stand above the rank")
	}

	q.bought("starving")

	if q.mayBuy("starving", ranked("over", ranks)) {
		t.Error("a purchase did not put the waiter back behind the rank")
	}
}

// A TIER WHOSE LAUNCH FAILED STEPS ASIDE FOR A WHILE, WHILE RANKS APPLY: its leases
// hand back at once, so its rank stays the lowest however often it buys. For a
// while, or a wide tier stepped aside would never accumulate the room to try again.
// With no ranks it rotates away by buying, as it always did.
func TestATierWhoseLaunchFailedStepsAsideForAWhile(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"failing": 0.1, "healthy": 0.5}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"broken", ranked("failing", ranks)}, waiterAt{"working", ranked("healthy", ranks)})

	if q.mayBuy("working", ranked("healthy", ranks)) {
		t.Error("a healthy waiter passed one further behind with no launch failed")
	}

	failedAt := q.now()
	q.launchResult("broken", true)

	if !q.mayBuy("working", ranked("healthy", ranks)) {
		t.Error("a waiter was held back by a tier whose launch just failed")
	}

	if got := q.ahead("working", ranked("healthy", ranks)); len(got) != 0 {
		t.Errorf("a tier whose launch just failed is listed ahead: %v", got)
	}

	// It keeps its place, and may still try: nobody holds it back but those ahead.
	if !q.mayBuy("broken", ranked("failing", ranks)) {
		t.Error("a tier whose launch failed lost its own turn")
	}

	// THE BACKOFF ENDS, with no launch having succeeded, and it may accumulate the
	// room to try again; another failure renews it.
	progress := func(at time.Time) {
		q.now = func() time.Time { return at }

		for label, w := range q.waiting {
			w.progress = at
			q.waiting[label] = w
		}
	}

	progress(failedAt.Add(LaunchBackoff - time.Second))

	if !q.mayBuy("working", ranked("healthy", ranks)) {
		t.Error("the backoff ended early")
	}

	progress(failedAt.Add(LaunchBackoff))

	if q.mayBuy("working", ranked("healthy", ranks)) {
		t.Error("the backoff never ended, so a recovered tier cannot accumulate room")
	}

	q.launchResult("broken", true)

	if !q.mayBuy("working", ranked("healthy", ranks)) {
		t.Error("another failure did not renew the backoff")
	}

	// A launch that succeeds ends it at once.
	q.launchResult("broken", false)

	if q.mayBuy("working", ranked("healthy", ranks)) {
		t.Error("a launch succeeding did not put the waiter back ahead")
	}

	// A FAILURE BEFORE THE FIRST REFUSAL COUNTS: a pool's first launch comes before
	// it is ever recorded waiting.
	first := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(first, waiterAt{"working", ranked("healthy", ranks)})
	first.launchResult("broken", true)

	first.waiting["broken"] = waitingTier{since: first.now(), where: ranked("failing", ranks), progress: first.now()}

	if !first.mayBuy("working", ranked("healthy", ranks)) {
		t.Error("a failure before the first refusal was lost")
	}

	// With no ranks the failure means nothing: the older waiter holds the line.
	plain := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(plain, waiterAt{"broken", on("host")}, waiterAt{"working", on("host")})
	plain.launchResult("broken", true)

	if plain.mayBuy("working", on("host")) {
		t.Error("with no weights a failed launch changed the order")
	}
}

// THE STATUS REPORT JUDGES EVERY WAITER BY ONE READ OF THE RANKS, the newest any
// buyer brought, so it cannot say each of two waiters holds the other.
func TestTheStatusReportJudgesWaitersByOneReadOfTheRanks(t *testing.T) {
	t.Parallel()

	first := map[string]float64{"a": 0.1, "b": 0.5}
	then := map[string]float64{"a": 0.5, "b": 0.1}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"tier-a", ranked("a", first)}, waiterAt{"tier-b", ranked("b", then)})

	q.mayBuy("tier-a", ranked("a", then))

	if holder, _, held := q.waiterHeldBy("tier-a"); !held || holder != "tier-b" {
		t.Errorf("tier-a is reported held by %q (%v); want tier-b, on the newest ranks", holder, held)
	}

	if _, _, held := q.waiterHeldBy("tier-b"); held {
		t.Error("tier-b is reported held too: each of two waiters holding the other")
	}
}

// A REFUSAL SAYS WHY A RANK PUT THE WAITER AHEAD, since a younger waiter named
// alone reads as a bug, and says nothing extra with no ranks.
func TestARefusalSaysWhyTheWaiterStandsAhead(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"heavy": 0.5, "light": 0.125}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"younger", ranked("light", ranks)})

	got := q.whyAhead("younger", ranked("heavy", ranks))
	if want := "its target holds 0.125 per unit of weight and this tier's 0.500"; got != want {
		t.Errorf("whyAhead = %q; want %q", got, want)
	}

	w := q.waiting["younger"]
	w.head = true
	q.waiting["younger"] = w

	if got := q.whyAhead("younger", ranked("heavy", ranks)); !strings.Contains(got, "first when it was refused") {
		t.Errorf("whyAhead for a head = %q", got)
	}

	w.where.BelowFloor = true
	q.waiting["younger"] = w

	if got := q.whyAhead("younger", ranked("heavy", ranks)); got != "it is below its floor" {
		t.Errorf("whyAhead below a floor = %q", got)
	}

	plain := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(plain, waiterAt{"older", on("host")})

	if got := plain.whyAhead("older", on("host")); got != "" {
		t.Errorf("whyAhead with no ranks = %q; want nothing", got)
	}

	r := refusal{waiter: "younger", since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), ahead: "because"}
	if got := r.String(); !strings.HasSuffix(got, "waiting since 2026-11-01T00:00:00Z: because") {
		t.Errorf("the refusal reads %q", got)
	}
}

// failingRunner refuses every launch, as a revoked key or a broken image does.
type failingRunner struct{ launchLog }

func (*failingRunner) Launch(context.Context, *alloc.Lease, dispatch.Job) error {
	return errors.New("no such image")
}

// THE LISTENER TELLS THE ORDER HOW ITS LAST LAUNCH WENT, and a refusal it records
// says why the waiter stands ahead: both are wiring the queue's own tests cannot see.
func TestAListenerTellsTheOrderItsLaunchFailedAndWhyItIsHeld(t *testing.T) {
	t.Parallel()

	a, _, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFair)
	l := listeners[0]

	// NOT WAITING: a pool's first launch comes before its first refusal.
	launch := func(runner dispatch.Runner, request int64) {
		t.Helper()

		leases, err := a.Escrow(t.Context(), l.tier, 1)
		if err != nil || len(leases) != 1 {
			t.Fatalf("Escrow: %v, %d leases", err, len(leases))
		}

		l.runner = runner

		// What the launch answers is not the question: a failed one hands its lease back.
		if err := l.launch(t.Context(), leases[0], dispatch.Job{RequestID: request}); err != nil {
			t.Logf("launch: %v", err)
		}
	}

	launch(&failingRunner{}, 1)

	if _, failed := l.order.launchFailed[l.tier]; !failed {
		t.Error("a failed launch did not reach the order")
	}

	launch(&launchLog{}, 2)

	if _, failed := l.order.launchFailed[l.tier]; failed {
		t.Error("a launch that succeeded did not clear the failure")
	}

	ranks := map[string]float64{"heavy": 0.5, "light": 0.125}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"younger", ranked("light", ranks)})
	l.order = q

	got := l.orderRefusal(ranked("heavy", ranks))
	if got.waiter != "younger" || !strings.Contains(got.String(), "its target holds 0.125") {
		t.Errorf("the refusal reads %q", got.String())
	}
}

// A STALLED WAITER WHOSE TARGET IS UNREAD DOES NOT TURN THE RANKS OFF: it holds
// nobody back, so nothing about it can change who goes first.
func TestAStalledWaiterLeavesTheRanksOn(t *testing.T) {
	t.Parallel()

	ranks := map[string]float64{"heavy": 0.5, "light": 0.1}

	q := newAdmissionQueue(config.AdmissionFair)
	waitingInOrder(q, waiterAt{"stalled", on("host")}, waiterAt{"older", ranked("heavy", ranks)},
		waiterAt{"younger", ranked("light", ranks)})

	// Only the unread waiter has made no progress.
	q.now = func() time.Time { return q.waiting["stalled"].since.Add(WaiterAllowance + time.Second) }

	for _, label := range []string{"older", "younger"} {
		w := q.waiting[label]
		w.progress = q.now()
		q.waiting[label] = w
	}

	if !q.mayBuy("younger", ranked("light", ranks)) || q.mayBuy("older", ranked("heavy", ranks)) {
		t.Error("a stalled waiter of an unread target turned the ranks off")
	}
}
