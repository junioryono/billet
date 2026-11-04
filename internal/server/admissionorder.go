package server

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// admissionQueue decides which waiting tier buys the room a finished job leaves.
//
// IT GATES PURCHASES, NEVER ADVERTISEMENTS, and that line is the whole lesson of
// #140: v0.11.0 controlled admission by rotating which tier was allowed to
// advertise, and a tier that is not advertising is never assigned work, so the
// scheduler never learned the work existed. Every tier here goes on advertising
// its ceiling whatever this queue says; what a tier waits for is a lease.
//
// ONE WINNER, CHOSEN BY A WAIT IT CANNOT EXTEND. Under fair the longest-waiting
// tier holds the freed room until its shape fits, which is head-of-line blocking
// on purpose: a large shape only ever fits when several small jobs end together,
// so first-come starves it outright on a fleet that is never idle. It cannot
// deadlock, because the winner is a single tier and waiting never moves it later
// in the order; only buying does (bought), so waiting tiers take turns.
//
// A WAITER HOLDS THE LINE ONLY WHILE ITS LISTENER IS MAKING PROGRESS. The block
// was said to be bounded by the longest job already running, and that assumed
// the winner was always there to buy. On 2026-09-23 it was not: the longest
// waiter's listener sat on a dead connection to GitHub for 18 minutes, could
// neither buy nor give up its place, and every tier sharing its host declined
// every assignment with 78 of 136 vCPU free. So a waiter is dated by its last
// admission progress (a refused reconciliation, or a launch starting or
// finishing) and, once that is older than WaiterAllowance, it stops holding back
// other tiers. A launch still in flight is not progress: on 2026-09-25 a waiter
// launching eight slots one at a time into a node whose command queue took four
// to five minutes a launch held every tier on the fleet for twenty minutes. It
// keeps its place: the moment its listener
// progresses again it is the longest waiter once more. Silence suspends the right
// to block others and proves nothing else; the queue releases, destroys and
// promises nothing, and every purchase is still one of the allocator's atomic
// transactions (an escrow, or a fallback's resize).
//
// A WAIT IS RELEASED BY DEMAND GOING AWAY AS WELL AS BY BEING SERVED. GitHub's
// count is the only thing that says a queued job was cancelled, so a record that
// outlived its demand would hold every other tier for as long as the control
// plane ran — a worse outage than the starvation fairness prevents.
//
// WHAT IT GATES IS EVERYTHING THAT SPENDS CAPACITY. Every one of the listener's
// three purchases asks it, and so does a remote fallback growing a lease that
// already exists to the larger shape the cloud sold (#172): that is a purchase
// of the difference, and before it asked, a fallback took pot a longest-waiting
// tier was accumulating while that tier's own purchases were refused. The
// node asks for it through the node plane's resize route, which internal/app
// binds to this queue through Server.ResizeAdmission once the scheduler exists;
// it is judged as a purchase out of turn, so it is granted while each waiter
// ahead could still be granted one lease (alloc.ResizeLeaving) and otherwise
// fails the launch as a fallback the budget refuses does. The node runtime run
// in process by billet's own harness calls the allocator directly and is not
// gated; no deployment runs one.
//
// A TIER HOLDS THE LINE ONLY WHERE ROOM COULD REACH IT, which is #157. A tier
// waiting on its own max_concurrent, pinned to a host that is gone, or with a
// shape no live host fits is never recorded: freed room elsewhere will never
// let it buy, so holding everyone behind it is an outage with no end. And a
// waiter blocks only the tiers it could take room from: those sharing at least
// one host it could be placed on, unless what refuses it is the deployment
// ceiling, so a tier waiting for a Mac does not stop a Linux tier whose room is
// on another machine (#157, #187).
//
// WEIGHTS CHOOSE THE NEXT HEAD, AND ONLY THAT (#530). When some target has a
// weight, the waiters competing for one room are taken in the order of what
// their targets hold per unit of weight (alloc.TierAdmission.TargetRank), the
// lowest first, so a target the order is behind on catches up while both wait;
// within a target, and between targets that rank alike, it is oldest first as
// above. A target with nobody waiting is no candidate, so its weight claims
// nothing, and a buyer with no wait on record is ranked too: one whose target is
// the furthest behind need not wait behind the others. It is judged on the
// buyer's own fresh read of every target's rank, because a waiter's is as old as
// its last refusal. With no weight, or with any candidate's target unknown, the
// order is oldest first alone: a failed read must not change who goes first.
//
// AND THE ORDER STILL ENDS, because a rank never takes the line from a head. A
// rank moves whenever any job ends, so ranked alone a wide waiter loses its turn
// to whichever rival's job ended last, the room it was accumulating is sold, and
// it never fits (Codex and Opus reviews, 2026-11-04). Three things stand above
// every rank, in this order. A tier below its floor: room is already held for it
// from everyone else, so it takes nothing a waiter could buy, and a waiter that
// its floor keeps short could otherwise be ahead of it forever. A waiter that was
// head when it was refused keeps the line until it buys, as under oldest first.
// And a waiter that has gone WeightPatience without buying, so a target ranked
// behind every rival, whose tier those rivals would otherwise outrank for as long
// as they have work, waits no longer than it would with no weights.
//
// A TIER WHOSE LAUNCH FAILED STEPS ASIDE FOR A WHILE, while ranks apply. Its leases
// hand back at once, so its rank stays the lowest however often it buys, where
// oldest first would rotate it away after each purchase; it would hold a lease's
// room idle for as long as its launches failed. For a while and not until one
// succeeds: a wide tier stepped aside never accumulates the room to try again.
//
// HOLD HOLDS ONE HOST, NOT EVERY HOST (#527). It is fair, except that a waiter given a
// host stops holding the others. Under fair a shape that fits nowhere holds freed room
// on every host it could use until it fits; under fill it may wait without bound. Under
// hold such a waiter is given one host, the one its running work is predicted to free
// soonest (alloc.HoldCandidates), no tier behind it places there, and every other host
// stays open. ONLY A WAITER WITH A HOST LEAVES THE LINE: one with none still holds it on
// the hosts it can use, and no host among them is given to a waiter behind it, because
// nothing else keeps smaller work off the room it waits for; a version that let every
// waiter out starved a shape as large as what the hosts run. At most one hold per host
// and one per tier, in the order's own order, a waiter keeping the host it has while
// that is still about as soon as any, because room accumulated on one host is lost by
// moving to another. It moves once: losing a second host it holds the line as under
// fair, whose bound is the first host that fits, because a host that keeps receding can
// otherwise lead it round the fleet. A HOLD BINDS ONLY WHO STANDS BEHIND ITS OWNER,
// because its owner cannot buy past a waiter ahead of it. Buying gives the host up, at
// the first lease of a pool pass, and the rest of the pass stays off every host still
// held. It is decided here, in memory, from what each waiter last read, and rebuilt
// after a restart from the waiting order, like the rest of this queue. It releases,
// destroys and promises nothing: the purchase is still the allocator's atomic
// transaction. A ceiling-bound waiter and a target's shared pot wait for room a hold
// cannot keep, so they get no host and hold the line for everyone they compete with.
type admissionQueue struct {
	policy config.AdmissionOrder
	now    func() time.Time
	log    *slog.Logger

	mu      sync.Mutex
	waiting map[string]waitingTier
	// lastRanks is the newest read of the targets' ranks any buyer brought, what the
	// status report judges every waiter by, so it never says each of two waiters
	// holds the other from reads taken at different times.
	lastRanks map[string]float64
	// launchFailed is when each tier's last launch failed, kept apart from waiting
	// because a pool's first launch comes before its first refusal.
	launchFailed map[string]time.Time
}

