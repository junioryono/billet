package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// A CONTROLLER THAT HAS BEEN REPLACED TEARS DOWN WITHOUT ACTING ON ANYTHING.
//
// Every step of an ordinary teardown is an authoritative act — destroying the
// compute a completion asked for, closing this deployment's message session,
// handing capacity back — and a process that has stopped being the controller
// has the right to none of them. The successor is already running and performs
// each one correctly: it holds the same leases, GitHub expires the session this
// one abandons, and its startup reap reclaims the escrow once the heartbeats
// stop. So a fenced stop is deliberately a hard kill, which is a recovery billet
// already implements.
//
// BOTH DIRECTIONS, IN ONE TABLE, AND THAT IS THE POINT. A test for the fenced
// half alone passes just as well against a teardown that has silently become
// destructive-never — which would strand a finished job's container on every
// ordinary restart, and leave a message session open that GitHub then makes the
// next control plane wait out. The two rows differ in one boolean and must
// produce opposite behaviour.
func TestOnlyAFencedTeardownActsOnNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fenced is the answer WithLeadershipLostCheck gives.
		fenced bool
		// wantDestroyed is whether the destroy request 9's completion asked for
		// still happens, and wantClosed whether the session is closed.
		wantDestroyed bool
		wantClosed    bool
	}{
		{
			name:          "an ordinary shutdown finishes what it owes",
			fenced:        false,
			wantDestroyed: true,
			wantClosed:    true,
		},
		{
			name:          "a replaced controller does nothing at all",
			fenced:        true,
			wantDestroyed: false,
			wantClosed:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tiers := []config.Tier{tier("billet-4vcpu-a")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
				alloc.WithLeaseTTL(outlivesTheDrain))

			var (
				mu        sync.Mutex
				destroyed []int64
			)

			runner := &fakeRunner{onDestroy: func(id int64) error {
				mu.Lock()
				destroyed = append(destroyed, id)
				mu.Unlock()

				return nil
			}}

			session := &fakeSession{}

			l := NewListener(a, tiers[0].Label, session, WithRunner(runner),
				WithDrainGrace(notDrainingHere), stopsWithoutWaiting(),
				WithLeadershipLostCheck(func() bool { return tc.fenced }))

			// A RUNNING JOB AND A DESTROY ITS COMPLETION ALREADY ASKED FOR. The
			// running one is what neither teardown may touch; the finished one is
			// what separates them.
			holdRunning(t, l, a, tiers[0].Label, 7)

			l.mu.Lock()
			l.cleanup = map[int64]*pendingCleanup{
				9: {job: Job{RequestID: 9}, at: time.Now().Add(time.Hour)},
			}
			l.mu.Unlock()

			before, err := a.Usage(t.Context())
			if err != nil {
				t.Fatalf("Usage: %v", err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancelDeadline()

			run := startRun(ctx, l)
			cancel()

			awaitRun(deadline, t, run)

			mu.Lock()
			got := append([]int64(nil), destroyed...)
			mu.Unlock()

			var tore9, tore7 bool

			for _, id := range got {
				switch id {
				case 9:
					tore9 = true
				case 7:
					tore7 = true
				}
			}

			if tore9 != tc.wantDestroyed {
				t.Errorf("the destroy request 9's completion asked for happened=%v, want %v. "+
					"Destroyed: %v", tore9, tc.wantDestroyed, got)
			}

			// NEITHER SIDE MAY EVER TOUCH RUNNING WORK, so this is asserted in both
			// rows rather than only in the fenced one. GitHub does not requeue a job
			// whose runner vanished after it started.
			if tore7 {
				t.Errorf("request 7 was still running and was destroyed. Destroyed: %v", got)
			}

			if closed := session.closes() > 0; closed != tc.wantClosed {
				t.Errorf("the message session was closed=%v, want %v", closed, tc.wantClosed)
			}

			// AND THE CAPACITY IS STILL CHARGED EITHER WAY. A stop leaves running work
			// alive with its lease held, because freeing a slot whose compute is live
			// is the overcommit the escrow ordering exists to prevent — and a fenced
			// controller could not have written the release in any case.
			after, err := a.Usage(t.Context())
			if err != nil {
				t.Fatalf("Usage after the teardown: %v", err)
			}

			if after.Leases != before.Leases || after.VCPU != before.VCPU {
				t.Errorf("the teardown changed usage from %+v to %+v", before, after)
			}
		})
	}
}

