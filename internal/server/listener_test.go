package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// THE invariant of the listener plane, and the reason the allocator exists.
//
// **The sum of what every listener has advertised to GitHub at any instant never
// exceeds the global budget.**
//
// Each tier is its own scale set with its own `maxCapacity`, sent on every
// long-poll as X-ScaleSetMaxCapacity. If each listener computed its own maximum
// from its own tier's headroom, GitHub could fill all of them at once: three
// tiers each advertising "I can take 10" on a host with room for 12 is a promise
// billet cannot keep, and GitHub has already assigned the jobs by the time
// anyone notices. Reserving on ASSIGNMENT is too late for the same reason.
//
// So capacity is escrowed BEFORE it is advertised, and a listener may only
// advertise what the escrow actually returned. This test drives several
// listeners at once against one allocator and watches what they advertise.
//
// It is written against a fake session rather than GitHub because the property
// is about billet's arithmetic, not about the wire protocol.
func TestAdvertisedCapacityNeverExceedsTheBudget(t *testing.T) {
	const (
		budget  = 12 // vCPU
		perTier = 4  // so at most 3 runners exist across all tiers at once
	)

	tiers := []config.Tier{
		tier("billet-4vcpu-a", perTier),
		tier("billet-4vcpu-b", perTier),
		tier("billet-4vcpu-c", perTier),
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: budget, MaxMemory: 64 * config.GiB}, tiers)

	// advertised tracks what each listener currently has outstanding, and the
	// peak of their sum. A listener holds its advertisement for the duration of
	// a poll, which is exactly the window in which GitHub may act on it.
	var (
		mu          sync.Mutex
		outstanding = map[string]int{}
		peak        int
	)

	observe := func(tierLabel string, capacity int) {
		mu.Lock()
		defer mu.Unlock()

		outstanding[tierLabel] = capacity

		sum := 0
		for _, c := range outstanding {
			sum += c
		}

		if sum > peak {
			peak = sum
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var polls atomic.Int32

	var wg sync.WaitGroup

	for _, tr := range tiers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			session := &fakeSession{
				onPoll: func(maxCapacity int) {
					// Recorded and NOT cleared afterwards. An advertisement is live
					// from the moment it is sent until it is replaced — GitHub may
					// act on the last maxCapacity it saw at any point — so the
					// honest measure is the sum of each listener's most recent one.
					observe(tr.Label, maxCapacity)

					if polls.Add(1) >= 60 {
						cancel()
					}
				},
			}

			l := NewListener(a, tr.Label, session)
			if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
				!errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("listener %s: %v", tr.Label, err)
			}
		}()
	}

	wg.Wait()

	if polls.Load() == 0 {
		t.Fatal("no listener ever polled; this test proves nothing")
	}

	// vCPU, not runner count: the budget is a vector and this is the axis the
	// tiers here contend on.
	if got := peak * perTier; got > budget {
		t.Errorf("listeners advertised %d vCPU at once against a %d vCPU budget", got, budget)
	}
}

func tier(label string, vcpu int) config.Tier {
	return config.Tier{
		Label:    label,
		Provider: config.ProviderFirecracker,
		GuestOS:  config.GuestLinux,
		VCPU:     vcpu,
		Memory:   4 * config.GiB,
		Image:    "ubuntu-2404-x64",
	}
}

func newAllocator(t *testing.T, limits alloc.Limits, tiers []config.Tier) *alloc.Allocator {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, limits, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	return a
}

// fakeSession stands in for a scale-set message session. It never returns work,
// so the listener does nothing but escrow, advertise, and release — which is the
// whole of what this test is about.
type fakeSession struct {
	onPoll func(maxCapacity int)
}

func (f *fakeSession) GetMessage(ctx context.Context, _ int64, maxCapacity int) (*Message, error) {
	if f.onPoll != nil {
		f.onPoll(maxCapacity)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// A real long poll returns (nil, nil) on timeout; the listener polls again.
	return nil, nil
}

func (f *fakeSession) DeleteMessage(context.Context, int64) error { return nil }

func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) { return ids, nil }

func (f *fakeSession) Close(context.Context) error { return nil }