// WaiterAllowance is how long a waiter keeps holding back other tiers without
// admission progress. A healthy listener reconciles before every poll and after
// every empty one, and the longest long poll measured was about 88 seconds; this
// is two of those, a policy choice rather than a proof that a quieter listener
// is dead. Too short costs a large tier its accumulated room; too long is the
// 2026-09-23 stall.
const WaiterAllowance = 3 * time.Minute

// WeightPatience is how long a waiter goes without buying before a rank no longer
// holds it back. A policy choice rather than a measurement: long enough that a
// target whose jobs turn over in minutes is ranked throughout, and short against
// what a job can wait now, so a weight shapes who goes next and cannot starve.
const WeightPatience = 15 * time.Minute

// LaunchBackoff is how long a tier whose launch failed holds nobody back while ranks
// apply: as long as WaiterAllowance, the longest a silent listener may hold others,
// so a failing launch holds them no longer than a silent one, and then it may
// accumulate the room to try again.
const LaunchBackoff = WaiterAllowance

// waitingTier is one tier's place in the order and where its room could come
// from.
type waitingTier struct {
	since time.Time
	// where is the tier's admission as it was last recorded: the hosts it could
	// be placed on, and whether the deployment ceiling or its target's share is
	// what refuses it. An empty host set means the hosts could not be read, which
	// competes with everyone rather than with nobody: an unknown is not a licence
	// to bypass the order.
	//
	// KEPT CURRENT WHILE since IS NOT. A tier's hosts change (one is drained,
	// another joins), and a waiter judged by the set it had when it first waited
	// would block tiers it no longer competes with and let past ones it now
	// does. Its PLACE in the order is what must not move.
	where alloc.TierAdmission
	// progress is the last time this tier's listener did admission work: a
	// refused reconciliation or a launch starting or finishing. Not a successful exchange
	// with GitHub, because a held message is redelivered on every poll while the
	// reconciliation that would re-judge the demand is skipped.
	progress time.Time
	// passed records that another tier has been let past this stalled waiter,
	// so the log says so once per stall rather than on every purchase.
	passed bool
	// head records that this waiter was first in the order when it was refused:
	// it keeps the line until it buys, whatever the ranks do. Only under ranks;
	// without them nothing but buying moves a waiter anyway.
	head bool
	// candidates are the hosts this waiter could be held, as it last read them, the
	// soonest ready first, and held is the one it has. Only under hold.
	candidates []alloc.HoldCandidate
	held       string
	// lost counts the hosts it has lost since it last bought: a second leaves it in the
	// line until it buys. spent records that a pool pass has bought and not yet ended.
	lost  int
	spent bool
}

