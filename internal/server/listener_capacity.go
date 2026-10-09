package server

import (
	"context"
	"fmt"
	"math"

	"github.com/junioryono/billet/internal/alloc"
)

// capacity is every lease this listener owns or has adopted, idle or not. Each
// came from the allocator, so the sum across listeners is bounded by the budget.
func (l *Listener) capacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.held) + len(l.releasing) + len(l.acquiring) + len(l.running) +
		len(l.adopted)
}

// idleEscrow is capacity this listener holds and nothing is using.
func (l *Listener) idleEscrow() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.held)
}

// committedCapacity is what this listener owes GitHub: work it has promised or
// is running, with idle escrow excluded. It is what a draining or sealed
// listener advertises, and the floor under steadyAdvertisement.
func (l *Listener) committedCapacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.releasing) + len(l.acquiring) + len(l.running) + len(l.adopted)
}

// steadyAdvertisement is what this tier tells GitHub it can run at once: its
// configured ceiling, never less than the work it already has, and never more
// than a configured maxCapacity (AdvertiseNothing sets that to zero).
//
// UNBACKED, DELIBERATELY, and this reverses the rule this listener was built on.
// Billet used to advertise only what it had already escrowed, so a tier that
// held nothing advertised zero — and GitHub.com assigns work only to a scale set
// that is advertising, and tells one advertising zero nothing at all. Holding
// one escrow per tier forever wasted most of a fleet (#116); passing one escrow
// between tiers left each advertising for one poll in eighteen minutes, and a
// job queued against an idle tier went unassigned for over four hours (#140).
//
// So every tier advertises what it could run, all the time, and the escrow is
// bought when a runner is launched instead (reconcilePool). A job GitHub assigns
// beyond what can run now waits, assigned, until a lease can be bought for it;
// the allocator's atomic purchase is what stops an overcommit, exactly as it
// always was.
//
// COULD-NOT-TELL ADVERTISES ONLY WHAT IS COMMITTED: a ceiling that cannot be
// read is not a licence to claim capacity.
func (l *Listener) steadyAdvertisement() int {
	advertised := l.committedCapacity()

	if l.alloc != nil {
		ceiling, err := l.alloc.AdvertisedCeiling(l.tier)
		if err != nil {
			l.log.Error("could not read this tier's advertised ceiling; advertising only the "+
				"work it already has", "tier", l.tier, "error", err)
		} else {
			advertised = max(advertised, ceiling)
		}
	}

	if l.maxCapacity != nil {
		advertised = min(advertised, *l.maxCapacity)
	}

	return max(advertised, 0)
}

// targetCapacity is the escrow this listener should hold with nothing in hand:
// exactly what is committed. There is no idle escrow to keep; see
// steadyAdvertisement.
func (l *Listener) targetCapacity() int {
	return l.targetCapacityFor(0)
}

// targetCapacityFor is the escrow needed to back what is committed plus
// `offered` more acquisitions, which is only ever the offer path's own need.
// Work GitHub has already assigned is backed when its runner is launched, not
// in advance.
func (l *Listener) targetCapacityFor(offered int) int {
	l.mu.Lock()
	active := len(l.acquiring) + len(l.running) + len(l.adopted)
	l.mu.Unlock()

	target := math.MaxInt
	if offered <= math.MaxInt-active {
		target = active + offered
	}
	if l.maxCapacity != nil && target > *l.maxCapacity {
		target = *l.maxCapacity
	}

	return max(target, 0)
}

// messageCapacityTarget is what the returned response can consume before any
// blocking acquisition, launch, or reconciliation begins. Available and
// Assigned are added because a direct assignment need not have appeared in this
// process's offer set; over-counting a job present in both is bounded by the
// message and safer than releasing its backing.
func (l *Listener) messageCapacityTarget(msg *Message) int {
	work := len(msg.Available)
	if len(msg.Assigned) > math.MaxInt-work {
		work = math.MaxInt
	} else {
		work += len(msg.Assigned)
	}

	return l.targetCapacityFor(work)
}

func (l *Listener) refreshAdoptedCapacity(ctx context.Context) error {
	ids, err := l.alloc.ServiceableRunnerLeaseIDs(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: refresh adopted capacity for %s: %w", l.tier, err)
	}
	serviceable := make(map[string]bool, len(ids))
	for _, id := range ids {
		serviceable[id] = true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	managed := make(map[string]bool, len(l.running))
	for _, lease := range l.running {
		managed[lease.ID] = true
	}
	for id := range l.adopted {
		if !serviceable[id] || managed[id] {
			delete(l.adopted, id)
		}
	}
	for id := range serviceable {
		if !managed[id] {
			l.adopted[id] = true
		}
	}

	return nil
}

// Held returns the leases this listener has escrowed and not yet handed to a job.
//
// Exported for tests, which need lease IDENTITY rather than a count: an escrow
// that was lost and rebuilt has the same size and different ids.
func (l *Listener) Held() []*alloc.Lease {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]*alloc.Lease(nil), l.held...)
}

