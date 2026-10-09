package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

// releaseSettled reports whether a release attempt ended the lease's claim on
// capacity, one way or another.
//
// A CONCLUSIVE ERROR IS AS GOOD AS SUCCESS, and telling the two apart from an
// outage is the whole point. ErrFenced means somebody else already reclaimed it
// and this caller's epoch is stale; ErrLeaseNotFound means it is gone or already
// terminal; ErrConflict means it is terminal with the opposite outcome. None can
// be improved by holding a reference and trying again — but a busy database or a
// cancelled context can.
func releaseSettled(err error) bool {
	return err == nil || errors.Is(err, alloc.ErrFenced) || errors.Is(err, alloc.ErrLeaseNotFound) ||
		errors.Is(err, alloc.ErrConflict)
}

// releaseAbsent returns capacity after the runner has proved no compute exists.
// A fenced release may name a lease the reaper quarantined while the proof was
// in flight; ResolveQuarantine is the only operation that turns that proof into
// returned capacity.
func (l *Listener) releaseAbsent(ctx context.Context, requestID int64, lease *alloc.Lease,
	outcome alloc.Phase, reason string,
) error {
	relErr := l.release(ctx, lease, outcome, reason)
	if !errors.Is(relErr, alloc.ErrFenced) {
		return relErr
	}

	if err := l.resolveQuarantine(ctx, lease, outcome, reason); err == nil {
		l.log.Warn("a cleanup release found its lease quarantined after compute was "+
			"confirmed gone; the capacity is back",
			"tier", l.tier, "request", requestID, "lease", lease.ID)

		return nil
	} else if !errors.Is(err, alloc.ErrLeaseNotFound) {
		return err
	}

	return relErr
}

// release terminalizes a lease with the outcome this listener decided, and
// with its reason when the outcome is a failure this listener explains.
//
// ONE TRANSACTION FOR BOTH, because a release that lands leaves nothing to
// carry the reason afterwards: the archive copies what the row holds at that
// moment, and a reason written a call later would explain a history row that
// has already closed. An ordinary release — a finished job, or a failure
// somebody else already explained — is exactly what it was.
func (l *Listener) release(ctx context.Context, lease *alloc.Lease, outcome alloc.Phase, reason string) error {
	if outcome == alloc.PhaseFailed && reason != "" {
		return l.alloc.ReleaseFailed(ctx, lease.ID, lease.Epoch, reason)
	}

	return l.alloc.Release(ctx, lease.ID, lease.Epoch, outcome)
}

// resolveQuarantine settles a quarantined lease with the outcome this listener
// decided, carrying its reason when the outcome is a failure it explains —
// the reaper reaches a parked launch failure before the retry does, and the
// reason must not be lost to the route the release took.
func (l *Listener) resolveQuarantine(ctx context.Context, lease *alloc.Lease, outcome alloc.Phase, reason string) error {
	if outcome == alloc.PhaseFailed && reason != "" {
		return l.alloc.ResolveQuarantineFailed(ctx, lease.ID, reason)
	}

	return l.alloc.ResolveQuarantine(ctx, lease.ID, outcome)
}

// releaseParked retries the local half of cleanup after the runner has proved no
// compute exists. It reports whether it found and handled a release-only entry.
//
// The allocator call stays outside the listener mutex. A SQLite writer can be
// busy behind an operator command, and holding the mutex there would stall lease
// renewal for every unrelated job in this tier.
func (l *Listener) releaseParked(ctx context.Context, requestID int64) (bool, bool) {
	l.mu.Lock()
	entry, ok := l.cleanup[requestID]
	if !ok || !entry.releaseOnly || entry.lease == nil {
		l.mu.Unlock()

		return false, false
	}

	lease := entry.lease
	job := entry.job
	outcome := entry.outcome
	reason := entry.failureReason
	if outcome == "" {
		outcome = alloc.PhaseDone
	}
	l.mu.Unlock()

	if err := l.recordReleaseOnly(ctx, job, lease, outcome); err != nil {
		l.mu.Lock()
		current, stillPending := l.cleanup[requestID]
		if stillPending && current == entry {
			entry.failed(time.Now(), l.retryFirst, l.retryMax)
		}
		l.mu.Unlock()
		l.log.Error("compute is confirmed absent, but capacity stays held until its release-only obligation is durable",
			"tier", l.tier, "request", requestID, "lease", lease.ID, "error", err)

		return true, false
	}

	relErr := l.releaseAbsent(ctx, requestID, lease, outcome, reason)

	l.mu.Lock()
	defer l.mu.Unlock()

	current, stillPending := l.cleanup[requestID]
	if !stillPending || current != entry {
		return true, false
	}

	if !releaseSettled(relErr) {
		entry.failed(time.Now(), l.retryFirst, l.retryMax)
		l.log.Error("could not release capacity after compute was confirmed absent; it stays "+
			"held until this is retried",
			"tier", l.tier, "request", requestID, "lease", lease.ID, "error", relErr)

		return true, false
	}

	delete(l.cleanup, requestID)
	delete(l.running, requestID)
	delete(l.runningJobs, requestID)
	delete(l.acquiring, requestID)

	return true, true
}

