package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/state"
)

// resultFailed is GitHub's result for a job that failed, as its completions spell it, in any
// case. A cancelled job is not held: nobody is waiting to see why it stopped.
const resultFailed = "failed"

// WithHoldOnFailure keeps a failed job's VM for up to d (tiers[].debug.hold_on_failure).
func WithHoldOnFailure(d time.Duration) Option {
	return func(l *Listener) { l.holdOnFailure = d }
}

// holdCompletion says whether this completion's destroy waits for a hold.
//
// A HOLD DELAYS A DESTROY THE COMPLETION ALREADY ASKED FOR, AND NEVER AUTHORISES ONE: nothing
// here reaches compute, and when it ends the destroy is what it always was. The lease stays
// in `running` for all of it, so it stays charged and heartbeated, exactly as while a destroy is
// being retried; after a restart the node that adopted it renews it, as any job that survived. A pool member's retirement asks too (retirementHeld), and a shutdown leaves a
// held entry alone as it leaves a running job (destroyAll). A launch that failed and a force
// operation destroy without asking: neither is a job's completion.
//
// A LEDGER THAT CANNOT ANSWER MEANS NO HOLD. Keeping capacity on a guess is the costlier
// mistake, and what it loses is one VM someone might have looked at.
func (l *Listener) holdCompletion(ctx context.Context, job dispatch.Job, lease *alloc.Lease) bool {
	if lease == nil || lease.ID == "" || !strings.EqualFold(job.Result, resultFailed) {
		return false
	}

	// ONLY A PROMISE: no VM was started to look at.
	l.mu.Lock()
	_, promised := l.acquiring[job.RequestID]
	l.mu.Unlock()

	if promised {
		return false
	}

	// A ROW ALREADY THERE STANDS WHATEVER THIS TIER SAYS NOW, opened by an earlier process or
	// under an earlier setting, and only a new one reads the setting.
	var (
		hold alloc.LeaseHold
		err  error
	)

	// NOR A NEW ONE WHILE A DRAIN IS WAITING: someone wants this tier empty, and an hour of a job that
	// failed after the seal is not what they are waiting for. THE POLL LOOP'S FLAGS ARE ONLY THE FAST
	// ANSWER: a restarted listener runs its cleanup before its session opens and so before any poll
	// has read admission (#783), and a seal another process wrote reaches them only a poll later, so
	// OpenLeaseHold reads the seal in the transaction that would write the hold.
	if l.holdOnFailure > 0 && !l.isDraining() && !l.isQuiesced() {
		hold, err = l.alloc.OpenLeaseHold(ctx, lease.ID, l.tier, time.Now().Add(l.holdOnFailure))
		if errors.Is(err, alloc.ErrAdmissionSealed) {
			return false
		}
	} else {
		hold, err = l.alloc.LeaseHoldOf(ctx, lease.ID)
		if errors.Is(err, alloc.ErrLeaseNotFound) {
			return false
		}
	}

	if err != nil {
		l.log.Warn("could not read the hold of a failed job's VM, so it is destroyed as usual",
			"tier", l.tier, "lease", lease.ID, "error", err)

		return false
	}

	if !l.holdStands(ctx, hold) {
		return false
	}

	l.parkHeld(ctx, job, lease, hold)

	return true
}

// holdStands says a hold is still holding, and ends one that has reached its bound. THE BOUND IS
// WHAT THE ROW SAYS, never this tier's setting now: a restart or an edit cannot move it.
func (l *Listener) holdStands(ctx context.Context, hold alloc.LeaseHold) bool {
	if !hold.Holding() {
		return false
	}

	if time.Now().Before(hold.Until) {
		return true
	}

	if _, err := l.alloc.EndLeaseHold(ctx, hold.LeaseID, alloc.HoldEndedByBound); err != nil {
		l.log.Warn("could not record the end of a failed job's hold; its VM is destroyed all the same",
			"tier", l.tier, "lease", hold.LeaseID, "error", err)
	}

	l.log.Info("a failed job's hold reached its bound, so its VM is destroyed", "tier", l.tier, "lease", hold.LeaseID)

	return false
}

