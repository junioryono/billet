package alloc

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// holdFleet is two hosts of 16 vCPU and 64 GiB, a 16 vCPU tier that fits only on an
// empty one, a tier of 4 vCPU and a tier of 8 pinned to each, and a clock the test
// moves.
func holdFleet(t *testing.T, now *time.Time, extra ...config.Tier) *Allocator {
	t.Helper()

	shape := func(label string, vcpu int, node string) config.Tier {
		out := tier(label, vcpu, config.ByteSize(vcpu)*4*config.GiB)
		out.Provider = config.ProviderDocker
		out.Node = node

		return out
	}

	tiers := append([]config.Tier{
		shape("wide", 16, ""), shape("small-a", 4, "a"), shape("small-b", 4, "b"),
		shape("eight-a", 8, "a"), shape("eight-b", 8, "b"),
	}, extra...)

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB}, tiers,
		WithClock(func() time.Time { return *now }), WithLeaseTTL(90*24*time.Hour))

	for _, name := range []string{"a", "b"} {
		mustRegister(t, a, NodeRegistration{Name: name, Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB})
	}

	return a
}

// startRunning buys a lease of a tier at the clock and has it running, started then.
func startRunning(t *testing.T, a *Allocator, label string) *Lease {
	t.Helper()

	lease, err := a.Reserve(t.Context(), label)
	if err != nil {
		t.Fatalf("Reserve(%s): %v", label, err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, ranJobs.Add(1)); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	for _, phase := range []Phase{PhaseLaunching, PhaseOnline, PhaseBusy} {
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("Advance to %s: %v", phase, err)
		}
	}

	return lease
}

func candidateNames(candidates []HoldCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Node)
	}

	return out
}

// THE HOST CHOSEN IS THE ONE PREDICTED TO FREE UP SOONEST, whatever its name: a host
// whose job started nine minutes ago, with no history saying otherwise, ahead of one
// whose job started one minute ago. And it is dated by when the shape fits.
func TestHoldCandidatesAreSoonestReadyFirst(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := holdFleet(t, &now)

	startRunning(t, a, "small-b")

	now = now.Add(8 * time.Minute)
	startRunning(t, a, "small-a")

	now = now.Add(time.Minute)

	got, err := a.HoldCandidates(t.Context(), "wide", nil)
	if err != nil {
		t.Fatalf("HoldCandidates: %v", err)
	}

	if !slices.Equal(candidateNames(got), []string{"b", "a"}) {
		t.Fatalf("the candidates are %v; want b, whose job ends first, then a", got)
	}

	if want := now.Add(-9 * time.Minute).Add(DefaultRuntime); !got[0].ReadyAt.Equal(want) {
		t.Errorf("host b is ready at %s; want its job's start plus the default runtime, %s", got[0].ReadyAt, want)
	}
}

// IT IS LEARNED FROM HISTORY: what decides is the jobs' own runtimes, not only when
// they started.
func TestHoldCandidatesFollowTheLearnedRuntimes(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := holdFleet(t, &now)

	// A tier's own jobs ran a minute, three times, so its level is known: small-a is
	// short, and small-b, with no history, takes the default.
	for range 3 {
		lease := startRunning(t, a, "small-a")

		now = now.Add(time.Minute)
		if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}

		now = now.Add(time.Minute)
	}

	now = now.Add(2 * time.Hour)

	startRunning(t, a, "small-b")
	startRunning(t, a, "small-a")

	got, err := a.HoldCandidates(t.Context(), "wide", nil)
	if err != nil {
		t.Fatalf("HoldCandidates: %v", err)
	}

	if !slices.Equal(candidateNames(got), []string{"a", "b"}) {
		t.Errorf("the candidates are %v; want a, whose jobs are known to be short, first", got)
	}
}

