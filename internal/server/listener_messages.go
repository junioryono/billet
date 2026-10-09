package server

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

// handle processes one message and acknowledges it.
//
// lastMessageID is advanced at the END, and only once the acknowledgement lands.
//
// It is sent to the SERVICE as ?lastMessageId= (session_client.go getMessage),
// so it is a claim about what billet is done with rather than a local note. What
// the source actually proves is narrower than it is tempting to state: the
// client only shows that the parameter is SENT. The service's own contract, per
// the client's doc comment, is that an undeleted message is returned again. How
// the queue treats the parameter is not established by anything billet can read,
// and this comment previously asserted it did.
//
// Advancing it after the acknowledgement is the conservative order under either
// reading. If the parameter does filter, advancing early skips a message whose
// work never happened; if it does not, advancing late costs one redelivery of a
// message that is safe to re-handle. The asymmetry is the whole argument.
//
// Every failure here is still fatal, and the one non-fatal session failure (a
// poll that failed past the client's retries) replaces the session in
// recoverSession, which starts the cursor at zero as a fresh listener does. A
// non-fatal path that keeps the session would inherit the question.
func (l *Listener) handle(ctx context.Context, msg *Message) error {
	l.mu.Lock()
	if l.heldMessageID != nil && *l.heldMessageID != msg.MessageID {
		heldID := *l.heldMessageID
		l.mu.Unlock()
		return fmt.Errorf("%w: message %d arrived while message %d holds candidate jobs",
			ErrUntrustworthySession, msg.MessageID, heldID)
	}
	l.mu.Unlock()

	// STARTS PRECEDE COMPLETIONS EVEN WHEN GITHUB BATCHES THEM TOGETHER. The
	// start is the authoritative runner-to-job binding; resolving the completion
	// first would either settle the request that caused launch or mistake a busy
	// runner for idle surplus.
	for i := range msg.Started {
		job, err := l.identifyStarted(ctx, msg.Started[i])
		if err != nil {
			if errors.Is(err, errQuarantinableStarted) {
				l.quarantineStarted(ctx, msg.Started[i], err)
				continue
			}

			return err
		}
		// The identity is the message's own, which is the evidence a cache
		// authority will be decided from; what each field holds per event is
		// measured on the fleet from this line (#226).
		l.log.Info("a pooled runner started a job", "tier", l.tier,
			"runner", job.RunnerName, "request", job.RequestID, "job", job.JobID,
			"run", msg.Started[i].RunID, "event", msg.Started[i].Event,
			"owner", msg.Started[i].Owner, "repository", msg.Started[i].Repository,
			"workflow_ref", msg.Started[i].WorkflowRef)
	}

	resolved, err := l.resolveMessage(ctx, msg)
	// Persist the hold before any later error can send Run into its drain.
	if len(resolved.held) > 0 {
		l.mu.Lock()
		id := msg.MessageID
		l.heldMessageID = &id
		l.mu.Unlock()
	}
	if err != nil {
		return err
	}

	// Completion precedes acquisition so its replacement can use released escrow.
	// Both acquisition filters use resolveActualJob's rule, including redelivery
	// after a completion has retired or been superseded by a newer delivery.
	finished := make([]actualJobIdentity, 0, len(resolved.completed))
	completed := make([]dispatch.Job, 0, len(resolved.completed))
	for i := range resolved.completed {
		entry := &resolved.completed[i]
		if containsActual(resolved.held, entry.actual) {
			continue
		}
		if entry.detached {
			// Still finished, so a redelivered offer for it is not acquired.
			finished = append(finished, entry.actual)
			l.log.Info("a job completed before any runner took it; the runner launched for it is running another job and is left to finish it",
				"tier", l.tier, "request", entry.cleanup.RequestID, "job", entry.job.JobID,
				"result", entry.job.Result)
			continue
		}
		if entry.binding != nil {
			if err := l.restorePoolLease(ctx, *entry.binding); err != nil {
				return err
			}
		}
		finished = append(finished, entry.actual)
		completed = append(completed, entry.cleanup)
	}

	handled := *msg
	handled.Completed = completed

	var poison error
	if len(resolved.poisoned) > 0 {
		poison = &poisonedMessageError{
			cause:       errors.Join(resolved.poisoned...),
			completions: slices.Clone(completed),
			held:        len(resolved.held) > 0,
		}
	}

	for i := range completed {
		job := completed[i]
		l.log.Info("received a completed job", "tier", l.tier, "request", job.RequestID,
			"runner", job.RunnerName, "result", job.Result)

		disposition, err := l.recordCompletion(ctx, job)
		if err != nil {
			return err
		}
		if disposition != state.PendingCompletionActionable {
			continue
		}
		l.complete(ctx, job)
	}

	// A zero advertisement is not the same as refusing work, and this is where
	// the difference is enforced.
	//
	// AdvertiseNothing stops billet asking for jobs; it does not stop GitHub
	// delivering a message that was already queued, or redelivering one that was
	// never acknowledged. Acquiring from that would claim a job nothing can run —
	// the precise outcome the dry run exists to avoid — so the refusal is local
	// as well as advertised.
	if l.maxCapacity != nil && *l.maxCapacity == 0 {
		if len(msg.Available) > 0 || len(msg.Assigned) > 0 {
			l.log.Warn("declining work while advertising no capacity",
				"tier", l.tier, "available", len(msg.Available), "assigned", len(msg.Assigned))
		}

		if poison != nil {
			return poison
		}

		if err := l.acknowledge(ctx, &handled); err != nil {
			return err
		}
		l.acknowledgeCompletions(ctx, &handled)

		// Advanced here too. The invariant is "a successful acknowledgement
		// advances the cursor", and an early return that acknowledges without
		// advancing is the same class of inconsistency the reordering fixed.
		l.lastMessageID = msg.MessageID

		return nil
	}

	// NO NEW WORK WHILE DRAINING, and the refusal has to be HERE rather than in
	// the advertisement.
	//
	// Releasing the idle escrow drops the number billet sends, which asks GitHub
	// to stop offering — it does not stop a message already queued from arriving,
	// or an unacknowledged one from being redelivered. Both would otherwise be
	// acquired, because the refill immediately below would take the capacity back
	// and the acquisition would then be perfectly backed. The drain would never
	// converge: every offer it accepted would extend it.
	//
	// ONLY OFFERS. Assigned still runs and Completed still completes — those are
	// promises made before the drain started, and abandoning them would strand the
	// leases the drain is waiting on and leave GitHub holding a job nothing will
	// launch.
	// A SEAL REFUSES OFFERS FOR THE SAME REASON A DRAIN DOES, and in the same
	// place. Lowering the advertisement asks GitHub to stop offering; it does not
	// stop a queued message arriving or an unacknowledged one being redelivered,
	// and either would otherwise be acquired against escrow the refill takes
	// straight back.
	switch {
	case l.isDraining() || l.isQuiesced():
		if len(msg.Available) > 0 {
			l.log.Info("declining an offer: this deployment is not taking new work",
				"tier", l.tier, "available", len(msg.Available),
				"reason", refusalReason(l.isDraining()))
		}
	default:
		// ONLY REAL ESCROW CAN BACK AN ACQUISITION, and it is bought here, for
		// exactly the offers in hand: the allocator's atomic purchase decides
		// whether there is room, and what it refuses is not acquired.
		// AND THE OFFER PATH ASKS THE ORDER, for the reason backAssignment does:
		// escrow bought here is room a waiting tier was accumulating (#157).
		// What is not bought is not acquired, and GitHub offers it again.
		if len(msg.Available) > 0 && l.order.mayBuy(l.tier, l.admission(ctx)) {
			if err := l.refillEscrowTo(ctx, l.targetCapacityFor(len(msg.Available))); err != nil {
				return err
			}
		}

		// AVAILABLE is what gets acquired. Available is the offer; Assigned is the
		// confirmation that an offer was claimed. Acquiring from Assigned asks
		// GitHub to claim work it has already handed over, and drops every offer.
		if err := l.acquireUnfinished(ctx, resolved.available, finished, resolved.held, resolved.committed); err != nil {
			return err
		}
	}

	// ASSIGNED is what consumes escrow. The lease is bound here because this is
	// the point at which the work is definitely billet's to run.
	//
	// LAUNCHING HAPPENS AFTER, outside the escrow mutex that assign holds. A
	// launch pulls images and talks to a hypervisor; doing it under the mutex
	// would stall every heartbeat behind it, and heartbeats are what keep the
	// escrow alive. So assign returns what it bound and the launches follow.
	assignmentDeficit := -1
	if msg.Statistics != nil && msg.Statistics.TotalAssignedJobs >= 0 && l.alloc != nil {
		active, err := l.activePoolMembers(ctx)
		if err != nil {
			return err
		}
		assignmentDeficit = max(msg.Statistics.TotalAssignedJobs-active, 0)
	}
	// Assignment filtering follows resolveActualJob, never the cleanup request.
	for i := range resolved.assigned {
		entry := &resolved.assigned[i]
		job := entry.job
		if containsActual(finished, entry.actual) || containsActual(resolved.held, entry.actual) {
			continue
		}
		if assignmentDeficit == 0 {
			l.releasePromise(job.RequestID)
			continue
		}

		if err := l.backAssignment(ctx, job.RequestID); err != nil {
			return err
		}

		lease, needsCompute, err := l.assignResolved(ctx, *entry)
		if err != nil {
			return err
		}

		if !needsCompute {
			// Declined, or a redelivery of something already running. Either way
			// there is nothing new to start.
			continue
		}

		if err := l.launch(ctx, lease, job); err != nil {
			return err
		}
		if assignmentDeficit > 0 {
			assignmentDeficit--
		}
	}

	// A delivery that no longer holds carries current statistics even while the
	// earlier hold blocks reconciliation until this message is acknowledged.
	if msg.Statistics != nil && len(resolved.held) == 0 {
		l.observed = msg.Statistics
		if err := l.reconcilePool(ctx, msg.Statistics.TotalAssignedJobs); err != nil {
			return err
		}
	}

	if poison != nil {
		return poison
	}

	if err := l.acknowledge(ctx, &handled); err != nil {
		return err
	}
	l.acknowledgeCompletions(ctx, &handled)

	// Only now. An unacknowledged message is redelivered, and re-handling one is
	// safe once the acquisition outcome is KNOWN: completions rebuild their own
	// tombstone set, offers already promised are skipped by reserve, and
	// assignments are idempotent by request id. Skipping a message is not
	// recoverable, so late beats early.
	//
	// AN AMBIGUOUS ACQUISITION KEEPS ITS PROMISES. A lost response says nothing
	// about which jobs GitHub acquired. Cancellation can continue into the drain,
	// so those leases must stay out of held until assignment or session closure.
	l.lastMessageID = msg.MessageID

	return nil
}

