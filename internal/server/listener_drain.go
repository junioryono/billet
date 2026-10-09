package server

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/state"
)

// handsOff reports whether a stop arriving now is a handoff: the option is set,
// this process is still the controller, and the ledger says admission is open.
// ctx is the cancelled run context, so the read gets its own short bound.
func (l *Listener) handsOff(ctx context.Context) bool {
	if !l.restartHandoff || l.alloc == nil || l.fenced() {
		return false
	}

	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handoffAdmissionRead)
	defer cancel()

	admission, err := l.alloc.Admission(readCtx)
	if err != nil {
		l.log.Warn("could not read whether this deployment is admitting work, so this stop "+
			"drains rather than handing over", "tier", l.tier, "error", err)

		return false
	}

	return admission.Mode == state.AdmissionOpen
}

// noteStop decides a stop that ends Run before its loop, so a cancellation
// arriving during startup is treated as one arriving later would be.
func (l *Listener) noteStop(ctx context.Context) {
	if ctx.Err() != nil && !l.handingOff && l.handsOff(ctx) {
		l.handingOff = true
	}
}

// handoffAdmissionRead bounds the one ledger read that decides a handoff.
const handoffAdmissionRead = 5 * time.Second

// drainEnded reports that this failure is the drain itself having been ended.
//
// IT WAS drainBudgetGone, AND THERE IS NO BUDGET ANY MORE. Nothing expires a
// drain; it ends when the work finishes or when a second signal cancels it. The
// name and the message both described a deadline, which would have sent the next
// reader looking for one.
//
// THE EXIT BRANCH IS AT THE TOP OF THE LOOP, so a call that fails BELOW it with
// the cancelled drain context must send the loop back round rather than return —
// otherwise the branch that decides never runs.
//
// That is not a tidiness point. Measured when a budget still existed: the drain
// announced itself, the admission read consumed the whole of it and warned, and
// then the listener returned in silence. `cancelledWhileServing` deliberately
// answers false once draining, so every error below the branch took `stopping`,
// which returns ctx.Err() without a word. The line nobody saw is the one naming
// which jobs were left running on their hosts — the only record an operator gets
// that anything is still out there.
//
// It cannot spin: the top of the loop sees the same cancelled context and returns.
//
// THE ERROR IS REPORTED RATHER THAN DROPPED. It was already being discarded —
// `stopping` returns ctx.Err() whenever the caller's context is cancelled, and
// during a drain it always is, so every one of these sites has been throwing the
// cause away since they were written. That is defensible for a cancellation and
// not for a broken ledger, and the two are indistinguishable in a journal that
// shows neither.
func (l *Listener) drainEnded(pollCtx context.Context, draining bool, err error) bool {
	if !draining || pollCtx.Err() == nil {
		return false
	}

	// Only when the error is something other than the cancellation itself, or
	// every ending would carry a line saying the context was cancelled — which
	// the exit line above it already says better.
	if err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		l.log.Warn("a call failed as the drain was ending; it is ending anyway and this "+
			"is what it was doing", "tier", l.tier, "error", err)
	}

	return true
}

