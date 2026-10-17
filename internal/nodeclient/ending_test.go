package nodeclient_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeending"
)

// endings collects what the loop reported, in order.
type endings struct {
	mu  sync.Mutex
	got []string
}

func (e *endings) record(ending nodeending.Ending, drainRequested bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	s := string(ending)
	if drainRequested {
		s += "+drain"
	}

	e.got = append(e.got, s)
}

func (e *endings) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.got...)
}

// endingOpts is a loop that reports its ending into rec and says when it is
// ready (registered and recovered).
func endingOpts(rec *endings, ready *atomic.Int32) nodeclient.LoopOptions {
	return nodeclient.LoopOptions{
		VCPU:         testNodeVCPU,
		Memory:       testNodeMemory,
		Provider:     config.ProviderDocker,
		Deployment:   deployment,
		Log:          slog.New(slog.DiscardHandler),
		Backoff:      20 * time.Millisecond,
		SweepEvery:   10 * time.Millisecond,
		DrainTimeout: time.Hour,
		Ready:        func() { ready.Add(1) },
		Ended:        rec.record,
	}
}

// runAndStop runs the loop, waits for ready, applies stop, and returns Run's
// error once it ends.
func runAndStop(
	t *testing.T, c *nodeclient.Client, compute nodeclient.Compute, opts nodeclient.LoopOptions,
	ready func() bool, stop func(cancel context.CancelFunc),
) error {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- nodeclient.Run(ctx, c, compute, opts) }()

	waitFor(t, ready)
	stop(cancel)

	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the node never stopped")

		return nil
	}
}

func wantEndings(t *testing.T, rec *endings, err error, want ...string) {
	t.Helper()

	if err != nil {
		t.Fatalf("Run returned %v, want a clean stop", err)
	}

	got := rec.list()
	if len(got) != len(want) {
		t.Fatalf("the loop reported %q, want %q", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the loop reported %q, want %q", got, want)
		}
	}
}

// EACH WAY OUT OF THE LOOP REPORTS ITS OWN ENDING, ONCE, and only the ones a
// stop proves are proving: a node holding nothing that knew what its host runs
// drained, one holding work drained once it finished, a second signal left it
// running, a handoff whose withdrawal landed handed off, a drain request turned
// a handoff into a drain, and a superseded node said so.
func TestEachWayOutOfTheLoopReportsItsEnding(t *testing.T) {
	t.Parallel()

	t.Run("holding nothing", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{}

		err := runAndStop(t, c, compute, endingOpts(rec, ready), func() bool { return ready.Load() == 1 },
			func(cancel context.CancelFunc) { cancel() })
		wantEndings(t, rec, err, "drained")
	})

	t.Run("holding work until it finishes", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{holding: true}

		err := runAndStop(t, c, compute, endingOpts(rec, ready), func() bool { return ready.Load() == 1 },
			func(cancel context.CancelFunc) {
				cancel()
				waitFor(t, func() bool { return compute.tended() > 0 })

				if got := rec.list(); len(got) != 0 {
					t.Errorf("the loop reported %q while it was still draining", got)
				}

				compute.setHolding(false)
			})
		wantEndings(t, rec, err, "drained")
	})

	t.Run("a second signal", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{holding: true}
		hurry := make(chan struct{})

		opts := endingOpts(rec, ready)
		opts.Hurry = hurry

		err := runAndStop(t, c, compute, opts, func() bool { return ready.Load() == 1 },
			func(cancel context.CancelFunc) {
				cancel()
				waitFor(t, func() bool { return compute.tended() > 0 })
				close(hurry)
			})
		wantEndings(t, rec, err, "left-running")
	})

	t.Run("a handoff", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{holding: true}

		opts := endingOpts(rec, ready)
		opts.HandOverOnStop = true

		err := runAndStop(t, c, compute, opts, func() bool { return ready.Load() == 1 },
			func(cancel context.CancelFunc) { cancel() })
		wantEndings(t, rec, err, "handed-off")
	})

	t.Run("a drain request over a handoff", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{holding: true}

		opts := endingOpts(rec, ready)
		opts.HandOverOnStop = true
		opts.DrainRequested = func() bool { return true }

		err := runAndStop(t, c, compute, opts, func() bool { return ready.Load() == 1 },
			func(cancel context.CancelFunc) {
				cancel()
				waitFor(t, func() bool { return compute.tended() > 0 })
				compute.setHolding(false)
			})
		wantEndings(t, rec, err, "drained+drain")
	})

	t.Run("superseded", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{holding: true}

		err := runAndStop(t, c, compute, endingOpts(rec, ready), func() bool { return ready.Load() == 1 },
			func(cancel context.CancelFunc) {
				other, err := nodeclient.New(nodeclient.Options{Base: c.BaseForTest(), Node: "n1"})
				if err != nil {
					t.Fatal(err)
				}

				if err := other.Register(t.Context(), nodeclient.Registration{
					Provider: config.ProviderDocker, Deployment: deployment,
					VCPU: testNodeVCPU, Memory: testNodeMemory,
				}); err != nil {
					t.Fatal(err)
				}

				waitFor(t, func() bool { return compute.supersededCount() > 0 })
				cancel()
			})
		wantEndings(t, rec, err, "superseded")
	})
}

