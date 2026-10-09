package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
)

// heartbeatLoop renews this listener's leases until the context ends.
//
// The interval is a fraction of the TTL so a single missed beat — a busy
// database, a slow write — does not expire anything.
func (l *Listener) heartbeatLoop(ctx context.Context) {
	// AN OVERRUN THAT FIRED IS JOINED BEFORE THE LOOP RETURNS, so a report under
	// way as the plane stops reaches whoever it tells before they stop too.
	var overruns sync.WaitGroup
	defer overruns.Wait()

	ticker := time.NewTicker(l.heartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// ARMED BEFORE THE PASS AND STOPPED AFTER IT, so an overrun is told
			// while the pass is still stuck, lock wait included, rather than once
			// it has ended and the evidence with it.
			disarm := l.armOverrun(&overruns)

			l.heartbeatPass(ctx)

			disarm()
		}
	}
}

// armOverrun starts the timer that reports this pass as overrun, counting it in
// told until it is either stopped before it fired or has finished telling. It
// returns the disarm, which the loop calls once the pass has ended.
func (l *Listener) armOverrun(told *sync.WaitGroup) func() {
	if l.heartbeatOverrun == nil {
		return func() {}
	}

	told.Add(1)

	timer := time.AfterFunc(l.heartbeatInterval(), func() {
		defer told.Done()

		l.heartbeatOverrun()
	})

	return func() {
		if timer.Stop() {
			told.Done()
		}
	}
}

// heartbeatPass renews everything this listener holds, once.
//
// EACH PASS IS BOUNDED so that the allocator calls made under l.mu cannot run
// on unboundedly. The loop's own context is already detached from Run's caller,
// so cancellation will not end them; what does is this deadline, which the
// allocator's context-aware SQLite operations honour. Without it a slow pass
// holds l.mu, and the teardown blocks on that mutex behind the defer that would
// have stopped the loop.
//
// THE BUDGET STARTS AFTER THE LOCK, AND THAT ORDER IS THE WHOLE POINT. Started
// at the tick, a pass spends its deadline WAITING FOR BILLET'S OWN MUTEX and
// then asks the allocator nothing — after which `renew` reads that silence as
// the allocator's rather than as its own. Past the TTL that is `renewalStale`,
// which drops a lease out of `running` and parks it in the cleanup set for a
// destroy: an ordinary stall — `assign` and the completion paths hold this mutex
// across allocator writes — manufacturing the conclusion that a healthy job's
// lease may already have been reaped, and tearing down somebody's build for it.
// Bounding the ALLOCATOR CALLS is what the deadline is for, and they do not
// begin until the lock is held.
//
// A function rather than the loop body so both the cancellation and the unlock
// are deferred: a panic in heartbeatHeld would otherwise leave the mutex held.
func (l *Listener) heartbeatPass(ctx context.Context) {
	l.lockForHeartbeat()

	pass, endPass := context.WithTimeout(ctx, l.heartbeatInterval())

	// REGISTERED IN THIS ORDER so they run in the other one: the mutex is
	// released before the pass's timer is stopped, rather than held across it.
	defer endPass()
	defer l.mu.Unlock()

	l.heartbeatHeld(pass)
}

// lockForHeartbeat takes the escrow mutex for one heartbeat pass.
//
// An indirection only so a test can stand exactly at the lock boundary. THE GATE
// DOES NOT REPLACE THE ACQUISITION: production takes l.mu here whether or not a
// gate is installed, so no callback can leave this returning without the mutex,
// take it twice, or release one it does not hold. See the heartbeatLock field
// for why the seam has to be at the lock and not above it.
func (l *Listener) lockForHeartbeat() {
	if l.heartbeatLock != nil {
		l.heartbeatLock()
	}

	l.mu.Lock()
}

// heartbeatInterval is how often held capacity is renewed: a third of the
// allocator's ACTUAL TTL, so two consecutive failures are survivable. Read from the
// ALLOCATOR, because with a shorter configured TTL a cadence derived from the
// default lets every lease expire between beats.
func (l *Listener) heartbeatInterval() time.Duration {
	if ttl := l.alloc.LeaseTTL(); ttl > 0 {
		return ttl / 3
	}

	return alloc.DefaultLeaseTTL / 3
}

