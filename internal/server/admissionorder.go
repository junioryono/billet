package server

import (
	"sync"
	"time"

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
type admissionQueue struct {
	policy config.AdmissionOrder
	now    func() time.Time

	mu      sync.Mutex
	waiting map[string]time.Time
}

// newAdmissionQueue builds the queue every listener of one control plane shares.
func newAdmissionQueue(policy config.AdmissionOrder) *admissionQueue {
	return &admissionQueue{
		policy:  policy.Or(),
		now:     time.Now,
		waiting: map[string]time.Time{},
	}
}

// mayBuy reports whether this tier may buy capacity now.
//
// A nil queue admits everything: a standalone listener has no peers to be fair
// between, and a test that builds one directly is not testing this.
func (q *admissionQueue) mayBuy(tier string) bool {
	if q == nil || q.policy == config.AdmissionFill {
		return true
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	first, since := "", time.Time{}

	for label, at := range q.waiting {
		// TIES BROKEN BY LABEL, so two tiers that began waiting inside one clock
		// tick still have one winner rather than alternating with the map's
		// iteration order and letting neither accumulate room.
		if first == "" || at.Before(since) || (at.Equal(since) && label < first) {
			first, since = label, at
		}
	}

	return first == "" || first == tier
}

// waits records that this tier has demand it could not buy for.
//
// The FIRST refusal is what dates the wait: a tier asked and refused on every
// poll would otherwise keep resetting its own place in the order and never
// reach the front.
func (q *admissionQueue) waits(tier string) {
	if q == nil {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if _, ok := q.waiting[tier]; !ok {
		q.waiting[tier] = q.now()
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

// waitingSince reports when a tier began waiting, for the status report.
func (q *admissionQueue) waitingSince(tier string) (time.Time, bool) {
	if q == nil {
		return time.Time{}, false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	at, ok := q.waiting[tier]

	return at, ok
}
