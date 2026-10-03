package nodeclient_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/server"
)

// gatedCompute holds every launch open until release is closed, and records how
// many were open at once and whether a destroy ran while any was.
type gatedCompute struct {
	*fakeCompute

	release chan struct{}

	mu              sync.Mutex
	inflight        int
	most            int
	destroyedDuring bool
}

func newGatedCompute() *gatedCompute {
	return &gatedCompute{fakeCompute: &fakeCompute{}, release: make(chan struct{})}
}

func (g *gatedCompute) Launch(
	ctx context.Context, lease *alloc.Lease, tier *nodeapi.TierSpec, job server.Job,
) error {
	g.mu.Lock()
	g.inflight++
	g.most = max(g.most, g.inflight)
	g.mu.Unlock()

	<-g.release

	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()

	return g.fakeCompute.Launch(ctx, lease, tier, job)
}

func (g *gatedCompute) Destroy(ctx context.Context, requestID int64) error {
	g.mu.Lock()
	if g.inflight > 0 {
		g.destroyedDuring = true
	}
	g.mu.Unlock()

	return g.fakeCompute.Destroy(ctx, requestID)
}

func (g *gatedCompute) peak() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.inflight, g.most
}

// runLoopWith is runLoop with the launch concurrency named.
func runLoopWith(t *testing.T, c *nodeclient.Client, compute nodeclient.Compute, concurrency int) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Go(func() {
		err := nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:              testNodeVCPU,
			Memory:            testNodeMemory,
			Provider:          config.ProviderDocker,
			GuestOS:           []config.GuestOS{config.GuestLinux},
			Deployment:        deployment,
			Log:               slog.New(slog.DiscardHandler),
			Backoff:           20 * time.Millisecond,
			LaunchConcurrency: concurrency,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	})

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func testLease(i int) *alloc.Lease {
	return &alloc.Lease{
		ID: fmt.Sprintf("l%d", i), Tier: "billet-2vcpu", VCPU: 2, Memory: 8 * config.GiB,
		GuestOS: config.GuestLinux, Providers: []config.ProviderKind{config.ProviderDocker}, Epoch: 1,
	}
}

// LAUNCHES OVERLAP, AND A DESTROY WAITS FOR THEM. A node that ran one command at a
// time started one runner every 30 to 65 seconds while 230 jobs waited
// (2026-10-03); letting launches overlap must not let a destroy overtake one.
func TestLaunchesOverlapAndADestroyWaitsForThem(t *testing.T) {
	t.Parallel()

	p, c := harnessWithCommandTimeout(t, 30*time.Second)
	compute := newGatedCompute()
	runLoopWith(t, c, compute, 4)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	var launches sync.WaitGroup
	for i := range 3 {
		launches.Go(func() {
			if err := p.NewRunner().Launch(t.Context(), testLease(i), server.Job{RequestID: int64(100 + i)}); err != nil {
				t.Errorf("Launch %d: %v", i, err)
			}
		})
	}

	waitFor(t, func() bool {
		inflight, _ := compute.peak()

		return inflight == 3
	})

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 42) }()

	// THE DESTROY IS HELD, which a short wait cannot prove by itself; the
	// compute's own record below is what does.
	select {
	case err := <-destroyed:
		close(compute.release)
		launches.Wait()
		t.Fatalf("a destroy finished while launches were in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(compute.release)
	launches.Wait()

	if err := <-destroyed; err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if _, most := compute.peak(); most != 3 {
		t.Errorf("at most %d launches ran at once, want 3", most)
	}

	compute.mu.Lock()
	during := compute.destroyedDuring
	compute.mu.Unlock()

	if during {
		t.Error("a destroy reached the compute while a launch was in flight")
	}
}

// ONE AT A TIME UNLESS THE PROVIDER SAYS OTHERWISE, which is every provider but
// Firecracker.
func TestLaunchesRunOneAtATimeByDefault(t *testing.T) {
	t.Parallel()

	p, c := harnessWithCommandTimeout(t, 30*time.Second)
	compute := newGatedCompute()
	runLoopWith(t, c, compute, 1)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	var launches sync.WaitGroup
	for i := range 2 {
		launches.Go(func() {
			if err := p.NewRunner().Launch(t.Context(), testLease(i), server.Job{RequestID: int64(200 + i)}); err != nil {
				t.Errorf("Launch %d: %v", i, err)
			}
		})
	}

	waitFor(t, func() bool {
		inflight, _ := compute.peak()

		return inflight == 1
	})

	// Long enough for a second launch to have arrived had the loop taken it.
	time.Sleep(100 * time.Millisecond)

	close(compute.release)
	launches.Wait()

	if _, most := compute.peak(); most != 1 {
		t.Errorf("%d launches ran at once on a node configured for one", most)
	}
}