// renewal is what a heartbeat established about one lease.
//
// FOUR OUTCOMES, NOT TWO. "Not renewable" would cover two different facts — the
// allocator SAYING the lease is not ours, and the allocator not answering at all
// — and only the first is evidence about who owns the compute.
type renewal int

const (
	// renewalOwned: the allocator confirmed the lease is still this listener's.
	renewalOwned renewal = iota
	// renewalLost: the allocator says it is not — fenced, or gone from the ledger.
	renewalLost
	// renewalUnknown: no answer, and not for long enough to matter yet.
	renewalUnknown
	// renewalStale: no answer for longer than a lease can survive, so the reaper
	// may already have taken it. Not evidence that it is lost, but no longer a
	// reason to advertise it.
	renewalStale
)

// advertisable reports whether a lease with this outcome may still be counted as
// capacity.
func (r renewal) advertisable() bool {
	return r == renewalOwned || r == renewalUnknown
}

// heartbeatHeld renews the leases this listener is advertising, and drops any it has
// lost.
//
// This is what makes the reaper safe to run at all: a lease expires after 90 seconds
// without a heartbeat while a long poll blocks for about 50, so escrow held across
// two polls would be reclaimed underneath a listener still advertising it.
//
// A lease that cannot be renewed is DROPPED rather than retried — failure means the
// allocator no longer agrees this listener owns it.
func (l *Listener) heartbeatHeld(ctx context.Context) {
	kept := l.held[:0]

	for _, lease := range l.held {
		// Escrow only: nothing has been launched against it, so dropping one owes
		// nobody anything. The ledger entry is left to the reaper.
		if l.renew(ctx, lease).advertisable() {
			kept = append(kept, lease)
		} else {
			delete(l.heldOrder, lease.ID)
		}
	}

	l.held = kept

	// A surplus release may wait behind another SQLite writer. It remains owned
	// and counted until Release confirms otherwise, so it needs the same heartbeat
	// protection as ordinary idle escrow while that call is in flight.
	for id, lease := range l.releasing {
		if l.renew(ctx, lease).advertisable() {
			continue
		}

		delete(l.releasing, id)
		delete(l.confirmed, id)
		delete(l.heldOrder, id)
	}

	// RUNNING and ACQUIRING leases are renewed too. They are open in the ledger
	// exactly like held ones, so a lease whose job is in flight — or whose job
	// billet has promised to run — expires just as readily, and its capacity would
	// then be escrowed by another tier while GitHub still believes this scale set
	// has the job.
	for id, lease := range l.running {
		if l.renew(ctx, lease).advertisable() {
			continue
		}

		// THE LEASE GOES; THE OBLIGATION DOES NOT. This listener launched a container and
		// losing the ledger entry does not stop it running; GitHub will not send the
		// completion again. So the entry moves to the cleanup set, where only a successful
		// destroy discharges it — deleting it leaves the container reachable by nothing but
		// an optional Sweeper.
		delete(l.running, id)
		delete(l.runningJobs, id)
		delete(l.confirmed, lease.ID)

		if _, pending := l.cleanup[id]; !pending {
			l.cleanup[id] = &pendingCleanup{job: dispatch.Job{RequestID: id}}
		}
	}

	for id, p := range l.acquiring {
		// REPORTED, not reclaimed. Billet acquired this job and owes GitHub a
		// runner for it; releasing the escrow on a timer would not hand the work
		// back, because there is no way to hand it back. What it would do is let
		// another tier take the slot and leave the eventual assignment with
		// nothing behind it.
		//
		// So the lease is renewed like any other and the operator is told. The
		// capacity is genuinely still owed; the thing that resolves it is the
		// session ending, which releases every promise with it.
		if !p.reported && time.Since(p.at) > l.stalePromise {
			p.reported = true

			l.log.Warn("an acquired job has gone unassigned for a long time; its escrow is "+
				"still held because billet owes github a runner for it",
				"tier", l.tier, "request", id, "waited", time.Since(p.at).Round(time.Second))
		}

		// NOTHING WAS LAUNCHED for a promise, so unlike a running lease there is no
		// compute to owe anyone. The acquisition billet made to GitHub cannot be
		// honoured without capacity, and no local record makes it honourable.
		if !l.renew(ctx, p.lease).advertisable() {
			delete(l.acquiring, id)
			delete(l.confirmed, p.lease.ID)
			delete(l.heldOrder, p.lease.ID)
		}
	}

	l.pruneConfirmed()
}

