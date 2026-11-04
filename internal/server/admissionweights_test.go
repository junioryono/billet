package server

import (
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// ranked is a tier's admission on one shared host under ranks.
func ranked(target string, ranks map[string]float64) alloc.TierAdmission {
	where := on("host")
	where.Target, where.TargetRank = target, ranks

	return where
}

// waiterAt is a tier and the ground it waits on.
type waiterAt struct {
	label string
	where alloc.TierAdmission
}

// waitingInOrder records each label as waiting a minute after the last, so the
// first named is the oldest.
func waitingInOrder(q *admissionQueue, waiters ...waiterAt) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return at }

	for _, w := range waiters {
		q.waits(w.label, w.where)
		at = at.Add(time.Minute)
	}
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

	y.order.waits("second-tier", y.admission(t.Context()))

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