func (l *Listener) recordCompletion(
	ctx context.Context,
	job dispatch.Job,
) (state.PendingCompletionDisposition, error) {
	if l.completionStore == nil || job.Result == "" {
		// NOTHING DURABLE TO BE SUPERSEDED BY. With no completion store there is
		// no stored delivery to compare this one against, so it is actionable by
		// construction and its result may be recorded.
		lease, _, _ := l.completionRelease(job.RequestID)
		l.recordJobResult(ctx, job, leaseIDOf(lease))

		return state.PendingCompletionActionable, nil
	}

	lease, outcome, releaseOnly := l.completionRelease(job.RequestID)
	completion := state.PendingCompletion{
		Tier: l.tier, RequestID: job.RequestID, RunID: job.RunID, Result: job.Result,
		MessageID: job.CompletionID,
	}
	withCompletionIdentity(&completion, job)
	if lease != nil {
		completion.LeaseID = lease.ID
		completion.LeaseEpoch = lease.Epoch
		completion.LeaseNode = lease.Node
		completion.Outcome = string(outcome)
		completion.ReleaseOnly = releaseOnly
	}
	disposition, err := l.completionStore.PutPendingCompletion(ctx, completion)
	if err != nil {
		return state.PendingCompletionActionable, fmt.Errorf("server: preserve completed result for %s request %d before acknowledging it: %w",
			l.tier, job.RequestID, err)
	}

	// AFTER THE DISPOSITION, NEVER BEFORE IT, and that ordering is the whole
	// safety of the record.
	//
	// GitHub reuses a request id and the escrow maps are keyed on it, so a STALE
	// redelivery of an old completion resolves to whatever lease holds that id
	// NOW. Recorded ahead of this decision, an old job's result lands on its
	// replacement's history: it fabricates an attributed failure for a job that
	// has not finished, and worse, it makes disruptionGuard refuse a real
	// disruption against that lease — a recorded result is exactly how the guard
	// knows GitHub has already reported.
	//
	// AND AGAINST THE LEASE THIS DELIVERY WAS RECORDED WITH, the snapshot
	// PutPendingCompletion has just made durable, rather than a second read of a
	// map that may have moved underneath it.
	if disposition == state.PendingCompletionActionable {
		l.recordJobResult(ctx, job, leaseIDOf(lease))
	}

	return disposition, nil
}

// recordJobResult stores GitHub's own conclusion for a finished job, so that a
// failure can later be read beside whatever billet's infrastructure was doing to
// that lease. It never re-runs anything and decides nothing.
//
// IT CANNOT FAIL THE CALLER, and that is not tidiness. recordCompletion's error
// is fatal: it stops this listener, which cancels every other listener, whose
// shutdowns destroy the jobs running on this host. A busy database while GitHub
// happened to report a completion would take the deployment down for the sake of
// a diagnostic — the same disproportion complete() already refuses for an
// unbacked assignment.
//
// THE LEASE IS HANDED IN RATHER THAN LOOKED UP, so it is the same one the caller
// settled the delivery's disposition against. Re-reading the escrow here would
// reopen the reused-request-id hole the caller just closed. Where the caller has
// none, the lease id encoded in the runner's name is the only route left after a
// restart lost the maps — the same fallback complete() uses.
//
// WHAT STAYS OPEN, said rather than papered over. That fallback resolves a lease
// this listener does NOT hold, so `pending_completions.lease_id` is empty for it
// — correctly, since teardown there is not this process's to finish — and the row
// carries no runner name either. So for that one class, a crash between
// PutPendingCompletion and this write is recovered by GitHub's redelivery and
// only by that: if the cleanup loop settles and retires the row first, every
// later delivery is refused and the diagnostic is gone. Closing it needs the
// result's own lease identity persisted beside the teardown one, which is a
// column and a migration for a missing report line — worth doing when something
// else needs that column, and not worth a seventh change to this path now. A
// line here that looked like it closed it would be worse: the obvious one
// (recording again from complete) survives every mutation, because complete's
// restored job carries no runner name to resolve.
func leaseIDOf(lease *alloc.Lease) string {
	if lease == nil {
		return ""
	}

	return lease.ID
}

func (l *Listener) recordJobResult(ctx context.Context, job dispatch.Job, leaseID string) {
	if l.alloc == nil || job.Result == "" {
		return
	}

	if leaseID == "" && job.RunnerName != "" {
		if encoded, ok := provider.LeaseOf(job.RunnerName); ok {
			leaseID = encoded
		}
	}

	if leaseID == "" {
		return
	}

	write := l.writeJobResult
	if write == nil {
		write = l.alloc.RecordJobResult
	}
	if err := write(ctx, leaseID, job.Result, job.RunID); err != nil {
		l.log.Warn("could not record what github concluded about a finished job; "+
			"`billet leases failures` may not be able to show whether billet's own "+
			"infrastructure was disrupted while its lease could still have been "+
			"running it",
			"tier", l.tier, "request", job.RequestID, "lease", leaseID, "error", err)
	}
	l.recordJobIdentity(ctx, leaseID, job)
}