// quarantineStarted confines a contradictory identity to the one pool member.
// A scale-set message is shared infrastructure; taking every tier down over one
// runner makes an already-bad registration a fleet-wide outage.
func (l *Listener) quarantineStarted(ctx context.Context, job dispatch.Job, cause error) {
	l.log.Error("a pooled runner reported a contradictory job identity; retiring only that member",
		"tier", l.tier, "runner", job.RunnerName, "runner_id", job.RunnerID,
		"job", job.JobID, "error", cause)
	if l.alloc != nil && job.RunnerName != "" {
		member, err := l.alloc.PoolRunnerByName(ctx, job.RunnerName)
		if errors.Is(err, alloc.ErrLeaseNotFound) {
			if leaseID, ok := provider.LeaseOf(job.RunnerName); ok {
				if legacy, leaseErr := l.alloc.JobForLease(ctx, leaseID); leaseErr == nil &&
					legacy.Tier == l.tier && legacy.RequestID != 0 {
					regErr := l.alloc.RegisterPoolRunner(ctx, alloc.PoolRunner{LeaseID: leaseID,
						Tier: l.tier, LaunchRequestID: legacy.RequestID, RunnerID: job.RunnerID,
						RunnerName: job.RunnerName})
					if regErr == nil {
						member, err = l.alloc.PoolRunnerByName(ctx, job.RunnerName)
					} else {
						err = regErr
					}
				}
			}
		}
		if err == nil {
			if member.Tier != l.tier {
				l.log.Error("refused to quarantine a runner owned by another tier",
					"tier", l.tier, "runner", job.RunnerName, "owner_tier", member.Tier)
				return
			}
			if retireErr := l.alloc.RetirePoolRunner(ctx, member.LeaseID); retireErr != nil &&
				!errors.Is(retireErr, alloc.ErrLeaseNotFound) {
				l.log.Error("could not journal the contradictory runner's retirement",
					"tier", l.tier, "runner", job.RunnerName, "error", retireErr)
				return
			}
			member.Status = alloc.PoolRunnerRetiring
			l.retirePoolMember(ctx, member)
			return
		}
		if !errors.Is(err, alloc.ErrLeaseNotFound) {
			l.log.Error("could not resolve the contradictory runner for retirement",
				"tier", l.tier, "runner", job.RunnerName, "error", err)
			return
		}
	}
	if l.registry != nil && (job.RunnerID > 0 || job.RunnerName != "") {
		if err := l.registry.RemoveRunner(ctx, job.RunnerID, job.RunnerName); err != nil {
			l.log.Error("could not remove an unrecognized contradictory registration",
				"tier", l.tier, "runner", job.RunnerName, "error", err)
		}
		// Deliberately NOT MarkDeregistered here. The removed name resolves to a
		// lease only by string, and RemoveRunner returning nil proves only that
		// this name is absent — the encoded lease may be another tier's, or still
		// in flight and about to register its own runner. Marking it would
		// false-exclude a live runner (a double-schedule); leaving it counted is
		// at worst a transient over-count that terminalization clears.
	}
}

