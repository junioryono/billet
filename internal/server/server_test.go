package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// Shutdown must hand back EVERY escrowed lease.
//
// Escrow is capacity set aside before it is advertised, so a lease left escrowed
// by a stopped process is capacity nothing can use until the reaper's TTL
// expires it. The symptom is the worst kind: billet comes back up, advertises
// less than the host can do, and jobs queue for a reason nothing reports.
//
// It holds today because of one deferred release in the listener, which is
// exactly the sort of line that gets moved during a refactor. Hence a test.
func TestShutdownReleasesEveryEscrowedLease(t *testing.T) {
	tiers := []config.Tier{
		tier("billet-4vcpu-a", 4),
		tier("billet-4vcpu-b", 4),
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	var polls atomic.Int32

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	prov := &fakeProvisioner{
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) {
				// Stop once both tiers have actually escrowed something, or the
				// test proves nothing about releasing.
				if polls.Add(1) >= 6 {
					cancel()
				}
			}}
		},
	}

	if err := New(a, prov, tiers, "test-owner", nil).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Leases != 0 {
		t.Errorf("%d leases still escrowed after shutdown, holding %d vCPU",
			usage.Leases, usage.VCPU)
	}
}

// Every tier's scale set is reconciled BEFORE any listener starts.
//
// Starting listeners as their scale sets appear looks equivalent and is not: a
// tier whose reconciliation fails would leave the others running, quietly
// splitting a budget with a tier that will never take work, and the operator
// sees a half-configured control plane reported as healthy.
func TestNoListenerStartsUntilEveryScaleSetExists(t *testing.T) {
	tiers := []config.Tier{
		tier("billet-4vcpu-a", 4),
		tier("billet-4vcpu-b", 4),
		tier("billet-4vcpu-c", 4),
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	var (
		mu         sync.Mutex
		reconciled int
		polled     bool
		early      bool
	)

	prov := &fakeProvisioner{
		onEnsure: func(string) error {
			// SLOW on purpose. With an instant fake the whole reconcile loop
			// finishes before the Go scheduler runs any listener goroutine, so a
			// build that starts listeners early is never observed doing it — the
			// test passes and discriminates nothing. Mutation testing is what
			// surfaced that; the test looked fine.
			time.Sleep(40 * time.Millisecond)

			mu.Lock()
			defer mu.Unlock()

			reconciled++

			return nil
		},
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) {
				mu.Lock()

				if reconciled < len(tiers) {
					early = true
				}

				polled = true

				mu.Unlock()
			}}
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	go func() {
		// Long enough for several poll rounds, short enough not to slow the suite.
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	if err := New(a, prov, tiers, "test-owner", nil).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if !polled {
		t.Fatal("no listener ever polled; this test proves nothing")
	}

	if early {
		t.Error("a listener polled before every tier's scale set had been reconciled")
	}
}

// A tier that cannot be reconciled stops the whole start-up, and says which one.
func TestReconciliationFailureStopsStartup(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a", 4), tier("billet-4vcpu-b", 4)}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	var polled atomic.Bool

	prov := &fakeProvisioner{
		onEnsure: func(label string) error {
			// Same reason as above: the failing tier must come far enough after the
			// first that a listener started early has a chance to poll.
			time.Sleep(40 * time.Millisecond)

			if label == "billet-4vcpu-b" {
				return errors.New("github said no")
			}

			return nil
		},
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) { polled.Store(true) }}
		},
	}

	err := New(a, prov, tiers, "test-owner", nil).Run(t.Context())
	if err == nil {
		t.Fatal("Run succeeded with a tier that could not be reconciled")
	}

	// Give any wrongly-started listener time to poll before concluding none did.
	time.Sleep(50 * time.Millisecond)

	if !strings.Contains(err.Error(), "billet-4vcpu-b") {
		t.Errorf("the error does not name the tier that failed: %v", err)
	}

	if polled.Load() {
		t.Error("a listener started despite a tier failing to reconcile")
	}
}

// fakeProvisioner stands in for GitHub's scale-set API.
type fakeProvisioner struct {
	onEnsure   func(label string) error
	newSession func(label string) Session

	mu   sync.Mutex
	next int
}

func (f *fakeProvisioner) EnsureScaleSet(_ context.Context, name, group string, _ []string) (*ScaleSet, error) {
	if f.onEnsure != nil {
		if err := f.onEnsure(name); err != nil {
			return nil, err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.next++

	return &ScaleSet{ID: f.next, Name: name, Group: group}, nil
}

func (f *fakeProvisioner) Session(_ context.Context, _ int, _ string) (Session, error) {
	if f.newSession == nil {
		return &fakeSession{}, nil
	}

	return f.newSession(""), nil
}
