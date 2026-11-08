package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/store/ceph"
)

// THE SWEEP RUNS BETWEEN FULL PASSES, every supersededSweepEvery, and the full pass
// at once and every cacheEvictionEvery, one at a time and through the tickers the
// node really runs on.
func TestTheNodeSweepsSupersededGenerationsBetweenFullPasses(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		var passes, sweeps, running int

		overlapped := false
		track := func(count *int) func() {
			return func() {
				running++
				if running > 1 {
					overlapped = true
				}
				*count++
				running--
			}
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			runEviction(ctx, cacheEvictionEvery, supersededSweepEvery, track(&passes), track(&sweeps))
		}()

		time.Sleep(cacheEvictionEvery + time.Minute)
		synctest.Wait()

		cancel()
		<-done

		if want := int(cacheEvictionEvery / supersededSweepEvery); sweeps != want {
			t.Errorf("the node swept %d times in %s, want %d", sweeps, cacheEvictionEvery, want)
		}
		if passes != 2 {
			t.Errorf("the node ran %d full passes in %s, want one at once and one at its end", passes,
				cacheEvictionEvery)
		}
		if overlapped {
			t.Error("a sweep and a pass ran at the same time")
		}
	})
}

// THE CEPH STORE IS ONE, so the node's assertion finds the sweep on the store it
// really runs; a store that lost the method would otherwise be swept never.
var _ supersededEvictor = (*ceph.Client)(nil)

// A PASS AND A SWEEP NEVER OVERLAP, AND A STOP ENDS THE LOOP AT AN IN-FLIGHT SWEEP:
// a sweep due while the first pass runs waits for it, and once the context ends
// the loop returns as soon as the sweep in hand does, starting nothing more.
func TestTheEvictionLoopSerialisesAndStopsAtAnInFlightSweep(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		releasePass := make(chan struct{})
		sweepEntered := make(chan struct{})

		var passes, sweeps int

		evict := func() {
			passes++
			if passes == 1 {
				<-releasePass
			}
		}
		sweep := func() {
			sweeps++
			if sweeps == 1 {
				close(sweepEntered)
				<-ctx.Done()
			}
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			runEviction(ctx, cacheEvictionEvery, supersededSweepEvery, evict, sweep)
		}()

		// THE FIRST PASS HOLDS PAST TWO SWEEP TICKS, and no sweep enters.
		time.Sleep(2*supersededSweepEvery + time.Minute)
		synctest.Wait()

		if sweeps != 0 {
			t.Fatalf("%d sweep(s) ran while the first pass held", sweeps)
		}

		close(releasePass)
		<-sweepEntered

		// A STOP WHILE THE SWEEP IS IN HAND: the loop returns once it does.
		cancel()
		<-done

		if sweeps != 1 {
			t.Errorf("the loop ran %d sweep(s), want the one in hand and none after the stop", sweeps)
		}
		if passes != 1 {
			t.Errorf("the loop ran %d pass(es), want the first only", passes)
		}
	})
}

// sweeperStore is a store with the superseded sweep, which answers err and
// records the context it was given.
type sweeperStore struct {
	sweeps int
	err    error
	ctx    context.Context
}

func (s *sweeperStore) Evict(context.Context, time.Duration) error { return nil }

func (s *sweeperStore) EvictSuperseded(ctx context.Context) error {
	s.sweeps++
	s.ctx = ctx

	return s.err
}

// passOnlyStore is a store without one.
type passOnlyStore struct{}

func (passOnlyStore) Evict(context.Context, time.Duration) error { return nil }

// THE SWEEP ASKS A STORE THAT HAS ONE, AND SAYS WHEN IT FAILED; a store without
// one is asked nothing.
func TestTheSweepAsksOnlyAStoreThatHasOne(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	store := &sweeperStore{err: errors.New("ceph: the cache lock was held 31s")}

	given, stop := context.WithCancel(t.Context())
	defer stop()

	sweepSuperseded(given, store, log)

	if store.sweeps != 1 {
		t.Errorf("the store was swept %d times, want once", store.sweeps)
	}

	stop()

	if store.ctx == nil || store.ctx.Err() == nil {
		t.Error("cancelling the sweep's context did not reach the store's, so a stop would not reach it")
	}
	if !strings.Contains(logged.String(), "could not sweep superseded cache generations") ||
		!strings.Contains(logged.String(), "held 31s") {
		t.Errorf("the failure was logged as %q, want it said with its cause", logged.String())
	}

	logged.Reset()
	sweepSuperseded(t.Context(), passOnlyStore{}, log)

	if logged.Len() != 0 {
		t.Errorf("a store without the sweep logged %q", logged.String())
	}
}