// pruneConfirmed drops renewal timestamps for leases this listener no longer
// holds.
//
// REBUILT FROM THE LIVE SETS rather than deleted at each departure. A lease
// leaves by many routes — completion, release, fencing, a reap, a failed launch,
// the shutdown drain — and a delete on each is a list that silently goes stale
// the next time a route is added. This map is bounded by what the listener
// actually holds, which the capacity budget already bounds, so one sweep per
// heartbeat costs nothing and cannot be forgotten.
func (l *Listener) pruneConfirmed() {
	if len(l.confirmed) == 0 {
		return
	}

	live := make(map[string]struct{},
		len(l.held)+len(l.releasing)+len(l.running)+len(l.acquiring))

	for _, lease := range l.held {
		live[lease.ID] = struct{}{}
	}
	for id := range l.releasing {
		live[id] = struct{}{}
	}

	for _, lease := range l.running {
		live[lease.ID] = struct{}{}
	}

	for _, p := range l.acquiring {
		live[p.lease.ID] = struct{}{}
	}

	for id := range l.confirmed {
		if _, ok := live[id]; !ok {
			delete(l.confirmed, id)
		}
	}
}

// renew heartbeats one lease and reports whether it is still this listener's.
func (l *Listener) renew(ctx context.Context, lease *alloc.Lease) renewal {
	err := l.alloc.Heartbeat(ctx, lease.ID, lease.Epoch)
	if err == nil {
		l.confirmed[lease.ID] = time.Now()

		return renewalOwned
	}

	// AN ANSWER OUTRANKS A DEADLINE. The allocator can say "this lease is not
	// yours" and have that answer arrive a moment after the pass deadline expired;
	// checking ctx.Err() first would discard it and keep advertising a lease
	// somebody else now holds. A context error explains why there is no answer, so
	// it may only be consulted when there is none.
	//
	// DEFENSIVE, and honestly labelled as such: no test drives it, because with
	// the real allocator a cancelled context fails inside the driver before any
	// query runs, so ErrFenced and a dead context cannot be produced together on
	// demand. The interleaving that reaches it — cancellation landing between a
	// successful query and this check — is real but not schedulable from a test.
	// The ordering costs nothing and the alternative is a known-wrong precedence.
	if errors.Is(err, alloc.ErrFenced) || errors.Is(err, alloc.ErrLeaseNotFound) {
		l.log.Warn("lost an escrowed lease; no longer advertising it",
			"tier", l.tier, "lease", lease.ID, "error", err)

		delete(l.confirmed, lease.ID)

		return renewalLost
	}

	// NO ANSWER. Shutting down, the pass ran out of its own deadline, or the database
	// is busy — the allocator never said this lease was not ours, so it is kept.
	// Dropping it would remove it from the release path too, and the ledger would keep
	// counting it until the reaper got it back.
	//
	// BUT NOT FOREVER. "No evidence it is lost" stops being a reason once the TTL has
	// passed without a single confirmed renewal: by then the reaper can have taken it,
	// and advertising capacity that is now someone else's is the exact double-admission
	// the escrow exists to prevent.
	//
	// The clock starts when the lease is TRACKED, not when the first renewal fails —
	// otherwise a never-confirmed lease gets an extra TTL it never earned. Every entry
	// point seeds `confirmed`, so a missing entry means the lease is not one of ours to
	// renew.
	last, seen := l.confirmed[lease.ID]
	if !seen {
		l.log.Warn("renewing a lease this listener never recorded; treating it as unknown",
			"tier", l.tier, "lease", lease.ID)

		l.confirmed[lease.ID] = time.Now()
	}

	if seen && time.Since(last) > l.alloc.LeaseTTL() {
		l.log.Error("could not renew an escrowed lease for longer than its TTL; it is no "+
			"longer being advertised, because the reaper may already have reclaimed it",
			"tier", l.tier, "lease", lease.ID, "since", time.Since(last).Round(time.Second),
			"error", err)

		delete(l.confirmed, lease.ID)

		return renewalStale
	}

	l.log.Warn("could not renew an escrowed lease; keeping it",
		"tier", l.tier, "lease", lease.ID, "error", err)

	return renewalUnknown
}
