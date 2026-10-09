package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
)

// assign moves one escrowed lease to the job GitHub gave it, and reports which
// lease that was.
//
// Returning the lease is what lets the caller launch OUTSIDE this function's
// mutex. The bool says whether there is anything to launch at all — a request
// already running, or one declined for want of escrow, needs nothing started.
// Explicit rather than a nil lease, because "no value and no error" is exactly
// the shape a caller mishandles without noticing.
func (l *Listener) assign(ctx context.Context, job dispatch.Job) (*alloc.Lease, bool, error) {
	entry, err := l.resolveActualJob(ctx, job, resolveAcquisition, nil)
	if err != nil {
		return nil, false, err
	}
	return l.assignResolved(ctx, entry)
}

// assignResolved consumes the resolved assignment without losing its aliases.
func (l *Listener) assignResolved(ctx context.Context, entry resolvedJob) (*alloc.Lease, bool, error) {
	commitments, err := l.resolveCommitments(ctx)
	if err != nil {
		return nil, false, err
	}
	job := entry.job

	// Held for the whole function, INCLUDING the allocator write.
	//
	// Releasing it around the write to keep heartbeats snappy looks obviously
	// right and is not: it opens a window where the write succeeds but a
	// concurrent heartbeat has already dropped the lease, so the assignment is
	// durable in the ledger and tracked nowhere in memory — capacity leaked until
	// the reaper expires it. The window is worth nothing anyway. handle() runs
	// only on the poll goroutine, so heartbeat is the sole concurrent writer, and
	// what it waits for is one local SQLite transaction against a 30-second beat.
	l.mu.Lock()
	defer l.mu.Unlock()

	var known []actualJobIdentity
	for _, c := range commitments {
		if c.heldBy(l) {
			known = append(known, c.actual)
		}
	}
	candidates := actualJobCandidates(entry.actual, entry.protocolID, known)
	if len(candidates) > 1 {
		return nil, false, fmt.Errorf("%w: assignment %d matches several commitments",
			ErrUntrustworthySession, job.RequestID)
	}
	actual := entry.actual
	if len(candidates) == 1 {
		actual = mergeActual(actual, candidates[0])
	}
	// Already ours. Retain any newly resolved aliases on redelivery as well.
	for _, c := range commitments {
		if c.promise == nil && c.heldBy(l) && sameActualJob(actual, c.actual) {
			l.runningJobs[c.key] = mergeActual(c.actual, actual)
			return nil, false, nil
		}
	}
	// The ownership key remains reserved even if its runner consumed another
	// actual job. Never overwrite the lease that must still be renewed.
	if _, ok := l.running[job.RequestID]; ok {
		return nil, false, nil
	}

	// NOR WHILE THE PREVIOUS RUN'S COMPUTE IS STILL OWED. Same reasoning as reserve: a
	// pending cleanup is addressed by request id, so accepting an assignment for that
	// id hands the old retry a container and a lease that belong to the new job.
	//
	// AND THIS IS NOT A DECLINE. Leaving a request out of AcquireJobs is a real
	// non-acquisition — GitHub can offer it to another scale set or offer it again.
	// There is no equivalent call for a job already ASSIGNED to this scale set: billet
	// does not launch it, acknowledges the message, and the job waits for GitHub's
	// pickup deadline to reassign it. A delay, not a loss, and the least bad option
	// here — holding the message unacknowledged would re-deliver it every poll and
	// block every message behind it. Doing better needs the assignment held locally
	// until the obligation clears, which needs a launch identity rather than a request
	// id. Tracked separately.
	if _, ok := l.cleanup[job.RequestID]; ok {
		l.log.Error("cannot start an assigned job while its previous run's compute is still "+
			"waiting to be destroyed; billet is not launching it and GitHub will reassign it "+
			"after its pickup deadline",
			"tier", l.tier, "request", job.RequestID)

		return nil, false, nil
	}

	// The offer may have used a different request alias. Its ownership key
	// names the promise to consume, not the request to bind on the lease.
	var lease *alloc.Lease
	var promiseKey int64
	var promiseKeys []int64
	promised := false
	for _, c := range commitments {
		if c.promise != nil && c.heldBy(l) && sameActualJob(actual, c.actual) {
			promiseKeys = append(promiseKeys, c.key)
			if !promised || c.key < promiseKey {
				lease, promiseKey, promised = c.lease, c.key, true
			}
			actual = mergeActual(actual, c.actual)
		}
	}

	if !promised {
		// No promise on file. GitHub can legitimately assign work this listener
		// never saw an offer for — after a restart, or when the offer was handled
		// by a process that is gone — so a free lease is used if there is one.
		if len(l.held) == 0 {
			// DECLINED, not fatal. Being assigned more than was advertised looks
			// like a protocol violation, but GitHub over-assigning is not the only
			// way to get here: billet's own escrow can vanish underneath an
			// acquisition — the heartbeat drops a fenced lease, a restart loses the
			// promise. Returning an error would kill the listener and take the
			// whole control plane down with it, stranding every tier's capacity
			// over one job.
			//
			// Declining keeps the invariant that matters: nothing runs without
			// escrow. The job is not acquired, GitHub reassigns it, and the
			// operator gets a loud line rather than an outage.
			l.log.Error("assigned a request with no escrow to back it; declining it",
				"tier", l.tier, "request", job.RequestID, "run", job.RunID)

			return nil, false, nil
		}

		lease = l.held[0]
	}

	if err := l.alloc.Assign(ctx, lease.ID, lease.Epoch, job.RunID, job.RequestID); err != nil {
		return nil, false, fmt.Errorf("server: assign lease %s: %w", lease.ID, err)
	}

	// Moved into running only AFTER the assignment is durable. Consuming it first
	// meant a failed Assign left the lease open in the database and absent from
	// the release path — capacity that nothing hands back and nothing reports,
	// until the reaper's TTL expires it.
	if promised {
		// One resolver-coalesced job owns these promises; only one needs a runner.
		for _, key := range promiseKeys {
			if key != promiseKey {
				l.held = append(l.held, l.acquiring[key].lease)
			}
			delete(l.acquiring, key)
		}
		l.sortHeld()
	} else {
		l.held = l.held[1:]
	}
	delete(l.heldOrder, lease.ID)

	l.running[job.RequestID] = lease
	l.runningJobs[job.RequestID] = actual

	return lease, true, nil
}