// beginDrain stops this listener taking new work and hands back the capacity nobody
// is using, returning the context the drain polls on.
//
// THE ADVERTISEMENT IS NOT FORCED TO ZERO: what billet sends is the scale set's
// TOTAL capacity, so a constant zero while a job runs is untrue. Releasing the idle
// escrow makes capacity() fall to the work still in flight and reach zero by itself.
//
// It is a courtesy to GitHub's scheduler and NOT the guard — a redelivered message
// can still arrive, and the local check in handle is what refuses it.
func (l *Listener) beginDrain(ctx context.Context) (context.Context, context.CancelFunc) {
	// NOT BOUNDED BY A DEADLINE, and that is deliberate. This context used to
	// expire after drainGrace, at which point the loop returned and the teardown
	// destroyed whatever was still running — a timer failing somebody's build. A
	// job may run for days; elapsed time is not evidence that it stopped making
	// progress, and billet imposes no job limit of its own.
	//
	// drainGrace survives as the moment billet starts SAYING the drain is long
	// (see warnDrainOverrun). It no longer ends anything.
	//
	// Built FIRST because the release below needs a live context — ctx is already
	// cancelled, and alloc.Release would fail on it and strand the capacity.
	drainCtx, endDrain := context.WithCancel(context.WithoutCancel(ctx))

	// A SECOND SIGNAL ENDS THE WAIT, not the teardown. The goroutine also selects
	// on drainCtx so it cannot outlive the drain.
	if l.hurry != nil {
		select {
		case <-l.hurry:
			// AN ALREADY RECEIVED SIGNAL ENDS THE WAIT BEFORE ANOTHER POLL.
			// Scheduling its observation would let the drain enter another poll.
			endDrain()
		default:
			go func() {
				select {
				case <-l.hurry:
					endDrain()
				case <-drainCtx.Done():
				}
			}()
		}
	}

	// NOT l.seal(), which stops the cleanup loop starting new destroys and belongs
	// at the teardown. A drain can last as long as a job, so sealing here would
	// leave every failed destroy un-retried for hours.
	//
	// MARKED BEFORE THE ESCROW GOES BACK: the other order leaves a window where
	// the capacity is released but an offer can still be accepted.
	l.mu.Lock()
	l.draining = true
	l.drainStarted = time.Now()
	l.mu.Unlock()

	// THE ESCROW IS NOT HANDED BACK HERE, and that is the fix for the rule this
	// file states at the teardown: the last maxCapacity GitHub saw stays live
	// until the session ends, so releasing escrow first leaves a positive
	// advertisement standing with nothing behind it. GitHub can assign against
	// that number, and the assignment arrives to find `held` empty and is
	// declined — a job delayed, and a loud error line in the middle of a
	// deliberate drain that reads exactly like the alarming cases it shares its
	// wording with.
	//
	// The loop advertises committedCapacity() from here on, and hands the escrow
	// back once a poll has carried that smaller number.
	l.log.Info("draining: not taking new work, waiting for what is already running for "+
		"as long as it takes",
		"tier", l.tier, "running", l.Running(), "held_until_next_poll", l.idleEscrow(),
		"report_after", l.drainGrace)

	return drainCtx, endDrain
}

// releaseStrandedEscrow hands back capacity whose machine has gone away.
//
// A held lease names its machine, and one whose machine has gone would be
// assigned a job that then fails to launch; releasing it lets the next purchase
// place on a host that is there.
//
// ONLY `held` LEASES: one has never been assigned, so giving it back costs a
// re-escrow at worst. Anything acquiring or running is somebody's job.
func (l *Listener) releaseStrandedEscrow(ctx context.Context) int {
	l.mu.Lock()
	snapshot := append([]*alloc.Lease(nil), l.held...)
	l.mu.Unlock()

	if len(snapshot) == 0 {
		return 0
	}

	ids := make([]string, 0, len(snapshot))
	for _, lease := range snapshot {
		ids = append(ids, lease.ID)
	}

	stranded, err := l.alloc.Stranded(ctx, ids)
	if err != nil {
		// NOT FATAL. Not knowing whether a machine is still there is a reason to
		// keep advertising what was already promised, not to tear it down: the
		// ledger will answer on the next pass, and releasing on a failed read
		// would hand back capacity over a database blip.
		l.log.Warn("could not check whether this tier's escrow still has machines behind it",
			"tier", l.tier, "error", err)

		return 0
	}

	if len(stranded) == 0 {
		return 0
	}

	gone := make(map[string]bool, len(stranded))
	for _, id := range stranded {
		gone[id] = true
	}

	released := 0

	for _, lease := range snapshot {
		if !gone[lease.ID] {
			continue
		}

		// PhaseDone, not PhaseFailed: nothing was attempted and nothing went
		// wrong. The reservation is simply being given back.
		if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			// LEFT IN `held`, so it stays renewed and correctly counted as this
			// listener's, and is tried again on the next pass. Dropping it here
			// would leak the capacity until the reaper.
			l.log.Warn("could not release escrow whose machine is gone; it stays this "+
				"listener's and will be tried again",
				"tier", l.tier, "lease", lease.ID, "error", err)

			continue
		}

		l.mu.Lock()
		l.held = slices.DeleteFunc(l.held, func(h *alloc.Lease) bool { return h.ID == lease.ID })
		delete(l.heldOrder, lease.ID)
		l.mu.Unlock()

		released++
	}

	if released > 0 {
		l.log.Info("released escrow whose machines are no longer in the fleet; this tier now "+
			"advertises less", "tier", l.tier, "released", released)
	}

	return released
}