// A NODE THAT NEVER LEARNED WHAT ITS HOST RUNS DID NOT DRAIN: one that never
// registered was given nothing and ended idle; one that registered and could
// not recover before its stop gave up left whatever it may own running.
func TestANodeThatNeverRecoveredDidNotDrain(t *testing.T) {
	t.Parallel()

	t.Run("never registered", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(nil)
		base := srv.URL
		srv.Close()

		c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
		if err != nil {
			t.Fatal(err)
		}

		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{}
		started := time.Now()

		err = runAndStop(t, c, compute, endingOpts(rec, ready),
			func() bool { return time.Since(started) > 100*time.Millisecond },
			func(cancel context.CancelFunc) { cancel() })
		wantEndings(t, rec, err, "idle")

		if ready.Load() != 0 {
			t.Error("a node that never registered was ready")
		}
	})

	t.Run("recovered by the stop itself", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{recoverErr: errors.New("the provider could not list its guests")}

		opts := endingOpts(rec, ready)
		opts.DrainRequested = func() bool { return true }

		// Every Recover before the stop fails, and only one that STARTED after it,
		// by the count taken at the cancel, is let succeed: so the drain rests on
		// the stop's own recovery, not on the ordinary loop's.
		err := runAndStop(t, c, compute, opts, func() bool { return compute.recoverCount() >= 1 },
			func(cancel context.CancelFunc) {
				cancel()
				atCancel := compute.recoverCount()
				waitFor(t, func() bool { return compute.recoverCount() >= atCancel+2 })

				compute.mu.Lock()
				compute.recoverErr = nil
				compute.mu.Unlock()
			})
		wantEndings(t, rec, err, "drained+drain")
	})

	t.Run("registered, never recovered", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{recoverErr: errors.New("the provider could not list its guests")}

		err := runAndStop(t, c, compute, endingOpts(rec, ready), func() bool { return compute.aliveCount() == 1 },
			func(cancel context.CancelFunc) { cancel() })
		wantEndings(t, rec, err, "left-running")
	})

	// A wait that empties Holding() is not a drain for a process that never
	// recovered: what it could not see was never in Holding() to wait for.
	t.Run("registered, never recovered, its own work finished", func(t *testing.T) {
		t.Parallel()

		_, c := harness(t)
		rec, ready := &endings{}, &atomic.Int32{}
		compute := &fakeCompute{holding: true, recoverErr: errors.New("the provider could not list its guests")}

		err := runAndStop(t, c, compute, endingOpts(rec, ready), func() bool { return compute.aliveCount() == 1 },
			func(cancel context.CancelFunc) {
				base := compute.tended()
				cancel()
				waitFor(t, func() bool { return compute.tended() > base })

				if got := rec.list(); len(got) != 0 {
					t.Errorf("the loop reported %q while it was still waiting", got)
				}

				compute.setHolding(false)
			})
		wantEndings(t, rec, err, "left-running")
	})
}
