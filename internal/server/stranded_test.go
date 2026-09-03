package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// AN ADVERTISEMENT OUTLIVES THE MACHINE BEHIND IT, and nothing was taking it
// back.
//
// Capacity is escrowed before it is advertised and the listener only ever ADDS:
// refillEscrow tops up, and what GitHub is told is however many leases this
// listener holds. So when the host those leases were taken from goes away, the
// number does not move. billet goes on telling GitHub it can take four jobs,
// GitHub goes on assigning them, and each one is acquired and then fails to
// launch because there is nowhere to put it.
//
// The leases to drop are exactly identifiable now that every reservation names
// its machine: the ones this listener is HOLDING — never assigned, so nothing is
// running under them — whose host is no longer live.
func TestEscrowIsReleasedWhenItsMachineGoesAway(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, tiers)

	// One machine, room for two of this tier.
	epoch, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "only", Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 64 * config.GiB,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		mu         sync.Mutex
		advertised []int
		polls      int
	)

	session := &fakeSession{}
	session.onPoll = func(capacity int) {
		mu.Lock()
		advertised = append(advertised, capacity)
		polls++
		gone := polls == 3
		mu.Unlock()

		// The host disappears once the listener has settled on a number.
		if gone {
			if err := a.NodeGone(context.WithoutCancel(ctx), "only", epoch); err != nil {
				t.Errorf("NodeGone: %v", err)
			}
		}

		mu.Lock()
		enough := polls >= 12
		mu.Unlock()

		if enough {
			cancel()
		}
	}

	l := NewListener(a, tiers[0].Label, session)

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(advertised) < 4 {
		t.Fatalf("only %d polls; this test proves nothing", len(advertised))
	}

	// It has to have advertised something first, or the drop below is vacuous.
	peak := 0
	for _, n := range advertised {
		peak = max(peak, n)
	}

	if peak == 0 {
		t.Fatal("the listener never advertised anything, so losing the machine changed nothing")
	}

	if last := advertised[len(advertised)-1]; last != 0 {
		t.Errorf("still advertising %d after the only machine went away (saw %v); GitHub will "+
			"keep assigning jobs that cannot be placed", last, advertised)
	}
}
