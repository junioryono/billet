package server

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
)

// reconcilePool matches a tier to GitHub's authoritative assigned-job count.
// Growth creates anonymous physical members because individual job entries are
// lifecycle data and may be truncated. Only idle members are selected for
// shrinkage; a busy member belongs to a job regardless of what any aggregate
// says while messages are in flight.
func (l *Listener) reconcilePool(ctx context.Context, desired int) error {
	if l.messageHeld() {
		return nil
	}
	if l.beforePoolReconcile != nil {
		l.beforePoolReconcile()
	}
	if l.alloc == nil || desired < 0 {
		return nil
	}
	runners, err := l.alloc.PoolRunners(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: read runner pool for %s reconciliation: %w", l.tier, err)
	}

	for i := range runners {
		if runners[i].Status == alloc.PoolRunnerRetiring {
			l.retirePoolMember(ctx, runners[i])
		}
	}

	runners, err = l.alloc.PoolRunners(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: refresh runner pool for %s reconciliation: %w", l.tier, err)
	}
	active, err := l.alloc.ActiveRunnerLeases(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: count active runner leases for %s reconciliation: %w", l.tier, err)
	}

	// READ ONCE, so every purchase in this loop and the record below judge the
	// tier as it stood at one instant.
	admission := l.admission(ctx)

	for active < desired {
		// BOUGHT ONE AT A TIME, LAUNCHED TOGETHER. Each purchase still asks the
		// order and the allocator on its own, exactly as before; what changes is
		// that up to poolLaunchBatch members bought in a row start at once rather
		// than each waiting for the last to boot. A tier launching one member at a
		// time started one runner every 30 to 65 seconds however much room was free
		// (2026-10-03, with 66 runners wanted on one tier).
		var batch []poolLaunch

		l.mu.Lock()
		l.batchBuying, l.batchBought = true, false
		l.mu.Unlock()

		for active < desired && len(batch) < max(l.poolLaunchBatch, 1) {
			// WHOSE TURN IT IS, asked before the purchase and not after: under
			// fair the room a finished job leaves belongs to the longest-waiting
			// tier until its shape fits, and a tier that bought first would have
			// taken it (admissionQueue).
			//
			// AND OUT OF TURN WHEN THAT TAKES NOTHING FROM THE WAITERS AHEAD: the
			// allocator buys only while each of them could still be granted one
			// lease of its own, in the purchase's transaction (#346).
			var protect []string
			if !l.order.mayBuy(l.tier, admission) {
				protect = l.order.ahead(l.tier, admission)
				if len(protect) == 0 {
					break
				}
			}

			err := l.backPoolSlotLeaving(ctx, protect)
			if err != nil {
				l.endBatchBuying()

				return errors.Join(err, l.launchPool(ctx, batch))
			}
			lease, job, err := l.assignPoolSlot(ctx)
			if err != nil {
				l.endBatchBuying()

				return errors.Join(err, l.launchPool(ctx, batch))
			}
			if lease == nil {
				break
			}
			batch = append(batch, poolLaunch{lease: lease, job: job})
			active++
		}

		l.endBatchBuying()

		if len(batch) == 0 {
			break
		}
		if err := l.launchPool(ctx, batch); err != nil {
			return err
		}
	}
	// WHAT THIS TIER STILL WANTS, recorded for the order. Demand that is met, or
	// that GitHub's count says has gone away, releases this tier's place rather
	// than holding the fleet behind work nobody is waiting for any more.
	l.mu.Lock()
	l.waitingFor = max(desired-active, 0)
	l.mu.Unlock()

	// A TIER THAT COULD NOT GROW WHATEVER ANYONE FREED DOES NOT HOLD THE LINE.
	// Its refusal is its own cap, a pin on a host that is gone, or a shape no
	// live host fits, and none of those is cured by another tier's job ending;
	// recorded as the longest waiter it would stop the whole fleet buying room
	// that is free (#157).
	//
	// READ AGAIN, AFTER THE PURCHASES: what this tier could grow to is judged as
	// it stands now, not as it stood before it bought. The tier that reached its
	// own cap in the loop above could still grow when the loop began, and
	// recording that answer is exactly the outage this fixes.
	if active < desired {
		admission = l.admission(ctx)
	}

	if active < desired && admission.CanGrow {
		l.order.waits(l.tier, admission)
	} else {
		l.order.served(l.tier)
	}

	surplus := active - desired
	if surplus <= 0 {
		l.retireOfflineMembers(ctx, runners)

		return nil
	}
	for i := range runners {
		if surplus == 0 {
			break
		}
		if runners[i].Status != alloc.PoolRunnerIdle {
			continue
		}
		if err := l.alloc.RetirePoolRunner(ctx, runners[i].LeaseID); err != nil {
			l.log.Error("could not claim an idle pool member for scale-down", "tier", l.tier,
				"runner", runners[i].RunnerName, "error", err)
			continue
		}
		runners[i].Status = alloc.PoolRunnerRetiring
		l.retirePoolMember(ctx, runners[i])
		surplus--
	}

	return nil
}

