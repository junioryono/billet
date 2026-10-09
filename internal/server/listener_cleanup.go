package server

import (
	"context"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
)

// pendingCleanup is a completion whose destroy has not succeeded yet.
type pendingCleanup struct {
	job dispatch.Job
	// lease is the capacity this obligation still holds, when the entry was
	// created for a lease that is NOT in `running` — a launch that failed and
	// whose release did not land. complete looks here when the running map has
	// nothing, because the request id has already been given up.
	lease *alloc.Lease
	// outcome is how the lease should be archived, or empty for the ordinary
	// completion. A launch that never started must not be recorded as `done`:
	// "done" for a runner that never ran is a lie the history keeps.
	outcome alloc.Phase
	// failureReason is why a failed outcome will be archived, when this
	// listener is the party that knows — a launch it watched fail — so a
	// release that lands on a later attempt records the same reason the first
	// attempt would have. Empty for every other obligation.
	failureReason string
	// releaseOnly means the runner has already proved there is no compute to
	// destroy: either Launch failed without custody, or a previous Destroy
	// succeeded. Retrying a remote destroy in either case can only delay the
	// ledger release, and during shutdown that delay can be a full node timeout.
	releaseOnly bool
	// retireOnly means teardown and capacity settlement are complete, but the
	// durable tombstone still needs to be written before this id can be reused.
	retireOnly bool
	// Doubles after each failure, up to maxRetryEvery.
	wait time.Duration
	// Zero means immediately, which is what a freshly recorded failure wants.
	at time.Time
	// GitHub re-offers an unacquired job indefinitely, so the message is worth
	// exactly once per obligation.
	declined bool
}

// due reports whether this entry may be attempted at the given moment.
func (p *pendingCleanup) due(now time.Time) bool {
	return !now.Before(p.at)
}

// failed pushes the next attempt out, doubling the wait to a ceiling.
//
// The FIRST failure is what created the entry, so the first retry is immediate:
// the overwhelmingly common case is a node that was briefly busy, and making it
// wait would slow down every ordinary recovery to protect against the rare
// permanent one.
func (p *pendingCleanup) failed(now time.Time, first, ceiling time.Duration) {
	if first <= 0 {
		// Pacing off, for tests whose subject is what a retry does rather than when.
		p.wait, p.at = 0, time.Time{}

		return
	}

	switch {
	case p.wait == 0:
		p.wait = first
	case p.wait < ceiling:
		p.wait *= 2
	}

	if p.wait > ceiling {
		p.wait = ceiling
	}

	p.at = now.Add(p.wait)
}

// cleanupLoop retries cleanup obligations on its own clock.
func (l *Listener) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(l.heartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.retryCleanup(ctx)
		}
	}
}

// retryCleanup finishes cleanup obligations whose destroy or release failed.
//
// A release-only obligation never reaches the runner again: its entry records
// the proof that no compute exists. Every other obligation goes through complete,
// which refuses to release until Destroy confirms the compute is gone.
func (l *Listener) retryCleanup(ctx context.Context) {
	// A REPLACED CONTROLLER STARTS NOTHING NEW, AND THIS IS THE ONE LOOP THAT
	// COULD. Destroy talks to a host rather than to the ledger, so nothing about
	// the fence reaches it: the refusal that fences this process stops its writes
	// and cannot stop this. The signal cancels the whole plane within a scheduling
	// quantum, and this closes the window between the two — a tick landing inside
	// it would tear down compute whose lease the successor now holds.
	if l.fenced() {
		return
	}

	now := time.Now()

	l.mu.Lock()

	// ONLY THE ONES THAT ARE DUE: retries are sequential and a single Destroy can
	// wait the full node command timeout, so entries whose node has been refusing for
	// an hour would push the one that just recovered behind them.
	pending := make([]dispatch.Job, 0, len(l.cleanup))

	for _, entry := range l.cleanup {
		if entry.due(now) {
			pending = append(pending, entry.job)
		}
	}

	l.mu.Unlock()

	// NO ERROR BRANCH, because attempt has none to give. A failure records its
	// own obligation and its own backoff before returning — the pacing lives with
	// the knowledge of what failed, rather than in a caller that would have to be
	// told.
	for i := range pending {
		l.attempt(ctx, pending[i])
	}
}

// attempt runs one cleanup retry, marked as in flight for its duration.
//
// THE UNMARKING IS A DEFER, which is the whole reason this is a function. The
// mark makes the shutdown skip a request, so an entry that outlives its attempt
// hides that request from teardown permanently — a container nobody destroys and
// nobody mentions. A panic under complete would do it.
func (l *Listener) attempt(ctx context.Context, job dispatch.Job) {
	l.mu.Lock()

	// CLAIMED UNDER THE SAME LOCK THAT SEALS, which is what makes the shutdown's
	// snapshot trustworthy. retryCleanup walks a snapshot and cancellation does not
	// stop it mid-list, so with only a context check the loop could finish a slow
	// destroy and start a brand new one for the next job while the teardown was
	// destroying it too. Checking a flag and taking the mark in two steps leaves the
	// same window one instruction wide.
	if l.sealed {
		l.mu.Unlock()

		return
	}

	entry, pending := l.cleanup[job.RequestID]
	if !pending {
		l.mu.Unlock()

		return
	}

	if entry.retireOnly {
		retireJob := entry.job
		l.mu.Unlock()
		l.retireParked(ctx, entry, retireJob)

		return
	}

	if entry.releaseOnly {
		l.mu.Unlock()
		if handled, settled := l.releaseParked(ctx, job.RequestID); handled && settled {
			if !l.forgetCompletion(ctx, job) {
				l.parkRetirement(job)
			}
		}

		return
	}

	l.destroying[job.RequestID] = true
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.destroying, job.RequestID)
		l.mu.Unlock()
	}()

	l.complete(ctx, job)
}

// seal stops the cleanup loop from starting any further destroys.
//
// Called BEFORE the loop is cancelled, because cancelling is a request and this is a
// fact. PERMANENT, and only safe because a listener is never reused.
func (l *Listener) seal() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sealed = true
}
