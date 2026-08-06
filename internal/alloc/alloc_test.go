package alloc

import (
	"database/sql"
	"errors"
	"strings"
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

// headroom asserts the query succeeded before returning the number.
//
// `n, _ := a.Headroom(...)` reads harmlessly and is not: Headroom returns 0
// alongside an error, so every assertion of the form `n != 0` passes when the
// query FAILS. That is the same defect that made two Usage assertions vacuous,
// and errcheck cannot catch it here because it is excluded for test files.
func headroom(t *testing.T, a *Allocator, tier string) int {
	t.Helper()

	n, err := a.Headroom(t.Context(), tier)
	if err != nil {
		t.Fatalf("Headroom(%s): %v", tier, err)
	}

	return n
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

	if n := headroom(t, a, "small"); n != 4 {
		t.Fatalf("Headroom = %d, want 4", n)
	}

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The point of escrowing: what a listener may advertise drops immediately,
	// before any job is assigned or any VM booted.
	if n := headroom(t, a, "small"); n != 3 {
		t.Fatalf("after Reserve, Headroom = %d, %v; want 3", n, err)
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

	if n := headroom(t, a, "small"); n != 4 {
		t.Fatalf("after Release, Headroom = %d, %v; want 4", n, err)
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

	if n := headroom(t, a, "fat"); n != 2 {
		t.Fatalf("Headroom = %d, want 2 (memory-bound, not core-bound)", n)
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

	if n := headroom(t, a, "large"); n != 2 {
		t.Fatalf("large Headroom = %d, want 2", n)
	}

	if _, err := a.Reserve(ctx, "small"); err != nil {
		t.Fatalf("Reserve small: %v", err)
	}

	// 12 vCPU and 48GiB remain, so only one large fits.
	if n := headroom(t, a, "large"); n != 1 {
		t.Errorf("after reserving small, large Headroom = %d, want 1", n)
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

	if n := headroom(t, a, "capped"); n != 2 {
		t.Fatalf("Headroom = %d, want 2 (the per-tier cap, not the machine)", n)
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

	if n := headroom(t, a, "mac-6"); n != config.DefaultMacOSVMLimit {
		t.Fatalf("mac-6 Headroom = %d, want %d", n, config.DefaultMacOSVMLimit)
	}

	// One guest from each tier fills the host's two slots.
	if _, err := a.Reserve(ctx, "mac-6"); err != nil {
		t.Fatalf("Reserve mac-6: %v", err)
	}

	if _, err := a.Reserve(ctx, "mac-12"); err != nil {
		t.Fatalf("Reserve mac-12: %v", err)
	}

	if n := headroom(t, a, "mac-6"); n != 0 {
		t.Errorf("mac-6 Headroom = %d after two guests on the host, want 0", n)
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
	if n := headroom(t, a, "mac-6"); n != config.DefaultMacOSVMLimit {
		t.Errorf("mac-6 Headroom = %d after 5 Linux guests, want %d", n, config.DefaultMacOSVMLimit)
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

	// Binding is part of the lifecycle, not an optional extra: launching means a
	// host is already bringing the instance up. This test previously skipped it,
	// which is what made Bind's placement checks routable-around.
	registerNode(t, a, "epyc-1", config.ProviderFirecracker)

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	for _, next := range []Phase{PhaseLaunching, PhaseOnline, PhaseBusy} {
		if err := a.Advance(ctx, lease.ID, lease.Epoch, next); err != nil {
			t.Fatalf("Advance to %s: %v", next, err)
		}
	}

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Terminal releases capacity. The error is checked: a failed query returns a
	// zero-valued Usage, which would satisfy this for the wrong reason.
	u, err := a.Usage(ctx)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if u.Leases != 0 {
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
	if got := headroom(t, a, "small"); got != 4 {
		t.Errorf("Headroom after reap = %d, want 4", got)
	}

	// The old holder must be FENCED, specifically — not merely refused.
	//
	// Asserting "some error" is not enough, and this test used to do exactly
	// that: reaping also makes the phase terminal, which produces
	// ErrLeaseNotFound on its own. Deleting `epoch = epoch + 1` from Reap left
	// the test green while the fence was gone entirely. Requiring ErrFenced is
	// what makes the epoch bump observable.
	if err := a.Heartbeat(ctx, lease.ID, lease.Epoch); !errors.Is(err, ErrFenced) {
		t.Errorf("reclaimed holder's heartbeat = %v, want ErrFenced", err)
	}

	if err := a.Advance(ctx, lease.ID, lease.Epoch, PhaseLaunching); !errors.Is(err, ErrFenced) {
		t.Errorf("reclaimed holder's advance = %v, want ErrFenced", err)
	}

	// And directly: the stored epoch advanced.
	var stored int64
	if err := a.db.Reader().QueryRowContext(ctx,
		`SELECT epoch FROM leases WHERE id = ?`, lease.ID).Scan(&stored); err != nil {
		t.Fatalf("read epoch: %v", err)
	}

	if stored <= lease.Epoch {
		t.Errorf("stored epoch = %d, want greater than the holder's %d", stored, lease.Epoch)
	}
}

// Escrow is what a listener advertises, because reading headroom and then
// advertising it leaves a gap where two listeners promise the same slots.
func TestEscrowReservesWhatItReports(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	// Asking for more than fits takes what fits — an ordinary outcome.
	leases, err := a.Escrow(ctx, "small", 10)
	if err != nil {
		t.Fatalf("Escrow: %v", err)
	}

	if len(leases) != 4 {
		t.Fatalf("escrowed %d leases, want 4", len(leases))
	}

	// Everything returned is already held, so a second listener sees nothing.
	more, err := a.Escrow(ctx, "small", 4)
	if err != nil {
		t.Fatalf("second Escrow: %v", err)
	}

	if len(more) != 0 {
		t.Errorf("second Escrow took %d leases; the first advertisement was not reserved", len(more))
	}
}

// Two listeners escrowing concurrently must not collectively advertise more than
// the machine holds. This is the failure Headroom cannot prevent on its own.
func TestConcurrentEscrowsCannotOveradvertise(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{
			tier("small", 4, 16*config.GiB),
			tier("also-small", 4, 16*config.GiB),
		})

	ctx := t.Context()

	var (
		wg    sync.WaitGroup
		total atomic.Int32
	)

	for _, label := range []string{"small", "also-small"} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			leases, err := a.Escrow(ctx, label, 4)
			if err != nil {
				t.Errorf("Escrow %s: %v", label, err)
				return
			}

			total.Add(int32(len(leases)))
		}()
	}

	wg.Wait()

	// Four slots exist. Two listeners each advertising four would be eight.
	if n := total.Load(); n != 4 {
		t.Errorf("two listeners advertised %d slots in total, want 4", n)
	}
}

// A lease pinned to one Mac must not bind to another: its licence slot stays
// charged to the first host, so the second would accept guests past Apple's
// limit while every individual decision looked correct.
func TestBindRefusesTheWrongNode(t *testing.T) {
	mac := config.Tier{
		Label: "mac-6", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
		Node: "mac-mini-1", VCPU: 6, Memory: 24 * config.GiB, Image: "macos-26",
	}

	a := newAllocator(t,
		Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB},
		[]config.Tier{mac})

	ctx := t.Context()

	registerNode(t, a, "mac-mini-1", config.ProviderTart)
	registerNode(t, a, "mac-mini-2", config.ProviderTart)

	lease, err := a.Reserve(ctx, "mac-6")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-2"); !errors.Is(err, ErrWrongNode) {
		t.Errorf("bind to the wrong host = %v, want ErrWrongNode", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); err != nil {
		t.Fatalf("bind to the pinned host: %v", err)
	}

	// Repeating the same bind is a retry, not a conflict.
	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); err != nil {
		t.Errorf("repeated bind = %v, want nil", err)
	}

	// Rebinding elsewhere afterwards is still refused.
	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-2"); !errors.Is(err, ErrWrongNode) {
		t.Errorf("rebind = %v, want ErrWrongNode", err)
	}
}

// Placement is read from the lease's own columns, so changing the catalog
// underneath in-flight leases cannot reclassify them.
func TestMacOSAccountingSurvivesCatalogChange(t *testing.T) {
	mac := config.Tier{
		Label: "mac-6", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
		Node: "mac-mini-1", VCPU: 6, Memory: 24 * config.GiB, Image: "macos-26",
	}

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	first, err := New(db, Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB}, []config.Tier{mac})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()

	for range config.DefaultMacOSVMLimit {
		if _, err := first.Reserve(ctx, "mac-6"); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	}

	// A restart with the tier RENAMED. The old leases are still running on that
	// Mac and must still count against its licence.
	renamed := mac
	renamed.Label = "mac-6-renamed"

	second, err := New(db, Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB}, []config.Tier{renamed})
	if err != nil {
		t.Fatalf("New after rename: %v", err)
	}

	if n := headroom(t, second, "mac-6-renamed"); n != 0 {
		t.Errorf("Headroom = %d after a rename, want 0 — in-flight guests were reclassified", n)
	}
}

// registerNode inserts a node row so leases can satisfy the foreign key on bind.
//
// The provider is explicit because Bind compares a lease's recorded provider
// against what the host REGISTERED: a helper that always claimed one backend
// would make every test agree with itself by construction.
func registerNode(t *testing.T, a *Allocator, name string, provider config.ProviderKind) {
	t.Helper()

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES (?, ?, ?)`,
			name, string(provider), ts(time.Now()))

		return err
	}); err != nil {
		t.Fatalf("register node %s: %v", name, err)
	}
}

// A retry that contradicts what was recorded is a conflict, not a success:
// returning nil while keeping the original assignment leaves the caller
// believing a job is scheduled that nothing will ever run.
func TestAssignRejectsAContradictoryRetry(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	lease, _ := a.Reserve(ctx, "small")

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 1, 2); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 1, 2); err != nil {
		t.Errorf("identical retry = %v, want nil", err)
	}

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 99, 98); !errors.Is(err, ErrConflict) {
		t.Errorf("reassignment to a different job = %v, want ErrConflict", err)
	}
}

func TestReleaseDistinguishesMissingFromFinished(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	// A lease that never existed is not a completed cleanup.
	if err := a.Release(ctx, "no-such-lease", 0, PhaseDone); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("release of an unknown id = %v, want ErrLeaseNotFound", err)
	}

	lease, _ := a.Reserve(ctx, "small")

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Errorf("identical re-release = %v, want nil", err)
	}

	// Re-finishing with a DIFFERENT outcome contradicts recorded history.
	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseFailed); !errors.Is(err, ErrConflict) {
		t.Errorf("re-release as failed = %v, want ErrConflict", err)
	}
}

// A reaped lease is the one most worth investigating, so it must not lose its
// job attribution on the way into history.
func TestReapedLeaseKeepsItsAttribution(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)},
		WithClock(func() time.Time { return clock() }),
		WithLeaseTTL(30*time.Second))

	ctx := t.Context()

	lease, _ := a.Reserve(ctx, "small")

	if err := a.Assign(ctx, lease.ID, lease.Epoch, 555, 666); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	now = now.Add(90 * time.Second)

	if n, err := a.Reap(ctx); err != nil || n != 1 {
		t.Fatalf("Reap = %d, %v; want 1", n, err)
	}

	var (
		runID      sql.NullInt64
		jobID      sql.NullInt64
		conclusion string
		assignedAt sql.NullString
	)

	if err := a.db.Reader().QueryRowContext(ctx,
		`SELECT run_id, job_id, conclusion, assigned_at FROM job_history WHERE lease_id = ?`, lease.ID).
		Scan(&runID, &jobID, &conclusion, &assignedAt); err != nil {
		t.Fatalf("read job_history: %v", err)
	}

	if runID.Int64 != 555 || jobID.Int64 != 666 {
		t.Errorf("history lost attribution: run=%v job=%v", runID, jobID)
	}

	if conclusion != string(PhaseFailed) {
		t.Errorf("conclusion = %q, want failed", conclusion)
	}

	// The assignment time must be when the job was assigned, not when it was
	// reaped — otherwise queue duration is fabricated rather than measured.
	if !assignedAt.Valid {
		t.Fatal("assigned_at was never recorded")
	}

	if assignedAt.String >= ts(now) {
		t.Errorf("assigned_at %q is not before the reap time %q", assignedAt.String, ts(now))
	}
}

// New is exported and cannot prove its catalog came through config.Load, so it
// validates every precondition its own arithmetic depends on.
func TestNewRejectsCatalogsThatBreakInvariants(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	limits := Limits{MaxVCPU: 64, MaxMemory: 64 * config.GiB}

	cases := map[string][]config.Tier{
		"zero vcpu divides by zero":   {tier("t", 0, config.GiB)},
		"zero memory divides by zero": {{Label: "t", VCPU: 1, Memory: 0, GuestOS: config.GuestLinux}},
		"negative max_concurrent": {func() config.Tier {
			x := tier("t", 1, config.GiB)
			x.MaxConcurrent = -1

			return x
		}()},
		"macOS tier with no node": {{
			Label: "mac", GuestOS: config.GuestMacOS, Provider: config.ProviderTart,
			VCPU: 6, Memory: config.GiB,
		}},
		"macOS tier with a whitespace node": {{
			Label: "mac", GuestOS: config.GuestMacOS, Provider: config.ProviderTart,
			Node: "   ", VCPU: 6, Memory: config.GiB,
		}},
		"duplicate labels": {tier("t", 1, config.GiB), tier("t", 2, config.GiB)},
		"no label":         {tier("", 1, config.GiB)},
	}

	for name, tiers := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(db, limits, tiers); err == nil {
				t.Error("New accepted a catalog that breaks an allocator invariant")
			}
		})
	}
}

// A non-positive TTL creates leases that are already expired, so Reap recycles
// live capacity immediately; a nil clock panics on first use.
func TestNewValidatesOptions(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	limits := Limits{MaxVCPU: 64, MaxMemory: 64 * config.GiB}
	tiers := []config.Tier{tier("t", 1, config.GiB)}

	if _, err := New(db, limits, tiers, WithLeaseTTL(0)); err == nil {
		t.Error("New accepted a zero lease TTL")
	}

	if _, err := New(db, limits, tiers, WithLeaseTTL(-time.Second)); err == nil {
		t.Error("New accepted a negative lease TTL")
	}

	if _, err := New(db, limits, tiers, WithClock(nil)); err == nil {
		t.Error("New accepted a nil clock")
	}
}

// Expiry is compared as a SQL string, so the timestamp format has to make
// lexical order match chronological order for EVERY value.
//
// time.RFC3339Nano does not: it strips trailing zeros, so a zero-nanosecond
// timestamp renders "12:00:30Z" while a fractional one renders "12:00:30.5Z",
// and 'Z' (0x5A) sorts after '.' (0x2E). An expired lease then survives its TTL
// with nothing reporting it. The existing reap tests all used whole seconds on
// both sides, which is exactly why they could not see this.
func TestExpiryOrderingSurvivesFractionalSeconds(t *testing.T) {
	// A lease created on a whole second expires on a whole second...
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)},
		WithClock(func() time.Time { return clock() }),
		WithLeaseTTL(30*time.Second))

	ctx := t.Context()

	if _, err := a.Reserve(ctx, "small"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// ...and we reap half a second past it, which is where the stripped-zero
	// format compares backwards.
	now = now.Add(30*time.Second + 500*time.Millisecond)

	n, err := a.Reap(ctx)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if n != 1 {
		t.Fatalf("reaped %d leases, want 1 — expiry compared as a string is mis-ordering "+
			"fractional seconds, so capacity is held past its TTL", n)
	}
}

// Guard the format directly too, so a future edit back to a stdlib constant
// fails here rather than as a mysterious capacity leak.
func TestTimestampFormatIsFixedWidth(t *testing.T) {
	base := time.Date(2026, time.August, 6, 12, 0, 30, 0, time.UTC)

	whole := ts(base)
	frac := ts(base.Add(500 * time.Millisecond))

	if len(whole) != len(frac) {
		t.Fatalf("timestamps are not fixed width: %q (%d) vs %q (%d)",
			whole, len(whole), frac, len(frac))
	}

	if whole >= frac {
		t.Errorf("%q should sort before %q", whole, frac)
	}

	// Ordering must hold across a range of nanosecond values, including the ones
	// a stripped-zero format renders shortest.
	prev := ts(base)

	for _, ns := range []int{1, 999, 1_000_000, 100_000_000, 500_000_000, 999_999_999} {
		cur := ts(base.Add(time.Duration(ns)))
		if cur <= prev {
			t.Errorf("ns=%d: %q should sort after %q", ns, cur, prev)
		}

		prev = cur
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

	if _, err := a.Headroom(ctx, "nope"); !errors.Is(err, ErrUnknownTier) {
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

// --- Per-host macOS limits ------------------------------------------------
//
// How many macOS guests a host may run is a deployment decision: an operator
// may keep a slot free for interactive use, or run a Mac purely as an arm64
// Linux builder. The tests above cover the default; these cover the override.

func macTier(label string, vcpu int, mem config.ByteSize) config.Tier {
	return config.Tier{
		Label:    label,
		Provider: config.ProviderTart,
		GuestOS:  config.GuestMacOS,
		Node:     "mac-mini-1",
		VCPU:     vcpu,
		Memory:   mem,
		Image:    "macos-26",
	}
}

// macPolicy is a declared host with an explicit macOS guest limit.
func macPolicy(name string, limit int) map[string]config.NodePolicy {
	return map[string]config.NodePolicy{
		name: {Name: name, MacOSVMLimit: &limit},
	}
}

func TestMacOSLimitHonoursPerHostOverride(t *testing.T) {
	a := newAllocator(t,
		Limits{
			MaxVCPU: 256, MaxMemory: 512 * config.GiB,
			Nodes: macPolicy("mac-mini-1", 1),
		},
		[]config.Tier{macTier("mac-6", 6, 24*config.GiB)})

	ctx := t.Context()

	if n := headroom(t, a, "mac-6"); n != 1 {
		t.Fatalf("Headroom = %d, want 1 from the host's lowered limit", n)
	}

	if _, err := a.Reserve(ctx, "mac-6"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := a.Reserve(ctx, "mac-6"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("a second macOS guest was allowed on a host limited to 1: %v", err)
	}
}

// A host set to zero schedules nothing, even though the machine has ample cores
// and memory. config.Load rejects this pairing, but New is exported and cannot
// prove its catalog came through that path.
func TestMacOSLimitZeroSchedulesNoGuests(t *testing.T) {
	a := newAllocator(t,
		Limits{
			MaxVCPU: 256, MaxMemory: 512 * config.GiB,
			Nodes: macPolicy("mac-mini-1", 0),
		},
		[]config.Tier{macTier("mac-6", 6, 24*config.GiB)})

	ctx := t.Context()

	if n := headroom(t, a, "mac-6"); n != 0 {
		t.Errorf("Headroom = %d on a host that permits no macOS guests, want 0", n)
	}

	if _, err := a.Reserve(ctx, "mac-6"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("Reserve = %v on a zero-limit host, want ErrNoCapacity", err)
	}
}

// Limits is a value type but carries a map of policies that themselves carry
// slices. A caller keeping a reference could otherwise raise a host's cap, or
// widen its allowlist, after construction — moving the rules out from under
// leases already counted against them. Copying the map alone is not enough,
// because the GuestOS backing arrays would still be shared.
func TestNodePolicyIsCopiedAtConstruction(t *testing.T) {
	raised := 1
	limits := Limits{
		MaxVCPU: 256, MaxMemory: 512 * config.GiB,
		Nodes: map[string]config.NodePolicy{
			"mac-mini-1": {
				Name:         "mac-mini-1",
				GuestOS:      []config.GuestOS{config.GuestMacOS},
				MacOSVMLimit: &raised,
			},
		},
	}

	a := newAllocator(t, limits, []config.Tier{macTier("mac-6", 6, 24*config.GiB)})

	// Mutate every reachable path: the map entry, and the slice inside it.
	limits.Nodes["mac-mini-1"] = config.NodePolicy{Name: "mac-mini-1"}
	raised = 5

	if n := headroom(t, a, "mac-6"); n != 1 {
		t.Errorf("Headroom = %d after the caller mutated its map, want the value captured at New (1)", n)
	}
}

// The allowlist slice must be copied too, not just the map that holds it.
func TestNodePolicyGuestOSSliceIsCopied(t *testing.T) {
	allowlist := []config.GuestOS{config.GuestMacOS}
	limits := Limits{
		MaxVCPU: 256, MaxMemory: 512 * config.GiB,
		Nodes: map[string]config.NodePolicy{
			"mac-mini-1": {Name: "mac-mini-1", GuestOS: allowlist},
		},
	}

	linux := config.Tier{
		Label: "linux-arm", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 12 * config.GiB, Image: "ubuntu-2404-arm64",
	}

	a := newAllocator(t, limits, []config.Tier{linux})

	// Widening the caller's slice in place must not widen the allocator's rules.
	allowlist[0] = config.GuestLinux

	ctx := t.Context()

	registerNode(t, a, "mac-mini-1", config.ProviderTart)

	lease, err := a.Reserve(ctx, "linux-arm")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); !errors.Is(err, ErrGuestOSNotAllowed) {
		t.Errorf("bind = %v; the caller widened the allowlist after New", err)
	}
}

// The config guard cannot see every placement: an UNPINNED tier names no host,
// and a node that declares no provider cannot be reasoned about at load time.
// Bind is where the host is actually known, so it is the enforcement point —
// without it a macOS-only Mac could be handed a Linux guest by any scheduler
// that simply picked a node with free capacity.
func TestBindRefusesAGuestOSTheHostDisallows(t *testing.T) {
	linux := config.Tier{
		Label: "linux-arm", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 12 * config.GiB, Image: "ubuntu-2404-arm64",
	}

	a := newAllocator(t,
		Limits{
			MaxVCPU: 256, MaxMemory: 512 * config.GiB,
			Nodes: map[string]config.NodePolicy{
				"mac-mini-1": {Name: "mac-mini-1", GuestOS: []config.GuestOS{config.GuestMacOS}},
			},
		},
		[]config.Tier{linux})

	ctx := t.Context()

	registerNode(t, a, "mac-mini-1", config.ProviderTart)

	lease, err := a.Reserve(ctx, "linux-arm")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); !errors.Is(err, ErrGuestOSNotAllowed) {
		t.Errorf("bind of a linux guest to a macos-only host = %v, want ErrGuestOSNotAllowed", err)
	}
}

// Tightening a host's allowlist must not turn an ALREADY-COMPLETED bind's retry
// into an error. The guest is running either way — refusing the repeat un-binds
// nothing, it just converts a harmless no-op into a hard failure that a node
// retrying after a transient error would read as "tear this job down".
//
// The policy gates NEW placements, so the idempotent repeat is answered first.
func TestBindRetryStaysIdempotentAfterPolicyTightens(t *testing.T) {
	linux := config.Tier{
		Label: "linux-arm", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 12 * config.GiB, Image: "ubuntu-2404-arm64",
	}

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	open := Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB}

	before, err := New(db, open, []config.Tier{linux})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()

	registerNode(t, before, "mac-mini-1", config.ProviderTart)

	lease, err := before.Reserve(ctx, "linux-arm")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := before.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// The operator now restricts that Mac to macOS and restarts.
	tightened := Limits{
		MaxVCPU: 256, MaxMemory: 512 * config.GiB,
		Nodes: map[string]config.NodePolicy{
			"mac-mini-1": {Name: "mac-mini-1", GuestOS: []config.GuestOS{config.GuestMacOS}},
		},
	}

	after, err := New(db, tightened, []config.Tier{linux})
	if err != nil {
		t.Fatalf("New after tightening: %v", err)
	}

	if err := after.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); err != nil {
		t.Errorf("retry of a completed bind = %v, want nil", err)
	}
}

// A host with no declared policy is unconstrained, which is what an existing
// deployment that never wrote a nodes section relies on.
func TestBindAllowsAnUndeclaredHost(t *testing.T) {
	linux := config.Tier{
		Label: "linux-arm", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 12 * config.GiB, Image: "ubuntu-2404-arm64",
	}

	a := newAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB}, []config.Tier{linux})

	ctx := t.Context()

	registerNode(t, a, "mac-mini-1", config.ProviderTart)

	lease, err := a.Reserve(ctx, "linux-arm")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); err != nil {
		t.Errorf("bind to an undeclared host = %v, want nil", err)
	}
}

// The guest OS is read from the lease's own column, so a catalog that changes
// underneath an in-flight lease cannot reclassify what it is allowed to bind
// to — the same reason target_node and macos_slot are stored rather than
// re-derived.
//
// Deliberately reserves a macOS lease rather than a Linux one. 'linux' is the
// schema's DEFAULT for the column, so a Linux lease would still read back as
// Linux even if insertLease stopped writing guest_os at all — the test would
// stay green with the persistence gone.
func TestBindReadsGuestOSFromTheLeaseNotTheCatalog(t *testing.T) {
	mac := config.Tier{
		Label: "shared-label", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
		Node: "mac-mini-1", VCPU: 6, Memory: 24 * config.GiB, Image: "macos-26",
	}

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	policy := map[string]config.NodePolicy{
		"mac-mini-1": {Name: "mac-mini-1", GuestOS: []config.GuestOS{config.GuestMacOS}},
	}
	limits := Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB, Nodes: policy}

	first, err := New(db, limits, []config.Tier{mac})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()

	registerNode(t, first, "mac-mini-1", config.ProviderTart)

	lease, err := first.Reserve(ctx, "shared-label")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The same label now describes a LINUX tier. The in-flight lease is still a
	// macOS guest, and the host permits only macOS — so a bind that consulted the
	// catalog instead of the lease would be refused.
	repurposed := mac
	repurposed.GuestOS = config.GuestLinux

	second, err := New(db, limits, []config.Tier{repurposed})
	if err != nil {
		t.Fatalf("New after repurpose: %v", err)
	}

	if err := second.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); err != nil {
		t.Errorf("bind = %v; the lease was reclassified by a catalog change", err)
	}
}

// Launching means a host is already bringing the instance up. A lease that
// reaches it unbound was placed by something that never told the allocator
// where — which makes Bind, and every placement check inside it, optional
// rather than an unavoidable gate.
func TestLaunchingRequiresABoundNode(t *testing.T) {
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

	if err := a.Advance(ctx, lease.ID, lease.Epoch, PhaseLaunching); !errors.Is(err, ErrNotPlaced) {
		t.Errorf("launching an unbound lease = %v, want ErrNotPlaced", err)
	}
}

// A lease whose placement facts cannot be verified must fail closed. Tolerating
// a blank provider would be a bypass rather than a courtesy: such a lease may
// still be UNBOUND, so it is not old work already placed — it is unplaced work
// whose backend nothing can check, free to bind to a host running anything.
func TestUnverifiableLegacyLeaseFailsClosed(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	registerNode(t, a, "epyc-1", config.ProviderFirecracker)

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Reproduce a row written before the provider column existed.
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE leases SET provider = '' WHERE id = ?`, lease.ID)

		return err
	}); err != nil {
		t.Fatalf("blank the provider: %v", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "epyc-1"); !errors.Is(err, ErrNotPlaceable) {
		t.Errorf("bind of a lease with no recorded provider = %v, want ErrNotPlaceable", err)
	}

	// Failing closed is only acceptable if the lease can still be cleaned up —
	// otherwise it holds capacity forever and the error tells the operator to do
	// something they cannot do. Release takes only terminal phases and Reap
	// archives directly, so neither consults the placement checks.
	if err := a.Release(ctx, lease.ID, lease.Epoch, PhaseFailed); err != nil {
		t.Errorf("releasing an unplaceable lease = %v, want nil; its capacity would be stranded", err)
	}

	// The error is checked: a failed query yields a zero-valued Usage, which
	// would satisfy this assertion for entirely the wrong reason.
	u, err := a.Usage(ctx)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if u.Leases != 0 {
		t.Errorf("usage after release = %+v, want no open leases", u)
	}
}

