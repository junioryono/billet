package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
)

// THE LOOP THAT DECIDES NEVER WAITS FOR A MACHINE TO START (#708).
//
// A launch can take minutes on a busy node, and a listener that waited for one
// heard nothing from GitHub meanwhile. On 2026-11-04 one tier logged no job start or
// completion for over five minutes while the node ran 30 launches for it, and every
// guest that had finished in that time stayed booted, its capacity charged, because
// only the completion that was waiting behind those launches lets it go. So a tier
// starved itself and every tier sharing its node, and the node looked full.
//
// With background launches the poll goroutine still makes every decision: it buys
// the lease and binds it to its job, in the same transactions as before, and the
// launch of that already-bought lease runs beside it. An ended launch wakes the
// loop to buy the next one. The long poll runs beside the loop too (exchange), never
// cancelled for that, because a poll cancelled in flight leaves unknown what GitHub
// took from it.
//
// WHAT STILL WAITS, and it must: a message naming a runner or a request whose launch
// has not returned. Everything that handles a start or a completion assumes the
// launch that created its subject has settled, and a message that may be redelivered
// costs nothing to hold for that one launch.

// launchFlight is one lease whose launch has been started and has not returned.
type launchFlight struct {
	leaseID    string
	requestID  int64
	runnerName string
	done       chan struct{}
}

// startLaunch runs the launch of an already-bought lease beside the poll goroutine.
//
// AT MOST poolLaunchBatch RUN AT ONCE across the pool and direct assignments, which
// is how many a node overlaps, because a command's timeout starts when it is queued:
// handing a node a queue of launches is how they come to fail for waiting.
func (l *Listener) startLaunch(ctx context.Context, lease *alloc.Lease, job dispatch.Job) {
	flight := &launchFlight{
		leaseID:    lease.ID,
		requestID:  job.RequestID,
		runnerName: provider.InstanceName(lease.ID),
		done:       make(chan struct{}),
	}

	l.mu.Lock()
	if l.flights == nil {
		l.flights = make(map[string]*launchFlight)
	}
	if l.launchSlots == nil {
		l.launchSlots = make(chan struct{}, max(l.poolLaunchBatch, 1))
	}
	slots := l.launchSlots
	l.flights[lease.ID] = flight
	l.mu.Unlock()

	l.launches.Go(func() {
		slots <- struct{}{}

		// A LAUNCH HANDED OFF IS FINISHED, NOT CANCELLED, BY A STOP (#393): cancelled
		// partway, it cannot verify what it started or settle its lease, and a drain then
		// waits on a lease nothing accounts for. One still waiting for its turn runs too:
		// its lease is bound to a job, and an assignment is a promise made before the stop
		// that a drain still keeps.
		//
		// EXCEPT FROM A CONTROLLER THAT HAS BEEN FENCED, which acts on nothing: a successor
		// owns this lease now, and it is not this process's to start. Asked here because a
		// launch can wait a long time for its turn, and only one that has not begun can
		// still decline.
		var (
			started bool
			err     error
		)

		if l.fenced() {
			l.log.Warn("not starting a launch: this process is no longer this deployment's controller",
				"tier", l.tier, "request", job.RequestID, "lease", lease.ID)
		} else {
			started, err = l.launchReporting(context.WithoutCancel(ctx), lease, job)
		}

		<-slots

		l.mu.Lock()
		delete(l.flights, lease.ID)
		// UNKNOWN UNTIL COUNTED AGAIN: a launch can fail and release its lease with
		// no error, and a demand published as met while its job waits is what a
		// drain would read as drained. The wake below counts it, except in a drain,
		// whose next empty exchange does.
		l.demandKnown = false
		if err != nil && l.launchFailure == nil {
			l.launchFailure = err
		}
		l.mu.Unlock()

		close(flight.done)

		// ONLY A LAUNCH THAT STARTED SOMETHING WAKES THE LOOP TO BUY THE NEXT, or one that
		// must stop it. launch settles an ordinary failure itself and returns no error,
		// and waking on that would buy and fail again at once for as long as the node
		// refuses, with nothing to slow it. A failure's retry waits for the next poll, as
		// it did when the loop waited for every launch.
		if !started && err == nil {
			return
		}

		l.wakeLoop()
	})
}

