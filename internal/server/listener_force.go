package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/state"
)

// forceDestroy carries out an operator's explicit decision to destroy compute
// that is still running a job.
//
// THE ONLY THING IN BILLET THAT FAILS A BUILD ON PURPOSE, and everything about
// its shape exists to keep it from being reached any other way. A drain timeout,
// a second signal, a systemd TimeoutStopSec, a failed rollout and a lost
// leadership epoch all leave running work alone; only a durable record naming an
// actor, a reason and an exact set of leases reaches this.
//
// IT ACTS ON THE RECORDED SET AND NOT ON WHAT THIS LISTENER HAPPENS TO HOLD. The
// operator was shown a list and approved that list; a job that started between
// the diagnostic and the confirmation was never approved, and destroying it here
// would be the implicit teardown the whole mechanism refuses.
//
// IT NEVER RETURNS AN ERROR, for the reason markAdmission does not: an error out
// of the poll loop stops this listener, one listener stopping cancels every
// other, and their teardown runs. A database blip must not be able to do that, so
// a failure here is logged and retried on the next poll — the record is durable
// and does not expire.
func (l *Listener) forceDestroy(ctx context.Context) {
	if l.alloc == nil {
		return
	}

	open, found, err := l.alloc.OpenForceDestroy(ctx)
	if err != nil {
		l.log.Warn("could not read whether an operator has asked for running compute to be "+
			"destroyed; nothing was destroyed, and this is retried on the next poll",
			"tier", l.tier, "error", err)

		return
	}

	if !found {
		return
	}

	targets, err := l.alloc.PendingForceTargets(ctx, open.Generation, l.tier)
	if err != nil {
		l.log.Warn("could not read which leases a force-destroy covers; nothing was "+
			"destroyed, and this is retried on the next poll",
			"tier", l.tier, "force", open.Generation, "error", err)

		return
	}

	if len(targets) == 0 {
		return
	}

	l.log.Warn("DESTROYING RUNNING COMPUTE because an operator asked for it; the jobs on "+
		"these leases fail, and GitHub does not requeue a job whose runner vanished "+
		"after it started",
		"tier", l.tier, "force", open.Generation, "actor", open.Actor,
		"reason", open.Reason, "leases", len(targets))

	// SPLIT BY WHO CAN REACH THE COMPUTE, not by phase. A lease this listener
	// launched is in its own escrow and goes through the full destroy pass, with
	// its custody, completion-persistence and parking behaviour. A lease that
	// outlived a control plane is ADOPTED — the node holds the guest and this
	// process holds no object for it — so the request id from the durable record
	// is the only handle there is. Keying the whole operation on the in-memory map
	// would make a force silently do nothing after exactly the restart that most
	// often precedes one.
	scope := make(map[int64]bool, len(targets))
	adopted := make([]state.ForceTarget, 0, len(targets))
	unaddressable := make([]state.ForceTarget, 0)

	l.mu.Lock()

	for i := range targets {
		t := &targets[i]

		if t.SchedulerRequest == 0 {
			unaddressable = append(unaddressable, *t)

			continue
		}

		_, running := l.running[t.SchedulerRequest]
		_, pending := l.cleanup[t.SchedulerRequest]

		if running || pending {
			scope[t.SchedulerRequest] = true

			continue
		}

		adopted = append(adopted, *t)
	}

	l.mu.Unlock()

	destroyed := map[int64]bool{}
	if len(scope) > 0 {
		destroyed = l.destroyAll(ctx, true, scope)
	}

	// SETTLED ONCE, AND TRACKED. Without this a target the adopted loop already
	// recorded is walked again below, which settles it a second time with a LESS
	// accurate detail and logs a second error about it. The settlement itself is
	// idempotent, so the damage is a diagnostic that contradicts the one above it —
	// which is exactly what an operator reads when they are trying to work out
	// what happened to a host.
	settled := make(map[string]bool, len(targets))

	settle := func(t *state.ForceTarget, disposition, detail string) {
		settled[t.LeaseID] = true

		l.settleForced(ctx, open.Generation, t, disposition, detail)
	}

	for i := range adopted {
		t := &adopted[i]

		// STRAIGHT AT THE RUNNER, because there is nothing else to go through. The
		// node is holding this guest on its own; Destroy is required to be
		// idempotent, so asking about compute that has already gone costs a no-op.
		if err := l.runner.Destroy(ctx, t.SchedulerRequest); err != nil {
			settle(t, state.ForceTargetFailed,
				fmt.Sprintf("destroying adopted compute failed: %v", err))

			continue
		}

		destroyed[t.SchedulerRequest] = true
	}

	for i := range unaddressable {
		t := &unaddressable[i]

		// NO SCHEDULER IDENTITY IS A CONCLUSIVE FAILURE FOR THIS LEASE, not a
		// reason to leave the request open. Nothing billet has can name the compute
		// to a node, so no amount of retrying reaches it, and an open request that
		// can never finish blocks the next force.
		settle(t, state.ForceTargetFailed,
			"this lease carries no scheduler request, so no node can be told which "+
				"compute to destroy")
	}

	for i := range targets {
		t := &targets[i]

		if t.SchedulerRequest == 0 || settled[t.LeaseID] {
			continue
		}

		if !destroyed[t.SchedulerRequest] {
			// NOT PROOF THE CONTAINER SURVIVED, and nothing is released on it. The
			// lease stays charged and the row says `failed`, which is what an
			// operator reads when a force reports it did not finish.
			settle(t, state.ForceTargetFailed,
				"the destroy did not confirm; this lease stays charged because a failed "+
					"teardown is not evidence the compute is gone")

			continue
		}

		switch err := l.alloc.ForceTerminate(ctx, t.LeaseID); {
		case errors.Is(err, alloc.ErrForceHeld):
			settle(t, state.ForceTargetFailed,
				"a node took custody of this lease before its capacity could be returned; "+
					"resolve it with `billet leases release --force` once you know the "+
					"compute is gone")
		case err != nil:
			settle(t, state.ForceTargetFailed,
				fmt.Sprintf("the compute was destroyed but its capacity could not be "+
					"returned: %v", err))
		default:
			// THE LEASE LEAVES `running` HERE, and forgetting to do this was a real
			// defect rather than untidiness. destroyAll does not clear that map — on
			// a shutdown, releaseAll is what walks it — so a forced lease stayed
			// there pointing at a row that is now terminal, and capacity() went on
			// counting it. The listener would then advertise a slot it does not
			// have until the next heartbeat happened to get ErrLeaseNotFound and
			// drop it, which is an overcommit window that exists only because
			// nobody removed a map entry.
			l.mu.Lock()
			delete(l.running, t.SchedulerRequest)
			delete(l.runningJobs, t.SchedulerRequest)
			delete(l.cleanup, t.SchedulerRequest)
			l.mu.Unlock()

			settle(t, state.ForceTargetDestroyed, "")
		}
	}
}

