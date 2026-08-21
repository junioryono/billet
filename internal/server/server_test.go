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
		tier("billet-4vcpu-a"),
		tier("billet-4vcpu-b"),
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Counting POLLS proved nothing: deleting refillEscrow entirely still yields
	// six zero-capacity polls and a final usage of zero, so the test passed
	// against an implementation that never escrowed anything to release. What has
	// to be observed is a POSITIVE advertisement from every tier.
	var (
		mu         sync.Mutex
		advertised = map[string]bool{}
	)

	// At least ONE tier, not every tier. Escrow is first-come and the budget is
	// shared, so a tier that polls first can legitimately take all of it and leave
	// the others advertising zero — see TestAGreedyTierCanTakeTheWholeBudget.
	// Requiring every tier here asserts a fairness property the allocator does not
	// currently have, which is how this test failed once it was made strict.
	sawCapacity := func(label string, capacity int) bool {
		mu.Lock()
		defer mu.Unlock()

		if capacity > 0 {
			advertised[label] = true
		}

		return len(advertised) > 0
	}

	prov := &fakeProvisioner{
		newSession: func(label string) Session {
			return &fakeSession{onPoll: func(capacity int) {
				if sawCapacity(label, capacity) {
					cancel()
				}
			}}
		},
	}

	if err := New(a, prov, tiers, "test-owner", nil).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	held := len(advertised)
	mu.Unlock()

	if held == 0 {
		t.Fatal("no tier ever advertised capacity; nothing was escrowed, so nothing was released")
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
		tier("billet-4vcpu-a"),
		tier("billet-4vcpu-b"),
		tier("billet-4vcpu-c"),
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

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// CANCELLED ON THE CONDITION, NOT ON A CLOCK, and that is what makes this test
	// deterministic rather than usually-true.
	//
	// It used to sleep 200ms and then cancel. Reconciliation deliberately takes
	// 40ms per tier — three tiers, 120ms — so a listener had roughly 80ms to be
	// scheduled and poll once. Under the full suite with -race and
	// -covermode=atomic on a loaded machine that margin disappears, `polled` stays
	// false, and the test fails with "no listener ever polled; this test proves
	// nothing" — a red that names the test's own guard rather than anything about
	// the code. Observed on CI; it is the transient that had been appearing
	// unattributed.
	//
	// Waiting for the poll instead keeps every property: `early` is still recorded
	// by onPoll the moment it happens, and the deadline below is a WATCHDOG on a
	// stall rather than a budget for the work, so a genuine failure to poll still
	// fails — just later.
	go func() {
		defer cancel()

		deadline := time.Now().Add(20 * time.Second)

		for {
			mu.Lock()
			done := polled
			mu.Unlock()

			if done || time.Now().After(deadline) {
				return
			}

			time.Sleep(time.Millisecond)
		}
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

func TestTrustedTierRefusesRunnerGroupPolicyDriftBeforeReconciliation(t *testing.T) {
	tr := tier("billet-4vcpu-a")
	tr.Trust = config.WorkloadTrusted
	tr.RunnerGroup = "trusted"
	tr.Workflows = []string{"acme/api/.github/workflows/ci.yml@refs/heads/main"}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 64 * config.GiB},
		[]config.Tier{tr})
	prov := &fakeProvisioner{validateErr: errors.New("workflow restriction drifted")}
	err := New(a, prov, []config.Tier{tr}, "test-owner", nil).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "workflow restriction drifted") {
		t.Fatalf("trusted tier policy error = %v", err)
	}
	if prov.next != 0 {
		t.Fatalf("reconciled %d scale sets after trusted policy refusal", prov.next)
	}
}