// newAdmissionQueue builds the queue every listener of one control plane shares.
func newAdmissionQueue(policy config.AdmissionOrder) *admissionQueue {
	return &admissionQueue{
		policy:       policy.Or(),
		now:          time.Now,
		log:          slog.Default(),
		waiting:      map[string]waitingTier{},
		launchFailed: map[string]time.Time{},
	}
}

// holdsTheLine requires q.mu. A waiter that progressed within WaiterAllowance
// holds back the tiers it competes with.
func holdsTheLine(waiter waitingTier, now time.Time) bool {
	return now.Sub(waiter.progress) <= WaiterAllowance
}

// gates reports whether this queue decides anything: under fill, and for the
// nil queue a standalone listener has, every purchase is admitted, so a caller
// need not read the ledger to ask.
func (q *admissionQueue) gates() bool {
	return q != nil && q.policy != config.AdmissionFill
}

// holds reports whether this queue holds hosts for waiters.
func (q *admissionQueue) holds() bool {
	return q != nil && q.policy == config.AdmissionHold
}

// blocks requires q.mu. It reports whether a waiter holds a buyer at where back. Under
// hold a waiter with a host has what it needs from the order, and one with none holds
// back only a buyer with nowhere else to go; a buyer that has is kept off the waiter's
// hosts (heldAgainstLocked). A ceiling and a shared pot are not on any host.
func (q *admissionQueue) blocks(where alloc.TierAdmission, waiter waitingTier) bool {
	if !competes(where, waiter.where) {
		return false
	}

	if q.policy != config.AdmissionHold {
		return true
	}

	if waiter.held != "" {
		return false
	}

	if waiter.where.CeilingBound || (where.Target == waiter.where.Target && where.TargetShared && waiter.where.TargetShared) {
		return true
	}

	if len(where.Nodes) == 0 || len(waiter.where.Nodes) == 0 {
		return true
	}

	return !slices.ContainsFunc(where.Nodes, func(host string) bool { return !slices.Contains(waiter.where.Nodes, host) })
}

// HoldSlack is how much later a host a waiter holds may be predicted ready than the
// soonest it could have, before it moves. Long enough that a prediction's noise does not
// throw away accumulated room; short enough that a hold does not sit for hours on a host
// whose job outran its kind while another would have freed in minutes. A policy choice.
const HoldSlack = 10 * time.Minute

// inOrder requires q.mu. It is the waiting tiers in the order's own order, as last read.
func (q *admissionQueue) inOrder(now time.Time) []string {
	order := make([]string, 0, len(q.waiting))
	for label := range q.waiting {
		order = append(order, label)
	}

	slices.SortFunc(order, func(x, y string) int {
		px, py := standing(x, q.waiting[x], q.lastRanks, now), standing(y, q.waiting[y], q.lastRanks, now)
		if px.before(py) {
			return -1
		}

		if py.before(px) {
			return 1
		}

		return 0
	})

	return order
}

