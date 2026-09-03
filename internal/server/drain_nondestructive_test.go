package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// A SHUTDOWN LEAVES A RUNNING JOB ALONE, AND KEEPS ITS CAPACITY CHARGED.
//
// This is the whole of the drain contract in one place. billet stopping is not
// evidence that the work on its hosts should end: the guest keeps running, the
// node goes on holding it, and the next control plane re-adopts the lease through
// ServiceableRunnerLeaseIDs. Destroying it instead fails a build GitHub does not
// requeue — which is what every shutdown used to do once the drain ran out.
//
// THE CAPACITY STAYING CHARGED IS PART OF THE PROPERTY, not an oversight. Freeing
// a slot whose container is still on the host is the overcommit the whole
// ordering exists to prevent, so the lease is deliberately NOT released. Capacity
// the reaper reclaims late is recoverable; a failed build is not.
func TestAShutdownLeavesARunningJobAliveAndItsCapacityCharged(t *testing.T) {
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

	var dl drainLog

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting(), dl.option())

	lease := holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithCancel(t.Context())
	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	run := startRun(ctx, l)
	cancel()

	awaitRun(deadline, t, run)

	mu.Lock()
	got := append([]int64(nil), destroyed...)
	mu.Unlock()

	if len(got) != 0 {
		t.Errorf("the shutdown destroyed %v; a running job must survive a stop", got)
	}

	// THE LEASE IS STILL OPEN, which is what makes the compute accountable and
	// re-adoptable. A released one would let another tier escrow the slot that
	// container is still using.
	open, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the lease after shutdown: %v", err)
	}

	if open.Phase == alloc.PhaseDone || open.Phase == alloc.PhaseFailed {
		t.Errorf("the shutdown terminalised lease %s as %s; its container is still on the "+
			"host and its capacity must stay charged", open.ID, open.Phase)
	}

	// AND IT SAID SO, because an operator who finds capacity charged after a stop
	// needs to know billet left it that way on purpose.
	if !dl.saw("leaving a running job alone") {
		t.Errorf("the shutdown did not report that it left work running:\n%s", dl.String())
	}
}

// A SHUTDOWN STILL PERFORMS THE DESTROY A COMPLETION ASKED FOR.
//
// The other half, and the one that makes the first half safe to state. "Never
// destroy on shutdown" would be the wrong rule: a job GitHub has already
// concluded is owed its teardown, and skipping that strands the container
// forever. What must not be destroyed is work still executing.
//
// Both are staged here for the reason the hurried-drain test states: a fixture
// with only one kind of work cannot tell "tore down what it owed" from "tore
// down everything".
func TestAShutdownStillDestroysWhatACompletionAskedFor(t *testing.T) {
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
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	holdRunning(t, l, a, tiers[0].Label, 7)

	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{
		9: {job: Job{RequestID: 9}, at: time.Now().Add(time.Hour)},
	}
	l.mu.Unlock()

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

	if !tore9 {
		t.Errorf("the shutdown skipped the destroy request 9's completion asked for; "+
			"that container is stranded. Destroyed: %v", got)
	}

	if tore7 {
		t.Errorf("the shutdown destroyed request 7, which was still running. "+
			"Destroyed: %v", got)
	}
}

// THE DRAIN REPORTS ITSELF, REPEATEDLY, BECAUSE NOTHING ELSE WILL.
//
// drain_timeout stopped being a deadline, so the only thing left that can tell an
// operator their deployment has been draining for a day is billet saying so on a
// cadence. A threshold crossed silently is a fleet that looks wedged, and the
// operator's next move is to kill the process — which is the outcome the whole
// change exists to avoid.
func TestAnOverrunningDrainKeepsSayingSo(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	var dl drainLog

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithDrainGrace(time.Millisecond), dl.option())

	holdRunning(t, l, a, tiers[0].Label, 7)

	l.mu.Lock()
	l.draining = true
	l.drainStarted = time.Now().Add(-time.Hour)
	l.mu.Unlock()

	l.warnDrainOverrun()

	if !dl.saw("still draining") {
		t.Fatalf("an overrunning drain said nothing:\n%s", dl.String())
	}

	// AND IT DOES NOT REPEAT ON EVERY PASS. The drain loop calls this once per
	// poll, so an unthrottled warning writes a line every few seconds for as long
	// as the longest job runs — which is a log nobody reads, and therefore the
	// same as saying nothing.
	first := strings.Count(dl.String(), "still draining")

	l.warnDrainOverrun()

	if got := strings.Count(dl.String(), "still draining"); got != first {
		t.Errorf("the drain warned again immediately (%d lines, was %d); at one line per "+
			"poll a multi-day drain buries everything else", got, first)
	}

	// UNTIL THE INTERVAL HAS PASSED, and then it says so again — because an
	// operator who walks up an hour later must be able to see it is still going.
	l.mu.Lock()
	l.drainWarnedAt = time.Now().Add(-2 * drainWarnEvery)
	l.mu.Unlock()

	l.warnDrainOverrun()

	if got := strings.Count(dl.String(), "still draining"); got <= first {
		t.Errorf("the drain stopped reporting itself after the first line (%d), so a "+
			"drain running for days is indistinguishable from a wedged one", got)
	}
}