// settleForced records what became of one forced lease.
//
// A SETTLEMENT THAT DOES NOT LAND IS LEFT PENDING, deliberately. The compute is
// already destroyed either way; what is lost is the record, and re-observing the
// target on the next poll re-runs an idempotent destroy against compute that has
// gone. The alternative — treating the write as best effort — leaves a request
// open forever with nothing saying why.
func (l *Listener) settleForced(
	ctx context.Context, generation int64, t *state.ForceTarget, disposition, detail string,
) {
	if err := l.alloc.SettleForceTarget(ctx, generation, t.LeaseID, disposition,
		detail); err != nil {
		l.log.Error("could not record what became of a lease an operator forced; the "+
			"force-destroy stays open and this is retried on the next poll",
			"tier", l.tier, "force", generation, "lease", t.LeaseID,
			"disposition", disposition, "error", err)

		return
	}

	if disposition == state.ForceTargetDestroyed {
		l.log.Warn("destroyed a running job's compute on an operator's instruction; that "+
			"build fails and GitHub will not requeue it",
			"tier", l.tier, "force", generation, "lease", t.LeaseID, "node", t.Node,
			"run", t.RunID, "request", t.SchedulerRequest)

		return
	}

	l.log.Error("a lease an operator asked to be destroyed was not settled as destroyed",
		"tier", l.tier, "force", generation, "lease", t.LeaseID, "node", t.Node,
		"run", t.RunID, "detail", detail)
}
