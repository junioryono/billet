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
	// deregistered means the loop withdrew this completion's GitHub registration before
	// parking it, so the destroy need not ask GitHub a second time. In memory only: a
	// restart asks again, which is safe because a removal is idempotent.
	deregistered bool
	// parkedAt is when a completion was handed to the cleanup loop, zero for any other
	// obligation.
	parkedAt time.Time
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

// cleanupConcurrency is how many cleanup attempts one tier has out at once while its
// launches run beside the loop. A node runs a destroy only after the launches in flight,
// and its commands in the order they arrive. Sent one at a time, each destroy waits behind
// whichever launches started since the last returned, so a tier's finished guests clear
// at one per batch of launches: 30 of them would take about 20 minutes at the 30 to 65
// seconds a batch took on a busy node (2026-10-03). Sent together, they clear behind the
// same launches. Bounded because a node command's timeout starts when it is queued, and a
// destroy takes seconds: this many fit well inside the 10-minute bound, and one that does
// not is retried like any other.
const cleanupConcurrency = 16

// cleanupLoop retries cleanup obligations on its own clock.
//
// EVERY ATTEMPT RETURNS BEFORE THIS DOES, because joining this loop is how a teardown
// knows no retry is still inside a destroy.
func (l *Listener) cleanupLoop(ctx context.Context) {
	defer l.attempts.Wait()

	ticker := time.NewTicker(l.heartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.retryCleanup(ctx)
		case <-l.cleanupWake:
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

	for id, entry := range l.cleanup {
		if entry.due(now) && !l.attempting[id] {
			pending = append(pending, entry.job)
		}
	}

	l.mu.Unlock()

	if l.backgroundLaunches {
		l.startAttempts(ctx, pending)

		return
	}

	// NO ERROR BRANCH, because attempt has none to give. A failure records its
	// own obligation and its own backoff before returning — the pacing lives with
	// the knowledge of what failed, rather than in a caller that would have to be
	// told.
	for i := range pending {
		l.attempt(ctx, pending[i])
	}
}

// startAttempts runs each due attempt beside the cleanup loop, up to cleanupConcurrency,
// so a destroy waiting on a node holds up neither the others nor a completion parked
// after it. What finds no room is taken up when an attempt ends.
func (l *Listener) startAttempts(ctx context.Context, pending []dispatch.Job) {
	for i := range pending {
		job := pending[i]

		l.mu.Lock()
		if len(l.attempting) >= cleanupConcurrency {
			l.attemptsWaiting = true
			l.mu.Unlock()

			return
		}
		l.attempting[job.RequestID] = true

		var parkedAt time.Time
		if entry := l.cleanup[job.RequestID]; entry != nil {
			parkedAt = entry.parkedAt
		}
		l.mu.Unlock()

		l.attempts.Go(func() {
			defer l.endAttempt(job.RequestID, parkedAt)

			l.attempt(ctx, job)
		})
	}
}

// endAttempt frees an attempt's place.
//
// WHAT IT RELEASED IS ROOM, and a tier waiting for it buys now rather than on its next
// poll, which is up to a long poll away. The cleanup loop is woken only for what found no
// room.
//
// ONE THAT LEFT ITS OBLIGATION DUE IS PUT BACK, however it failed. Most failures set their
// own backoff, and a tombstone or a retirement that could not be written sets none, so
// with every place taken each end would wake the next attempt at the same failure, at
// the speed the ledger refuses it.
func (l *Listener) endAttempt(requestID int64, parkedAt time.Time) {
	l.mu.Lock()
	delete(l.attempting, requestID)

	entry, pending := l.cleanup[requestID]
	if pending && entry.due(time.Now()) {
		entry.failed(time.Now(), l.retryFirst, l.retryMax)
	}

	waiting := l.attemptsWaiting
	l.attemptsWaiting = false
	l.mu.Unlock()

	if !pending && !parkedAt.IsZero() {
		l.timings.teardownTook(l.tier, time.Since(parkedAt))
	}

	if waiting {
		l.wakeCleanup()
	}

	l.wakeLoop()
}

// wakeCleanup asks the cleanup loop to look again without waiting for its tick.
func (l *Listener) wakeCleanup() {
	select {
	case l.cleanupWake <- struct{}{}:
	default:
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

	// A FORCE ON A LIVE LISTENER CLAIMS WHAT IT DESTROYS, and two destroys of one request
	// would race for the node's command slot, and either's end would clear the other's mark.
	if l.destroying[job.RequestID] {
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
