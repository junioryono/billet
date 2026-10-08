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

// overrunHarness is a listener whose heartbeat loop a test drives tick by
// tick, holding a pass at its lock when asked, with the overrun reported
// through the control plane's option.
type overrunHarness struct {
	l       *Listener
	ticks   chan time.Time
	passed  chan struct{}
	reached chan struct{}
	open    chan struct{}
	hold    atomic.Bool
	opened  atomic.Bool
	reports atomic.Int32
	overran chan struct{}
	cancel  context.CancelFunc
	loop    sync.WaitGroup
}

func newOverrunHarness(
	t *testing.T, ttl time.Duration, timer func(time.Duration, func()) func() bool, stopped func(),
) *overrunHarness {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	h := &overrunHarness{
		ticks:   make(chan time.Time),
		passed:  make(chan struct{}, 8),
		reached: make(chan struct{}, 1),
		open:    make(chan struct{}),
		overran: make(chan struct{}, 8),
	}

	s := New(a, nil, tiers, "overrun-test", nil, WithHeartbeatOverrun(func() {
		h.reports.Add(1)

		select {
		case h.overran <- struct{}{}:
		default:
		}
	}))

	h.l = NewListener(a, tiers[0].Label, &fakeSession{}, append(s.listenerOpts(nil), WithRunner(&fakeRunner{}))...)
	h.l.heartbeatTicks = h.ticks
	h.l.heartbeatPassed = func() { h.passed <- struct{}{} }
	h.l.overrunTimer = timer
	h.l.heartbeatStopped = stopped
	h.l.heartbeatLock = func() {
		if h.hold.Load() {
			h.reached <- struct{}{}
			<-h.open
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel

	h.loop.Go(func() { h.l.heartbeatLoop(ctx) })

	t.Cleanup(func() {
		h.release()
		cancel()
		h.loop.Wait()
	})

	return h
}

func (h *overrunHarness) release() {
	if h.opened.CompareAndSwap(false, true) {
		close(h.open)
	}
}

func (h *overrunHarness) tick(t *testing.T) {
	t.Helper()

	select {
	case h.ticks <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the heartbeat loop did not take a tick")
	}
}

func (h *overrunHarness) awaitPass(t *testing.T, what string) {
	t.Helper()

	select {
	case <-h.passed:
	case <-time.After(10 * time.Second):
		t.Fatal(what)
	}
}

// A PASS STILL RUNNING WHEN THE NEXT IS DUE IS REPORTED WHILE IT RUNS, on the
// real timer and through the control plane's option, so the report can
// capture the stall itself.
func TestAHeartbeatPassThatOverrunsIsReportedWhileItRuns(t *testing.T) {
	t.Parallel()

	// An interval of 100ms, which a pass held at its lock until the report
	// arrives overruns whatever the machine's load.
	h := newOverrunHarness(t, 300*time.Millisecond, nil, nil)

	h.hold.Store(true)
	h.tick(t)

	select {
	case <-h.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the held pass never reached the lock")
	}

	select {
	case <-h.overran:
	case <-time.After(10 * time.Second):
		t.Fatal("a pass held past its interval was never reported while it ran")
	}

	h.hold.Store(false)
	h.release()
	h.awaitPass(t, "the held pass never ended once released")

	if n := h.reports.Load(); n != 1 {
		t.Errorf("one overrunning pass was reported %d times", n)
	}
}

// fakeTimer records the timer a pass armed and whether it was stopped.
type fakeTimer struct {
	mu      sync.Mutex
	armed   []time.Duration
	stopped int
	fire    func()
}

func (f *fakeTimer) after(d time.Duration, fn func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.armed = append(f.armed, d)
	f.fire = fn

	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.stopped++

		return true
	}
}

// A PASS THAT ENDS IN TIME IS NOT REPORTED: its timer is armed for the
// interval before the pass and stopped once the pass has ended.
func TestAPassThatEndsInTimeStopsItsOverrunTimer(t *testing.T) {
	t.Parallel()

	timer := &fakeTimer{}
	h := newOverrunHarness(t, 90*time.Second, timer.after, nil)

	h.tick(t)
	h.awaitPass(t, "a pass that was not held never ended")

	timer.mu.Lock()
	defer timer.mu.Unlock()

	if len(timer.armed) != 1 || timer.armed[0] != 30*time.Second {
		t.Errorf("the pass armed %v, want one timer for its 30s interval", timer.armed)
	}

	if timer.stopped != 1 {
		t.Errorf("the pass stopped its timer %d times, want once", timer.stopped)
	}

	if n := h.reports.Load(); n != 0 {
		t.Errorf("a pass that ended in time was reported %d times", n)
	}
}

// AN OVERRUN THAT FIRED IS JOINED: the loop does not return while a report it
// started is still being told, so the recorder is not stopped under it.
func TestTheHeartbeatLoopWaitsForAnOverrunBeingTold(t *testing.T) {
	t.Parallel()

	telling, told, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})

	// A timer that has already fired: stop reports false, and the report runs
	// on a goroutine of its own, held until the test lets it finish.
	fired := func(_ time.Duration, fn func()) func() bool {
		go func() {
			close(telling)
			<-told
			fn()
		}()

		return func() bool { return false }
	}

	var let atomic.Bool

	letTell := func() {
		if let.CompareAndSwap(false, true) {
			close(told)
		}
	}

	h := newOverrunHarness(t, 90*time.Second, fired, func() { close(returned) })

	// Registered after the harness's, so it runs first and the loop it joins
	// can return.
	t.Cleanup(letTell)

	h.tick(t)
	h.awaitPass(t, "the pass never ended")

	select {
	case <-telling:
	case <-time.After(10 * time.Second):
		t.Fatal("the fired timer never began telling")
	}

	h.cancel()

	select {
	case <-returned:
		t.Fatal("the heartbeat loop returned while an overrun it started was still being told")
	case <-time.After(100 * time.Millisecond):
	}

	letTell()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the heartbeat loop never returned once the overrun was told")
	}

	if n := h.reports.Load(); n != 1 {
		t.Errorf("the overrun was told %d times before the loop returned, want once", n)
	}
}
