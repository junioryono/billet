package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
)

// teardownBudget is how long the whole shutdown may take: the cleanup-loop join
// and the destroy pass each get the destroy budget, then the close and the
// release get theirs.
//
// Every phase derives from a deadline this far out, so each of them is min(its
// own budget, what is left) and none can outlive the renewal that protects them.
func (l *Listener) teardownBudget() time.Duration {
	return sumBudgets(l.shutdownGrace, l.shutdownGrace, l.closeGrace, l.releaseGrace)
}

// waitWithin waits for a WaitGroup, giving up when the context is done.
//
// Reports whether the wait completed. The watcher goroutine deliberately outlives
// a giving-up caller: the thing being waited for is by definition misbehaving.
func waitWithin(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// abandon ends a listener whose process is no longer this deployment's
// controller, without acting on anything it holds.
//
// IT STILL STOPS ITS OWN GOROUTINES, and that is the one thing it must do. A
// cleanup retry or a heartbeat pass outliving Run reaches the allocator after
// the caller was entitled to close the ledger — the same failure the ordinary
// teardown joins for. Their writes are refused by the fence either way; what is
// being prevented is the goroutine, not the write.
//
// THE JOIN IS BOUNDED AND THEREFORE NOT A GUARANTEE, exactly as the ordinary
// teardown's is, and saying so is the point: a cleanup retry can be inside a
// provider Destroy that does not honour cancellation, and no budget here can
// reach into one. What this promises is that no NEW external action starts —
// retryCleanup refuses outright once fenced — and that a retry still running
// when the grace expires is REPORTED rather than left silent. Making it a real
// guarantee means bounding every provider operation, which is a change to the
// Runner contract rather than to this teardown.
//
// SEALED FIRST, THEN CANCELLED, exactly as the ordinary teardown does it:
// cancelling says nothing about what the cleanup loop is midway through
// starting.
//
// IT SAYS SO BEFORE IT WAITS. The join is bounded by shutdownGrace, which is
// minutes, and an operator watching a control plane stop needs to know why now
// rather than after it.
func (l *Listener) abandon(
	ctx context.Context,
	sweeping, beating *sync.WaitGroup,
	stopSweeping, stopBeating context.CancelFunc,
) {
	l.seal()
	stopSweeping()
	stopBeating()

	l.mu.Lock()
	running, pending := len(l.running), len(l.cleanup)
	l.mu.Unlock()

	l.log.Warn("this process is no longer this deployment's controller, so it is stopping "+
		"without destroying compute, closing its message session or handing back capacity — "+
		"the jobs running here keep running and whichever controller replaced this one "+
		"adopts them, GitHub expires the session this one is abandoning, and the leases come "+
		"back once they stop being renewed",
		"tier", l.tier, "running", running, "cleanup", pending)

	// ITS OWN BUDGET, DETACHED FROM ctx. A teardown runs because ctx is already
	// done, so a join bounded by it would not wait at all.
	overall, endOverall := context.WithTimeout(context.WithoutCancel(ctx), l.teardownBudget())
	defer endOverall()

	// A PHASE EACH, under one overall cap, exactly as the ordinary teardown does
	// it. Sharing a single deadline between the two lets a cleanup retry stuck
	// inside a slow Destroy spend the whole grace and leave the renewal join with
	// none — which then reports "lease renewal did not stop" about a loop that was
	// never waited for, blaming one half for the other's overrun.
	sweepCtx, endSweep := context.WithTimeout(overall, l.shutdownGrace)
	defer endSweep()

	if !waitWithin(sweepCtx, sweeping) {
		l.log.Error("a cleanup retry did not return before this listener stopped; the ledger "+
			"refuses its writes, but it may still outlive the handle on it",
			"tier", l.tier, "grace", l.shutdownGrace)
	}

	beatCtx, endBeat := context.WithTimeout(overall, l.shutdownGrace)
	defer endBeat()

	if !waitWithin(beatCtx, beating) {
		l.log.Error("lease renewal did not stop before this listener did",
			"tier", l.tier, "grace", l.shutdownGrace)
	}
}

// destroyAll tears down compute this listener is responsible for and reports
// which requests are confirmed gone.
//
// includeRunning DECIDES WHETHER THIS FAILS SOMEBODY'S BUILD, and it is the whole
// difference between a shutdown and an emergency. `cleanup` holds destroy
// obligations for jobs GitHub has already CONCLUDED, so tearing one down ends
// nothing — it is the teardown that completion asked for. `running` holds jobs
// that are still executing, and destroying one fails that build, because GitHub
// does not requeue a job whose runner vanished after it started.
//
// SHUTDOWN PASSES FALSE. Nothing about this process stopping is evidence that the
// work on its hosts should end: the guests keep running, the node goes on holding
// them, and a restart re-adopts them through ServiceableRunnerLeaseIDs. Only an
// operator saying so passes true, through forceDestroy.
//
// scope NARROWS IT TO WHAT AN OPERATOR ACTUALLY APPROVED, and nil means everything
// this listener holds. A force enumerates its targets, shows them to a person and
// records them durably before anything is destroyed, so the destroy pass must act
// on that set and not on whatever the listener happens to hold by the time it
// runs — otherwise a job that started between the diagnostic and the confirmation
// is destroyed without ever having been approved. Shutdown passes nil because it
// approves nothing and destroys only what a completion already asked for.
//
// Concurrent, because each Destroy can wait the node command timeout. Bounded,
// because a node executes commands one at a time. The backoff is ignored — it
// exists so a hopeless record cannot crowd out a live one, and this is the last
// pass.
func (l *Listener) destroyAll(
	ctx context.Context, includeRunning bool, scope map[int64]bool,
) map[int64]bool {
	// NIL IS EVERYTHING, AN EMPTY MAP IS NOTHING, and the two must not collapse.
	// A force whose targets have all been settled hands an empty map, and reading
	// that as "no filter" would destroy every job the listener holds — the exact
	// unapproved teardown the scope exists to prevent.
	inScope := func(id int64) bool { return scope == nil || scope[id] }

	l.mu.Lock()

	requests := make([]dispatch.Job, 0, len(l.running)+len(l.cleanup))

	// NOT WHAT A RETRY IS ALREADY INSIDE. That destroy is still happening, and a
	// second one would win the single teardown slot and spend the budget on work
	// already in progress while unrelated requests are never reached.
	skipped := make([]int64, 0, len(l.destroying))

	if includeRunning {
		for id := range l.running {
			if !inScope(id) {
				continue
			}

			if l.destroying[id] {
				skipped = append(skipped, id)

				continue
			}

			job := dispatch.Job{RequestID: id}
			if entry := l.cleanup[id]; entry != nil && entry.job.Result != "" {
				job = entry.job
			}
			requests = append(requests, job)
		}
	}

	// ALREADY ADDED ONLY IF THE LOOP ABOVE RAN. The sets overlap — a completion
	// whose destroy failed keeps its lease in `running` AND a record here — and
	// this guard exists so such a request is destroyed once rather than once per
	// set. It was written when the running loop was unconditional, so reading it
	// as "skip anything also running" silently drops the destroy a COMPLETED job
	// is owed whenever includeRunning is false, which is every shutdown.
	alreadyAdded := func(id int64) bool {
		if !includeRunning {
			return false
		}

		_, running := l.running[id]

		return running
	}

	for id, entry := range l.cleanup {
		if entry.releaseOnly || entry.retireOnly {
			continue
		}

		if !inScope(id) {
			continue
		}

		if l.destroying[id] {
			if !alreadyAdded(id) {
				skipped = append(skipped, id)
			}

			continue
		}

		if !alreadyAdded(id) {
			requests = append(requests, entry.job)
		}
	}

	// CLAIMED UNDER THE SAME LOCK THAT SELECTED THEM, and this direction of the
	// exclusion used to be missing. `attempt` marks a request while it is inside a
	// destroy and this pass skips those — but nothing stopped a cleanup retry
	// STARTING on a request this pass had already picked. At shutdown that was
	// inert, because seal() stops the retry loop before any of this runs. A force
	// runs on a live listener with the retry loop working, so the guard has to hold
	// in both directions or two destroys race for one node's single command slot.
	for i := range requests {
		l.destroying[requests[i].RequestID] = true
	}

	claimed := requests

	defer func() {
		l.mu.Lock()

		for i := range claimed {
			delete(l.destroying, claimed[i].RequestID)
		}

		l.mu.Unlock()
	}()

	l.mu.Unlock()

	for _, id := range skipped {
		l.log.Warn("not destroying this job's compute during shutdown because a cleanup "+
			"retry is still inside a destroy for it; that attempt is the one that counts, "+
			"and it did not return within its own budget",
			"tier", l.tier, "request", id)
	}

	var (
		mu   sync.Mutex
		done = make(map[int64]bool, len(requests))
		wg   sync.WaitGroup
		slot = make(chan struct{}, teardownConcurrency)
	)

	for i := range requests {
		wg.Add(1)

		go func(job *dispatch.Job) {
			defer wg.Done()
			requestID := job.RequestID

			// CHECKED BEFORE THE SELECT, because select picks uniformly among ready
			// cases rather than preferring a ready cancellation over a ready slot: with
			// an expired budget and a free slot, roughly half these goroutines would go
			// on to call Destroy anyway.
			if ctx.Err() == nil {
				select {
				case slot <- struct{}{}:
					defer func() { <-slot }()
				case <-ctx.Done():
				}
			}

			if ctx.Err() != nil {
				// NAMED, not silently skipped: returning quietly makes a request that was
				// never attempted indistinguishable from one destroyed, and a cleanup-only
				// record has no lease either, so the obligation evaporates on an ordinary
				// shutdown.
				l.log.Error("the shutdown grace ran out before billet tried to destroy this "+
					"job's compute; it was never attempted, and if no lease accounts for it "+
					"nothing will reclaim it until its host is swept or restarted",
					"tier", l.tier, "request", requestID)

				return
			}

			lease, outcome, _ := l.completionRelease(requestID)
			err := l.destroyCompleted(ctx, *job, lease, outcome)

			// CUSTODY DISCHARGES THE OBLIGATION RATHER THAN FAILING IT.
			//
			// The node asked its backend to stop the guest without receiving proof it
			// stopped, and is holding the lease until the compute is provably gone.
			// Nothing here can improve on that: retrying re-issues a teardown whose
			// outcome is already being reconciled, and keeping the entry — lease
			// and all — has this listener releasing capacity the node's janitor is
			// about to release itself.
			//
			// Counted as done for exactly that reason. It is not a confirmed
			// destroy, but it IS a request that no longer needs anything from this
			// listener, which is what the caller reads this map for.
			held := errors.Is(err, dispatch.ErrCustody)
			persisted := true
			retired := true
			if held {
				retired = l.forgetCompletion(ctx, *job)
				if !retired {
					l.parkRetirement(*job)
				}
			} else if err == nil {
				if lease == nil {
					retired = l.forgetCompletion(ctx, *job)
					persisted = retired
					if !retired {
						l.parkRetirement(*job)
					}
				} else if persistErr := l.recordReleaseOnly(ctx, *job, lease, outcome); persistErr != nil {
					persisted = false
					l.parkReleaseOnly(*job, lease, outcome)
					l.log.Error("compute was confirmed absent, but its release-only obligation could not be made durable; capacity stays held until this is retried",
						"tier", l.tier, "request", requestID, "lease", lease.ID, "error", persistErr)
				}
			}

			mu.Lock()
			done[requestID] = (err == nil && persisted) || held
			mu.Unlock()

			if held {
				// AND THE LEASE LEAVES `running`, WHICH IS THE HALF THAT MATTERS AT
				// SHUTDOWN.
				//
				// releaseAll releases every lease still in `running` whose request
				// this map marks destroyed — so marking a custody answer "done" and
				// leaving the lease behind would have shutdown release the very
				// capacity the node just took responsibility for, seconds after the
				// handoff. It has to be both or neither, and dropping it is correct:
				// the node's janitor holds this lease now, heartbeats it, and
				// releases it once the guest is provably gone. That janitor outlives
				// this listener.
				l.log.Info("the compute for this job was asked to stop and has not been "+
					"confirmed gone; the runner is holding its capacity until it is",
					"tier", l.tier, "request", requestID)

				l.mu.Lock()
				delete(l.running, requestID)
				delete(l.runningJobs, requestID)
				if retired {
					if entry := l.cleanup[requestID]; entry == nil ||
						entry.job.CompletionID == 0 || entry.job.CompletionID == job.CompletionID {
						delete(l.cleanup, requestID)
					}
				}
				l.mu.Unlock()

				return
			}

			if err != nil {
				l.log.Error("could not destroy the compute for a job before stopping; it is "+
					"still running on its host, and if no lease accounts for it nothing will "+
					"reclaim it until that host is swept or restarted",
					"tier", l.tier, "request", requestID, "error", err)

				return
			}

			l.mu.Lock()

			// AN ENTRY CARRYING A LEASE IS NOT DISCHARGED BY THE DESTROY ALONE.
			//
			// Most cleanup entries exist only to destroy compute, so confirming
			// that is the end of them. One parked by a failed launch also holds
			// CAPACITY whose release never landed, and deleting it here dropped the
			// last reference before releaseAll could see it — leaving the ledger
			// charging for a job that never started.
			if entry, ok := l.cleanup[requestID]; (!ok || entry.lease == nil) && retired {
				delete(l.cleanup, requestID)
			}

			l.mu.Unlock()
		}(&requests[i])
	}

	wg.Wait()

	return done
}

// releaseAll hands back capacity that was escrowed and never used.
//
// Given a context that outlives cancellation, because the ordinary reason for
// getting here is that the context was cancelled — and escrowed capacity that is
// never released is capacity no tier can use until the reaper expires it.
func (l *Listener) releaseAll(ctx context.Context, destroyed map[int64]bool) {
	// SNAPSHOT under the mutex, tear down OUTSIDE it.
	//
	// Destroy talks to a docker daemon or a remote node and has no bound. Holding
	// the escrow mutex across that starves the heartbeat, which is the thing
	// keeping every OTHER lease alive — so a single hung teardown could expire
	// the leases it was trying to protect, and could stop the process exiting at
	// all.
	l.mu.Lock()

	held := append([]*alloc.Lease(nil), l.held...)
	running := make(map[int64]*alloc.Lease, len(l.running))

	for id, lease := range l.running {
		running[id] = lease
	}

	promised := make([]*alloc.Lease, 0, len(l.acquiring))
	for _, p := range l.acquiring {
		promised = append(promised, p.lease)
	}

	// PARKED OBLIGATIONS TOO. A failed launch whose release did not land keeps
	// its lease here rather than in `running`, and shutdown is the last chance
	// anything in this process has to hand that capacity back.
	parked := make(map[int64]*pendingCleanup, len(l.cleanup))
	retirements := make([]dispatch.Job, 0, len(l.cleanup))

	for id, entry := range l.cleanup {
		if entry.retireOnly {
			retirements = append(retirements, entry.job)
		}
		if entry.lease != nil {
			parked[id] = entry
		}
	}

	l.held = nil
	l.acquiring = make(map[int64]*promise)
	l.mu.Unlock()
	for i := range retirements {
		l.forgetCompletion(ctx, retirements[i])
	}

	// WHAT DID NOT LAND IS PUT BACK, rather than dropped on the floor. These were
	// cleared above so nothing new could take them mid-shutdown; a release that
	// failed for a reason a retry could fix has to stay somewhere the next pass —
	// or a supervisor's restart — can still find it.
	var stuck []*alloc.Lease

	release := func(lease *alloc.Lease) {
		err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone)
		if err != nil {
			l.log.Warn("could not release escrowed capacity",
				"tier", l.tier, "lease", lease.ID, "error", err)

			if !releaseSettled(err) {
				stuck = append(stuck, lease)
			}
		}
	}

	for _, lease := range held {
		release(lease)
	}

	// A RUNNING LEASE IS RELEASED ONLY IF ITS COMPUTE WAS CONFIRMED GONE, and on
	// an ordinary shutdown none of them is: destroyAll is called with
	// includeRunning false, so anything still executing is deliberately left
	// alive. Freeing that capacity would be the overcommit this ordering exists
	// to prevent — a container on the host and another tier escrowing its slot.
	//
	// SO THE CAPACITY STAYS CHARGED, and that is the intended outcome rather than
	// a leak. The guest keeps running, the node keeps holding it, and the next
	// control plane adopts the lease through ServiceableRunnerLeaseIDs. Capacity
	// the reaper reclaims late is recoverable; a build failed to reclaim it early
	// is not.
	//
	// The entries that DO arrive here confirmed are the ones an operator forced,
	// and completions whose destroy this shutdown owed.
	for requestID, lease := range running {
		if !destroyed[requestID] {
			// NOT AN ERROR, and it used to be logged as one. On every ordinary
			// shutdown this is the normal path for every running job, and calling
			// it "needs manual cleanup" sent operators looking for containers to
			// remove by hand — which is exactly the work billet does not want them
			// doing, because that compute is somebody's job and it is still fine.
			l.log.Info("leaving a running job alone; its lease and capacity stay charged "+
				"until a host proves the compute is gone, and the next control plane "+
				"re-adopts it",
				"tier", l.tier, "request", requestID, "lease", lease.ID)

			continue
		}

		err := l.releaseAbsent(ctx, requestID, lease, alloc.PhaseDone, "")
		if !releaseSettled(err) {
			l.log.Warn("could not release completed capacity after compute was confirmed absent",
				"tier", l.tier, "request", requestID, "lease", lease.ID, "error", err)
			stuck = append(stuck, lease)

			continue
		}
		if entry := parked[requestID]; entry != nil {
			l.forgetCompletion(ctx, entry.job)
		}
	}

	// Promised escrow too. The acquisition was made to GitHub, but nothing has
	// been assigned yet and nothing can be launched, so holding it past shutdown
	// would strand it until the reaper.
	for _, lease := range promised {
		release(lease)
	}

	// And the parked ones, with the outcome they were parked with: a launch that
	// never started did not finish, whatever the ordinary path records.
	for id, entry := range parked {
		// A RELEASE-ONLY ENTRY ALREADY HAS ABSENCE PROOF. Every other parked
		// entry still needs destroyAll to confirm teardown or hand custody to the
		// runner. In particular, a completion restored before its holder registers
		// must keep its authoritative result and lease unchanged across shutdown.
		if !entry.releaseOnly && !destroyed[id] {
			l.log.Error("not releasing cleanup capacity because its compute was not confirmed absent; the durable completion remains pending for the next process",
				"tier", l.tier, "request", id, "lease", entry.lease.ID)

			continue
		}

		outcome := entry.outcome
		if outcome == "" {
			outcome = alloc.PhaseDone
		}
		if err := l.recordReleaseOnly(ctx, entry.job, entry.lease, outcome); err != nil {
			l.log.Warn("not releasing cleanup capacity because its release-only obligation is not durable",
				"tier", l.tier, "request", id, "lease", entry.lease.ID, "error", err)

			continue
		}
		err := l.releaseAbsent(ctx, id, entry.lease, outcome, entry.failureReason)
		if err != nil {
			l.log.Warn("could not release cleanup capacity after compute was confirmed absent",
				"tier", l.tier, "request", id, "lease", entry.lease.ID, "error", err)

			if !releaseSettled(err) {
				stuck = append(stuck, entry.lease)
			}
		}
		if releaseSettled(err) {
			l.forgetCompletion(ctx, entry.job)
		}
	}

	// WHAT DID NOT LAND IS THE REAPER'S, and saying so is the honest version.
	//
	// An earlier attempt put these back on `held` "so the next pass can find
	// them", which was wrong in a way worth recording: releaseAll runs once, from
	// Run's teardown, on a listener that is single-use — there is no next pass,
	// and a restart builds a new listener that has never heard of this map. The
	// reference was findable by nothing.
	//
	// What actually recovers them is the reaper, within a TTL, because nothing is
	// left to heartbeat them. That is a real mechanism rather than an imagined
	// one, and it is why this is a warning rather than a failure.
	if len(stuck) > 0 {
		l.log.Warn("shutting down with escrow whose release did not land; the reaper reclaims "+
			"it once it stops being renewed",
			"tier", l.tier, "leases", len(stuck))
	}
}