// recordJobIdentity writes which job a lease ran onto its history row. It is a
// diagnostic: a failure is logged and never changes what the caller does.
//
// ONLY FROM A MESSAGE THAT SAYS WHAT THE RUNNER RAN: JobStarted's binding and the
// completion. An assignment names the job a pooled runner was LAUNCHED for, and
// GitHub may give that runner another; recorded there, the first write would win
// and the job that actually ran could never be written.
//
// BOUNDED ON ITS OWN, because the writer retries contention until its context
// ends and the caller's context is the poll's.
func (l *Listener) recordJobIdentity(ctx context.Context, leaseID string, job dispatch.Job) {
	if l.alloc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, identityWriteLimit)
	defer cancel()
	if err := l.alloc.RecordJobIdentity(ctx, leaseID, historyJob(job)); err != nil {
		l.log.Warn("could not record which github job a lease ran; its history row "+
			"will not name the repository, workflow or job",
			"tier", l.tier, "lease", leaseID, "job", job.JobID, "error", err)
	}
}

// recordReleaseOnly makes successful node teardown durable before the local
// lease release is attempted. It deliberately outlives cancellation for one
// bounded local-write budget: otherwise shutdown would preserve a row that asks
// restart recovery to contact a node for compute already proved absent.
// withCompletionIdentity keeps what the completion itself said about its job on
// the durable row, so a completion restored after a restart is still evidence
// of its own rather than a copy of the binding it is compared with.
func withCompletionIdentity(completion *state.PendingCompletion, job dispatch.Job) {
	completion.JobID = job.JobID
	completion.JobOwner = job.Owner
	completion.JobRepository = job.Repository
	completion.JobWorkflowRef = job.WorkflowRef
	completion.JobEvent = job.Event
}

func (l *Listener) recordReleaseOnly(
	ctx context.Context,
	job dispatch.Job,
	lease *alloc.Lease,
	outcome alloc.Phase,
) error {
	if l.completionStore == nil || job.Result == "" || lease == nil {
		return nil
	}
	if outcome == "" {
		outcome = alloc.PhaseDone
	}
	persistCtx, cancel := withoutCancelWithin(ctx, l.releaseGrace)
	defer cancel()
	completion := state.PendingCompletion{
		Tier: l.tier, RequestID: job.RequestID, RunID: job.RunID, Result: job.Result,
		LeaseID: lease.ID, LeaseEpoch: lease.Epoch, LeaseNode: lease.Node,
		Outcome: string(outcome), ReleaseOnly: true, MessageID: job.CompletionID,
	}
	withCompletionIdentity(&completion, job)
	disposition, err := l.completionStore.PutPendingCompletion(persistCtx, completion)
	if err != nil {
		return fmt.Errorf("server: preserve release-only completion for %s request %d: %w",
			l.tier, job.RequestID, err)
	}
	if disposition != state.PendingCompletionActionable {
		return fmt.Errorf("server: completion for %s request %d message %d is no longer current",
			l.tier, job.RequestID, job.CompletionID)
	}

	return nil
}

// withoutCancelWithin survives an ordinary caller cancellation without extending
// an existing shutdown deadline.
func withoutCancelWithin(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < grace {
		return context.WithDeadline(base, deadline)
	}

	return context.WithTimeout(base, grace)
}

