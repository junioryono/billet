package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// overrunHarness is a listener's heartbeat loop running in a synctest bubble on
// its own ticker and its own overrun timer, holding a pass at its lock when
// asked, with the overrun reported through the control plane's option.
type overrunHarness struct {
	l        *Listener
	interval time.Duration
	reached  chan struct{}
	open     chan struct{}
	hold     atomic.Bool
	opened   atomic.Bool
	passes   atomic.Int32
	reports  atomic.Int32
	cancel   context.CancelFunc
	loop     sync.WaitGroup
}

// newOverrunHarness starts the loop inside the caller's bubble. tell, when set,
// runs inside every report, so a test can hold one while it is being told.
func newOverrunHarness(t *testing.T, ttl time.Duration, tell func()) *overrunHarness {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	h := &overrunHarness{
		interval: ttl / 3,
		reached:  make(chan struct{}, 1),
		open:     make(chan struct{}),
	}

	s := New(a, nil, tiers, "overrun-test", nil, WithHeartbeatOverrun(func() {
		h.reports.Add(1)

		if tell != nil {
			tell()
		}
	}))

	h.l = NewListener(a, tiers[0].Label, &fakeSession{}, append(s.listenerOpts(nil), WithRunner(&fakeRunner{}))...)
	h.l.heartbeatLock = func() {
		h.passes.Add(1)

		if h.hold.Load() {
			h.reached <- struct{}{}
			<-h.open
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel

	h.loop.Go(func() { h.l.heartbeatLoop(ctx) })

	// HOLDING ENDS BEFORE THE GATE OPENS: a test that failed while a pass was
	// held leaves reached full, and a later tick's pass would block sending
	// to it, so the loop could never be joined.
	t.Cleanup(func() {
		h.hold.Store(false)
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

// A PASS STILL RUNNING WHEN THE NEXT IS DUE IS REPORTED WHILE IT RUNS, on the
// loop's own timer, exactly one interval after the pass began, and through the
// control plane's option, so the report can capture the stall itself.
func TestAHeartbeatPassThatOverrunsIsReportedWhileItRuns(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := newOverrunHarness(t, 90*time.Second, nil)

		h.hold.Store(true)

		// The first tick, and the pass it starts held at its lock.
		time.Sleep(h.interval)
		synctest.Wait()

		select {
		case <-h.reached:
		default:
			t.Fatal("the first tick's pass never reached the lock")
		}

		// NOT A MOMENT EARLY: an overrun is a pass still running when the next
		// is due, and a timer armed for less would report a pass that was merely
		// slow.
		time.Sleep(h.interval - time.Nanosecond)
		synctest.Wait()

		if n := h.reports.Load(); n != 0 {
			t.Fatalf("a pass was reported %d times before its interval had passed", n)
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()

		if n := h.reports.Load(); n != 1 {
			t.Fatalf("a pass held past its interval was reported %d times while it ran, want once", n)
		}

		h.hold.Store(false)
		h.release()
		synctest.Wait()

		if !h.l.mu.TryLock() {
			t.Fatal("the held pass never ended once released")
		}

		h.l.mu.Unlock()

		if n := h.reports.Load(); n != 1 {
			t.Errorf("one overrunning pass was reported %d times", n)
		}
	})
}

// A PASS THAT ENDS IN TIME IS NOT REPORTED: its timer is stopped once the pass
// has ended, so ten passes and the moments each timer would have fired come
// and go without a report.
func TestAPassThatEndsInTimeStopsItsOverrunTimer(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := newOverrunHarness(t, 90*time.Second, nil)

		time.Sleep(10*h.interval + h.interval/2)
		synctest.Wait()

		if n := h.passes.Load(); n != 10 {
			t.Fatalf("the loop made %d passes in ten intervals, want 10; the test proves nothing", n)
		}

		if n := h.reports.Load(); n != 0 {
			t.Errorf("passes that ended in time were reported %d times; a timer outlived its pass", n)
		}
	})
}

// AN OVERRUN THAT FIRED IS JOINED: the loop does not return while a report it
// started is still being told, so the recorder is not stopped under it.
func TestTheHeartbeatLoopWaitsForAnOverrunBeingTold(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		telling, told := make(chan struct{}), make(chan struct{})
		began := sync.OnceFunc(func() { close(telling) })

		h := newOverrunHarness(t, 90*time.Second, func() {
			began()
			<-told
		})

		letTell := sync.OnceFunc(func() { close(told) })

		// Registered after the harness's, so it runs first and the loop it joins
		// can return.
		t.Cleanup(letTell)

		// A pass held past its interval, so its timer fires and the report begins
		// and is held.
		h.hold.Store(true)
		time.Sleep(2 * h.interval)
		synctest.Wait()

		select {
		case <-telling:
		default:
			t.Fatal("the overrun never began telling")
		}

		// The pass ends after its timer fired, so stopping it cannot take the
		// report back, and the loop is told to stop.
		h.hold.Store(false)
		h.release()

		returned := make(chan struct{})

		go func() {
			h.loop.Wait()
			close(returned)
		}()

		h.cancel()
		synctest.Wait()

		select {
		case <-returned:
			t.Fatal("the heartbeat loop returned while an overrun it started was still being told")
		default:
		}

		letTell()
		synctest.Wait()

		select {
		case <-returned:
		default:
			t.Fatal("the heartbeat loop never returned once the overrun was told")
		}

		if n := h.reports.Load(); n != 1 {
			t.Errorf("the overrun was told %d times before the loop returned, want once", n)
		}
	})
}