// Acquiring reports how many offers this listener has escrow promised to and has
// not yet been assigned. Exported for tests, which cannot read the guarded field
// safely.
func (l *Listener) Acquiring() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.acquiring)
}

// Running reports how many jobs this listener currently has leases for. Exported
// for tests, which cannot read the guarded field safely.
func (l *Listener) Running() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.running)
}

// refusalReason names which of the two states declined an offer, because they
// mean different things to whoever reads the log: a drain ends with this process
// stopping, a seal ends when an operator resumes.
func refusalReason(draining bool) string {
	if draining {
		return "draining"
	}

	return "admission sealed"
}

// markAdmission brings this listener into line with the deployment's admission
// state, and reports whether it is now sealed. It CHANGES NO CAPACITY — handing
// escrow back is handBackIdleEscrow's job, and the split is the invariant.
//
// MARK FIRST, RELEASE SECOND, and this ordering is DEFENSIVE rather than load
// bearing today — said plainly because the mutation survives. Reversing the two
// leaves a window where the capacity is gone but the flag is not yet set, and an
// offer accepted in it takes the escrow straight back so the quiesce never
// converges. Nothing can reach that window while handle runs on the poll loop's
// own goroutine, which is what actually prevents it. Keep the order anyway: it
// costs nothing, and the thing that makes it safe is an incidental property of
// where handle is called from, not a rule anybody restated when moving it.
//
// AN UNREADABLE STATE COUNTS AS SEALED, consistently with the ledger: escrow is
// already refused when admission cannot be read, so a listener that kept
// accepting offers would be claiming work it cannot back. The cost is a poll
// spent not accepting, which the next one recovers; the alternative is admitting
// work into a deployment somebody sealed.
//
// IT NEVER RETURNS AN ERROR, and that is deliberate rather than lazy. An error
// out of this loop stops the listener, one listener stopping cancels every
// other, and their teardown destroys the compute they hold. A transient database
// blip must not be able to do that.
func (l *Listener) markAdmission(ctx context.Context) (bool, bool) {
	if l.alloc == nil {
		return false, true
	}

	admission, err := l.alloc.Admission(ctx)

	sealed, known := admission.Sealed(), err == nil
	if err != nil {
		// REFUSING IS FAIL-CLOSED; HANDING CAPACITY BACK IS NOT, and separating
		// the two is what this second return value is for. Declining offers on a
		// read that failed costs a poll and cannot admit work billet cannot back.
		// Handing the escrow back is an ACTION premised on knowing the deployment
		// is sealed — which is exactly what just failed to be established — and it
		// is not free: a listener that returns its escrow on a transient database
		// blip hands the gap to another tier and retakes it on the next poll,
		// which is the flapping the escrow exists to prevent.
		//
		// So an unreadable state quiesces and keeps what it holds.
		sealed = true

		l.log.Warn("could not read whether this deployment is taking new work; declining "+
			"offers until it can be read, and keeping the capacity already escrowed",
			"tier", l.tier, "error", err)
	}

	l.mu.Lock()
	was := l.quiesced
	l.quiesced = sealed
	l.mu.Unlock()

	// SAID ONLY WHEN IT IS KNOWN. "This deployment is no longer taking new work"
	// is a claim about the DEPLOYMENT, and a read that failed establishes nothing
	// about it — this listener's own caution is the only fact there is. Logging
	// it anyway is how "could not verify" becomes a verdict in somebody's journal.
	switch {
	case sealed && known && !was:
		l.log.Info("this deployment is no longer taking new work; handing back idle capacity "+
			"and letting what is running finish", "tier", l.tier)
	case !sealed && was:
		l.log.Info("this deployment is taking work again", "tier", l.tier)
	}

	return sealed, known
}

// handBackIdleEscrow returns capacity a sealed listener is holding and cannot
// use. It is separate from marking, and it runs AFTER any message already in
// hand has been handled.
//
// WHY IT WAITS FOR THE MESSAGE. GitHub can assign work this listener holds no
// in-memory promise for — after a restart, or on the direct-assignment path —
// and `assign` backs such a job from `held`. Releasing before `handle` empties
// `held`, so a seal landing during a long poll would decline a job GitHub had
// already assigned against capacity advertised before the seal. That is
// revoking a commitment already made, which is the one thing sealing must not
// do: it refuses NEW work, and an assignment in hand is not new work.
//
// Marking still happens first, so nothing can accept an OFFER out of that same
// message while this waits.
func (l *Listener) handBackIdleEscrow(ctx context.Context, sealed bool) {
	if !sealed || l.alloc == nil {
		return
	}

	if released := l.releaseIdleEscrow(ctx); released > 0 {
		l.log.Info("handed back idle capacity", "tier", l.tier, "leases", released)
	}
}