// parkReleaseOnly retains capacity whose compute is gone until persistence and
// allocator release both settle.
func (l *Listener) parkReleaseOnly(job dispatch.Job, lease *alloc.Lease, outcome alloc.Phase) {
	if lease == nil {
		return
	}
	if outcome == "" {
		outcome = alloc.PhaseDone
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cleanup == nil {
		l.cleanup = make(map[int64]*pendingCleanup)
	}
	entry := l.cleanup[job.RequestID]
	if entry != nil && entry.job.CompletionID > job.CompletionID {
		return
	}
	if entry == nil {
		entry = &pendingCleanup{job: job}
		l.cleanup[job.RequestID] = entry
	} else if job.Result != "" {
		entry.job = job
	}
	entry.lease = lease
	entry.outcome = outcome
	entry.releaseOnly = true
	entry.failed(time.Now(), l.retryFirst, l.retryMax)
}

// parkUnreachable keeps a completion's obligation without renewing its lease.
//
// The entry carries the lease so the retry can address the same holder, and is
// NOT release-only: nothing has proved the compute absent, so neither the retry
// loop nor a shutdown may release it (releaseAll refuses a parked entry without
// proof). Leaving `running` is the whole effect — the heartbeat pass renews
// what is in `running`, and this lease is now the reaper's to quarantine unless
// the process holding its compute renews it.
func (l *Listener) parkUnreachable(job dispatch.Job, lease *alloc.Lease, outcome alloc.Phase) {
	if outcome == "" {
		outcome = alloc.PhaseDone
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cleanup == nil {
		l.cleanup = make(map[int64]*pendingCleanup)
	}

	// A LATER DELIVERY OWNS THE OBLIGATION, and this one changes nothing — the
	// lease included, which stays wherever that delivery left it.
	entry := l.cleanup[job.RequestID]
	if entry != nil && entry.job.CompletionID > job.CompletionID {
		return
	}

	delete(l.running, job.RequestID)
	delete(l.runningJobs, job.RequestID)
	delete(l.confirmed, lease.ID)

	if entry == nil {
		entry = &pendingCleanup{job: job}
		l.cleanup[job.RequestID] = entry
	} else if job.Result != "" {
		entry.job = job
	}

	entry.lease = lease
	entry.outcome = outcome
	entry.releaseOnly = false
	entry.failed(time.Now(), l.retryFirst, l.retryMax)
}

// completionRelease snapshots the capacity obligation before GitHub's message
// can be acknowledged. A node teardown and a ledger release are one ordered
// completion; restart recovery needs the fenced lease identity to finish the
// second half if the process stops between them.
func (l *Listener) completionRelease(requestID int64) (*alloc.Lease, alloc.Phase, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if entry := l.cleanup[requestID]; entry != nil && entry.lease != nil {
		outcome := entry.outcome
		if outcome == "" {
			outcome = alloc.PhaseDone
		}

		return entry.lease, outcome, entry.releaseOnly
	}
	if lease := l.running[requestID]; lease != nil {
		return lease, alloc.PhaseDone, false
	}
	if promise := l.acquiring[requestID]; promise != nil {
		return promise.lease, alloc.PhaseDone, false
	}

	return nil, "", false
}

func (l *Listener) forgetCompletion(ctx context.Context, job dispatch.Job) bool {
	if l.completionStore != nil && job.Result != "" {
		if err := l.completionStore.RetirePendingCompletion(
			ctx, l.tier, job.RequestID, job.CompletionID,
		); err != nil {
			l.log.Error("a completed job settled, but its durable tombstone could not be written; its request id stays blocked until this is retried",
				"tier", l.tier, "request", job.RequestID, "error", err)

			return false
		}
	}
	if l.alloc != nil {
		if err := l.alloc.SettlePoolRunner(ctx, l.tier, job.RequestID); err != nil {
			l.log.Error("a completed pool runner settled, but its physical identity could not be preserved through source acknowledgement",
				"tier", l.tier, "request", job.RequestID, "error", err)

			return false
		}
	}

	return true
}

func (l *Listener) parkRetirement(job dispatch.Job) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cleanup == nil {
		l.cleanup = make(map[int64]*pendingCleanup)
	}
	entry := l.cleanup[job.RequestID]
	if entry != nil && entry.job.CompletionID > job.CompletionID {
		return
	}
	if entry == nil {
		entry = &pendingCleanup{}
		l.cleanup[job.RequestID] = entry
	}
	entry.job = job
	entry.lease = nil
	entry.releaseOnly = false
	entry.retireOnly = true
	entry.failed(time.Now(), l.retryFirst, l.retryMax)
}

// retireParked retries only the durable tombstone, never the teardown it follows.
func (l *Listener) retireParked(ctx context.Context, entry *pendingCleanup, job dispatch.Job) {
	if !l.forgetCompletion(ctx, job) {
		return
	}
	l.mu.Lock()
	if l.cleanup[job.RequestID] == entry {
		delete(l.cleanup, job.RequestID)
	}
	l.mu.Unlock()
}

// retryParkedRetirement reports whether this delivery is already past teardown.
func (l *Listener) retryParkedRetirement(ctx context.Context, job dispatch.Job) bool {
	l.mu.Lock()
	entry := l.cleanup[job.RequestID]
	if entry == nil || !entry.retireOnly ||
		(entry.job.CompletionID != 0 && entry.job.CompletionID != job.CompletionID) {
		l.mu.Unlock()

		return false
	}
	retireJob := entry.job
	l.mu.Unlock()
	l.retireParked(ctx, entry, retireJob)

	return true
}

// deleteCompletionCleanup cannot let an older message remove a reused id's obligation.
func (l *Listener) deleteCompletionCleanup(job dispatch.Job) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.cleanup[job.RequestID]
	if entry == nil {
		return
	}
	if entry.job.CompletionID != 0 && entry.job.CompletionID != job.CompletionID {
		return
	}
	delete(l.cleanup, job.RequestID)
}