// wakeLoop asks the loop to reconcile again, beside a poll that may be out for a while.
// One pending wake is as good as many, since a reconciliation reads the current state.
func (l *Listener) wakeLoop() {
	select {
	case l.loopWake <- struct{}{}:
	default:
	}
}

// parkCompletion hands a completion's teardown to the cleanup loop (#708). A destroy
// waits behind the node's in-flight launches and can take minutes, and on the poll
// goroutine it stopped this tier hearing GitHub, so a finished guest could hold its
// capacity for minutes after the job ended. recordCompletion has already made the
// completion durable, and the cleanup loop is what retries one whose destroy failed:
// it already marks what it is destroying, stops when sealed, and is joined by a teardown.
//
// ONLY THE DESTROY MOVES. A lease counts as a runner GitHub can route to until it is
// deregistered, and the pool pass that follows this completion counts them: with the
// removal parked too, the finished runner still counted, so the pass read a runner just
// launched for the next job as surplus and retired it. The removal is one call to GitHub
// and never waits on a node, so it stays here, and a failed one leaves the lease counted
// and the cleanup loop asking again, as a failed one always did.
//
// THE LEASE GOES WITH IT. A renewal that fails drops a lease from `running`, leaving an
// entry with nothing to release, and a parked completion can wait long enough to meet
// one: its destroy would then succeed and its capacity stay charged with nothing naming
// it. It stays in `running` too, because the compute is alive and is renewed until gone.
//
// IT REPORTS WHETHER IT PARKED, and it does not for a completion this listener holds
// nothing for, by complete's own rule: an entry whose retry can accomplish nothing would
// only grow the map. Nor with no registry, where only the release ends the count. Nor for
// a job only promised: the promise's lease stays where the next pool pass can take it for
// another job, so a destroy out beside that pass could release a lease with new compute
// on it. Such a job's destroy stays on the caller.
func (l *Listener) parkCompletion(ctx context.Context, job dispatch.Job) bool {
	if l.registry == nil {
		return false
	}

	l.mu.Lock()
	entry, parked := l.cleanup[job.RequestID]
	lease, held := l.running[job.RequestID]
	_, promised := l.acquiring[job.RequestID]
	// PAST THE REMOVAL ALREADY, or a redelivery that repeats it could address a runner a
	// reused request id has since been given. complete takes these paths for the same reason.
	pastRemoval := l.destroying[job.RequestID] || (parked && (entry.retireOnly || entry.releaseOnly ||
		entry.lease != nil || (entry.deregistered && entry.job.CompletionID == job.CompletionID)))
	l.mu.Unlock()

	if promised || (!parked && !held && (l.completionStore == nil || job.Result == "")) {
		return false
	}

	deregistered := false

	if !pastRemoval {
		deregistered = l.deregisterBeforeParking(ctx, job)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if entry, ok := l.cleanup[job.RequestID]; ok {
		// A heartbeat can create the obligation before GitHub's completion arrives,
		// and its retry must take the same result-dependent path this one would.
		if job.Result != "" {
			entry.job = job
		}
		if !pastRemoval {
			entry.deregistered = deregistered
		}
		if entry.lease == nil {
			entry.lease = lease
		}
		if entry.parkedAt.IsZero() {
			entry.parkedAt = time.Now()
		}
	} else {
		if l.cleanup == nil {
			l.cleanup = make(map[int64]*pendingCleanup)
		}

		l.cleanup[job.RequestID] = &pendingCleanup{
			job: job, lease: lease, deregistered: deregistered, parkedAt: time.Now(),
		}
	}

	l.wakeCleanup()

	return true
}

// deregisterBeforeParking claims a finished runner for retirement and withdraws its
// registration, and reports whether that is done. A failure is for the cleanup loop,
// which runs the same two steps again before it destroys anything.
func (l *Listener) deregisterBeforeParking(ctx context.Context, job dispatch.Job) bool {
	if job.RunnerName != "" {
		if leaseID, ok := provider.LeaseOf(job.RunnerName); ok {
			if err := l.alloc.RetirePoolRunner(ctx, leaseID); err != nil &&
				!errors.Is(err, alloc.ErrLeaseNotFound) {
				l.log.Error("could not mark a completed pool runner for retirement; the cleanup loop asks again",
					"tier", l.tier, "runner", job.RunnerName, "error", err)

				return false
			}
		}
	}

	lease, _, _ := l.completionRelease(job.RequestID)

	deregistered, err := l.deregisterCompleted(ctx, job, lease)
	if err != nil {
		l.log.Warn("could not withdraw a finished runner's registration; its lease stays counted and "+
			"the cleanup loop asks again", "tier", l.tier, "request", job.RequestID, "error", err)

		return false
	}

	return deregistered
}

// deregisteredWhenParked says whether this delivery's registration was already withdrawn
// on its way to the cleanup loop.
func (l *Listener) deregisteredWhenParked(job dispatch.Job) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, parked := l.cleanup[job.RequestID]

	return parked && entry.deregistered && entry.job.CompletionID == job.CompletionID
}