// A HOST ALREADY RUNNING A LEASE AS LARGE AS THE SHAPE IS NEVER CHOSEN: that lease
// ending is room the shape fits in, with no hold needed. As large in both dimensions.
func TestAHostRunningALeaseAsLargeAsTheShapeIsNotACandidate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	// vcpu-only runs as many vCPU as "wider" asks, with less memory.
	vcpuOnly := tier("vcpu-only", 8, 4*config.GiB)
	vcpuOnly.Provider, vcpuOnly.Node = config.ProviderDocker, "b"

	wider := tier("wider", 8, 40*config.GiB)
	wider.Provider = config.ProviderDocker

	a := holdFleet(t, &now, vcpuOnly, wider)

	// Each host too full for the shape, a by a lease as large as an 8 vCPU shape.
	startRunning(t, a, "eight-a")
	startRunning(t, a, "eight-a")
	startRunning(t, a, "eight-b")
	startRunning(t, a, "small-b")
	startRunning(t, a, "small-b")

	got, err := a.HoldCandidates(t.Context(), "eight-a", nil)
	if err != nil {
		t.Fatalf("HoldCandidates: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("a shape as small as what its host runs has candidates: %v", got)
	}

	// The wide shape: nothing as large runs anywhere, so both hosts are candidates.
	got, err = a.HoldCandidates(t.Context(), "wide", nil)
	if err != nil || len(got) != 2 {
		t.Errorf("the wide shape's candidates are %v, %v; want both hosts", got, err)
	}

	// A lease of as many vCPU and less memory is not as large, so neither host here
	// is ruled out for the shape asking more memory than any of their leases holds.
	other := holdFleet(t, &now, vcpuOnly, wider)

	startRunning(t, other, "eight-a")
	startRunning(t, other, "eight-a")
	startRunning(t, other, "vcpu-only")
	startRunning(t, other, "small-b")
	startRunning(t, other, "small-b")

	got, err = other.HoldCandidates(t.Context(), "wider", nil)
	if err != nil {
		t.Fatalf("HoldCandidates(wider): %v", err)
	}

	if !slices.Equal(candidateNames(got), []string{"a", "b"}) {
		t.Errorf("the candidates for 8 vCPU and 40 GiB are %v; want both, their 8 vCPU leases holding less memory", got)
	}
}

// A SHAPE THAT FITS NOW HAS NOTHING TO HOLD FOR, unless the only room is on a host held
// for another tier, when it may be held another.
func TestAShapeThatFitsNowHasNoCandidates(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := holdFleet(t, &now)

	got, err := a.HoldCandidates(t.Context(), "wide", nil)
	if err != nil || len(got) != 0 {
		t.Errorf("an empty fleet gave %v, %v; want no candidates", got, err)
	}

	startRunning(t, a, "small-a")

	// b has room, and b is held for another tier: a is what is left, b is ready now.
	got, err = a.HoldCandidates(t.Context(), "wide", []string{"b"})
	if err != nil || !slices.Equal(candidateNames(got), []string{"b", "a"}) || !got[0].ReadyAt.Equal(now) {
		t.Errorf("the candidates are %v, %v; want b, ready now, then a", got, err)
	}

	got, err = a.HoldCandidates(t.Context(), "wide", []string{"a"})
	if err != nil || len(got) != 0 {
		t.Errorf("room on a host not avoided gave %v, %v; want no candidates", got, err)
	}
}

// A REMOTE BACKEND BUYS ITS SHAPES, and a macOS slot is a licence rather than room:
// neither is held for.
func TestARemoteHostOrAMacIsNotHeld(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	spanning := tier("spanning", 16, 64*config.GiB)
	spanning.Provider = ""
	spanning.Providers = []config.ProviderKind{config.ProviderDocker, config.ProviderEC2}
	spanning.Image = "ami-1"

	mac := tier("mac", 16, 64*config.GiB)
	mac.Provider, mac.GuestOS, mac.Node = config.ProviderTart, config.GuestMacOS, "mac-1"

	a := holdFleet(t, &now, spanning, mac)

	mustRegister(t, a, NodeRegistration{
		Name: "cloud", Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{Type: "m7i.8xlarge", VCPU: 32, Memory: 128 * config.GiB}},
	})
	mustRegister(t, a, NodeRegistration{Name: "mac-1", Provider: config.ProviderTart, VCPU: 16, Memory: 64 * config.GiB})

	if got, err := a.HoldCandidates(t.Context(), "mac", nil); err != nil || got != nil {
		t.Errorf("a macOS tier's candidates are %v, %v; want none", got, err)
	}

	for range 2 {
		startRunning(t, a, "small-a")
		startRunning(t, a, "small-b")
	}

	got, err := a.HoldCandidates(t.Context(), "spanning", []string{"cloud"})
	if err != nil {
		t.Fatalf("HoldCandidates: %v", err)
	}

	if slices.Contains(candidateNames(got), "cloud") || !slices.Contains(candidateNames(got), "a") {
		t.Errorf("the candidates are %v; want the hosts, never the remote one", got)
	}
}