func (l *Listener) restoreCompletions(ctx context.Context) error {
	if l.completionStore == nil {
		return nil
	}
	completions, err := l.completionStore.PendingCompletions(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: restore pending completions for %s: %w", l.tier, err)
	}

	l.mu.Lock()
	for i := range completions {
		completion := &completions[i]
		job := dispatch.Job{RequestID: completion.RequestID, RunID: completion.RunID, Result: completion.Result,
			CompletionID: completion.MessageID, JobID: completion.JobID,
			Owner: completion.JobOwner, Repository: completion.JobRepository,
			WorkflowRef: completion.JobWorkflowRef, Event: completion.JobEvent}
		if completion.Retired {
			continue
		}
		if entry := l.cleanup[job.RequestID]; entry != nil {
			entry.job = job
			if completion.LeaseID != "" {
				entry.lease = &alloc.Lease{ID: completion.LeaseID, Epoch: completion.LeaseEpoch,
					Node: completion.LeaseNode}
				entry.outcome = alloc.Phase(completion.Outcome)
				entry.releaseOnly = completion.ReleaseOnly
			}

			continue
		}
		entry := &pendingCleanup{job: job}
		if completion.LeaseID != "" {
			entry.lease = &alloc.Lease{ID: completion.LeaseID, Epoch: completion.LeaseEpoch,
				Node: completion.LeaseNode}
			entry.outcome = alloc.Phase(completion.Outcome)
			entry.releaseOnly = completion.ReleaseOnly
		}
		l.cleanup[job.RequestID] = entry
	}

	l.mu.Unlock()

	// THE RESULT IS RE-RECORDED FROM THE DURABLE ROW, OUTSIDE THE MUTEX.
	//
	// PutPendingCompletion commits before RecordJobResult, so a process that died
	// between them left GitHub's conclusion on disk and nothing in job_history.
	// The completion is then settled by the cleanup clock and its tombstone
	// retired, after which every redelivery is classified as already handled and
	// the diagnostic is lost for good. This is the one place that still holds the
	// evidence, so it is where the second write is retried.
	//
	// READ FROM THE ROWS RATHER THAN FROM l.cleanup, so the result and the lease
	// come from ONE durable delivery. The map's entries are pointers the cleanup
	// loop mutates, and a restored row merged onto a pre-existing entry can leave
	// this job beside another delivery's lease — which is the reused-request-id
	// hole again, reached from the other side.
	//
	// First-observation-wins makes it idempotent for the ordinary restart, where
	// the result is already recorded and this costs one read each. The count is
	// bounded by the completions in flight, and it runs before the poll loop
	// starts, so it cannot delay anything that is serving.
	// A RETIRED ROW IS INCLUDED HERE AND EXCLUDED ABOVE, and the asymmetry is the
	// point. Retired means the TEARDOWN obligation settled; it says nothing about
	// whether the diagnostic was written, and the row still carries GitHub's word
	// and the lease it belongs to. Skipping it strands the very case this recovery
	// exists for: the result write failed, teardown then settled and retired the
	// row, and every redelivery from that moment is classified as already handled
	// — so nothing else will ever look at the evidence again before an
	// acknowledgement deletes it.
	for i := range completions {
		completion := &completions[i]
		if completion.LeaseID == "" {
			continue
		}

		l.recordJobResult(ctx, dispatch.Job{RequestID: completion.RequestID, RunID: completion.RunID,
			Result: completion.Result, CompletionID: completion.MessageID,
			JobID: completion.JobID, Owner: completion.JobOwner, Repository: completion.JobRepository,
			WorkflowRef: completion.JobWorkflowRef, Event: completion.JobEvent}, completion.LeaseID)
	}

	return nil
}