// releaseIdleEscrow hands back every lease this listener holds but has not given
// to a job, reporting how many.
//
// Only `held`. `acquiring` has been promised to a job that is starting and
// `running` is backing a live container; releasing either would let another tier
// escrow capacity that is already spoken for.
func (l *Listener) releaseIdleEscrow(ctx context.Context) int {
	// A SNAPSHOT TO ITERATE, BUT EACH LEASE LEAVES `held` ONLY WHEN IT IS ACTUALLY
	// RELEASED. Out of `held` the heartbeat cannot see it, so a pass longer than a TTL
	// lets the reaper reclaim one, and appending it back would advertise capacity this
	// listener no longer owns. Deleting by id also avoids resurrecting a lease
	// heartbeatHeld just dropped.
	l.mu.Lock()
	snapshot := append([]*alloc.Lease(nil), l.held...)
	l.mu.Unlock()

	released := 0

	for _, lease := range snapshot {
		if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			// LEFT IN `held`, so it is still renewed, still correctly counted as
			// this listener's, and tried again by the teardown's own release pass
			// on its own budget. Dropping it here would leak the capacity until the
			// reaper; advertising it after losing it would be worse.
			l.log.Warn("could not release idle escrow; it stays this "+
				"listener's and will be released at shutdown",
				"tier", l.tier, "lease", lease.ID, "error", err)

			continue
		}

		l.mu.Lock()
		l.held = slices.DeleteFunc(l.held, func(h *alloc.Lease) bool { return h.ID == lease.ID })
		delete(l.heldOrder, lease.ID)
		l.mu.Unlock()

		released++
	}

	return released
}

// releaseIdleEscrowAbove hands back only unassigned leases above target total
// capacity. Acquiring, running, adopted, and already-releasing members remain
// counted in the target but can never be selected for release here.
func (l *Listener) releaseIdleEscrowAbove(ctx context.Context, target int) {
	released := 0
	for {
		l.mu.Lock()
		total := len(l.held) + len(l.releasing) + len(l.acquiring) + len(l.running) +
			len(l.adopted)
		if total <= target || len(l.held) == 0 {
			l.mu.Unlock()
			break
		}

		// Worst placement first. Moving it rather than snapshotting every surplus
		// candidate makes a concurrent heartbeat loss visible before another is
		// selected, while releasing still counts as owned until the allocator
		// confirms otherwise.
		lease := l.held[len(l.held)-1]
		l.held = l.held[:len(l.held)-1]
		l.releasing[lease.ID] = lease
		l.mu.Unlock()

		release := l.alloc.Release
		if l.releaseCapacity != nil {
			release = l.releaseCapacity
		}
		err := release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone)

		l.mu.Lock()
		_, stillTracked := l.releasing[lease.ID]
		delete(l.releasing, lease.ID)
		definitivelyLost := !stillTracked || errors.Is(err, alloc.ErrFenced) ||
			errors.Is(err, alloc.ErrLeaseNotFound)
		if err == nil || definitivelyLost {
			delete(l.confirmed, lease.ID)
			delete(l.heldOrder, lease.ID)
			if err == nil {
				released++
			}
		} else {
			l.held = append(l.held, lease)
			l.sortHeld()
		}
		l.mu.Unlock()

		if err != nil {
			if definitivelyLost {
				l.log.Warn("surplus idle escrow was lost while its release was in flight; it is no longer advertised",
					"tier", l.tier, "lease", lease.ID, "error", err)
				continue
			}
			l.log.Warn("could not release surplus idle escrow; it stays this listener's and will be retried",
				"tier", l.tier, "lease", lease.ID, "error", err)
			break
		}
	}

	if released > 0 {
		l.log.Info("released escrow this tier held beyond its committed work",
			"tier", l.tier, "released", released, "target_capacity", target)
	}
}