// launch starts the compute for a lease, and hands the capacity back if it will
// not start.
//
// Called with the mutex NOT held. A launch pulls images and talks to a
// hypervisor, and holding the escrow mutex across that stalls every heartbeat.
//
// A failed launch RELEASES the lease rather than keeping it. That is the
// opposite of the rule for a failed session close, and the difference is whether
// anything is running: a lease whose compute never started is backing nothing, so
// holding it withholds capacity from every other tier for no reason. GitHub
// reassigns the job when its pickup deadline passes.
func (l *Listener) launch(ctx context.Context, lease *alloc.Lease, job dispatch.Job) error {
	// EVERY LAUNCH, on the pool path and the direct-assignment path alike: each
	// end of a launch dates a waiter's progress, so a waiter launching slot after
	// slot keeps its line, and one launch longer than WaiterAllowance does not.
	l.order.launchBegins(l.tier)
	err := l.runner.Launch(ctx, lease, job)
	l.order.launchEnds(l.tier)

	if err == nil {
		// STILL OURS? The mutex was released for the duration of the launch, and
		// the heartbeat runs in that window. If it found this lease fenced or
		// missing it dropped it — the allocator has already given the capacity to
		// somebody else — and the compute that just started is now referenced by
		// nothing and accounted for by nothing.
		//
		// Destroying it is the only honest outcome: the machine is not billet's
		// to use any more.
		l.mu.Lock()
		_, stillOurs := l.running[job.RequestID]
		l.mu.Unlock()

		if stillOurs {
			binding, regErr := l.alloc.PoolRunnerByLease(ctx, lease.ID)
			if errors.Is(regErr, alloc.ErrLeaseNotFound) {
				regErr = l.alloc.RegisterPoolRunner(ctx, alloc.PoolRunner{LeaseID: lease.ID,
					Tier: l.tier, LaunchRequestID: job.RequestID,
					RunnerName: provider.InstanceName(lease.ID)})
			} else if regErr == nil && (binding.Tier != l.tier ||
				binding.LaunchRequestID != job.RequestID) {
				regErr = fmt.Errorf("%w: lease is registered for tier %q request %d",
					alloc.ErrConflict, binding.Tier, binding.LaunchRequestID)
			}
			if regErr != nil {
				return fmt.Errorf("server: verify pool runner for lease %s: %w", lease.ID, regErr)
			}
			return nil
		}

		l.log.Error("the lease was reclaimed while its job was starting; destroying the "+
			"compute, which is no longer backed by any capacity",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID)

		if destroyErr := l.destroyCompleted(ctx, job, lease, alloc.PhaseFailed); destroyErr != nil {
			l.log.Error("could not destroy compute whose lease was reclaimed; it is running "+
				"unaccounted for and needs manual cleanup",
				"tier", l.tier, "request", job.RequestID, "error", destroyErr)
		}

		return nil
	}

	// THE CAPACITY IS NOT HANDED BACK IF THE RUNNER IS STILL HOLDING IT.
	//
	// A launch that failed ambiguously may have started something, and the runner
	// says so by returning ErrCustody: it has taken the lease into its own
	// janitor, will keep heartbeating it, and will release it once the compute is
	// confirmed gone. Releasing here as well would double-count the capacity —
	// the listener would re-advertise it while a container is possibly running.
	if errors.Is(err, dispatch.ErrCustody) {
		l.log.Warn("a job failed to start and its compute could not be confirmed gone; "+
			"the runner is holding the capacity until it is",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", err)

		l.mu.Lock()
		delete(l.running, job.RequestID)
		delete(l.runningJobs, job.RequestID)
		l.mu.Unlock()

		return nil
	}
	registrationCtx, registrationCancel := withoutCancelWithin(ctx, l.releaseGrace)
	defer registrationCancel()
	binding, bindingErr := l.alloc.PoolRunnerByLease(registrationCtx, lease.ID)
	if bindingErr == nil {
		if retireErr := l.alloc.RetirePoolRunner(registrationCtx, lease.ID); retireErr != nil {
			l.log.Error("a failed launch's registration could not be claimed for cleanup; its capacity stays held",
				"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", retireErr)
			return nil
		}
		binding.Status = alloc.PoolRunnerRetiring
		l.retirePoolMember(registrationCtx, binding)
		return nil
	}
	if !errors.Is(bindingErr, alloc.ErrLeaseNotFound) {
		l.log.Error("a failed launch's registration could not be resolved; its capacity stays held",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", bindingErr)
		return nil
	}

	l.log.Error("could not start the compute for an assigned job; handing the capacity back",
		"tier", l.tier, "request", job.RequestID, "run", job.RunID,
		"lease", lease.ID, "error", err)

	// Not fatal. The launch already failed; failing the listener as well would
	// take every tier down over one job that GitHub will simply reassign.
	//
	// WITH ITS REASON, IN THE SAME TRANSACTION. Nothing outlives a process here
	// — the lease never held compute and archives at once — so the reason is
	// for the report: `billet leases failures` shows a failure nothing explains
	// as a row an operator cannot act on, and the node writes the same reason
	// for a launch that failed after starting something.
	relErr := l.alloc.ReleaseFailed(ctx, lease.ID, lease.Epoch, alloc.LaunchFailedReason)
	if relErr != nil {
		l.log.Warn("could not release the lease of a job that failed to start",
			"tier", l.tier, "lease", lease.ID, "error", relErr)
	}

	// THE REQUEST ID IS ALWAYS GIVEN UP; THE OBLIGATION MOVES.
	//
	// Keeping the lease in `running` after a release that did not land was worse
	// than dropping it, and in a way that does not heal. Nothing retries a
	// release from that map — the cleanup loop walks its own — and the heartbeat
	// renews the entry forever, because the lease is still open at the same
	// epoch. Meanwhile the request id is wedged: GitHub reassigns a job that
	// never started, and `assign` treats a redelivery for an id already in
	// `running` as its own work and silently swallows it, every time. The
	// advertisement counts a phantom and a drain waits its full grace for a
	// completion that cannot arrive.
	//
	// So an unsettled release parks the lease in the cleanup set, which is the
	// mechanism that already exists for exactly this — retried on its own clock,
	// backing off, and refusing a re-assignment of that id until it clears.
	// Archived as FAILED, because nothing ran: recording it as done would put a
	// lie in the history.
	l.mu.Lock()

	delete(l.running, job.RequestID)
	delete(l.runningJobs, job.RequestID)

	if !releaseSettled(relErr) {
		if l.cleanup == nil {
			l.cleanup = make(map[int64]*pendingCleanup)
		}

		if _, pending := l.cleanup[job.RequestID]; !pending {
			l.cleanup[job.RequestID] = &pendingCleanup{
				job: job, lease: lease, outcome: alloc.PhaseFailed, releaseOnly: true,
				failureReason: alloc.LaunchFailedReason,
			}
		}
	}

	l.mu.Unlock()

	return nil
}

// leaseFor is the lease this listener currently associates with a request, from
// wherever it is being tracked.
func (l *Listener) leaseFor(requestID int64) *alloc.Lease {
	l.mu.Lock()
	defer l.mu.Unlock()

	if lease, ok := l.running[requestID]; ok {
		return lease
	}

	if p, ok := l.acquiring[requestID]; ok {
		return p.lease
	}

	if entry, ok := l.cleanup[requestID]; ok {
		return entry.lease
	}

	return nil
}