// A tier that cannot be reconciled stops the whole start-up, and says which one.
func TestReconciliationFailureStopsStartup(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a"), tier("billet-4vcpu-b")}

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

// The reaper must never reclaim capacity a live listener is still advertising.
//
// This pair nearly shipped broken in BOTH directions. Without a reaper, a hard
// kill leaves escrowed leases in the database forever and every restart
// advertises less than the host can do. With a reaper and no heartbeats, escrow
// held across two long polls — 100 seconds against a 90 second TTL — is
// reclaimed underneath a listener that is still advertising it, and another tier
// escrows the same capacity. Enabling one without the other converts a leak into
// a double-admission, which is worse.
//
// Driven with a TTL far shorter than the poll cadence so the race is certain
// rather than occasional.
func TestReaperDoesNotReclaimCapacityStillAdvertised(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(150*time.Millisecond))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		mu         sync.Mutex
		advertised []int
	)

	prov := &fakeProvisioner{
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(capacity int) {
				mu.Lock()
				advertised = append(advertised, capacity)
				mu.Unlock()

				// Slower than the TTL, so a listener that does not heartbeat loses
				// its escrow between polls.
				time.Sleep(80 * time.Millisecond)
			}}
		},
	}

	go func() {
		time.Sleep(1200 * time.Millisecond)
		cancel()
	}()

	// The reaper must actually FIRE inside this test, or it proves nothing about
	// the interaction it is named for.
	var reaps atomic.Int32

	observeReaps := ControlPlaneOption(func(s *Server) {
		s.onReap = func(int) { reaps.Add(1) }
	})

	srv := New(a, prov, tiers, "test-owner", nil,
		WithReapInterval(30*time.Millisecond), observeReaps)
	if err := srv.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Deleting the periodic reaper entirely used to pass every assertion below —
	// nothing reclaims, so nothing is over-advertised. A test named for what the
	// reaper must not do has to first establish that a reaper ran.
	if reaps.Load() == 0 {
		t.Fatal("the reaper never ran; this proves nothing about what it does or does not reclaim")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(advertised) < 4 {
		t.Fatalf("only %d polls; not enough to outlive the TTL and prove anything", len(advertised))
	}

	// Every advertisement after the first must still be backed. A listener whose
	// escrow was reaped re-escrows and the number climbs above what the budget
	// allows, or collapses to zero because the capacity went elsewhere.
	for i, capacity := range advertised {
		if capacity*tierVCPU > 8 {
			t.Errorf("poll %d advertised %d runners (%d vCPU) against an 8 vCPU budget",
				i, capacity, capacity*tierVCPU)
		}
	}

	// And the steady state is nonzero: escrow that is being renewed stays held,
	// so this listener keeps advertising rather than flapping to nothing.
	if advertised[len(advertised)-1] == 0 {
		t.Errorf("the listener ended up advertising nothing; escrow was not renewed: %v", advertised)
	}
}

// Escrow is FIRST-COME, so one tier can take the entire budget.
//
// Not a bug today, and not obviously right either — so it is written down as a
// test rather than left to be rediscovered. With a shared ceiling and no per-tier
// reservation, whichever listener polls first escrows everything it can use and
// the others advertise zero until it releases. A busy 4-vCPU tier can starve a
// 16-vCPU one indefinitely.
//
// Fixing it means per-tier floors or a fairer escrow, which is a scheduling
// decision rather than a correctness one. The invariant that matters — never
// advertising more than the budget — holds either way.
func TestAGreedyTierCanTakeTheWholeBudget(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a"), tier("billet-4vcpu-b")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	leases, err := a.Escrow(t.Context(), "billet-4vcpu-a", 2)
	if err != nil {
		t.Fatalf("Escrow: %v", err)
	}

	if len(leases) != 2 {
		t.Fatalf("escrowed %d leases, want 2 (the whole budget)", len(leases))
	}

	room, err := a.Headroom(t.Context(), "billet-4vcpu-b")
	if err != nil {
		t.Fatalf("Headroom: %v", err)
	}

	if room != 0 {
		t.Errorf("the second tier has headroom %d while the first holds the whole budget", room)
	}
}

// fakeProvisioner stands in for GitHub's scale-set API.
type fakeProvisioner struct {
	onEnsure    func(label string) error
	newSession  func(label string) Session
	validateErr error
	validated   []string

	mu     sync.Mutex
	next   int
	labels map[int]string
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

	if f.labels == nil {
		f.labels = map[int]string{}
	}

	f.labels[f.next] = name

	return &ScaleSet{ID: f.next, Name: name, Group: group}, nil
}

func (f *fakeProvisioner) Session(_ context.Context, scaleSetID int, _ string) (Session, error) {
	if f.newSession == nil {
		return &fakeSession{}, nil
	}

	// The label the set was created under, so a test can tell the tiers apart.
	f.mu.Lock()
	label := f.labels[scaleSetID]
	f.mu.Unlock()

	return f.newSession(label), nil
}

func (*fakeProvisioner) RemoveRunner(context.Context, int64, string) error { return nil }

func (f *fakeProvisioner) ValidateTrustedRunnerGroup(_ context.Context, group string,
	_ []string,
) error {
	f.validated = append(f.validated, group)
	return f.validateErr
}