// A DUE CLEANUP RETRY STARTS NOTHING ONCE THIS PROCESS HAS BEEN REPLACED.
//
// THE TEARDOWN IS NOT THE ONLY THING THAT REACHES A HOST. Destroy talks to a
// node rather than to the ledger, so nothing about the fence reaches it: the
// refusal that fences this process stops its writes and leaves the cleanup loop
// ticking on its own clock. That loop tearing down compute whose lease the
// successor now holds is exactly the "acts on nothing" claim failing, and it
// fails on a path the teardown test cannot see.
//
// retryCleanup IS DRIVEN DIRECTLY, because what has to be observed is a retry
// that is DUE — a loop-level test would pass whenever the tick simply had not
// arrived yet, which is the wait-satisfied-by-something-else trap.
func TestAReplacedControllerStartsNoCleanupDestroy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fenced      bool
		wantDestroy bool
	}{
		{name: "an ordinary controller finishes what it owes", fenced: false, wantDestroy: true},
		{name: "a replaced controller starts nothing", fenced: true, wantDestroy: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tiers := []config.Tier{tier("billet-4vcpu-a")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
				alloc.WithLeaseTTL(outlivesTheDrain))

			var (
				mu        sync.Mutex
				destroyed []int64
			)

			runner := &fakeRunner{onDestroy: func(id int64) error {
				mu.Lock()
				destroyed = append(destroyed, id)
				mu.Unlock()

				return nil
			}}

			l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
				WithLeadershipLostCheck(func() bool { return tc.fenced }))

			// DUE NOW, not in an hour: the whole question is what happens to a
			// retry the loop would actually run.
			l.mu.Lock()
			l.cleanup = map[int64]*pendingCleanup{
				9: {job: Job{RequestID: 9}, at: time.Now().Add(-time.Minute)},
			}
			l.mu.Unlock()

			l.retryCleanup(t.Context())

			mu.Lock()
			got := append([]int64(nil), destroyed...)
			mu.Unlock()

			if destroyedIt := len(got) > 0; destroyedIt != tc.wantDestroy {
				t.Errorf("the due cleanup retry destroyed=%v, want %v. Destroyed: %v",
					destroyedIt, tc.wantDestroy, got)
			}
		})
	}
}

// THE CONTROL PLANE FORWARDS THE CHECK TO ITS LISTENERS.
//
// PROVING THE MECHANISM IS NOT PROVING IT IS USED. Every test above builds a
// listener with the option in hand, so deleting the one line in listenerOpts
// that forwards it would leave all of them green and leave a real control plane
// destroying compute on its way out of a deployment it no longer owns.
func TestTheControlPlaneForwardsTheLeadershipCheckToItsListeners(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	s := New(a, nil, tiers, "test-owner", nil,
		WithLeadershipLost(func() bool { return true }))

	if l := NewListener(a, tiers[0].Label, &fakeSession{}, s.listenerOpts(s.prov)...); !l.fenced() {
		t.Error("a listener built by a control plane that was given a leadership check " +
			"does not carry it")
	}

	// AND THE OTHER DIRECTION, or a listenerOpts that hard-coded a fenced listener
	// would satisfy the assertion above and break every ordinary shutdown.
	plain := New(a, nil, tiers, "test-owner", nil)

	if l := NewListener(a, tiers[0].Label, &fakeSession{}, plain.listenerOpts(plain.prov)...); l.fenced() {
		t.Error("a control plane that was given no leadership check produced a fenced listener")
	}
}

// A LISTENER WITH NO LEADERSHIP CHECK TEARS DOWN NORMALLY.
//
// The predicate is nil everywhere but the control plane — internal/e2e, every
// test that builds a listener directly, and any embedder — so a nil that read as
// "fenced" would turn every one of those into a teardown that destroys nothing
// and closes no session, silently.
func TestAListenerWithNoLeadershipCheckStillTearsDown(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	session := &fakeSession{}

	l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{}),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	if l.fenced() {
		t.Fatal("a listener with no leadership check must not read as fenced")
	}

	ctx, cancel := context.WithCancel(t.Context())
	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	run := startRun(ctx, l)
	cancel()

	awaitRun(deadline, t, run)

	if session.closes() == 0 {
		t.Error("a listener with no leadership check must still close its session")
	}
}