// cleanupOwns says whether the cleanup loop is responsible for this request's compute,
// so a pool pass on the poll goroutine must not retire it as well. Waiting for every
// teardown made that impossible, and a second destroy would spend a node command on
// work already under way.
func (l *Listener) cleanupOwns(requestID int64) bool {
	if !l.backgroundLaunches {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	_, parked := l.cleanup[requestID]

	return parked || l.destroying[requestID]
}

// launchRoom is how many more launches may start now.
func (l *Listener) launchRoom() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return max(max(l.poolLaunchBatch, 1)-len(l.flights), 0)
}

// launchInFlight says whether this lease's launch has not returned.
func (l *Listener) launchInFlight(leaseID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	_, flying := l.flights[leaseID]

	return flying
}

// takeLaunchFailure returns, once, an error a background launch would have returned
// to the loop had it waited.
func (l *Listener) takeLaunchFailure() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	err := l.launchFailure
	l.launchFailure = nil

	return err
}

// awaitFlights waits for every launch match picks, reporting whether it picked any.
func (l *Listener) awaitFlights(ctx context.Context, match func(*launchFlight) bool) (bool, error) {
	l.mu.Lock()
	var waiting []*launchFlight
	for _, flight := range l.flights {
		if match(flight) {
			waiting = append(waiting, flight)
		}
	}
	l.mu.Unlock()

	for _, flight := range waiting {
		select {
		case <-flight.done:
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}

	return len(waiting) > 0, nil
}

// awaitFlightsNamedBy waits for the launches of the runners and requests a message's
// starts and completions name.
func (l *Listener) awaitFlightsNamedBy(ctx context.Context, msg *Message) error {
	names := make(map[string]bool)
	requests := make(map[int64]bool)

	for _, jobs := range [][]dispatch.Job{msg.Started, msg.Completed} {
		for i := range jobs {
			if jobs[i].RunnerName != "" {
				names[jobs[i].RunnerName] = true
			}
			if jobs[i].RequestID != 0 {
				requests[jobs[i].RequestID] = true
			}
		}
	}

	_, err := l.awaitFlights(ctx, func(flight *launchFlight) bool {
		return names[flight.runnerName] || requests[flight.requestID]
	})

	return err
}

// awaitFlightsResolved waits for the launches a resolved message's completions turn
// out to concern, reporting whether it did. A completion that names no runner is
// matched only here: on GitHub.com its request id on the wire is zero, and it is the
// resolver that finds the launch identity behind it.
func (l *Listener) awaitFlightsResolved(ctx context.Context, resolved *resolvedMessage) (bool, error) {
	leases := make(map[string]bool)
	requests := make(map[int64]bool)

	for i := range resolved.completed {
		entry := &resolved.completed[i]
		if entry.cleanup.RequestID != 0 {
			requests[entry.cleanup.RequestID] = true
		}
		if entry.binding != nil {
			leases[entry.binding.LeaseID] = true
		}
		if entry.launched != nil {
			leases[entry.launched.LeaseID] = true
		}
	}

	return l.awaitFlights(ctx, func(flight *launchFlight) bool {
		return leases[flight.leaseID] || requests[flight.requestID]
	})
}

// joinLaunches waits for every background launch before anything that would release
// what they hold. BOUNDED, as every join in a teardown is, and reported when it runs
// out; a launch that has begun is finished, not cancelled, by a stop (#393).
func (l *Listener) joinLaunches(ctx context.Context) {
	join, end := context.WithTimeout(context.WithoutCancel(ctx), l.shutdownGrace)
	defer end()

	if !waitWithin(join, &l.launches) {
		l.log.Error("a launch did not return within its shutdown budget; it may still write "+
			"to the ledger after this listener has stopped", "tier", l.tier, "grace", l.shutdownGrace)
	}
}

// exchanged is one answer from GitHub, and what went wrong beside it.
type exchanged struct {
	msg *Message
	err error
	// at is when the poll returned, taken where it returned: with background launches
	// the loop may consume the answer long after, inside a reconciliation, and an
	// answer dated at its consumption would date an old count as current.
	at time.Time
	// beside is a failure of the reconciliation an ended launch woke while the poll
	// was out, for the loop to treat as it treats its own reconciliation's. The
	// answer in hand is not handled then, and GitHub delivers it again.
	beside error
}

// exchange long-polls GitHub. With background launches the poll runs aside, so the
// loop can buy the next lease the moment a launch ends rather than up to a long poll
// later (#346), and it is awaited, never cancelled here, because a poll cancelled in
// flight leaves unknown what GitHub took from it.
//
// STILL ONE WRITER: this goroutine is the only one that decides anything, and the
// poll aside touches nothing but what it was handed.
func (l *Listener) exchange(ctx context.Context, advertised int, reconcile bool) exchanged {
	if !l.lastAnswered.IsZero() {
		l.timings.pollGapTook(l.tier, time.Since(l.lastAnswered))
	}

	defer func() { l.lastAnswered = time.Now() }()

	if !l.backgroundLaunches {
		msg, err := l.timedPoll(ctx, l.session, l.lastMessageID, advertised)

		return exchanged{msg: msg, err: err, at: l.clock()}
	}

	var (
		answer  exchanged
		polling sync.WaitGroup
	)

	session, lastMessageID := l.session, l.lastMessageID
	answered := make(chan struct{})

	polling.Go(func() {
		defer close(answered)

		answer.msg, answer.err = l.timedPoll(ctx, session, lastMessageID, advertised)
		answer.at = l.clock()
	})

	var beside error

	for {
		select {
		case <-answered:
			polling.Wait()
			answer.beside = beside

			return answer
		case <-l.loopWake:
			if beside == nil {
				beside = l.takeLaunchFailure()
			}
			if beside == nil && reconcile && l.observed != nil && !l.messageHeld() {
				beside = l.reconcilePool(ctx, l.observed.TotalAssignedJobs)
				l.reportCapacity(ctx, nil, "")
			}
		}
	}
}

// timedPoll polls GitHub and reports how long an answered poll took. One that failed is
// left out: its time is the client's retries, and it says nothing of GitHub's long poll.
func (l *Listener) timedPoll(ctx context.Context, session Session, lastMessageID int64, advertised int,
) (*Message, error) {
	began := time.Now()

	msg, err := session.GetMessage(ctx, lastMessageID, advertised)
	if err == nil || errors.Is(err, ErrNoMessage) {
		l.timings.pollTook(l.tier, time.Since(began))
	}

	return msg, err
}
