package server

import (
	"log/slog"
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
// in the order.
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
// promises nothing, and every purchase is still the allocator's atomic escrow.
//
// A WAIT IS RELEASED BY DEMAND GOING AWAY AS WELL AS BY BEING SERVED. GitHub's
// count is the only thing that says a queued job was cancelled, so a record that
// outlived its demand would hold every other tier for as long as the control
// plane ran — a worse outage than the starvation fairness prevents.
//
// WHAT IT GATES IS THE PURCHASE OF A LEASE. Every one of the listener's three
// purchases asks it, and one thing that spends capacity does not: a remote
// fallback resizes a lease that already exists (`alloc.Resize`) to the shape the
// cloud actually sold, which is re-authorised against the deployment ceiling and
// every floor but not against this queue. The order admitted that lease; the
// resize is asked for by the node, through the node plane's resize route or the
// in-process runtime, and the queue is handed only to listeners, so nothing on
// that path holds it. A fallback to a larger shape can therefore take pot a
// waiter was accumulating, which is #172 and is exempt until that says otherwise.
//
// A TIER HOLDS THE LINE ONLY WHERE ROOM COULD REACH IT, which is #157. A tier
// waiting on its own max_concurrent, pinned to a host that is gone, or with a
// shape no live host fits is never recorded: freed room elsewhere will never
// let it buy, so holding everyone behind it is an outage with no end. And a
// waiter blocks only the tiers it could take room from: those sharing at least
// one host it could be placed on, so a tier waiting for a Mac does not stop a
// Linux tier whose room is on another machine.
type admissionQueue struct {
	policy config.AdmissionOrder
	now    func() time.Time
	log    *slog.Logger

	mu      sync.Mutex
	waiting map[string]waitingTier
}

// WaiterAllowance is how long a waiter keeps holding back other tiers without
// admission progress. A healthy listener reconciles before every poll and after
// every empty one, and the longest long poll measured was about 88 seconds; this
// is two of those, a policy choice rather than a proof that a quieter listener
// is dead. Too short costs a large tier its accumulated room; too long is the
// 2026-09-23 stall.
const WaiterAllowance = 3 * time.Minute

// waitingTier is one tier's place in the order and where its room could come
// from.
type waitingTier struct {
	since time.Time
	// where is the tier's admission as it was last recorded: the hosts it could
	// be placed on, and whether the deployment ceiling is a pot shared with every
	// other tier. An empty host set means the hosts could not be read, which
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
}

// newAdmissionQueue builds the queue every listener of one control plane shares.
func newAdmissionQueue(policy config.AdmissionOrder) *admissionQueue {
	return &admissionQueue{
		policy:  policy.Or(),
		now:     time.Now,
		log:     slog.Default(),
		waiting: map[string]waitingTier{},
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

// competes reports whether a buyer contends with a waiter for the same room.
//
// THREE WAYS TO COMPETE, and the last two are easy to miss: they share a host
// either could be placed on; the deployment's own ceiling is smaller than the
// hosts it caps, which makes it one pot every tier buys from; or they belong to
// one target with a share, which is a pot its tiers buy from wherever their
// hosts are. Under such a pot a tier pinned to one host and a tier pinned to
// another still take room from each other, and a rule that looked only at hosts
// let a stream of small jobs on one host starve a large tier waiting on the
// other.
//
// A tier whose hosts are unknown competes with every tier, so a failed read
// never lets a purchase skip the order.
//
// ONE WAY NOT TO, AND IT IS DIRECTIONAL (#192). A waiter held by its own
// target's share takes nothing another target frees, so it does not hold that
// target's buyers back. Only the WAITER's flag decides it: a buyer's own share
// being full says nothing about whether the waiter it would pass could use the
// room. Both targets must be known, because an unknown one may be the waiter's
// own. The flag is the waiter's view at its last refusal, so it can be one poll
// stale in the direction that favours the other target.
func competes(buyer, waiter alloc.TierAdmission) bool {
	known := buyer.Target != "" && waiter.Target != ""

	if known && buyer.Target != waiter.Target && waiter.ShareBound {
		return false
	}

	if known && buyer.Target == waiter.Target && buyer.TargetShared && waiter.TargetShared {
		return true
	}

	if buyer.CeilingShared && waiter.CeilingShared {
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

	first, since := "", time.Time{}
	now := q.now()

	var stalled []string

	for label, waiter := range q.waiting {
		if label != tier && !competes(where, waiter.where) {
			continue
		}

		// A STALLED WAITER STEPS ASIDE BUT KEEPS ITS PLACE. Its own record is never
		// skipped, so a tier is not let past itself.
		if label != tier && !holdsTheLine(waiter, now) {
			stalled = append(stalled, label)

			continue
		}

		// TIES BROKEN BY LABEL, so two tiers that began waiting inside one clock
		// tick still have one winner rather than alternating with the map's
		// iteration order and letting neither accumulate room.
		if first == "" || waiter.since.Before(since) || (waiter.since.Equal(since) && label < first) {
			first, since = label, waiter.since
		}
	}

	allowed := first == "" || first == tier
	if allowed {
		q.letPast(tier, stalled)
	}

	return allowed
}

// letPast requires q.mu. It logs, once per stall, each stalled waiter this
// buyer really did get ahead of: one that would otherwise have come first.
func (q *admissionQueue) letPast(tier string, stalled []string) {
	own, waiting := q.waiting[tier]

	for _, label := range stalled {
		waiter := q.waiting[label]
		if waiter.passed || waiting && !waiter.since.Before(own.since) {
			continue
		}

		waiter.passed = true
		q.waiting[label] = waiter
		q.log.Warn("letting a tier past a waiter whose listener has made no admission progress; "+
			"it keeps its place and holds the line again when it progresses",
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

	// THE PLACE IS KEPT AND THE GROUND UNDER IT IS REFRESHED: a tier that waited
	// for a host which has since drained competes for where it could go NOW,
	// while the wait that earns it the freed room is still dated from its first
	// refusal.
	if previous, ok := q.waiting[tier]; ok {
		previous.where = where
		q.progressed(tier, previous)

		return
	}

	now := q.now()
	q.waiting[tier] = waitingTier{since: now, where: where, progress: now}
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
