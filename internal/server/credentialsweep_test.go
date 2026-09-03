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

// runPlaneWith runs a one-tier control plane with a fast reaper until stop is
// called or the deadline passes.
func runPlaneWith(t *testing.T, opts ...ControlPlaneOption) (stop context.CancelFunc, done <-chan error) {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)

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
	deadline := time.Now().Add(5 * time.Second)
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
func TestAFencedControlPlaneNeverSweepsStagedCredentials(t *testing.T) {
	sweeper := &countingSweeper{}

	var ticks atomic.Int32

	stop, done := runPlaneWith(t,
		WithStagedCredentialSweeper(sweeper),
		WithLeadershipLost(func() bool { return true }),
		ControlPlaneOption(func(s *Server) {
			s.onReap = func(int) { ticks.Add(1) }
		}),
	)

	// Several reaper ticks, so a sweep that ran would have had every chance to.
	deadline := time.Now().Add(5 * time.Second)
	for ticks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	stop()
	<-done

	if ticks.Load() < 3 {
		t.Fatal("the reaper never ticked, so this proves nothing about what it skipped")
	}

	if got := sweeper.calls.Load(); got != 0 {
		t.Fatalf("a fenced control plane swept %d time(s)", got)
	}
}