// New must refuse a catalog whose leases could not be placed. A blank provider
// makes them unverifiable; a NONBLANK invalid one is worse, comparing unequal to
// every registered provider and stranding the lease at bind time.
func TestNewRejectsUnplaceableTiers(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	for name, mutate := range map[string]func(*config.Tier){
		"blank provider":   func(t *config.Tier) { t.Provider = "" },
		"unknown provider": func(t *config.Tier) { t.Provider = config.ProviderKind("bogus") },
		"blank guest_os":   func(t *config.Tier) { t.GuestOS = "" },
		"unknown guest_os": func(t *config.Tier) { t.GuestOS = config.GuestOS("plan9") },
	} {
		t.Run(name, func(t *testing.T) {
			bad := tier("small", 4, 16*config.GiB)
			mutate(&bad)

			if _, err := New(db, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
				[]config.Tier{bad}); err == nil {
				t.Errorf("New accepted a tier with %s", name)
			}
		})
	}
}

// Binding is NOT launching: a lease can be bound while still in `capacity`, so a
// policy tightened in between must not let the instance start anyway. The check
// has to run against policy in force at the launch boundary, not be trusted from
// whenever the bind happened.
func TestLaunchingRevalidatesCurrentPolicy(t *testing.T) {
	linux := config.Tier{
		Label: "linux-arm", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 12 * config.GiB, Image: "ubuntu-2404-arm64",
	}

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	open := Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB}

	before, err := New(db, open, []config.Tier{linux})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()

	registerNode(t, before, "mac-mini-1", config.ProviderTart)

	lease, err := before.Reserve(ctx, "linux-arm")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Bound while still in `capacity` — nothing is running yet.
	if err := before.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	tightened := Limits{
		MaxVCPU: 256, MaxMemory: 512 * config.GiB,
		Nodes: map[string]config.NodePolicy{
			"mac-mini-1": {Name: "mac-mini-1", GuestOS: []config.GuestOS{config.GuestMacOS}},
		},
	}

	after, err := New(db, tightened, []config.Tier{linux})
	if err != nil {
		t.Fatalf("New after tightening: %v", err)
	}

	if err := after.Assign(ctx, lease.ID, lease.Epoch, 1, 2); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := after.Advance(ctx, lease.ID, lease.Epoch, PhaseLaunching); !errors.Is(err, ErrGuestOSNotAllowed) {
		t.Errorf("launching onto a now-forbidden host = %v, want ErrGuestOSNotAllowed", err)
	}
}

