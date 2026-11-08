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

// sweeperStore is a store with the superseded sweep, which answers err.
type sweeperStore struct {
	sweeps int
	err    error
}

func (s *sweeperStore) Evict(context.Context, time.Duration) error { return nil }

func (s *sweeperStore) EvictSuperseded(context.Context) error {
	s.sweeps++

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
	sweepSuperseded(t.Context(), store, log)

	if store.sweeps != 1 {
		t.Errorf("the store was swept %d times, want once", store.sweeps)
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