// isDraining reports whether this listener has been asked to stop and is
// waiting out the work it already has.
//
// Distinct from `sealed`, which stops the cleanup loop at teardown. The two are
// hours apart in the life of a drain and mean different things.
func (l *Listener) isDraining() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.draining
}

// drained reports whether this listener still owes anybody a running job.
//
// RUNNING ONLY, AND `acquiring` DELIBERATELY NOT: GitHub may never assign a promise,
// and what resolves one is the session ending — which happens in the teardown, on
// the far side of this wait. A drain waiting for it would spend its whole budget
// and destroy nothing.
func (l *Listener) drained() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.running) == 0
}

// idleMembersOnly is the launch ids of what this listener still runs when every
// entry is an anonymous pool slot the ledger records as never having started a job,
// and nil otherwise.
//
// Such a slot is not work the drain owes anyone, and leaving it registered and
// charged is what a second signal does. A runner launched for an assigned job (one
// with a runningJobs identity) is waited for until it completes. A read that fails
// is "could not tell", which keeps the drain waiting.
func (l *Listener) idleMembersOnly(ctx context.Context) map[int64]bool {
	if l.alloc == nil {
		return nil
	}

	l.mu.Lock()
	remaining := make(map[int64]bool, len(l.running))
	for id := range l.running {
		if _, forJob := l.runningJobs[id]; forJob {
			l.mu.Unlock()

			return nil
		}
		remaining[id] = true
	}
	l.mu.Unlock()

	if len(remaining) == 0 {
		return nil
	}

	members, err := l.alloc.PoolRunners(ctx, l.tier)
	if err != nil {
		l.log.Warn("could not read the runner pool to judge the drain; still waiting",
			"tier", l.tier, "error", err)

		return nil
	}

	idle := make(map[int64]bool, len(remaining))
	for i := range members {
		member := &members[i]
		if !remaining[member.LaunchRequestID] {
			continue
		}
		if member.Status != alloc.PoolRunnerIdle || member.ActualRequestID != 0 {
			return nil
		}
		idle[member.LaunchRequestID] = true
	}

	if len(idle) != len(remaining) {
		return nil
	}

	return idle
}

// subsetOf reports whether every key of a is in b.
func subsetOf(a, b map[int64]bool) bool {
	for id := range a {
		if !b[id] {
			return false
		}
	}

	return true
}

// drainStartedAt is when this listener began draining, or the zero time.
func (l *Listener) drainStartedAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.drainStarted
}

// drainWarnEvery bounds how often an overrunning drain repeats itself.
//
// Often enough that an operator watching a log learns the deployment is still
// waiting, rarely enough that a multi-day drain does not write a book.
const drainWarnEvery = 15 * time.Minute

// warnDrainOverrun says that a drain has run past drainGrace, and keeps saying it.
//
// THIS IS WHAT drainGrace IS NOW FOR. It used to be a deadline that destroyed
// what was still running; a drain that cannot end itself needs the operator to
// know it is happening, and a threshold crossed silently is a fleet that looks
// wedged. What it reports is what an automation asking "why is this not done"
// needs: how long, and what is still here.
func (l *Listener) warnDrainOverrun() {
	l.mu.Lock()

	started, warned := l.drainStarted, l.drainWarnedAt
	running := len(l.running)

	if started.IsZero() {
		l.mu.Unlock()

		return
	}

	now := time.Now()
	waited := now.Sub(started)

	if waited < l.drainGrace || (!warned.IsZero() && now.Sub(warned) < drainWarnEvery) {
		l.mu.Unlock()

		return
	}

	l.drainWarnedAt = now
	grace := l.drainGrace
	l.mu.Unlock()

	l.log.Warn("still draining, and this will not time out: billet waits for a running "+
		"job for as long as it runs, because elapsed time is not evidence that one "+
		"stopped making progress. Nothing here will be destroyed by waiting",
		"tier", l.tier, "running", running, "waited", waited.Truncate(time.Second),
		"drain_timeout", grace)
}

// isQuiesced reports whether the deployment's admission is sealed.
func (l *Listener) isQuiesced() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.quiesced
}
