package server

import (
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// overlapRunner holds every launch open until release closes, recording how
// many were open at once.
type overlapRunner struct {
	fakeRunner

	release chan struct{}

	mu       sync.Mutex
	inflight int
	most     int
}

func (r *overlapRunner) launchHeld(int64) error {
	r.mu.Lock()
	r.inflight++
	r.most = max(r.most, r.inflight)
	r.mu.Unlock()

	<-r.release

	r.mu.Lock()
	r.inflight--
	r.mu.Unlock()

	return nil
}

func (r *overlapRunner) peak() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.inflight, r.most
}

func newOverlapRunner() *overlapRunner {
	r := &overlapRunner{release: make(chan struct{})}
	r.onLaunch = r.launchHeld

	return r
}

// A FIRECRACKER TIER STARTS ITS POOL MEMBERS TOGETHER. One tier wanted 66
// runners on 2026-10-03 and, launching each before buying the next, started one
// every 30 to 65 seconds.
func TestAPoolBatchLaunchesItsMembersTogether(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("a-work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8 * tierVCPU, MaxMemory: 256 * config.GiB}, tiers)
	runner := newOverlapRunner()
	work := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner), WithPoolLaunchBatch(4))

	done := make(chan error, 1)
	go func() { done <- work.reconcilePool(t.Context(), 3) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if inflight, _ := runner.peak(); inflight == 3 {
			break
		}
		if time.Now().After(deadline) {
			close(runner.release)
			<-done
			_, most := runner.peak()
			t.Fatalf("at most %d pool launches ran at once, want all 3 of the batch", most)
		}
		time.Sleep(time.Millisecond)
	}

	close(runner.release)

	if err := <-done; err != nil {
		t.Fatalf("reconcilePool: %v", err)
	}
}

// AND EVERY OTHER TIER STILL STARTS ONE AT A TIME, because its node takes one
// command at a time and a launch queued behind another runs out its timeout.
func TestAPoolLaunchesOneAtATimeByDefault(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("a-work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8 * tierVCPU, MaxMemory: 256 * config.GiB}, tiers)

	var (
		mu       sync.Mutex
		inflight int
		most     int
	)
	work := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(int64) error {
			mu.Lock()
			inflight++
			most = max(most, inflight)
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inflight--
			mu.Unlock()

			return nil
		},
	}))

	if err := work.reconcilePool(t.Context(), 3); err != nil {
		t.Fatalf("reconcilePool: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Errorf("%d pool launches ran at once without a batch, want one at a time", most)
	}
}

// ONLY A TIER SERVED BY FIRECRACKER ALONE IS BATCHED.
func TestOnlyAFirecrackerOnlyTierIsBatched(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		tier config.Tier
		want int
	}{
		{config.Tier{Provider: config.ProviderFirecracker}, poolLaunchBatch},
		{config.Tier{Provider: config.ProviderTart}, 1},
		{config.Tier{Provider: config.ProviderDocker}, 1},
		{config.Tier{Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2}}, 1},
	} {
		if got := poolLaunchBatchFor(&tc.tier); got != tc.want {
			t.Errorf("a tier on %v starts %d at once, want %d", tc.tier.AcceptableProviders(), got, tc.want)
		}
	}
}
