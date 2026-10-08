package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// A PASS STILL RUNNING WHEN THE NEXT IS DUE IS REPORTED WHILE IT RUNS, through
// the control plane's option, so the report can capture the stall itself; and
// a pass that ends in time is not reported at all.
func TestAHeartbeatPassThatOverrunsIsReportedWhileItRuns(t *testing.T) {
	t.Parallel()

	// An interval of 100ms: a pass held at the lock for seconds overruns it
	// many times over, and one that is not held finishes well inside it.
	const ttl = 300 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	var reports atomic.Int32

	overran := make(chan struct{}, 8)

	s := New(a, nil, tiers, "overrun-test", nil, WithHeartbeatOverrun(func() {
		reports.Add(1)

		select {
		case overran <- struct{}{}:
		default:
		}
	}))

	l := NewListener(a, tiers[0].Label, &fakeSession{}, append(s.listenerOpts(nil), WithRunner(&fakeRunner{}))...)

	ticks := make(chan time.Time)
	passed := make(chan struct{}, 8)

	var (
		hold    atomic.Bool
		reached = make(chan struct{}, 1)
		open    = make(chan struct{})
		opened  atomic.Bool
	)

	release := func() {
		if opened.CompareAndSwap(false, true) {
			close(open)
		}
	}

	l.heartbeatTicks = ticks
	l.heartbeatPassed = func() { passed <- struct{}{} }
	l.heartbeatLock = func() {
		if hold.Load() {
			reached <- struct{}{}
			<-open
		}
	}

	ctx, cancel := context.WithCancel(t.Context())

	var loop sync.WaitGroup

	loop.Go(func() { l.heartbeatLoop(ctx) })

	t.Cleanup(func() {
		release()
		cancel()
		loop.Wait()
	})

	tick := func() {
		select {
		case ticks <- time.Now():
		case <-time.After(10 * time.Second):
			t.Fatal("the heartbeat loop did not take a tick")
		}
	}

	// A PASS THAT ENDS IN TIME, and then three intervals in which a timer
	// left running would have fired.
	tick()

	select {
	case <-passed:
	case <-time.After(10 * time.Second):
		t.Fatal("a pass that was not held never ended")
	}

	time.Sleep(3 * (ttl / 3))

	if n := reports.Load(); n != 0 {
		t.Fatalf("a pass that ended in time was reported as an overrun %d times", n)
	}

	// A PASS HELD AT THE LOCK, reported while it is still held.
	hold.Store(true)
	tick()

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the held pass never reached the lock")
	}

	select {
	case <-overran:
	case <-time.After(10 * time.Second):
		t.Fatal("a pass held past its interval was never reported while it ran")
	}

	hold.Store(false)
	release()

	select {
	case <-passed:
	case <-time.After(10 * time.Second):
		t.Fatal("the held pass never ended once released")
	}

	if n := reports.Load(); n != 1 {
		t.Errorf("one overrunning pass was reported %d times", n)
	}
}