// planHolds requires q.mu. It gives each waiting tier one host, in the order's own
// order, each keeping the host it has if nobody ahead took it. So the first in line
// always has first pick. A waiter that stepped aside, is in a pool pass that has bought,
// or has no candidates holds nothing, and the last of those keeps the hosts it can use
// from everyone behind it.
func (q *admissionQueue) planHolds() {
	if q.policy != config.AdmissionHold {
		return
	}

	now := q.now()
	order := q.inOrder(now)

	taken := map[string]bool{}
	next := make(map[string]string, len(order))

	// KEPT WHILE IT IS STILL ABOUT AS SOON AS ANY: a hold must not sit on a host whose
	// job outran its kind while another would free in minutes.
	keeps := func(waiter *waitingTier) bool {
		return slices.ContainsFunc(waiter.candidates, func(c alloc.HoldCandidate) bool {
			return c.Node == waiter.held && !c.ReadyAt.After(waiter.candidates[0].ReadyAt.Add(HoldSlack))
		})
	}

	for _, label := range order {
		waiter := q.waiting[label]
		if q.steppedAside(label, waiter, q.lastRanks, now) || waiter.spent {
			continue
		}

		if waiter.held != "" && keeps(&waiter) && !taken[waiter.held] {
			taken[waiter.held] = true
			next[label] = waiter.held

			continue
		}

		// COUNTED WHEN A HOST IS LOST, so a waiter losing a second has none left.
		if waiter.held != "" {
			waiter.lost++
			q.waiting[label] = waiter
		}

		pick := ""

		if waiter.lost < 2 {
			for _, c := range waiter.candidates {
				if !taken[c.Node] {
					pick = c.Node

					break
				}
			}
		}

		if pick != "" {
			taken[pick] = true
			next[label] = pick

			continue
		}

		for _, host := range waiter.where.Nodes {
			taken[host] = true
		}
	}

	for _, label := range order {
		waiter := q.waiting[label]
		if next[label] == waiter.held {
			continue
		}

		switch {
		case next[label] != "":
			q.log.Info("holding a host for a waiting tier whose shape fits on none; no other tier is placed there",
				"tier", label, "host", next[label], "released", waiter.held, "waiting_since", waiter.since)
		default:
			q.log.Info("a waiting tier no longer holds a host", "tier", label, "host", waiter.held)
		}

		waiter.held = next[label]
		q.waiting[label] = waiter
	}
}

// heldAgainstLocked requires q.mu and planned holds. It is the hosts this tier places
// nothing on, each with its owner: those held for tiers standing ahead of it, and those
// of a waiter ahead with no host, unless it is one this purchase leaves room for
// (protect). A tier not waiting stands behind all, and one in a pool pass that has
// bought keeps off every hold.
func (q *admissionQueue) heldAgainstLocked(tier string, protect []string, now time.Time) map[string]string {
	out := map[string]string{}
	own := q.waiting[tier]
	behind := true

	for _, label := range q.inOrder(now) {
		if label == tier {
			if !own.spent {
				break
			}

			behind = false

			continue
		}

		waiter := q.waiting[label]

		switch {
		case waiter.held != "":
			out[waiter.held] = label
		case behind && !waiter.spent && !slices.Contains(protect, label) && !q.steppedAside(label, waiter, q.lastRanks, now):
			for _, host := range waiter.where.Nodes {
				if _, owned := out[host]; !owned {
					out[host] = label
				}
			}
		}
	}

	return out
}