// A row written by an older binary can sit in `launching` with no bound node.
// Gating only the launching EDGE would let it walk on to online untouched.
func TestOnlineRequiresABoundNode(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Reproduce the legacy row directly: launching, never bound.
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = 'launching', node = NULL WHERE id = ?`, lease.ID)

		return err
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err := a.Advance(ctx, lease.ID, lease.Epoch, PhaseOnline); !errors.Is(err, ErrNotPlaced) {
		t.Errorf("online from an unbound launching row = %v, want ErrNotPlaced", err)
	}
}

// runningPhases is spelled out rather than derived from requiresPlacement,
// which is the production predicate these tests exist to guard. Deriving it
// meant dropping PhaseBusy from requiresPlacement made the tests skip every
// edge into busy and stay green — the mutation silencing its own detector.
var runningPhases = []Phase{PhaseLaunching, PhaseOnline, PhaseBusy}

// The launch gate is only unavoidable while launching is the ONLY way into the
// running phases. Asserting that here means adding an assigned -> online edge
// breaks this test rather than silently opening a route around the gate.
func TestRunningPhasesAreReachableOnlyThroughLaunching(t *testing.T) {
	running := make(map[Phase]bool, len(runningPhases))
	for _, p := range runningPhases {
		running[p] = true
	}

	for from, allowed := range validTransitions {
		if running[from] {
			continue // movement WITHIN the running phases is expected
		}

		for _, to := range allowed {
			if running[to] && to != PhaseLaunching {
				t.Errorf("%s -> %s enters a running phase without passing through launching", from, to)
			}
		}
	}
}

// Each running phase must reject an unbound lease independently. A single test
// covering only the launching edge would leave online and busy unguarded, which
// is the route a legacy row or a deleted node opens.
func TestEveryRunningPhaseRejectsAnUnboundLease(t *testing.T) {
	for _, phase := range runningPhases {
		t.Run(string(phase), func(t *testing.T) {
			a := newAllocator(t,
				Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
				[]config.Tier{tier("small", 4, 16*config.GiB)})

			ctx := t.Context()

			lease, err := a.Reserve(ctx, "small")
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}

			// Seed the phase this transition would arrive from, with no node —
			// the shape an older binary or a deleted node leaves behind.
			from := map[Phase]string{
				PhaseLaunching: "assigned",
				PhaseOnline:    "launching",
				PhaseBusy:      "online",
			}[phase]

			if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`UPDATE leases SET phase = ?, node = NULL WHERE id = ?`, from, lease.ID)

				return err
			}); err != nil {
				t.Fatalf("seed %s: %v", from, err)
			}

			if err := a.Advance(ctx, lease.ID, lease.Epoch, phase); !errors.Is(err, ErrNotPlaced) {
				t.Errorf("entering %s unbound = %v, want ErrNotPlaced", phase, err)
			}
		})
	}
}

// The recovery advice depends on the phase the lease is in NOW. From `assigned`
// a bind is exactly what is missing and Bind accepts it; from a running phase
// the lease is an orphan that Bind refuses to adopt, so identical advice would
// send the operator into a second refusal. Telling both cases to release would
// destroy work that only needed binding.
func TestOrphanAdviceDependsOnTheCurrentPhase(t *testing.T) {
	for name, tc := range map[string]struct {
		seed, to Phase
		want     string
	}{
		"assigned needs a bind":  {PhaseAssigned, PhaseLaunching, "bind it to a node first"},
		"launching is an orphan": {PhaseLaunching, PhaseOnline, "release it"},
		"online is an orphan":    {PhaseOnline, PhaseBusy, "release it"},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAllocator(t,
				Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
				[]config.Tier{tier("small", 4, 16*config.GiB)})

			ctx := t.Context()

			lease, err := a.Reserve(ctx, "small")
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}

			if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`UPDATE leases SET phase = ?, node = NULL WHERE id = ?`, string(tc.seed), lease.ID)

				return err
			}); err != nil {
				t.Fatalf("seed %s: %v", tc.seed, err)
			}

			err = a.Advance(ctx, lease.ID, lease.Epoch, tc.to)
			if !errors.Is(err, ErrNotPlaced) {
				t.Fatalf("Advance = %v, want ErrNotPlaced", err)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("advice for a %s lease = %v, want it to recommend %q", tc.seed, err, tc.want)
			}
		})
	}
}

// A repeated Advance into a running phase is an AUTHORIZATION, not an
// acknowledgement: its caller reads nil as "you may launch". Answering it
// without checking lets a recovery loop get permission for a placement that is
// no longer legal — or, for a pre-v7 row, was never verifiable.
func TestSamePhaseAdvanceStillChecksPlacement(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	registerNode(t, a, "epyc-1", config.ProviderFirecracker)

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// A bound pre-v7 row already sitting in launching: no recorded provider, so
	// its placement cannot be verified.
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = 'launching', node = 'epyc-1', provider = '' WHERE id = ?`,
			lease.ID)

		return err
	}); err != nil {
		t.Fatalf("seed legacy launching row: %v", err)
	}

	if err := a.Advance(ctx, lease.ID, lease.Epoch, PhaseLaunching); !errors.Is(err, ErrNotPlaceable) {
		t.Errorf("re-advancing an unverifiable launching lease = %v, want ErrNotPlaceable", err)
	}
}

