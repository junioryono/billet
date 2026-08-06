package alloc

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func tier(label string, vcpu int, mem config.ByteSize) config.Tier {
	return config.Tier{
		Label:    label,
		Provider: config.ProviderFirecracker,
		GuestOS:  config.GuestLinux,
		VCPU:     vcpu,
		Memory:   mem,
		Image:    "ubuntu-2404-x64",
	}
}

func newAllocator(t *testing.T, limits Limits, tiers []config.Tier, opts ...Option) *Allocator {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	a, err := New(db, limits, tiers, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return a
}

func TestReserveHoldsCapacity(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	if n, err := a.Advertisable(ctx, "small"); err != nil || n != 4 {
		t.Fatalf("Advertisable = %d, %v; want 4", n, err)
	}

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The point of escrowing: what a listener may advertise drops immediately,
	// before any job is assigned or any VM booted.
	if n, err := a.Advertisable(ctx, "small"); err != nil || n != 3 {
		t.Fatalf("after Reserve, Advertisable = %d, %v; want 3", n, err)
	}

	usage, err := a.Usage(ctx)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.VCPU != 4 || usage.Memory != 16*config.GiB || usage.Leases != 1 {
		t.Errorf("usage = %+v, want 4 vCPU / 16GiB / 1 lease", usage)
	}

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if n, err := a.Advertisable(ctx, "small"); err != nil || n != 4 {
		t.Fatalf("after Release, Advertisable = %d, %v; want 4", n, err)
	}
}

// Capacity is a vector. A tier can be blocked by memory while cores are free,
// and treating either as "the" limit overcommits the other.
func TestMemoryCanBindBeforeVCPU(t *testing.T) {
	a := newAllocator(t,
		// 64 cores, but only 32GiB: memory allows 2, cores would allow 16.
		Limits{MaxVCPU: 64, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("fat", 4, 16*config.GiB)})

	ctx := t.Context()

	if n, err := a.Advertisable(ctx, "fat"); err != nil || n != 2 {
		t.Fatalf("Advertisable = %d, %v; want 2 (memory-bound, not core-bound)", n, err)
	}

	for i := range 2 {
		if _, err := a.Reserve(ctx, "fat"); err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
	}

	if _, err := a.Reserve(ctx, "fat"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("third Reserve err = %v, want ErrNoCapacity", err)
	}
}

// Two tier listeners sharing one machine is the case the allocator exists for.
// Reserving against tier A must shrink what tier B can advertise.
func TestTiersShareOneGlobalCeiling(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{
			tier("small", 4, 16*config.GiB),
			tier("large", 8, 32*config.GiB),
		})

	ctx := t.Context()

	if n, _ := a.Advertisable(ctx, "large"); n != 2 {
		t.Fatalf("large Advertisable = %d, want 2", n)
	}

	if _, err := a.Reserve(ctx, "small"); err != nil {
		t.Fatalf("Reserve small: %v", err)
	}

	// 12 vCPU and 48GiB remain, so only one large fits.
	if n, _ := a.Advertisable(ctx, "large"); n != 1 {
		t.Errorf("after reserving small, large Advertisable = %d, want 1", n)
	}
}

// THE race this package exists to prevent. Many concurrent reservations against
// a machine with room for four must produce exactly four, never more — the
// check and the insert have to be one transaction, not a read then a write.
func TestConcurrentReservationsNeverOvercommit(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	const attempts = 64

	var (
		wg        sync.WaitGroup
		granted   atomic.Int32
		refused   atomic.Int32
		otherErrs atomic.Int32
	)

	for range attempts {
		wg.Add(1)

		go func() {
			defer wg.Done()

			switch _, err := a.Reserve(ctx, "small"); {
			case err == nil:
				granted.Add(1)
			case errors.Is(err, ErrNoCapacity):
				refused.Add(1)
			default:
				otherErrs.Add(1)
				t.Errorf("unexpected Reserve error: %v", err)
			}
		}()
	}

	wg.Wait()

	if n := otherErrs.Load(); n != 0 {
		t.Fatalf("%d reservations failed for unexpected reasons", n)
	}

	if g := granted.Load(); g != 4 {
		t.Fatalf("granted %d reservations, want exactly 4 — the machine was overcommitted", g)
	}

	if r := refused.Load(); r != attempts-4 {
		t.Errorf("refused %d, want %d", r, attempts-4)
	}

	usage, err := a.Usage(ctx)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.VCPU > 16 || usage.Memory > 64*config.GiB {
		t.Errorf("usage %+v exceeds the ceiling", usage)
	}
}