// heldAgainst is the hosts this tier places nothing on, each with its owner. Empty
// unless the order holds hosts.
func (q *admissionQueue) heldAgainst(tier string) map[string]string {
	if !q.holds() {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.planHolds()

	return q.heldAgainstLocked(tier, nil, q.now())
}

// avoiding is the hosts a purchase of this tier places nothing on, in a fixed order.
func (q *admissionQueue) avoiding(tier string) []string {
	return slices.Sorted(maps.Keys(q.heldAgainst(tier)))
}

// avoidedBy is what a purchase of this tier and each tier it leaves room for place
// nothing on, read once under one plan: a purchase and the room it must leave its
// waiters are measured against the same holds.
func (q *admissionQueue) avoidedBy(tier string, protect []string) map[string][]string {
	if !q.holds() {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.planHolds()

	now := q.now()
	out := make(map[string][]string, len(protect)+1)

	for _, label := range append([]string{tier}, protect...) {
		out[label] = slices.Sorted(maps.Keys(q.heldAgainstLocked(label, protect, now)))
	}

	return out
}

// boughtOne records one lease of a pool pass bought, whose re-dating waits for the end
// of the pass. The host goes now: a tier with a backlog would otherwise keep its place
// and sweep every host a younger waiter holds, in one pass.
func (q *admissionQueue) boughtOne(tier string) {
	if !q.holds() {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if waiter, ok := q.waiting[tier]; ok {
		waiter.held, waiter.candidates, waiter.spent = "", nil, true
		q.waiting[tier] = waiter
	}
}

// ownHold is the host held for this tier, or empty.
func (q *admissionQueue) ownHold(tier string) string {
	if !q.holds() {
		return ""
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.planHolds()

	return q.waiting[tier].held
}

// mayHold records the hosts a waiting tier could be held, as it just read them.
func (q *admissionQueue) mayHold(tier string, candidates []alloc.HoldCandidate) {
	if !q.holds() {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if waiter, ok := q.waiting[tier]; ok {
		waiter.candidates = candidates
		q.waiting[tier] = waiter
	}

	q.planHolds()
}

// competes reports whether a buyer contends with a waiter for the same room.
//
// THREE WAYS TO COMPETE, and the last two are easy to miss: they share a host
// either could be placed on; the waiter is ceiling-bound, so what refuses it is
// the deployment ceiling, which every purchase anywhere spends; or they belong
// to one target with a share, which is a pot its tiers buy from wherever their
// hosts are. Under such a pot a tier pinned to one host and a tier pinned to
// another still take room from each other, and a rule that looked only at hosts
// let a stream of small jobs on one host starve a large tier waiting on the
// other.
//
// THE CEILING IS THE WAITER'S, NOT THE DEPLOYMENT'S (#187). It used to be a
// fact about the deployment (the ceiling is smaller than the hosts), true of
// every tier at once, so a ceiling a couple of vCPU below the hosts had a tier
// waiting for a Mac's macOS slot hold every Linux tier on another machine. A
// waiter whose own hosts are full is waiting for those hosts, and only a buyer
// on one of them takes what it waits for, wherever the ceiling sits; the
// buyer's own relation to the ceiling says nothing about that.
//
// A tier whose hosts are unknown competes with every tier, so a failed read
// never lets a purchase skip the order.
//
// ONE WAY NOT TO, AND IT IS DIRECTIONAL (#192). A waiter held by its own
// target's share takes nothing another target frees, so it does not hold that
// target's buyers back. Only the WAITER's flag decides it: a buyer's own share
// being full says nothing about whether the waiter it would pass could use the
// room. Both targets must be known, because an unknown one may be the waiter's
// own. The flags are the waiter's view at its last refusal, so each can be one
// poll stale: a share-bound one in the direction that favours the other target,
// a host-bound one in the direction that lets one purchase elsewhere take the
// ceiling it would have needed, after which it is ceiling-bound.
func competes(buyer, waiter alloc.TierAdmission) bool {
	known := buyer.Target != "" && waiter.Target != ""

	if known && buyer.Target != waiter.Target && waiter.ShareBound {
		return false
	}

	if known && buyer.Target == waiter.Target && buyer.TargetShared && waiter.TargetShared {
		return true
	}

	if waiter.CeilingBound {
		return true
	}

	if len(buyer.Nodes) == 0 || len(waiter.Nodes) == 0 {
		return true
	}

	for _, x := range buyer.Nodes {
		if slices.Contains(waiter.Nodes, x) {
			return true
		}
	}

	return false
}

// What stands above a rank, in order; a tier in neither is ranked.
const (
	placeBelowFloor = iota
	placeHead
	placeRanked
)

// place is where one tier stands in the order: by what stands above a rank, then
// by its target's rank, then by when it began waiting, then by label, so two tiers
// inside one clock tick still have one winner rather than alternating with the
// map's iteration order and letting neither accumulate room. With no weights every
// tier is ranked alike, which leaves the wait and the label.
type place struct {
	class int
	rank  float64
	since time.Time
	label string
}

func (p place) before(other place) bool {
	if p.class != other.class {
		return p.class < other.class
	}

	if p.rank != other.rank {
		return p.rank < other.rank
	}

	if !p.since.Equal(other.since) {
		return p.since.Before(other.since)
	}

	return p.label < other.label
}

// notWaiting dates a buyer with no wait on record: after every waiter of its
// rank, whatever the clock says.
var notWaiting = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// steppedAside requires q.mu. It reports a waiter that holds nobody back for now and
// keeps its place: one whose listener stalled, or, under ranks, whose launch just
// failed.
func (q *admissionQueue) steppedAside(label string, waiter waitingTier, ranks map[string]float64,
	now time.Time,
) bool {
	if !holdsTheLine(waiter, now) {
		return true
	}

	failed, failing := q.launchFailed[label]

	return ranks != nil && failing && now.Sub(failed) < LaunchBackoff
}

// ranksFor requires q.mu. It is the ranks a buyer at where is judged by, nil when
// there are none or when they cannot place the buyer and every waiter holding it
// back, which leaves the order oldest first.
func (q *admissionQueue) ranksFor(tier string, where alloc.TierAdmission, now time.Time) map[string]float64 {
	if where.TargetRank == nil {
		return nil
	}

	if _, known := where.TargetRank[where.Target]; !known {
		return nil
	}

	for label := range q.waiting {
		waiter := q.waiting[label]
		if label != tier && (!q.blocks(where, waiter) || !holdsTheLine(waiter, now)) {
			continue
		}

		if _, known := where.TargetRank[waiter.where.Target]; !known {
			return nil
		}
	}

	return where.TargetRank
}

// standing is where a waiter stands for a buyer judged by ranks.
func standing(label string, waiter waitingTier, ranks map[string]float64, now time.Time) place {
	out := place{class: placeRanked, since: waiter.since, label: label}

	switch {
	case ranks == nil:
	case waiter.where.BelowFloor:
		out.class = placeBelowFloor
	case waiter.head || now.Sub(waiter.since) >= WeightPatience:
		out.class = placeHead
	default:
		out.rank = ranks[waiter.where.Target]
	}

	return out
}

// placeOf requires q.mu. It is where a buyer at where stands: as its own record
// says if it is waiting, on the ground it reads now, and after every waiter of its
// rank if it is not.
func (q *admissionQueue) placeOf(tier string, where alloc.TierAdmission, ranks map[string]float64,
	now time.Time,
) place {
	own, waiting := q.waiting[tier]
	if !waiting {
		own.since = notWaiting
	}

	own.where = where

	return standing(tier, own, ranks, now)
}

// mayBuy reports whether this tier may buy capacity now.
//
// A nil queue admits everything: a standalone listener has no peers to be fair
// between, and a test that builds one directly is not testing this.
// The admission is where this tier could be placed now; only a waiter it
// competes with can hold it back.
func (q *admissionQueue) mayBuy(tier string, where alloc.TierAdmission) bool {
	if q == nil || q.policy == config.AdmissionFill {
		return true
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.noteRanks(where)
	q.planBefore(tier, where)

	first, _, stalled := q.head(tier, where)

	allowed := first == "" || first == tier
	if allowed {
		q.letPast(tier, where, stalled)
	}

	return allowed
}

// head requires q.mu. It is the waiter that holds the line for a tier at where,
// which may be the tier itself, and the stalled waiters that stepped aside for
// it; an empty label means nobody holds it. It logs nothing, so naming the
// holder does not spend letPast's once-per-stall line.
func (q *admissionQueue) head(tier string, where alloc.TierAdmission) (string, time.Time, []string) {
	return q.headAmong(tier, where, q.blocks)
}

// planBefore requires q.mu. Under hold it brings what the order reads up to this read
// before it decides anything: this buyer's own standing, which its record may have kept
// since it was below its floor, and the holds a purchase will be made against, so
// whether it may buy, whom it leaves room for and what it avoids are one plan.
func (q *admissionQueue) planBefore(tier string, where alloc.TierAdmission) {
	if q.policy != config.AdmissionHold {
		return
	}

	if waiter, ok := q.waiting[tier]; ok {
		waiter.where = where
		waiter.where.Nodes = slices.Clone(where.Nodes)
		q.waiting[tier] = waiter
	}

	q.planHolds()
}

// headAmong is head over the waiters inLine says hold this buyer back.
func (q *admissionQueue) headAmong(tier string, where alloc.TierAdmission,
	inLine func(alloc.TierAdmission, waitingTier) bool,
) (string, time.Time, []string) {
	var best place

	now := q.now()
	ranks := q.ranksFor(tier, where, now)

	var stalled []string

	for label := range q.waiting {
		waiter := q.waiting[label]
		if label != tier && !inLine(where, waiter) {
			continue
		}

		// A STALLED WAITER STEPS ASIDE BUT KEEPS ITS PLACE. Its own record is never
		// skipped, so a tier is not let past itself.
		if label != tier && q.steppedAside(label, waiter, ranks, now) {
			stalled = append(stalled, label)

			continue
		}

		candidate := standing(label, waiter, ranks, now)
		if label == tier {
			candidate = q.placeOf(tier, where, ranks, now)
		}

		if best.label == "" || candidate.before(best) {
			best = candidate
		}
	}

	// A BUYER WITH NO WAIT ON RECORD IS RANKED TOO, so a target the order is behind
	// on need not queue behind the targets it is behind.
	if _, waiting := q.waiting[tier]; !waiting {
		if own := q.placeOf(tier, where, ranks, now); best.label == "" || own.before(best) {
			best = own
		}
	}

	return best.label, best.since, stalled
}

// heldBy names the waiter that holds the line ahead of a tier at where, and
// when it began waiting: what mayBuy refused this tier for. False when nothing
// does, which includes the tier being at the head itself.
func (q *admissionQueue) heldBy(tier string, where alloc.TierAdmission) (string, time.Time, bool) {
	if q == nil || q.policy == config.AdmissionFill {
		return "", time.Time{}, false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	return q.heldByLocked(tier, where)
}

// waiterHeldBy is heldBy for a tier the queue records as waiting, judged at the
// ground it last waited on; false for a tier that is not waiting, which has no
// purchase being held back. It is what the status report publishes, asked of
// the queue when the report is written rather than remembered from a refusal,
// so it names who holds the line now.
func (q *admissionQueue) waiterHeldBy(tier string) (string, time.Time, bool) {
	if q == nil || q.policy == config.AdmissionFill {
		return "", time.Time{}, false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	own, waiting := q.waiting[tier]
	if !waiting {
		return "", time.Time{}, false
	}

	where := own.where
	if q.lastRanks != nil {
		where.TargetRank = q.lastRanks
	}

	return q.heldByLocked(tier, where)
}

// whyAhead says why the waiter holding this tier back stands ahead of it, where a
// rank put it there: a refusal that said only who, with a younger waiter named,
// would read as a bug. Empty with no ranks, where the wait says it all.
func (q *admissionQueue) whyAhead(waiter string, where alloc.TierAdmission) string {
	if q == nil || q.policy == config.AdmissionFill {
		return ""
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.now()

	ranks := q.ranksFor(waiter, where, now)
	if ranks == nil {
		return ""
	}

	w, ok := q.waiting[waiter]
	if !ok {
		return ""
	}

	switch standing(waiter, w, ranks, now).class {
	case placeBelowFloor:
		return "it is below its floor"
	case placeHead:
		return "it was first when it was refused, or has waited past the patience a weight allows"
	default:
		return fmt.Sprintf("its target holds %.3f per unit of weight and this tier's %.3f", ranks[w.where.Target],
			ranks[where.Target])
	}
}

// heldByLocked requires q.mu.
func (q *admissionQueue) heldByLocked(tier string, where alloc.TierAdmission) (string, time.Time, bool) {
	first, since, _ := q.head(tier, where)
	if first == "" || first == tier {
		return "", time.Time{}, false
	}

	return first, since, true
}

// ahead lists the waiters that hold the line ahead of tier, judged as mayBuy
// judges them: competing, not stalled, and before it in the order.
// A tier refused by mayBuy may still buy if what it buys leaves each of these
// room for one of its own, which the allocator decides atomically
// (alloc.EscrowLeaving).
func (q *admissionQueue) ahead(tier string, where alloc.TierAdmission) []string {
	if q == nil || q.policy == config.AdmissionFill {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.planBefore(tier, where)

	now := q.now()
	ranks := q.ranksFor(tier, where, now)
	own := q.placeOf(tier, where, ranks, now)

	var out []string
	for label := range q.waiting {
		waiter := q.waiting[label]
		if label == tier || !q.blocks(where, waiter) || q.steppedAside(label, waiter, ranks, now) {
			continue
		}

		if own.before(standing(label, waiter, ranks, now)) {
			continue
		}

		out = append(out, label)
	}

	slices.Sort(out)

	return out
}

// letPast requires q.mu. It logs, once per stall, each stalled waiter this
// buyer really did get ahead of: one that would otherwise have come first.
func (q *admissionQueue) letPast(tier string, where alloc.TierAdmission, stalled []string) {
	now := q.now()
	ranks := q.ranksFor(tier, where, now)
	own := q.placeOf(tier, where, ranks, now)

	for _, label := range stalled {
		waiter := q.waiting[label]
		if waiter.passed || own.before(standing(label, waiter, ranks, now)) {
			continue
		}

		waiter.passed = true
		q.waiting[label] = waiter
		q.log.Warn("letting a tier past a waiter whose listener has made no admission progress, or whose "+
			"launches are failing; it keeps its place and holds the line again when that ends",
			"tier", tier, "waiter", label, "waiting_since", waiter.since,
			"last_progress", waiter.progress, "allowance", WaiterAllowance)
	}
}

// waits records that this tier has demand it could not buy for.
//
// The FIRST refusal is what dates the wait: a tier asked and refused on every
// poll would otherwise keep resetting its own place in the order and never
// reach the front.
func (q *admissionQueue) waits(tier string, where alloc.TierAdmission) {
	if q == nil {
		return
	}

	where.Nodes = slices.Clone(where.Nodes)

	q.mu.Lock()
	defer q.mu.Unlock()

	q.noteRanks(where)

	// THE PLACE IS KEPT AND THE GROUND UNDER IT IS REFRESHED: a tier that waited
	// for a host which has since drained competes for where it could go NOW,
	// while the wait that earns it the freed room is still dated from its first
	// refusal.
	if previous, ok := q.waiting[tier]; ok {
		previous.where = where
		q.progressed(tier, previous)
	} else {
		now := q.now()
		q.waiting[tier] = waitingTier{since: now, where: where, progress: now}
	}

	// A HEAD THAT WAS REFUSED KEEPS THE LINE until it buys, so what it is
	// accumulating is not sold to whichever rival's job ended last. A waiter with a
	// host counts here, under hold as under fair, or everyone behind one would read
	// themselves as first.
	if first, _, _ := q.headAmong(tier, where, func(where alloc.TierAdmission, waiter waitingTier) bool {
		return competes(where, waiter.where)
	}); first == tier && where.TargetRank != nil {
		waiter := q.waiting[tier]
		waiter.head = true
		q.waiting[tier] = waiter
	}
}

// noteRanks requires q.mu.
func (q *admissionQueue) noteRanks(where alloc.TierAdmission) {
	if where.TargetRank != nil {
		q.lastRanks = where.TargetRank
	}
}

// progressed requires q.mu. It dates a waiter's admission progress and says so
// when that ends a stall other tiers were let past.
func (q *admissionQueue) progressed(tier string, waiter waitingTier) {
	if waiter.passed {
		q.log.Info("a waiter's listener is making admission progress again; it holds the line from its original place",
			"waiter", tier, "waiting_since", waiter.since)
	}

	waiter.progress = q.now()
	waiter.passed = false
	q.waiting[tier] = waiter
}

// launchBegins and launchEnds date a waiter's progress at each end of a launch.
// The launch between them is not progress, so one that outlasts
// WaiterAllowance lets other tiers past.
func (q *admissionQueue) launchBegins(tier string) { q.launchMark(tier) }

func (q *admissionQueue) launchEnds(tier string) { q.launchMark(tier) }

// launchResult records how this tier's last launch went. A failure that left
// compute in the runner's custody counts too: nothing is running for the job.
func (q *admissionQueue) launchResult(tier string, failed bool) {
	if q == nil {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if failed {
		q.launchFailed[tier] = q.now()
	} else {
		delete(q.launchFailed, tier)
	}
}

func (q *admissionQueue) launchMark(tier string) {
	if q == nil {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if waiter, ok := q.waiting[tier]; ok {
		q.progressed(tier, waiter)
	}
}

// bought moves a waiting tier that has just bought capacity to the back of the
// order, keeping it waiting.
//
// A PLACE IS WON FOR ONE PURCHASE, NOT FOR A BACKLOG. A waiter used to keep its
// place until its whole demand was met, and a tier whose demand keeps arriving
// is never met: on 2026-09-25 a repository's CI backlog held the line for half
// an hour and an organization's tier on the same host started nothing, its
// assignments declined and reassigned every five minutes. Re-dated here, the
// waiting tiers take turns one purchase each, and a large shape at the head
// still holds the freed room until it fits.
func (q *admissionQueue) bought(tier string) {
	if q == nil {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	waiter, ok := q.waiting[tier]
	if !ok {
		return
	}

	// A PURCHASE GIVES UP THE HOST AS IT GIVES UP THE PLACE: the waiter reads what it
	// could hold next at its next refusal, and until then no host sits idle for it.
	waiter.since = q.now()
	waiter.head = false
	waiter.held, waiter.candidates, waiter.lost, waiter.spent = "", nil, 0, false
	q.progressed(tier, waiter)
}

// served records that this tier's demand is met, or gone.
func (q *admissionQueue) served(tier string) {
	if q == nil {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.waiting, tier)
}

// waitingSince reports when a tier began waiting and when its listener last made
// admission progress, for the status report.
func (q *admissionQueue) waitingSince(tier string) (time.Time, time.Time, bool) {
	if q == nil {
		return time.Time{}, time.Time{}, false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	waiter, ok := q.waiting[tier]

	return waiter.since, waiter.progress, ok
}