// retirementHeld says a pool member's retirement waits for a hold. A COMPLETION JOURNALS ITS RUNNER
// AS RETIRING BEFORE ANYTHING ELSE, and every reconcile retires what is journaled, so without this
// the next message would destroy a VM the completion had just kept.
//
// AND IT WAITS FOR THE COMPLETION'S OWN DESTROY ONCE THE HOLD HAS ENDED. That one is bound to the node
// holding the VM and carries the proof a release needs; a retirement's goes by request id, which after
// a restart can answer that nothing is there with the VM still running. The cleanup clock's next pass
// does it.
func (l *Listener) retirementHeld(ctx context.Context, member alloc.PoolRunner) bool {
	hold, err := l.alloc.LeaseHoldOf(ctx, member.LeaseID)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return false
	}

	if err != nil {
		l.log.Warn("could not read the hold of a retiring runner's VM, so it retires as usual",
			"tier", l.tier, "lease", member.LeaseID, "error", err)

		return false
	}

	if l.holdStands(ctx, hold) {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, owed := l.cleanup[member.LaunchRequestID]

	return owed && !entry.releaseOnly && !entry.retireOnly
}

// parkHeld keeps a held completion's entry for the cleanup clock to look at again, with no
// failure's backoff, and withdraws its runner once: GitHub must not route to a registration
// that has finished, and the withdrawal is the one part of a teardown a hold does not delay.
func (l *Listener) parkHeld(ctx context.Context, job dispatch.Job, lease *alloc.Lease, hold alloc.LeaseHold) {
	l.mu.Lock()

	if l.cleanup == nil {
		l.cleanup = make(map[int64]*pendingCleanup)
	}

	entry, ok := l.cleanup[job.RequestID]
	if !ok {
		entry = &pendingCleanup{}
		l.cleanup[job.RequestID] = entry
	}

	// A LATER DELIVERY OWNS THE OBLIGATION: a pass that began before it arrived must not put this one
	// back, or settling it later retires the wrong delivery.
	if entry.job.CompletionID <= job.CompletionID {
		entry.job = job
	}

	entry.held = true
	entry.wait, entry.at = 0, time.Time{}
	// A COMPLETION PARKED FOR THE CLEANUP LOOP WAS WITHDRAWN AS IT WAS PARKED, and this is the same
	// flag: the destroy that follows the hold then withdraws nothing again.
	withdrawn := entry.deregistered && entry.job.CompletionID == job.CompletionID
	l.mu.Unlock()

	if withdrawn {
		return
	}

	l.log.Info("a failed job's VM is held, so its capacity stays charged until the hold ends",
		"tier", l.tier, "lease", lease.ID, "until", hold.Until)

	done, err := l.deregisterCompleted(ctx, job, lease)
	if err != nil {
		l.log.Warn("could not withdraw a held job's runner; trying again on the next pass",
			"tier", l.tier, "lease", lease.ID, "error", err)

		return
	}

	if done {
		l.mu.Lock()
		entry.deregistered = true
		l.mu.Unlock()
	}
}

// restoreHolds marks the restored completions a hold is keeping, before any pass has looked: a process
// told to stop right after it started must leave them alone, as the one before it did. WHAT CANNOT BE
// READ IS LEFT ALONE TOO, since a shutdown that leaves one costs the next process a look, and the
// completion is durable; the pass that decides reads the row again.
//
// ITS LEASE IS NOT TAKEN BACK HERE: exactly one party renews a lease, and after a restart that is the
// node, whose registration adopts what survived.
func (l *Listener) restoreHolds(ctx context.Context, completions []state.PendingCompletion) {
	if l.alloc == nil {
		return
	}

	for i := range completions {
		completion := &completions[i]
		if completion.Retired || completion.LeaseID == "" {
			continue
		}

		hold, err := l.alloc.LeaseHoldOf(ctx, completion.LeaseID)
		if errors.Is(err, alloc.ErrLeaseNotFound) || (err == nil && !hold.Holding()) {
			continue
		}

		l.mu.Lock()
		if entry := l.cleanup[completion.RequestID]; entry != nil {
			entry.held = true
		}
		l.mu.Unlock()
	}
}

// notHeld says this completion's destroy is no longer waiting for a hold, so a shutdown owes it as it
// owes any other.
func (l *Listener) notHeld(requestID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if entry := l.cleanup[requestID]; entry != nil {
		entry.held = false
	}
}