// AND IT SAYS NOTHING BEFORE THE THRESHOLD.
//
// Without this the test above passes against a warning that fires on every drain
// from the first instant, which would make the ordinary case — a drain that
// finishes in seconds — noisy enough that the real one goes unread.
func TestADrainInsideItsThresholdIsQuiet(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	var dl drainLog

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithDrainGrace(time.Hour), dl.option())

	l.mu.Lock()
	l.draining = true
	l.drainStarted = time.Now()
	l.mu.Unlock()

	l.warnDrainOverrun()

	if dl.saw("still draining") {
		t.Errorf("a drain reported itself overrunning after no time at all:\n%s", dl.String())
	}
}

// A DRAIN THAT NEVER BEGAN HAS NOTHING TO REPORT.
//
// warnDrainOverrun is called from the loop before anything guarantees a drain
// started, and a zero start time is an hour ago as far as subtraction is
// concerned. Reading it as one would have every listener announce an overrunning
// drain the moment it was asked.
func TestAListenerThatIsNotDrainingReportsNoOverrun(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var dl drainLog

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithDrainGrace(time.Millisecond), dl.option())

	l.warnDrainOverrun()

	if dl.saw("still draining") {
		t.Errorf("a listener that never drained reported an overrun:\n%s", dl.String())
	}
}

// destroyAll's PARAMETER IS THE WHOLE SAFETY DISTINCTION, so it is exercised
// directly in both positions.
//
// Every other test here drives it through Run, where a fixture change can quietly
// stop reaching it. This asks the function itself, and asserts the two answers
// differ — a destroyAll that ignored the flag would satisfy either half alone.
func TestDestroyAllTakesRunningWorkOnlyWhenAsked(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	for _, tc := range []struct {
		name           string
		includeRunning bool
		wantDestroyed  bool
	}{
		{"a shutdown", false, false},
		{"an operator asking for it", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

			holdRunning(t, l, a, tiers[0].Label, 7)

			done := l.destroyAll(t.Context(), tc.includeRunning, nil)

			mu.Lock()
			got := append([]int64(nil), destroyed...)
			mu.Unlock()

			if tc.wantDestroyed {
				if len(got) != 1 || got[0] != 7 {
					t.Errorf("destroyAll(includeRunning=true) destroyed %v, want [7]", got)
				}

				if !done[7] {
					t.Error("destroyAll did not report request 7 confirmed gone, so its " +
						"capacity is never released")
				}

				return
			}

			if len(got) != 0 {
				t.Errorf("destroyAll(includeRunning=false) destroyed %v; a shutdown must "+
					"leave running work alone", got)
			}

			if done[7] {
				t.Error("destroyAll reported a request it never destroyed as confirmed " +
					"gone, which releases capacity a container is still using")
			}
		})
	}
}

// AND A COMPLETION-OWED DESTROY IS PERFORMED IN BOTH POSITIONS.
//
// The de-dup guard between the two sets was written when the running loop was
// unconditional, so reading it as "skip anything also running" silently dropped
// the destroy a COMPLETED job was owed on every shutdown. That is a stranded
// container, and it survived the whole suite until one test noticed the count.
func TestACompletionOwedDestroyHappensWhicheverWayDestroyAllIsCalled(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	for _, includeRunning := range []bool{false, true} {
		t.Run(map[bool]string{false: "a shutdown", true: "an operator asking"}[includeRunning],
			func(t *testing.T) {
				a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB},
					tiers, alloc.WithLeaseTTL(outlivesTheDrain))

				var attempts int

				runner := &fakeRunner{onDestroy: func(int64) error {
					attempts++

					return nil
				}}

				l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

				// IN BOTH SETS, which is the ordinary state of a completion whose
				// destroy has not landed, and the state the guard is about.
				holdRunningOwedDestroy(t, l, a, tiers[0].Label, 7)

				l.destroyAll(t.Context(), includeRunning, nil)

				if attempts != 1 {
					t.Errorf("destroyAll(includeRunning=%v) destroyed request 7 %d times, "+
						"want exactly 1", includeRunning, attempts)
				}
			})
	}
}