// poolLaunchBatch is how many pool members one pass starts at once on a tier
// served only by Firecracker, matching how many launches such a node runs at once.
const poolLaunchBatch = 4

// poolLaunch is one pool member bought and assigned, waiting to start.
type poolLaunch struct {
	lease *alloc.Lease
	job   dispatch.Job
}

// endBatchBuying closes a pool pass's purchases and, if it bought anything,
// re-dates the tier in the admission order once for the whole pass.
func (l *Listener) endBatchBuying() {
	l.mu.Lock()
	bought := l.batchBought
	l.batchBuying, l.batchBought = false, false
	l.mu.Unlock()

	if bought {
		l.order.bought(l.tier)
	}
}

// launchPool starts every member of batch at once and waits for all of them.
// Every launch is waited for even when one fails, because each has already
// been assigned its lease and has to settle it, success or not.
func (l *Listener) launchPool(ctx context.Context, batch []poolLaunch) error {
	if len(batch) == 1 {
		return l.launch(ctx, batch[0].lease, batch[0].job)
	}

	errs := make([]error, len(batch))

	var wg sync.WaitGroup
	for i := range batch {
		wg.Go(func() { errs[i] = l.launch(ctx, batch[i].lease, batch[i].job) })
	}
	wg.Wait()

	return errors.Join(errs...)
}

// backPoolSlot buys the one lease the next pool member needs, at the moment it
// is needed, when nothing is already set aside for it.
//
// THIS IS WHERE CAPACITY IS RESERVED NOW, instead of in advance of an
// advertisement (steadyAdvertisement). The purchase is the allocator's ordinary
// atomic Escrow, so the ceiling, per-node fit, floors, max_concurrent, macOS
// slots and the admission seal all still decide it; a full or sealed fleet buys
// nothing, and the assigned job waits at GitHub until a later reconciliation
// can. ONE AT A TIME, because the loop that calls this launches what it buys
// before asking for another, so a burst of assignments never holds more than
// one lease it has not yet started.
//
// NOT WHILE DRAINING. A drain waits for the work it has and takes no more, and
// the drain-time reconciliation still runs; buying here would start a runner
// after the drain began, which it could only then destroy.
func (l *Listener) backPoolSlot(ctx context.Context) error {
	return l.backPoolSlotLeaving(ctx, nil)
}

// backPoolSlotLeaving is backPoolSlot for a purchase ahead of this tier's turn,
// leaving room for each waiter in protect.
func (l *Listener) backPoolSlotLeaving(ctx context.Context, protect []string) error {
	if l.isDraining() {
		return nil
	}

	l.mu.Lock()
	ready := len(l.acquiring) > 0 || len(l.held) > 0
	l.mu.Unlock()

	if ready {
		return nil
	}

	return l.refillEscrowLeaving(ctx, l.capacity()+1, 1, protect)
}