// identifyStarted records which job a registered pool member actually consumed,
// using resolveActualJob for its scheduler aliases.
func (l *Listener) identifyStarted(ctx context.Context, job dispatch.Job) (dispatch.Job, error) {
	if l.alloc == nil {
		return dispatch.Job{}, fmt.Errorf("%w: %s started runner %q without a ledger",
			ErrUntrustworthySession, l.tier, job.RunnerName)
	}
	if job.RunnerID <= 0 || job.RunnerName == "" || job.JobID == "" {
		return dispatch.Job{}, fmt.Errorf("%w: %s received an incomplete started identity for runner %q",
			errQuarantinableStarted, l.tier, job.RunnerName)
	}
	resolved, err := l.resolveActualJob(ctx, job, resolveAcquisition, nil)
	if err != nil {
		return dispatch.Job{}, err
	}
	identified := resolved.job
	member, err := l.alloc.PoolRunnerByName(ctx, job.RunnerName)
	leaseID := member.LeaseID
	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		var ok bool
		leaseID, ok = provider.LeaseOf(job.RunnerName)
		if !ok {
			return dispatch.Job{}, fmt.Errorf("%w: started runner %q has no Billet lease identity",
				errQuarantinableStarted, job.RunnerName)
		}
		legacy, leaseErr := l.alloc.JobForLease(ctx, leaseID)
		if leaseErr != nil {
			if errors.Is(leaseErr, alloc.ErrLeaseNotFound) {
				return dispatch.Job{}, fmt.Errorf("%w: cannot adopt unknown started runner %q",
					errQuarantinableStarted, job.RunnerName)
			}
			return dispatch.Job{}, fmt.Errorf("server: read lease identity for started runner %q: %w",
				job.RunnerName, leaseErr)
		}
		if legacy.Tier != l.tier || legacy.RequestID == 0 {
			return dispatch.Job{}, fmt.Errorf("%w: started runner %q resolves outside tier %q",
				ErrUntrustworthySession, job.RunnerName, l.tier)
		}
		if regErr := l.alloc.RegisterPoolRunner(ctx, alloc.PoolRunner{LeaseID: leaseID,
			Tier: l.tier, LaunchRequestID: legacy.RequestID, RunnerName: job.RunnerName}); regErr != nil {
			if errors.Is(regErr, alloc.ErrConflict) {
				return dispatch.Job{}, fmt.Errorf("%w: cannot adopt started runner %q: %w",
					errQuarantinableStarted, job.RunnerName, regErr)
			}
			return dispatch.Job{}, fmt.Errorf("server: adopt pool runner %q: %w", job.RunnerName, regErr)
		}
	case err != nil:
		return dispatch.Job{}, fmt.Errorf("server: read pool runner %q: %w", job.RunnerName, err)
	case member.Tier != l.tier:
		return dispatch.Job{}, fmt.Errorf("%w: started runner %q belongs to tier %q, not %q",
			ErrUntrustworthySession, job.RunnerName, member.Tier, l.tier)
	}

	if _, err := l.alloc.StartPoolRunner(ctx, leaseID, l.tier, job.RunnerID,
		job.RunnerName, identified.RequestID, identified.RunID, identified.JobID,
		alloc.JobIdentity{Owner: job.Owner, Repository: job.Repository,
			WorkflowRef: job.WorkflowRef, Event: job.Event}); err != nil {
		if errors.Is(err, alloc.ErrConflict) || errors.Is(err, alloc.ErrLeaseNotFound) {
			return dispatch.Job{}, fmt.Errorf("%w: cannot bind started runner %q: %w",
				errQuarantinableStarted, job.RunnerName, err)
		}
		return dispatch.Job{}, fmt.Errorf("server: bind started runner %q: %w", job.RunnerName, err)
	}
	l.recordJobIdentity(ctx, leaseID, job)
	return identified, nil
}