func TestPerTierConcurrencyCap(t *testing.T) {
	capped := tier("capped", 1, config.GiB)
	capped.MaxConcurrent = 2

	a := newAllocator(t,
		Limits{MaxVCPU: 64, MaxMemory: 64 * config.GiB},
		[]config.Tier{capped})

	ctx := t.Context()

	if n, _ := a.Advertisable(ctx, "capped"); n != 2 {
		t.Fatalf("Advertisable = %d, want 2 (the per-tier cap, not the machine)", n)
	}

	for range 2 {
		if _, err := a.Reserve(ctx, "capped"); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	}

	if _, err := a.Reserve(ctx, "capped"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("Reserve past the cap = %v, want ErrNoCapacity", err)
	}
}

// Apple's limit belongs to the physical Mac, not to a tier. Two individually
// legal macOS tiers pinned to one node still share one machine, so reserving on
// either must consume the same two slots.
func TestMacOSLimitIsPerHostAcrossTiers(t *testing.T) {
	six := config.Tier{
		Label: "mac-6", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
		Node: "mac-mini-1", VCPU: 6, Memory: 24 * config.GiB, Image: "macos-26",
	}
	twelve := config.Tier{
		Label: "mac-12", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
		Node: "mac-mini-1", VCPU: 12, Memory: 48 * config.GiB, Image: "macos-26",
	}

	a := newAllocator(t,
		Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB},
		[]config.Tier{six, twelve})

	ctx := t.Context()

	if n, _ := a.Advertisable(ctx, "mac-6"); n != config.MacOSVMLimit {
		t.Fatalf("mac-6 Advertisable = %d, want %d", n, config.MacOSVMLimit)
	}

	// One guest from each tier fills the host's two slots.
	if _, err := a.Reserve(ctx, "mac-6"); err != nil {
		t.Fatalf("Reserve mac-6: %v", err)
	}

	if _, err := a.Reserve(ctx, "mac-12"); err != nil {
		t.Fatalf("Reserve mac-12: %v", err)
	}

	if n, _ := a.Advertisable(ctx, "mac-6"); n != 0 {
		t.Errorf("mac-6 Advertisable = %d after two guests on the host, want 0", n)
	}

	if _, err := a.Reserve(ctx, "mac-6"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("a third macOS guest was allowed on one host: %v", err)
	}
}

// A Linux tier on the same Mac is NOT subject to Apple's macOS-guest limit.
func TestLinuxGuestsOnAMacAreNotCapped(t *testing.T) {
	mac := config.Tier{
		Label: "mac-6", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
		Node: "mac-mini-1", VCPU: 6, Memory: 24 * config.GiB, Image: "macos-26",
	}
	linux := config.Tier{
		Label: "linux-arm", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		Node: "mac-mini-1", VCPU: 4, Memory: 12 * config.GiB, Image: "ubuntu-2404-arm64",
	}

	a := newAllocator(t,
		Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB},
		[]config.Tier{mac, linux})

	ctx := t.Context()

	for range 5 {
		if _, err := a.Reserve(ctx, "linux-arm"); err != nil {
			t.Fatalf("Reserve linux-arm: %v", err)
		}
	}

	// Five Linux guests must not have consumed any macOS licence slot.
	if n, _ := a.Advertisable(ctx, "mac-6"); n != config.MacOSVMLimit {
		t.Errorf("mac-6 Advertisable = %d after 5 Linux guests, want %d", n, config.MacOSVMLimit)
	}
}

func TestLifecycleTransitions(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 111, 222); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	for _, next := range []Phase{PhaseLaunching, PhaseOnline, PhaseBusy} {
		if err := a.Advance(ctx, lease.ID, lease.Epoch, next); err != nil {
			t.Fatalf("Advance to %s: %v", next, err)
		}
	}

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Terminal releases capacity.
	if u, _ := a.Usage(ctx); u.Leases != 0 {
		t.Errorf("usage after release = %+v, want no open leases", u)
	}
}

// A lease that has released its capacity must never move backwards and
// re-acquire it — that is what a double-admit looks like from the inside.
func TestInvalidTransitionsAreRefused(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// capacity -> online skips assignment and launch.
	if err := a.Advance(ctx, lease.ID, lease.Epoch, PhaseOnline); !errors.Is(err, ErrBadTransition) {
		t.Errorf("capacity -> online = %v, want ErrBadTransition", err)
	}

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Backwards out of a terminal phase.
	if err := a.Advance(ctx, lease.ID, lease.Epoch, PhaseBusy); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("done -> busy = %v, want ErrLeaseNotFound", err)
	}
}

// Retrying a transition after a lost response must succeed, or a node that
// cannot see its own success will keep trying forever.
func TestTransitionsAreIdempotent(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	lease, _ := a.Reserve(ctx, "small")

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 1, 2); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 1, 2); err != nil {
		t.Errorf("repeated Assign = %v, want nil", err)
	}

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Releasing twice is how a retried cleanup looks.
	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Errorf("repeated Release = %v, want nil", err)
	}
}