// backAssignment buys one lease for an assignment this listener holds no promise
// for, when no idle lease is held to take it: GitHub can assign a request after a
// restart, or one a control plane that is gone acquired. A lease bought for a job
// whose promise sits under another alias is idle afterwards, and the release after
// the message hands it back. Not while draining, for backPoolSlot's reason.
func (l *Listener) backAssignment(ctx context.Context, requestID int64) error {
	if l.isDraining() {
		return nil
	}

	l.mu.Lock()
	_, promised := l.acquiring[requestID]
	_, running := l.running[requestID]
	ready := promised || running || len(l.held) > 0
	l.mu.Unlock()

	if ready {
		return nil
	}

	// THIS PATH ASKS THE ORDER TOO. On GitHub.com a job arrives as an assignment
	// rather than an offer, so a stream of small direct assignments took every
	// fragment of room the longest-waiting tier was accumulating and fairness
	// protected only the pool path (#157). The assignment is not acquired here:
	// billet declines it and GitHub reassigns it after its pickup deadline.
	if !l.order.mayBuy(l.tier, l.admission(ctx)) {
		return nil
	}

	return l.refillEscrowUngated(ctx, l.capacity()+1, 1)
}

// admission is what the order needs to know about this tier: whether room could
// ever reach it, and the hosts it competes for.
//
// A READ THAT FAILED IS NOT A WAY PAST THE ORDER, and it is not a refusal
// either. The tier is kept eligible, because a ledger that could not be read is
// no evidence that room can never reach it, and the queue decides who buys first
// rather than whether a purchase is safe (the allocator's atomic escrow does
// that). Its host set is unknown, which competes with every waiter, so such a
// purchase is still refused whenever another tier is ahead of it.
func (l *Listener) admission(ctx context.Context) alloc.TierAdmission {
	if l.alloc == nil || !l.order.gates() {
		return alloc.TierAdmission{CanGrow: true}
	}

	view, err := l.alloc.AdmissionView(ctx)
	if err != nil {
		l.log.Warn("could not read which tiers room could reach; admitting this purchase",
			"tier", l.tier, "error", err)

		return alloc.TierAdmission{CanGrow: true}
	}

	return view[l.tier]
}

func (l *Listener) activePoolMembers(ctx context.Context) (int, error) {
	active, err := l.alloc.ActiveRunnerLeases(ctx, l.tier)
	if err != nil {
		return 0, fmt.Errorf("server: read runner pool for %s assignment: %w", l.tier, err)
	}

	return active, nil
}

// assignPoolSlot turns one escrowed lease into a physical runner whose durable
// identity is the lease rather than one entry from GitHub's truncated message.
func (l *Listener) assignPoolSlot(ctx context.Context) (*alloc.Lease, dispatch.Job, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var (
		lease     *alloc.Lease
		promiseID int64
	)
	for id, promised := range l.acquiring {
		if lease == nil || promised.at.Before(l.acquiring[promiseID].at) ||
			(promised.at.Equal(l.acquiring[promiseID].at) && id < promiseID) {
			lease = promised.lease
			promiseID = id
		}
	}
	fromPromise := lease != nil
	if !fromPromise {
		if len(l.held) == 0 {
			return nil, dispatch.Job{}, nil
		}
		lease = l.held[0]
	}

	requestID, err := l.alloc.IdentifyPoolSlot(ctx, lease.ID)
	if err != nil {
		return nil, dispatch.Job{}, fmt.Errorf("server: identify pool slot for lease %s: %w", lease.ID, err)
	}
	if err := l.alloc.Assign(ctx, lease.ID, lease.Epoch, 0, requestID); err != nil {
		return nil, dispatch.Job{}, fmt.Errorf("server: assign pool slot lease %s: %w", lease.ID, err)
	}

	if fromPromise {
		delete(l.acquiring, promiseID)
	} else {
		l.held = l.held[1:]
	}
	delete(l.heldOrder, lease.ID)
	l.running[requestID] = lease

	return lease, dispatch.Job{RequestID: requestID}, nil
}