// WHEN THE SHAPE FITS IS THE END THAT FREES ENOUGH: the leases are added up soonest
// first until both dimensions fit, and a host they never free enough on is no candidate.
func TestReadyAtAddsUpTheRoomEachLeaseFrees(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return now.Add(time.Duration(minutes) * time.Minute) }
	lease := func(vcpu int, memory config.ByteSize, end time.Time) heldLease {
		return heldLease{cost: placementCost{vcpu: vcpu, memory: memory * config.GiB}, end: end}
	}

	wide := placementCost{vcpu: 16, memory: 64 * config.GiB}

	for _, c := range []struct {
		name   string
		vcpu   int
		memory config.ByteSize
		leases []heldLease
		want   time.Time
		ok     bool
	}{
		{"room now", 16, 64, nil, now, true},
		{"the second end frees enough", 8, 32, []heldLease{lease(4, 16, at(9)), lease(4, 16, at(3))}, at(9), true},
		{"sorted by end, not by order", 12, 48, []heldLease{lease(4, 16, at(9)), lease(4, 16, at(3))}, at(3), true},
		{"memory binds", 16, 32, []heldLease{lease(2, 16, at(3)), lease(2, 16, at(9))}, at(9), true},
		{"never enough", 4, 16, []heldLease{lease(4, 16, at(3))}, time.Time{}, false},
		{"a lease as large", 0, 0, []heldLease{lease(16, 64, at(3))}, time.Time{}, false},
	} {
		got, ok := readyAt(wide, c.vcpu, c.memory*config.GiB, c.leases, now)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s: ready at %s (%v); want %s (%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// A PURCHASE PLACES NOTHING ON AN AVOIDED HOST, and takes the room elsewhere; a tier
// below its floor ignores it, because that room is promised.
func TestAPurchaseAvoidsAHeldHostUnlessItsFloorIsUnmet(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	anywhere := tier("anywhere", 4, 16*config.GiB)
	anywhere.Provider = config.ProviderDocker

	reserved := tier("reserved", 4, 16*config.GiB)
	reserved.Provider, reserved.Reserved = config.ProviderDocker, 1

	a := holdFleet(t, &now, anywhere, reserved)

	leases, err := a.EscrowFor(t.Context(), "anywhere", 8, Purchase{Avoid: []string{"a"}})
	if err != nil {
		t.Fatalf("EscrowFor: %v", err)
	}

	if len(leases) == 0 {
		t.Fatal("a purchase avoiding one host bought nothing, with the other free")
	}

	for _, lease := range leases {
		if lease.TargetNode == "a" {
			t.Errorf("a lease was placed on the avoided host")
		}
	}

	// Pinned to the avoided host, there is nowhere else.
	pinned, err := a.EscrowFor(t.Context(), "small-a", 1, Purchase{Avoid: []string{"a"}})
	if err != nil || len(pinned) != 0 {
		t.Errorf("a purchase with only the avoided host gave %v, %v; want nothing", pinned, err)
	}

	floor, err := a.EscrowFor(t.Context(), "reserved", 1, Purchase{Avoid: []string{"a"}})
	if err != nil || len(floor) != 1 || floor[0].TargetNode != "a" {
		t.Fatalf("a tier below its floor, with room only on the avoided host, bought %v, %v", floor, err)
	}

	// At its floor it avoids it like anyone else, even with the room there.
	again, err := a.EscrowFor(t.Context(), "reserved", 1, Purchase{Avoid: []string{"a"}})
	if err != nil || len(again) != 0 {
		t.Errorf("a tier at its floor bought %v, %v on the avoided host", again, err)
	}
}

// NO AVOIDED HOST IS EVER PLACED ON AND THE FLEET NEVER OVERCOMMITS, however many buy at
// once: what the purchase avoids is decided in its own transaction.
func TestConcurrentPurchasesAvoidingAHostNeverOvercommit(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	anywhere := tier("anywhere", 4, 16*config.GiB)
	anywhere.Provider = config.ProviderDocker

	a := holdFleet(t, &now, anywhere)

	var (
		mu     sync.Mutex
		leases []*Lease
		wg     sync.WaitGroup
	)

	for range 12 {
		wg.Go(func() {
			got, err := a.EscrowFor(t.Context(), "anywhere", 1, Purchase{Avoid: []string{"a"}})
			if err != nil {
				t.Errorf("EscrowFor: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()

			leases = append(leases, got...)
		})
	}

	wg.Wait()

	if len(leases) != 4 {
		t.Errorf("%d leases bought on one host of room for four", len(leases))
	}

	for _, lease := range leases {
		if lease.TargetNode != "b" {
			t.Errorf("a lease was placed on %s", lease.TargetNode)
		}
	}
}