// Deleting a node sets leases.node to NULL, so a lease still running on it LOOKS
// unbound while its original host keeps a valid epoch and keeps heartbeating.
// Binding it elsewhere would make two hosts owners of one slot.
func TestBindRefusesToAdoptAnAlreadyRunningLease(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)})

	ctx := t.Context()

	registerNode(t, a, "epyc-1", config.ProviderFirecracker)
	registerNode(t, a, "epyc-2", config.ProviderFirecracker)

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Running, then its node row is deleted: ON DELETE SET NULL blanks the column.
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = 'online', node = NULL WHERE id = ?`, lease.ID)

		return err
	}); err != nil {
		t.Fatalf("seed orphaned running lease: %v", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "epyc-2"); !errors.Is(err, ErrWrongNode) {
		t.Errorf("adopting a running lease onto a new host = %v, want ErrWrongNode", err)
	}
}

// A Firecracker lease cannot run on a Tart host. The comparison is against the
// provider the node REGISTERED, not one a catalog claims about it.
func TestBindRefusesANodeRunningAnotherProvider(t *testing.T) {
	a := newAllocator(t,
		Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{tier("small", 4, 16*config.GiB)}) // firecracker

	ctx := t.Context()

	registerNode(t, a, "mac-mini-1", config.ProviderTart)

	lease, err := a.Reserve(ctx, "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(ctx, lease.ID, lease.Epoch, "mac-mini-1"); !errors.Is(err, ErrWrongProvider) {
		t.Errorf("bind of a firecracker lease to a tart host = %v, want ErrWrongProvider", err)
	}
}

func TestNewRejectsNegativeMacOSLimit(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	defer db.Close()

	limits := Limits{
		MaxVCPU: 16, MaxMemory: 64 * config.GiB,
		Nodes: macPolicy("mac-mini-1", -1),
	}

	if _, err := New(db, limits, nil); err == nil {
		t.Error("New accepted a negative per-host macOS limit")
	}
}
