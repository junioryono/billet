package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// countingSweeper is a StagedCredentialSweeper that records being asked.
type countingSweeper struct {
	calls atomic.Int32
	err   error
}

func (s *countingSweeper) SweepStagedCredentials(context.Context) error {
	s.calls.Add(1)

	return s.err
}

// sweepGuard is how long a test waits for the reaper ticks it counts, and how
// long the plane may run before a test ends it. It is a failure deadline, never a
// condition: a test stops the plane once it has counted what it needs, which a
// 20 ms reaper reaches long before it however loaded the machine. Five seconds
// for three ticks ran out at 5.89 s on a loaded CI runner (#188).
const sweepGuard = 2 * time.Minute

// runPlaneWith runs a one-tier control plane with a fast reaper until stop is
// called or the guard passes.
func runPlaneWith(t *testing.T, opts ...ControlPlaneOption) (stop context.CancelFunc, done <-chan error) {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), sweepGuard+time.Minute)

	prov := &fakeProvisioner{
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) { time.Sleep(20 * time.Millisecond) }}
		},
	}

	finished := make(chan error, 1)

	go func() {
		finished <- New(a, prov, tiers, "test-owner", nil,
			append([]ControlPlaneOption{WithReapInterval(20 * time.Millisecond)}, opts...)...,
		).Run(ctx)
	}()

	return cancel, finished
}

// THE SWEEP RUNS ON THE REAPER'S CLOCK, and a failing pass neither stops the
// plane nor the next pass.
//
// Deleting the call from reapPeriodically leaves every other test in this package
// green — nothing else observes it — so the assertion is that the sweeper was
// ASKED, more than once, while the plane was up, and that an error from it changed
// nothing about either.
func TestTheStagedCredentialSweepRunsOnTheReapersClock(t *testing.T) {
	sweeper := &countingSweeper{err: errors.New("the api could not be reached")}

	var passes atomic.Int32

	stop, done := runPlaneWith(t,
		WithStagedCredentialSweeper(sweeper),
		ControlPlaneOption(func(s *Server) {
			s.onCredentialSweep = func(err error) {
				if err == nil {
					t.Error("the sweeper's error was swallowed before the hook saw it")
				}

				if passes.Add(1) >= 3 {
					// Stop only after several passes, so what is proved is a CLOCK
					// rather than one call at startup.
					go func() { time.Sleep(10 * time.Millisecond) }()
				}
			}
		}),
	)

	// Wait for the third pass, then stop the plane.
	deadline := time.Now().Add(sweepGuard)
	for passes.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	stop()

	if err := <-done; err != nil {
		t.Fatalf("a failing sweep took the control plane down: %v", err)
	}

	if got := sweeper.calls.Load(); got < 3 {
		t.Fatalf("the sweeper was asked %d time(s); it is meant to run on every reaper tick", got)
	}
}

// A FENCED CONTROL PLANE SWEEPS NOTHING. Deleting a parameter is an act in the
// deployment's name, and once the ledger has refused this process as its
// controller the successor performs it.
//
// FENCED AFTER THE PLANE IS RUNNING, as a claim is lost in production. A plane
// fenced from the start opens no session at all (openSession refuses a process
// that is no longer the controller, #207) and stops before its reaper ticks.
//
// A RUN THAT SAW TOO FEW FENCED TICKS IS RUN AGAIN, and a sweep after the fence
// fails at once. The fence also stops the plane, since a fenced listener ends the
// group the reaper runs in, so how many ticks fall between the fence and the stop
// is a race: three or more on a quiet host, none on a loaded CI runner once
// (#919). That outcome proves nothing either way; a fenced sweep is the defect.
func TestAFencedControlPlaneNeverSweepsStagedCredentials(t *testing.T) {
	const attempts = 10

	for range attempts {
		ticks, swept := fencedSweeps(t)
		if swept != 0 {
			t.Fatalf("a fenced control plane swept %d time(s)", swept)
		}

		if ticks >= 4 {
			return
		}

		t.Logf("the plane stopped after %d tick(s), fewer than three after the fence; running it again", ticks)
	}

	t.Fatalf("in %d runs the reaper never ticked three times after the fence, so this proves "+
		"nothing about what it skipped", attempts)
}

// fencedSweeps runs a plane that is fenced at the end of its reaper's first tick,
// until the reaper has ticked three more times or the plane stops. It returns how
// many times the reaper ticked, then how many sweeps the plane made after the
// fence.
func fencedSweeps(t *testing.T) (int32, int32) {
	t.Helper()

	sweeper := &countingSweeper{}

	var (
		ticks    atomic.Int32
		fenced   atomic.Bool
		baseline atomic.Int32
	)

	stop, done := runPlaneWith(t,
		WithStagedCredentialSweeper(sweeper),
		WithLeadershipLost(fenced.Load),
		ControlPlaneOption(func(s *Server) {
			// ON THE REAPER'S OWN GOROUTINE, at the end of the first tick: the next
			// tick cannot begin until this returns, so every tick after the first
			// is wholly fenced and the baseline holds every sweep before the fence.
			s.onReap = func(int) {
				if ticks.Add(1) == 1 {
					baseline.Store(sweeper.calls.Load())
					fenced.Store(true)
				}
			}
		}),
	)

	// Three wholly fenced ticks, inside runPlaneWith's own lifetime, or the plane's
	// own stop.
	deadline := time.Now().Add(sweepGuard)
	stopped := false
	for !stopped && ticks.Load() < 4 && time.Now().Before(deadline) {
		select {
		case <-done:
			stopped = true
		case <-time.After(5 * time.Millisecond):
		}
	}

	stop()
	if !stopped {
		<-done
	}

	// NO FENCE, NO BASELINE: a plane that stopped inside its first tick may have
	// swept, unfenced, before the fence was set, and that sweep is not one after it.
	if ticks.Load() == 0 {
		return 0, 0
	}

	return ticks.Load(), sweeper.calls.Load() - baseline.Load()
}