// The fence is the difference between an orderly takeover and two concurrent
// owners of one slot. A holder that comes back after being reclaimed must find
// every write refused.
func TestReapFencesTheOldHolder(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)},
		WithClock(func() time.Time { return clock() }),
		WithLeaseTTL(30*time.Second))

	ctx := t.Context()

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Nothing expires while the holder is heartbeating.
	if n, err := a.Reap(ctx); err != nil || n != 0 {
		t.Fatalf("Reap before expiry = %d, %v; want 0", n, err)
	}

	now = now.Add(31 * time.Second)

	n, err := a.Reap(ctx)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if n != 1 {
		t.Fatalf("reaped %d leases, want 1", n)
	}

	// Capacity came back.
	if got, _ := a.Advertisable(ctx, "small"); got != 4 {
		t.Errorf("Advertisable after reap = %d, want 4", got)
	}

	// And the old holder is fenced out — every write refused.
	if err := a.Heartbeat(ctx, lease.ID, lease.Epoch); err == nil {
		t.Error("the reclaimed holder's heartbeat succeeded; the fence is not working")
	}

	if err := a.Advance(ctx, lease.ID, lease.Epoch, PhaseLaunching); err == nil {
		t.Error("the reclaimed holder advanced a lease it no longer owns")
	}
}

// A heartbeat is what proves a holder is alive; it must actually push expiry out.
func TestHeartbeatExtendsTheLease(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)},
		WithClock(func() time.Time { return clock() }),
		WithLeaseTTL(30*time.Second))

	ctx := t.Context()

	lease, _ := a.Reserve(ctx, "small")

	now = now.Add(20 * time.Second)

	if err := a.Heartbeat(ctx, lease.ID, lease.Epoch); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	// Past the ORIGINAL expiry, but inside the extended one.
	now = now.Add(20 * time.Second)

	if n, err := a.Reap(ctx); err != nil || n != 0 {
		t.Fatalf("Reap after heartbeat = %d, %v; want 0 — the extension did not take", n, err)
	}
}

// A stale epoch must be refused even while the lease is perfectly healthy: this
// is the case where a partitioned holder returns and the lease has since been
// reclaimed and re-issued.
func TestStaleEpochIsRefused(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	lease, _ := a.Reserve(ctx, "small")

	if err := a.Heartbeat(ctx, lease.ID, lease.Epoch+1); !errors.Is(err, ErrFenced) {
		t.Errorf("future epoch = %v, want ErrFenced", err)
	}

	if err := a.Advance(ctx, lease.ID, lease.Epoch+7, PhaseAssigned); !errors.Is(err, ErrFenced) {
		t.Errorf("stale epoch = %v, want ErrFenced", err)
	}
}

func TestUnknownTierIsRejected(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	if _, err := a.Reserve(ctx, "nope"); !errors.Is(err, ErrUnknownTier) {
		t.Errorf("Reserve unknown tier = %v, want ErrUnknownTier", err)
	}

	if _, err := a.Advertisable(ctx, "nope"); !errors.Is(err, ErrUnknownTier) {
		t.Errorf("Advertisable unknown tier = %v, want ErrUnknownTier", err)
	}
}

// Without a ceiling there is nothing to escrow against, so the allocator refuses
// to exist rather than silently admitting everything.
func TestNewRequiresPositiveLimits(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	for name, limits := range map[string]Limits{
		"no vcpu":   {MaxVCPU: 0, MaxMemory: 64 * config.GiB},
		"no memory": {MaxVCPU: 16, MaxMemory: 0},
		"negative":  {MaxVCPU: -1, MaxMemory: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(db, limits, nil); err == nil {
				t.Error("New accepted a non-positive ceiling")
			}
		})
	}
}

// A finished lease must leave a record, or "why did this job queue" is
// unanswerable the moment the lease row stops being open.
func TestReleaseArchivesToHistory(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	lease, _ := a.Reserve(ctx, "small")

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 777, 888); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var (
		tierName   string
		runID      int64
		conclusion string
	)

	err := a.db.Reader().QueryRowContext(ctx,
		`SELECT tier, run_id, conclusion FROM job_history WHERE lease_id = ?`, lease.ID).
		Scan(&tierName, &runID, &conclusion)
	if err != nil {
		t.Fatalf("read job_history: %v", err)
	}

	if tierName != "small" || runID != 777 || conclusion != string(PhaseDone) {
		t.Errorf("history = (%s, %d, %s), want (small, 777, done)", tierName, runID, conclusion)
	}
}
