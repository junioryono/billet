package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// A HEARTBEAT PASS MUST ARRIVE WITH ITS BUDGET INTACT, because a pass that
// spends its whole deadline waiting for billet's own mutex asks the allocator
// NOTHING and then reads its own silence as the allocator's.
//
// The consequence is not a missed beat. `renew` is deliberately three-valued —
// owned, lost, and no-answer — and a no-answer older than the lease's TTL
// escalates to `renewalStale`, which drops the lease out of `running` and parks
// it in the cleanup set for a destroy. That escalation is correct when the
// ALLOCATOR is unreachable; here nothing was ever asked, and the "unreachable
// allocator" is a lock this process is holding. So an ordinary stall — `assign`
// and the completion paths hold this mutex across allocator writes — can
// manufacture the conclusion that a healthy running job's lease may already have
// been reaped, and somebody's build is then torn down for it.
//
// It is the same could-not-tell/no collapse this codebase refuses everywhere
// else, wearing the clothes of a timeout: `Heartbeat` returning the pass's own
// expired context is not the allocator declining to confirm the lease.
//
// THE STALL IS ESTABLISHED, NOT ASSUMED, AND THE SEAM IS THE LOCK ITSELF.
// The first version started the loop and slept holding the mutex, which proves
// nothing about where the loop was — a tick landing after the sleep let the
// wrong ordering build a fresh deadline and pass. A hook merely placed above the
// lock is no better: it proves it ran, and the goroutine can be descheduled
// between it and the deadline. Standing IN the acquisition proves that
// everything the pass sequences BEFORE the lock has already happened, which
// under the wrong ordering is the deadline itself — so the stall that follows
// burns a budget that already exists, and the mutant fails because of what the
// code did rather than because of how it was scheduled.
func TestAHeartbeatPassIsNotSpentWaitingForBilletsOwnLock(t *testing.T) {
	t.Parallel()

	// The interval is a third of this, and that interval is the budget one real
	// SQLite renewal has to complete in after the stall — so it is sized to be
	// generous under `-race` on a loaded machine rather than as small as the
	// mechanism allows. A tighter TTL makes the CORRECT implementation fail here
	// for reasons that have nothing to do with the ordering under test.
	const ttl = 900 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	lease := holdRunning(t, l, a, tiers[0].Label, 7)

	// One good pass, so there is a confirmed renewal for the staleness clock to
	// measure from — exactly as a launched job has.
	l.mu.Lock()
	l.heartbeatHeld(t.Context())
	l.mu.Unlock()

	// The gate IS the lock, for one pass. Reaching it proves the pass is at the
	// lock boundary; holding it there is the stall.
	var (
		reached = make(chan struct{}, 1)
		open    = make(chan struct{})
		once    sync.Once
	)

	l.heartbeatLock = func() {
		once.Do(func() {
			reached <- struct{}{}
			<-open
		})
	}

	ctx, cancel := context.WithCancel(t.Context())

	var loop sync.WaitGroup

	loop.Add(1)

	go func() {
		defer loop.Done()

		l.heartbeatLoop(ctx)
	}()

	// A goroutine outliving its test is on this project's own list of ways a
	// test lies, so the loop is joined rather than abandoned.
	t.Cleanup(func() {
		cancel()
		loop.Wait()
	})

	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		close(open)
		t.Fatal("no heartbeat pass ever reached the lock")
	}

	// THE STALL. The pass is held at the lock boundary for longer than the TTL,
	// so that when it proceeds, the last confirmation is old enough for the stale
	// branch to be reachable — and, under the wrong ordering, its deadline has
	// already been spent.
	time.Sleep(2 * ttl)
	close(open)

	deadline := time.Now().Add(30 * time.Second)

	for {
		l.mu.Lock()
		_, running := l.running[7]
		confirmed, seen := l.confirmed[lease.ID]
		l.mu.Unlock()

		if !running {
			t.Fatalf("a healthy running job was dropped from the fleet's books because a "+
				"heartbeat pass spent its whole budget waiting for billet's own mutex; "+
				"nothing was ever asked of the allocator (lease %s, confirmed=%v)",
				lease.ID, seen)
		}

		// A pass that ASKED updates the confirmation, and only a successful
		// Heartbeat writes it. That is the positive signal: without it, "still
		// running" would also be satisfied by a loop that never ran at all.
		if seen && confirmed.After(time.Now().Add(-ttl)) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("no heartbeat pass confirmed the lease after the stall ended")
		}

		time.Sleep(5 * time.Millisecond)
	}
}