// retirePoolMember removes routing before compute, then returns its capacity.
// The retiring row is the crash-recovery journal: any failed phase is retried on
// the next scale-set message before another idle member is selected.
func (l *Listener) retirePoolMember(ctx context.Context, member alloc.PoolRunner) {
	lease, err := l.alloc.Lease(ctx, member.LeaseID)
	if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) {
		l.log.Error("could not read a retiring pool member's lease", "tier", l.tier,
			"runner", member.RunnerName, "lease", member.LeaseID, "error", err)
		return
	}
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		lease = nil
	}
	job := dispatch.Job{RequestID: member.LaunchRequestID, RunnerID: member.RunnerID,
		RunnerName: member.RunnerName}
	if err := l.destroyCompleted(ctx, job, lease, alloc.PhaseDone); err != nil {
		if errors.Is(err, dispatch.ErrCustody) {
			// KEPT UNTIL CUSTODY SETTLES. A recovery tombstone is the fence that
			// stops a delayed JobStarted recreating a busy binding after the node
			// was already told to tear this guest down. The next reconciliation
			// retries and forgets it only after custody no longer owns compute.
			l.dropPoolMember(member)
			return
		}
		l.log.Error("could not retire an idle pool member; its capacity stays held", "tier", l.tier,
			"runner", member.RunnerName, "error", err)
		return
	}
	if lease != nil {
		if err := l.releaseAbsent(ctx, member.LaunchRequestID, lease, alloc.PhaseDone, ""); !releaseSettled(err) {
			l.log.Error("could not release a retired pool member; its capacity stays held", "tier", l.tier,
				"runner", member.RunnerName, "lease", lease.ID, "error", err)
			return
		}
	}
	if err := l.alloc.ForgetPoolRunner(ctx, member.LeaseID); err != nil {
		l.log.Warn("a retired pool member's journal could not be removed", "tier", l.tier,
			"runner", member.RunnerName, "error", err)
		return
	}
	l.dropPoolMember(member)
}

func (l *Listener) dropPoolMember(member alloc.PoolRunner) {
	l.mu.Lock()
	delete(l.running, member.LaunchRequestID)
	delete(l.runningJobs, member.LaunchRequestID)
	delete(l.acquiring, member.LaunchRequestID)
	delete(l.confirmed, member.LeaseID)
	l.mu.Unlock()
}

// restorePoolLease reconnects restart-safe pool identity to the completion
// machinery, whose durable result record must carry the exact lease and epoch.
// Without this handoff a post-restart completion could destroy compute and then
// forget which capacity it had proved safe to release.
func (l *Listener) restorePoolLease(ctx context.Context, binding alloc.PoolRunner) error {
	lease, err := l.alloc.Lease(ctx, binding.LeaseID)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: cannot restore completed runner %q lease %s: %w",
			ErrUntrustworthySession, binding.RunnerName, binding.LeaseID, err)
	}
	if lease.Tier != l.tier || lease.RequestID != binding.LaunchRequestID {
		return fmt.Errorf("%w: pool runner %q lease %s identifies tier %q request %d, want tier %q request %d",
			ErrUntrustworthySession, binding.RunnerName, binding.LeaseID, lease.Tier,
			lease.RequestID, l.tier, binding.LaunchRequestID)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if prior := l.running[binding.LaunchRequestID]; prior != nil && prior.ID != lease.ID {
		return fmt.Errorf("%w: request %d is already bound to lease %s, not pool lease %s",
			ErrUntrustworthySession, binding.LaunchRequestID, prior.ID, lease.ID)
	}
	// This lease is now managed in running, so it is no longer adopted compute;
	// dropping it here avoids a one-iteration double-count before the next
	// refreshAdoptedCapacity would reconcile it.
	delete(l.adopted, lease.ID)
	l.running[binding.LaunchRequestID] = lease

	return nil
}
