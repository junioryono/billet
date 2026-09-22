package server

import (
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
// so first-come starves it outright on a fleet that is never idle. The block is
// bounded by the longest job already running, and it cannot deadlock, because
// the winner is a single tier and waiting never moves it later in the order.
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
// node plane, where the resize happens, is below the scheduler and cannot reach
// the queue. A fallback to a larger shape can therefore take pot a waiter was
// accumulating, which is #172 and is exempt until that says otherwise.
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

	mu      sync.Mutex
	waiting map[string]waitingTier
}

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
}

// newAdmissionQueue builds the queue every listener of one control plane shares.
func newAdmissionQueue(policy config.AdmissionOrder) *admissionQueue {
	return &admissionQueue{
		policy:  policy.Or(),
		now:     time.Now,
		waiting: map[string]waitingTier{},
	}
}

// gates reports whether this queue decides anything: under fill, and for the
// nil queue a standalone listener has, every purchase is admitted, so a caller
// need not read the ledger to ask.
func (q *admissionQueue) gates() bool {
	return q != nil && q.policy != config.AdmissionFill
}

// competes reports whether two tiers contend for the same room.
//
// TWO WAYS TO COMPETE, and the second is easy to miss: they share a host either
// could be placed on, OR the deployment's own ceiling is smaller than the hosts
// it caps, which makes it one pot every tier buys from. Under such a ceiling a
// tier pinned to one host and a tier pinned to another still take room from each
// other, and a rule that looked only at hosts let a stream of small jobs on one
// host starve a large tier waiting on the other.
//
// A tier whose hosts are unknown competes with every tier, so a failed read
// never lets a purchase skip the order.
func competes(a, b alloc.TierAdmission) bool {
	if a.CeilingShared && b.CeilingShared {
		return true
	}

	if len(a.Nodes) == 0 || len(b.Nodes) == 0 {
		return true
	}

	for _, x := range a.Nodes {
		if slices.Contains(b.Nodes, x) {
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

	for label, waiter := range q.waiting {
		if label != tier && !competes(where, waiter.where) {
			continue
		}

		// TIES BROKEN BY LABEL, so two tiers that began waiting inside one clock
		// tick still have one winner rather than alternating with the map's
		// iteration order and letting neither accumulate room.
		if first == "" || waiter.since.Before(since) || (waiter.since.Equal(since) && label < first) {
			first, since = label, waiter.since
		}
	}

	return first == "" || first == tier
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
		q.waiting[tier] = previous

		return
	}

	q.waiting[tier] = waitingTier{since: q.now(), where: where}
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

// waitingSince reports when a tier began waiting, for the status report.
func (q *admissionQueue) waitingSince(tier string) (time.Time, bool) {
	if q == nil {
		return time.Time{}, false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	waiter, ok := q.waiting[tier]

	return waiter.since, ok
}