// complete releases the lease a finished job was running on.
//
// Idempotent, because a redelivered Completed message must not fail: a job billet
// has already released is simply not in the map. Release with PhaseDone rather
// than inspecting a conclusion — the lease's job is finished either way, and the
// outcome belongs to job history rather than to the capacity ledger.
// IT CANNOT FAIL, and the signature says so.
//
// Everything that can go wrong here — a destroy the node refused, a release the
// ledger could not take — is recorded as a pending obligation and retried on the
// cleanup clock. Returning an error would be worse than useless: complete runs
// on the poll path, where an error stops the listener, cancels every other
// listener, and destroys every job running on this host.
//
// Returning nothing keeps that from being re-learned. An error return that is
// always nil is an invitation to wire the next failure mode through it, and the
// branch handling it would never run.
func (l *Listener) complete(ctx context.Context, job dispatch.Job) {
	if job.RunnerName != "" {
		if leaseID, ok := provider.LeaseOf(job.RunnerName); ok {
			if err := l.alloc.RetirePoolRunner(ctx, leaseID); err != nil &&
				!errors.Is(err, alloc.ErrLeaseNotFound) {
				l.log.Error("could not mark a completed pool runner for retirement",
					"tier", l.tier, "runner", job.RunnerName, "error", err)
				return
			}
		}
	}
	// A redelivery after teardown and release settled can only retry the durable
	// tombstone. Repeating teardown here could address replacement compute if the
	// source reused the request id after an acknowledgement failure.
	if l.retryParkedRetirement(ctx, job) {
		return
	}

	// A previous attempt already established that no compute exists. Repeating a
	// remote destroy adds no proof, and on shutdown it can wait a full node timeout
	// before the local release that is the only work left.
	if handled, settled := l.releaseParked(ctx, job.RequestID); handled {
		if settled {
			if !l.forgetCompletion(ctx, job) {
				l.parkRetirement(job)
			}
		}

		return
	}

	// DESTROYED BEFORE RELEASED, and outside the mutex.
	//
	// Releasing first would hand the capacity to another tier while this job's
	// container or microVM is still running on the host — the budget would be
	// satisfied on paper and overcommitted in fact. Same shape as closing the
	// session before releasing the escrow.
	//
	// Idempotent by contract, so a redelivered completion, a request this
	// listener never launched, and a second attempt after a failure all reach
	// this safely.
	//
	// THE LEASE IS NOTED BEFORE THE DESTROY, because the maps can change during
	// it. A remote destroy has no bound, and the heartbeat runs the whole time:
	// if it finds this lease fenced — which is what the reaper quarantining it
	// looks like — it drops the entry and records a cleanup obligation carrying
	// no lease. The destroy then SUCCEEDS, proving the container is gone, and
	// the code that would resolve the quarantine has nothing left to name. The
	// capacity stays charged until a node happens to report an inventory, or
	// forever if that node never comes back.
	before, beforeOutcome, _ := l.completionRelease(job.RequestID)

	if err := l.destroyCompleted(ctx, job, before, beforeOutcome); err != nil {
		// THE RUNNER IS HOLDING IT, SO THIS LISTENER LETS GO.
		//
		// A backend whose teardown is asynchronous cannot answer this call with
		// proof that the compute is gone. The node takes the lease into its own
		// janitor instead, keeps heartbeating it, and releases it once the compute
		// is provably gone.
		//
		// So there is nothing here to release and nothing to retry. Recording a
		// cleanup obligation would re-issue a terminate on every pass for the life
		// of the process while the outcome is already being reconciled, and keeping
		// the lease in `running` would have two parties heartbeating one lease.
		//
		// The request id is given up either way: GitHub has been told this job is
		// finished, and a redelivered completion must not find this listener still
		// claiming the job.
		if errors.Is(err, dispatch.ErrCustody) {
			retired := l.forgetCompletion(ctx, job)
			if !retired {
				l.parkRetirement(job)
			}
			l.log.Info("the compute for a finished job was asked to stop and has not been "+
				"confirmed gone; the runner is holding its capacity until it is",
				"tier", l.tier, "request", job.RequestID)

			l.mu.Lock()
			delete(l.running, job.RequestID)
			delete(l.runningJobs, job.RequestID)
			if retired {
				if entry := l.cleanup[job.RequestID]; entry == nil ||
					entry.job.CompletionID == 0 || entry.job.CompletionID == job.CompletionID {
					delete(l.cleanup, job.RequestID)
				}
			}
			l.mu.Unlock()

			return
		}

		// THE HOLDER CANNOT BE REACHED, SO THIS LISTENER STOPS RENEWING AND KEEPS
		// THE OBLIGATION.
		//
		// The plane bound this completion to the process that launched the
		// compute, and that process is gone: dead, or replaced by one that
		// truthfully knows nothing about the build. Treating that as an ordinary
		// failed destroy — keep the lease in `running`, keep renewing, retry —
		// was a loop with no exit: the lease never expired, so it was never
		// quarantined, so the replacement's inventory could never settle it, so
		// every retry got the same answer, for as long as this process ran. It
		// showed as capacity charged with nothing held, and `--force` refused it
		// as busy.
		//
		// Renewal is what a party holding COMPUTE does, and this listener holds
		// none. Whoever does — a superseded process draining its custody, a
		// replacement that adopted the build — renews the lease itself; if nobody
		// does, the reaper quarantines it and its capacity stays charged until a
		// proof arrives, which is the identical protection one phase over. The
		// retry keeps asking, through the same bound destroy, and the plane
		// settles it from the replacement's inventory once the grace has passed.
		if errors.Is(err, dispatch.ErrHolderUnavailable) && before != nil {
			l.parkUnreachable(job, before, beforeOutcome)

			l.log.Warn("a finished job's compute is bound to a node process this control plane "+
				"cannot reach; its lease is no longer renewed here and stays charged until "+
				"whoever holds the compute renews it or its host proves the compute gone",
				"tier", l.tier, "request", job.RequestID, "lease", before.ID, "error", err)

			return
		}

		// NOT released, and NOT fatal. Two separate decisions.
		//
		// Not released, because the compute may still be running and freeing the
		// capacity now is exactly the overcommit this ordering prevents. The lease
		// stays in `running`, so it keeps being heartbeated and keeps being
		// counted — which is what makes holding it safe rather than a slow leak.
		//
		// Not fatal, because a listener error cancels every other listener and
		// their shutdowns then destroy every running job on the host. A docker
		// daemon hiccup while cleaning up ONE finished job would take down the
		// fleet. That is the same disproportion already rejected for an unbacked
		// assignment.
		l.log.Error("could not destroy the compute for a finished job; keeping its capacity "+
			"held and retrying, because releasing it now would let another tier use a "+
			"machine this job may still be on",
			"tier", l.tier, "request", job.RequestID, "error", err)

		// AND RETRIED, which is what turns "held" into something other than a leak.
		//
		// The comment above was true and incomplete: holding the capacity is safe,
		// and holding it FOREVER is not. Nothing else was ever going to try again —
		// GitHub's completion has been acknowledged, the node has already destroyed
		// what it had, and the lease sits in `running` being heartbeated by this
		// listener for the life of the process.
		l.mu.Lock()

		// ONLY IF THIS LISTENER ACTUALLY HOLDS THE LEASE. A completion can arrive
		// for a job this listener never assigned — a restart lost the in-memory map
		// while the lease lived on for the reaper — and recording those would grow
		// the map with entries whose retry can never accomplish anything.
		_, held := l.running[job.RequestID]

		if entry, ok := l.cleanup[job.RequestID]; ok {
			// A heartbeat can create the obligation before GitHub's completion
			// arrives. Keep the authoritative result so its retry takes the same
			// result-dependent teardown path as this attempt.
			if job.Result != "" {
				entry.job = job
			}
			// A RETRY THAT FAILED AGAIN, so it waits longer before the next one.
			entry.failed(time.Now(), l.retryFirst, l.retryMax)
		} else if _, promised := l.acquiring[job.RequestID]; held || promised ||
			(l.completionStore != nil && job.Result != "") {
			if l.cleanup == nil {
				l.cleanup = make(map[int64]*pendingCleanup)
			}

			// Recorded ready to run: the first retry is immediate because a node
			// that was briefly busy is far and away the common case.
			l.cleanup[job.RequestID] = &pendingCleanup{job: job}
		}

		// AN EXISTING RECORD IS NEVER DROPPED HERE, unlike the rule above.
		//
		// Losing the lease does not prove the container is gone. The record exists
		// because this listener launched something and could not destroy it; once the
		// lease is fenced or reaped the capacity is someone else's, but the compute is
		// still ours to remove, and GitHub will not redeliver the completion that would
		// ask again. Sweeper is not a substitute: it is OPTIONAL on the Runner
		// interface, so a non-sweeping runner leaves the container until the host
		// restarts.
		//
		// Entries back off (retryEvery, capped at maxRetryEvery) so a node that is never
		// coming back cannot occupy every pass ahead of one that has just recovered. The
		// map is in memory, so this does not survive a restart; durable cleanup state is
		// tracked separately.

		l.mu.Unlock()

		return
	}

	durableLease, durableOutcome, _ := l.completionRelease(job.RequestID)
	if durableLease == nil {
		durableLease, durableOutcome = before, beforeOutcome
	}
	if err := l.recordReleaseOnly(ctx, job, durableLease, durableOutcome); err != nil {
		l.parkReleaseOnly(job, durableLease, durableOutcome)
		l.log.Error("compute was confirmed absent, but its release-only obligation could not be made durable; capacity stays held until this is retried",
			"tier", l.tier, "request", job.RequestID, "lease", durableLease.ID, "error", err)

		return
	}

	l.mu.Lock()

	lease, ok := l.running[job.RequestID]

	// A RELEASE THAT NEVER LANDED PARKED THE LEASE HERE. The request id was given
	// up so a redelivery is not swallowed, and this is the only remaining
	// reference to the capacity it holds.
	outcome := alloc.PhaseDone

	if entry, parked := l.cleanup[job.RequestID]; parked && entry.lease != nil && !ok {
		lease, ok = entry.lease, true

		if entry.outcome != "" {
			outcome = entry.outcome
		}
	}

	// OR THE ONE NOTED BEFORE THE DESTROY, if the heartbeat dropped it while that
	// was in flight. AFTER the parked branch on purpose: that one carries an
	// outcome as well as a lease, and taking the bare reference first would
	// archive a job that never started as `done`.
	if !ok && before != nil {
		lease, ok = before, true
	}

	if !ok {
		// A job can also complete while it is still only PROMISED — GitHub cancels
		// an assignment no runner picks up in time, and that cancellation arrives
		// as a completion. The reserved escrow has to come back, or it is held for
		// an assignment that will never arrive.
		var p *promise

		if p, ok = l.acquiring[job.RequestID]; ok {
			lease = p.lease
		}
	}

	if !ok {
		// Not ours, or already released. Both are ordinary: GitHub can report a
		// job completed that this listener never assigned, if a restart lost the
		// in-memory map while the lease lives on in the ledger for the reaper.
		//
		// Nothing left to retry either way — including for a lease that was fenced
		// or reaped out from under this listener, whose entry would otherwise sit
		// in the map being retried for the life of the process.
		l.mu.Unlock()
		if l.forgetCompletion(ctx, job) {
			l.deleteCompletionCleanup(job)
		} else {
			l.parkRetirement(job)
		}

		return
	}

	relErr := l.alloc.Release(ctx, lease.ID, lease.Epoch, outcome)

	// FENCED NO LONGER PROVES THE CAPACITY CAME BACK.
	//
	// It used to: the reaper terminalized whatever it took, so a stale epoch
	// meant the lease was already finished. Quarantine changed that — a lease
	// with compute behind it is moved aside and KEPT CHARGED, so a release
	// refused for a stale epoch may be refused by a lease that is still holding
	// its host's capacity.
	//
	// This listener can settle it, because it has the one thing the quarantine is
	// waiting for: the destroy above SUCCEEDED, so the compute is confirmed gone.
	// That is the same proof a node offers, and it goes through the same door.
	// Terminalizing at the current epoch instead would be the dangerous version
	// of this — it would free the capacity on the strength of the epoch alone,
	// which says nothing about whether a container exists.
	if errors.Is(relErr, alloc.ErrFenced) {
		// WITH THE OUTCOME THIS PATH ALREADY KNOWS. The job finished and its
		// compute is confirmed gone; archiving it as failed because the reaper got
		// to the lease first would record a job GitHub reported completed as one
		// that did not.
		if err := l.alloc.ResolveQuarantine(ctx, lease.ID, outcome); err == nil {
			l.log.Warn("a finished job's lease had been quarantined before its release "+
				"landed; its compute is confirmed gone, so the capacity is back",
				"tier", l.tier, "request", job.RequestID, "lease", lease.ID)

			relErr = nil
		} else if !errors.Is(err, alloc.ErrLeaseNotFound) {
			// Not quarantined, or the ledger could not answer. Either way this is
			// not settled by us.
			relErr = err
		}
	}

	if err := relErr; !releaseSettled(err) {
		// PARKED AND NOT RETURNED, because of who is calling.
		//
		// complete runs on the poll path as well as the cleanup loop, and there
		// the returned error is FATAL: it stops the listener, which cancels every
		// other listener, whose shutdowns destroy every job running on this host.
		// A busy database while GitHub happens to report a completion would take
		// the whole deployment down.
		//
		// That is also self-defeating. The reason for returning the error was to
		// keep the obligation alive for a retry — and the retry runs in the
		// process the error kills. So the obligation is recorded here, where it
		// will be picked up, and the caller is told the completion was handled.
		if l.cleanup == nil {
			l.cleanup = make(map[int64]*pendingCleanup)
		}

		if entry, pending := l.cleanup[job.RequestID]; pending {
			entry.lease, entry.outcome, entry.releaseOnly = lease, outcome, true
			entry.failed(time.Now(), l.retryFirst, l.retryMax)
		} else {
			l.cleanup[job.RequestID] = &pendingCleanup{
				job: job, lease: lease, outcome: outcome, releaseOnly: true,
			}
		}

		l.log.Error("could not release the lease of a finished job; its capacity is held "+
			"until this is retried",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", err)

		delete(l.running, job.RequestID)
		delete(l.runningJobs, job.RequestID)
		delete(l.acquiring, job.RequestID)
		l.mu.Unlock()

		return
	}

	// RELEASED, so the job is finally over and there is nothing to retry.
	delete(l.running, job.RequestID)
	delete(l.runningJobs, job.RequestID)
	delete(l.acquiring, job.RequestID)
	l.mu.Unlock()
	if l.forgetCompletion(ctx, job) {
		l.deleteCompletionCleanup(job)
	} else {
		l.parkRetirement(job)
	}
}

func (l *Listener) destroyCompleted(
	ctx context.Context,
	job dispatch.Job,
	lease *alloc.Lease,
	outcome alloc.Phase,
) error {
	if l.registry != nil {
		var binding alloc.PoolRunner
		var err error
		if lease != nil && lease.ID != "" {
			binding, err = l.alloc.PoolRunnerByLease(ctx, lease.ID)
		} else if job.RunnerName != "" {
			binding, err = l.alloc.PoolRunnerByName(ctx, job.RunnerName)
		}
		if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) {
			return fmt.Errorf("server: resolve runner registration before teardown: %w", err)
		}
		if errors.Is(err, alloc.ErrLeaseNotFound) && lease != nil && lease.ID != "" {
			binding.RunnerName = provider.InstanceName(lease.ID)
			err = nil
		}
		// Withdraw the registration by whatever identity is available: prefer the
		// durable binding, fall back to the identity the completion itself carries
		// (an id-only completion never populates the binding, which is resolved
		// from the lease and runner name). Skip removal only when there is
		// genuinely no registration to withdraw anywhere — no binding, no lease,
		// no runner id or name — because RemoveRunner(0, "") is refused "needs an
		// id or name" every time and would wedge the completion's capacity in an
		// unbounded teardown retry. An absent registration needs no removal.
		removeID, removeName := binding.RunnerID, binding.RunnerName
		if removeID == 0 && removeName == "" {
			removeID, removeName = job.RunnerID, job.RunnerName
		}
		// AN ID THE JOB CARRIES FOR THE SAME NAME IS KEPT, so the removal refuses a
		// registration that has since taken the name under another id. An offline
		// member's row has a name and no id, and the id its retirement withdrew
		// arrives here only on the job.
		if removeID == 0 && job.RunnerID > 0 && removeName == job.RunnerName {
			removeID = job.RunnerID
		}
		if err == nil && (removeID > 0 || removeName != "") {
			if err := l.registry.RemoveRunner(ctx, removeID, removeName); err != nil {
				return fmt.Errorf("server: remove runner %q before teardown: %w", removeName, err)
			}
			// The runner is gone from GitHub. Stop counting this lease as a live
			// runner even if the compute-destroy below retries, so a lingering
			// teardown does not over-count against the assignment deficit. The
			// mark is unfenced on purpose: a reap may quarantine this lease before
			// we get here, and the runner is gone regardless. A failure here only
			// leaves it counted, which is the safe direction.
			if lease != nil && lease.ID != "" {
				if err := l.alloc.MarkDeregistered(ctx, lease.ID); err != nil {
					l.log.Warn("could not record runner deregistration; the lease stays counted until it terminalizes",
						"tier", l.tier, "lease", lease.ID, "error", err)
				}
			}
		}
	}
	if outcome == "" {
		outcome = alloc.PhaseDone
	}
	var authority dispatch.CacheAuthority
	if job.Result != "" && lease != nil {
		authority = l.completionCacheAuthority(ctx, job, lease.ID)
	}
	if runner, ok := l.runner.(dispatch.BoundCompletionAwareRunner); ok && job.Result != "" &&
		lease != nil && lease.ID != "" && lease.Node != "" {
		return runner.DestroyCompletedBound(
			ctx, job.RequestID, job.Result, lease.ID, lease.Node, lease.Epoch, outcome, authority)
	}
	if runner, ok := l.runner.(dispatch.CompletionAwareRunner); ok && job.Result != "" {
		return runner.DestroyCompleted(ctx, job.RequestID, job.Result, authority)
	}

	return l.runner.Destroy(ctx, job.RequestID)
}
