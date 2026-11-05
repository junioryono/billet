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

	got, _, err := a.HoldCandidates(t.Context(), "wide", nil, "")
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

	// A tier's own jobs ran a minute, three times, so its level is known: small-b is
	// short, and small-a, with no history, takes the default.
	for range 3 {
		lease := startRunning(t, a, "small-b")

		now = now.Add(time.Minute)
		if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}

		now = now.Add(time.Minute)
	}

	now = now.Add(2 * time.Hour)

	startRunning(t, a, "small-b")
	startRunning(t, a, "small-a")

	got, _, err := a.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil {
		t.Fatalf("HoldCandidates: %v", err)
	}

	if !slices.Equal(candidateNames(got), []string{"b", "a"}) {
		t.Errorf("the candidates are %v; want b, whose jobs are known to be short, first", got)
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

	got, _, err := a.HoldCandidates(t.Context(), "eight-a", nil, "")
	if err != nil {
		t.Fatalf("HoldCandidates: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("a shape as small as what its host runs has candidates: %v", got)
	}

	// The wide shape: nothing as large runs anywhere, so both hosts are candidates.
	got, _, err = a.HoldCandidates(t.Context(), "wide", nil, "")
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

	got, _, err = other.HoldCandidates(t.Context(), "wider", nil, "")
	if err != nil {
		t.Fatalf("HoldCandidates(wider): %v", err)
	}

	if !slices.Equal(candidateNames(got), []string{"a", "b"}) {
		t.Errorf("the candidates for 8 vCPU and 40 GiB are %v; want both, their 8 vCPU leases holding less memory", got)
	}
}

// A LEASE NOT YET STARTED IS PREDICTED FROM NOW, not from no start at all, which would
// read as a job overdue by years.
func TestAHoldPredictsALeaseNotYetStartedFromNow(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := holdFleet(t, &now)

	if _, err := a.Reserve(t.Context(), "small-a"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := a.Reserve(t.Context(), "small-b"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	got, _, err := a.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("HoldCandidates: %v, %v", got, err)
	}

	if want := now.Add(DefaultRuntime); !got[0].ReadyAt.Equal(want) {
		t.Errorf("a host running a lease not yet started is ready at %s; want %s", got[0].ReadyAt, want)
	}
}

// A LEASE IN CUSTODY OR QUARANTINE ENDS ON PROOF OR BY AN OPERATOR, not by any prediction:
// a host that needs one to end is no candidate, or a hold on it would never end.
func TestAHostNeedingALeaseInCustodyIsNotACandidate(t *testing.T) {
	for _, phase := range []Phase{PhaseCustody, PhaseQuarantine} {
		t.Run(string(phase), func(t *testing.T) {
			now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			a := holdFleet(t, &now)

			lease := startRunning(t, a, "small-a")
			startRunning(t, a, "small-b")

			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
				t.Fatalf("Advance: %v", err)
			}

			got, _, err := a.HoldCandidates(t.Context(), "wide", nil, "")
			if err != nil || !slices.Equal(candidateNames(got), []string{"b"}) {
				t.Errorf("the candidates are %v, %v; want b alone", got, err)
			}
		})
	}
}

// BEHIND A LEASE AS LARGE IS THE ONE REASON FOR NO CANDIDATE THAT LETS A NARROWER BUYER PAST:
// on every host the tier could use, each lease one that will end, and none that fits now.
func TestBehindALeaseAsLargeIsReportedOnlyWhenItIsTheWholeReason(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	anywhere := tier("anywhere", 8, 32*config.GiB)
	anywhere.Provider = config.ProviderDocker

	a := holdFleet(t, &now, anywhere)

	behindNow := func() ([]HoldCandidate, bool) {
		t.Helper()

		got, behind, err := a.HoldCandidates(t.Context(), "anywhere", nil, "")
		if err != nil {
			t.Fatalf("HoldCandidates: %v", err)
		}

		return got, behind
	}

	if _, behind := behindNow(); behind {
		t.Error("a shape that fits now reads as behind a lease as large")
	}

	startRunning(t, a, "eight-a")
	startRunning(t, a, "eight-a")

	first := startRunning(t, a, "eight-b")

	if _, behind := behindNow(); behind {
		t.Error("a shape with room on one host reads as behind a lease as large")
	}

	// NOR FOR A WAITER THAT HOLDS ONE: it fits on b, and it holds a.
	if _, behind, err := a.HoldCandidates(t.Context(), "anywhere", nil, "a"); err != nil || behind {
		t.Errorf("a waiter that fits now and holds a host reads as behind a lease as large: %v, %v", behind, err)
	}

	// ON ITS OWN HOST TOO: b has room for it beside a lease as large, and b is what it holds,
	// so it keeps it.
	own, behind, err := a.HoldCandidates(t.Context(), "anywhere", nil, "b")
	if err != nil || behind || !slices.Equal(candidateNames(own), []string{"b"}) {
		t.Errorf("a waiter with room on its own host reads %v, %v, %v; want that host kept", own, behind, err)
	}

	last := startRunning(t, a, "eight-b")

	if got, behind := behindNow(); len(got) != 0 || !behind {
		t.Errorf("with a lease as large on each host it reads %v, %v", got, behind)
	}

	// ONE THAT MAY NEVER END PROMISES IT NOTHING.
	for _, lease := range []*Lease{first, last} {
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseCustody); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}

	if _, behind := behindNow(); behind {
		t.Error("a lease in custody read as a lease that will end")
	}
}

// A LEASE AS LARGE THAT HAS RUN LATE PROMISES NOTHING: the shape may fit beside it, so the host
// is a candidate again, or narrower work could refill the rest for as long as it runs. Started
// or bought and never started.
func TestALateLeaseAsLargeLeavesTheHostACandidate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	anywhere := tier("anywhere", 8, 32*config.GiB)
	anywhere.Provider, anywhere.Node = config.ProviderDocker, "a"

	for name, buy := range map[string]func(*Allocator){
		"started": func(a *Allocator) { startRunning(t, a, "eight-a") },
		"never started": func(a *Allocator) {
			if _, err := a.Reserve(t.Context(), "eight-a"); err != nil {
				t.Fatalf("Reserve: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			at := now
			a := holdFleet(t, &at, anywhere)

			buy(a)
			startRunning(t, a, "small-a")
			startRunning(t, a, "small-a")

			got, behind, err := a.HoldCandidates(t.Context(), "anywhere", nil, "")
			if err != nil || len(got) != 0 || !behind {
				t.Fatalf("in time it reads %v, %v, %v; want it behind the lease as large", got, behind, err)
			}

			at = at.Add(HoldLateFactor*DefaultRuntime + time.Minute)

			got, behind, err = a.HoldCandidates(t.Context(), "anywhere", nil, "")
			if err != nil || !slices.Equal(candidateNames(got), []string{"a"}) || behind {
				t.Errorf("past the factor it reads %v, %v, %v; want the host a candidate", got, behind, err)
			}
		})
	}
}

// A JOB RUN LATE ON THE HOST SAYS SO, only when the shape needs it gone.
func TestACandidateSaysWhenAJobItNeedsGoneIsLate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := holdFleet(t, &now)

	startRunning(t, a, "small-a")
	startRunning(t, a, "small-b")

	got, _, err := a.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil || len(got) != 2 || got[0].Late || got[1].Late {
		t.Fatalf("a fresh host reads %v, %v", got, err)
	}

	start := now
	now = start.Add(HoldLateFactor*DefaultRuntime - time.Minute)

	got, _, err = a.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil || len(got) != 2 || got[0].Late || got[1].Late {
		t.Errorf("short of the factor the host reads %v, %v; want it in time", got, err)
	}

	now = start.Add(HoldLateFactor*DefaultRuntime + time.Minute)

	got, _, err = a.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil || len(got) != 2 || !got[0].Late || !got[1].Late {
		t.Errorf("past the factor the host reads %v, %v; want it late", got, err)
	}

	// ONE BOUGHT AND NEVER STARTED IS LATE TOO, from when it was bought: read from now it
	// never would be, and a hold on its host would have no bound.
	idle := holdFleet(t, &now)

	for _, label := range []string{"small-a", "small-b"} {
		if _, err := idle.Reserve(t.Context(), label); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	}

	got, _, err = idle.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil || len(got) != 2 || got[0].Late {
		t.Fatalf("a lease just bought reads %v, %v", got, err)
	}

	now = now.Add(HoldLateFactor*DefaultRuntime + time.Minute)

	got, _, err = idle.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil || len(got) != 2 || !got[0].Late {
		t.Errorf("a lease never started, past the factor, reads %v, %v; want it late", got, err)
	}
}

// A SHAPE THAT FITS NOW HAS NOTHING TO HOLD FOR, unless the only room is on a host held
// for another tier, when it may be held another.
func TestAShapeThatFitsNowHasNoCandidates(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := holdFleet(t, &now)

	got, _, err := a.HoldCandidates(t.Context(), "wide", nil, "")
	if err != nil || len(got) != 0 {
		t.Errorf("an empty fleet gave %v, %v; want no candidates", got, err)
	}

	startRunning(t, a, "small-a")

	// b has room, and b is held for another tier: a is what is left, b is ready now.
	got, _, err = a.HoldCandidates(t.Context(), "wide", map[string]string{"b": "someone"}, "")
	if err != nil || !slices.Equal(candidateNames(got), []string{"b", "a"}) || !got[0].ReadyAt.Equal(now) {
		t.Errorf("the candidates are %v, %v; want b, ready now, then a", got, err)
	}

	got, _, err = a.HoldCandidates(t.Context(), "wide", map[string]string{"a": "someone"}, "")
	if err != nil || len(got) != 0 {
		t.Errorf("room on a host not avoided gave %v, %v; want no candidates", got, err)
	}

	// ROOM ABOVE WHAT AN OWNER NEEDS IS ROOM IT HAS: b is held for a tier of 8, and has 16.
	got, _, err = a.HoldCandidates(t.Context(), "small-b", map[string]string{"b": "eight-b"}, "")
	if err != nil || len(got) != 0 {
		t.Errorf("room above the owner's shape gave %v, %v; want it read as fitting now", got, err)
	}

	// ROOM ELSEWHERE DOES NOT GIVE UP THE HOST IT HOLDS: a pass the order refuses must not
	// hand back the room it accumulated. a has a lease, b is free, and a is what it holds.
	got, _, err = a.HoldCandidates(t.Context(), "wide", nil, "a")
	if err != nil || !slices.Equal(candidateNames(got), []string{"a"}) {
		t.Errorf("holding a with room on b gave %v, %v; want a alone", got, err)
	}

	// ROOM ON THE HOST IT HOLDS KEEPS THE HOLD: a job ending between the refusal and this
	// read must not hand the room it accumulated to the next small purchase.
	got, _, err = a.HoldCandidates(t.Context(), "wide", nil, "b")
	if err != nil || len(got) == 0 || got[0].Node != "b" || !got[0].ReadyAt.Equal(now) {
		t.Errorf("room on its own held host gave %v, %v; want that host, ready now, first", got, err)
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

	mac := tier("mac", 8, 32*config.GiB)
	mac.Provider, mac.GuestOS, mac.Node = config.ProviderTart, config.GuestMacOS, "mac-1"

	macSmall := tier("mac-small", 2, 8*config.GiB)
	macSmall.Provider, macSmall.GuestOS, macSmall.Node = config.ProviderTart, config.GuestMacOS, "mac-1"

	spanningEight := spanning
	spanningEight.Label, spanningEight.VCPU, spanningEight.Memory = "spanning-eight", 8, 32*config.GiB

	a := holdFleet(t, &now, spanning, spanningEight, mac, macSmall)

	mustRegister(t, a, NodeRegistration{
		Name: "cloud", Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{Type: "m7i.8xlarge", VCPU: 32, Memory: 128 * config.GiB}},
	})
	mustRegister(t, a, NodeRegistration{Name: "mac-1", Provider: config.ProviderTart, VCPU: 16, Memory: 64 * config.GiB})

	for range config.DefaultMacOSVMLimit {
		startRunning(t, a, "mac-small")
	}

	// Its slots spent by smaller guests, with the vCPU free, so only the exemption
	// stands between it and the host.
	if got, behind, err := a.HoldCandidates(t.Context(), "mac", nil, ""); err != nil || got != nil || behind {
		t.Errorf("a macOS tier's candidates are %v, %v, %v; want none, and no lease as large", got, behind, err)
	}

	for range 2 {
		startRunning(t, a, "small-a")
		startRunning(t, a, "small-b")
	}

	got, _, err := a.HoldCandidates(t.Context(), "spanning", map[string]string{"cloud": "someone"}, "")
	if err != nil {
		t.Fatalf("HoldCandidates: %v", err)
	}

	// A REMOTE BACKEND IT COULD USE IS NEVER BEHIND A LEASE AS LARGE, even with one on
	// every host: a budget is not room that accumulates.
	startRunning(t, a, "eight-a")
	startRunning(t, a, "eight-b")

	if _, behind, err := a.HoldCandidates(t.Context(), "spanning-eight", map[string]string{"cloud": "someone"}, ""); err != nil || behind {
		t.Errorf("a tier that can use a remote backend reads as behind a lease as large: %v, %v", behind, err)
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
	} {
		got, _, ok := readyAt(wide, c.vcpu, c.memory*config.GiB, c.leases, now)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s: ready at %s (%v); want %s (%v)", c.name, got, ok, c.want, c.ok)
		}
	}

	// LATE ONLY IF THE SHAPE NEEDS THAT JOB GONE: one that ends after it fits is not its wait.
	inTime, late := lease(4, 16, at(3)), lease(4, 16, at(9))
	late.late = true

	if _, isLate, ok := readyAt(wide, 12, 48*config.GiB, []heldLease{inTime, late}, now); !ok || isLate {
		t.Errorf("with the late job not needed gone it reads late %v (%v)", isLate, ok)
	}

	if _, isLate, ok := readyAt(wide, 8, 32*config.GiB, []heldLease{inTime, late}, now); !ok || !isLate {
		t.Errorf("with the late job needed gone it reads late %v (%v)", isLate, ok)
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

	leases, err := a.EscrowFor(t.Context(), "anywhere", 8, Purchase{Avoid: map[string]map[string]string{"anywhere": {"a": "someone"}}})
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
	pinned, err := a.EscrowFor(t.Context(), "small-a", 1, Purchase{Avoid: map[string]map[string]string{"small-a": {"a": "someone"}}})
	if err != nil || len(pinned) != 0 {
		t.Errorf("a purchase with only the avoided host gave %v, %v; want nothing", pinned, err)
	}

	floor, err := a.EscrowFor(t.Context(), "reserved", 1, Purchase{Avoid: map[string]map[string]string{"reserved": {"a": "someone"}}})
	if err != nil || len(floor) != 1 || floor[0].TargetNode != "a" {
		t.Fatalf("a tier below its floor, with room only on the avoided host, bought %v, %v", floor, err)
	}

	// AS FAR AS ITS FLOOR AND NO FURTHER: the rest of a backlog bought at once does not
	// spend the room held for another.
	bulk := tier("bulk", 2, 8*config.GiB)
	bulk.Provider, bulk.Node, bulk.Reserved = config.ProviderDocker, "b", 1

	bulkFleet := holdFleet(t, &now, bulk)

	got, err := bulkFleet.EscrowFor(t.Context(), "bulk", 3, Purchase{Avoid: map[string]map[string]string{"bulk": {"b": "someone"}}})
	if err != nil || len(got) != 1 {
		t.Errorf("a tier one short of its floor, asking for three on the held host, bought %v, %v", got, err)
	}

	// A TIER THE HOLD DOES NOT BIND PLACES THERE, its owner above all.
	own, err := a.EscrowFor(t.Context(), "small-a", 1, Purchase{Avoid: map[string]map[string]string{"someone-else": {"a": "someone"}}})
	if err != nil || len(own) != 1 {
		t.Errorf("a tier no hold binds bought %v, %v", own, err)
	}

	// At its floor it avoids it like anyone else, even with the room there.
	again, err := a.EscrowFor(t.Context(), "reserved", 1, Purchase{Avoid: map[string]map[string]string{"reserved": {"a": "someone"}}})
	if err != nil || len(again) != 0 {
		t.Errorf("a tier at its floor bought %v, %v on the avoided host", again, err)
	}
}

// NO AVOIDED HOST IS EVER PLACED ON AND THE FLEET NEVER OVERCOMMITS, however many buy at
// once: each purchase's headroom, and where it places, is its own transaction's.
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
			got, err := a.EscrowFor(t.Context(), "anywhere", 1, Purchase{Avoid: map[string]map[string]string{"anywhere": {"a": "someone"}}})
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

// TIGHTEST PREFERS THE HOST LEFT WITH THE LEAST ROOM, in shares of its size, where pack
// sees two hosts that each fit the same number of this tier as alike and takes the name.
func TestTightestPrefersTheHostLeftWithTheLeastRoom(t *testing.T) {
	small := tier("small", 4, 8*config.GiB)
	small.Provider = config.ProviderDocker

	filler := func(label string, vcpu int, node string) config.Tier {
		out := tier(label, vcpu, config.ByteSize(vcpu)*2*config.GiB)
		out.Provider, out.Node = config.ProviderDocker, node

		return out
	}

	type host struct {
		name string
		vcpu int
	}

	// place puts the fillers on two hosts and asks where one lease of small goes.
	place := func(policy config.PlacementPolicy, hosts []host, fillers ...config.Tier) string {
		t.Helper()

		a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
			append([]config.Tier{small}, fillers...), WithPlacement(policy))

		for _, h := range hosts {
			mustRegister(t, a, NodeRegistration{Name: h.name, Provider: config.ProviderDocker, VCPU: h.vcpu,
				Memory: config.ByteSize(h.vcpu) * 2 * config.GiB})
		}

		for i := range fillers {
			if _, err := a.Reserve(t.Context(), fillers[i].Label); err != nil {
				t.Fatalf("Reserve(%s): %v", fillers[i].Label, err)
			}
		}

		lease, err := a.Reserve(t.Context(), "small")
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}

		return lease.TargetNode
	}

	// 15 vCPU left on a-host and 13 on z-host: each fits three of small, so pack sees
	// them as alike and takes the name, and tightest the one left with less.
	even := []host{{"a-host", 16}, {"z-host", 16}}
	fill := []config.Tier{filler("on-a", 1, "a-host"), filler("on-z", 3, "z-host")}

	if got := place(config.PlacementPack, even, fill...); got != "a-host" {
		t.Errorf("pack placed on %s; want the name, which is what this tells tightest from", got)
	}

	if got := place(config.PlacementTightest, even, fill...); got != "z-host" {
		t.Errorf("tightest placed on %s; want the host left with less", got)
	}

	// A SHARE OF THE HOST'S OWN SIZE: 8 vCPU left of 64 is tighter than 6 of 8, though
	// it is more vCPU, and likewise its memory.
	sized := []host{{"a-large", 64}, {"z-small", 8}}
	nearly := []config.Tier{filler("on-large", 56, "a-large"), filler("on-small", 2, "z-small")}

	if got := place(config.PlacementTightest, sized, nearly...); got != "a-large" {
		t.Errorf("tightest placed on %s; want the host left with the smaller share of itself", got)
	}
}

// A FLOOR'S PROMISE IS ROOM, NOT THE HELD HOST: with room elsewhere it goes there, or
// each of its jobs would land on the host being emptied.
func TestATierBelowItsFloorGoesElsewhereWhenItCan(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	reserved := tier("reserved", 4, 16*config.GiB)
	reserved.Provider, reserved.Reserved = config.ProviderDocker, 1

	a := holdFleet(t, &now, reserved)

	// Pack prefers the fuller host; hold that one.
	startRunning(t, a, "small-a")

	got, err := a.EscrowFor(t.Context(), "reserved", 1, Purchase{Avoid: map[string]map[string]string{"reserved": {"a": "someone"}}})
	if err != nil || len(got) != 1 || got[0].TargetNode != "b" {
		t.Fatalf("a tier below its floor with room elsewhere bought %v, %v; want it on b", got, err)
	}
}

// A WAITER AHEAD IS PROTECTED ON THE HOSTS IT CAN USE: room on a host held for another
// tier is no room for it, so a purchase that takes the last of the rest is refused.
func TestAPurchaseOutOfTurnLeavesAWaiterRoomOffTheHeldHosts(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	anywhere := tier("anywhere", 8, 32*config.GiB)
	anywhere.Provider = config.ProviderDocker

	a := holdFleet(t, &now, anywhere)

	// b has room for one more of "anywhere", and a for one and held for someone else.
	startRunning(t, a, "eight-b")
	startRunning(t, a, "eight-a")

	purchase := Purchase{Protect: []string{"anywhere"}, Avoid: map[string]map[string]string{"anywhere": {"a": "someone"}, "eight-b": {"a": "someone"}}}

	got, err := a.EscrowFor(t.Context(), "eight-b", 1, purchase)
	if err != nil || len(got) != 0 {
		t.Errorf("a purchase took the waiter's last usable room: %v, %v", got, err)
	}

	// A hold that does not bind the waiter leaves it that room.
	purchase.Avoid = map[string]map[string]string{"eight-b": {"a": "someone"}}

	got, err = a.EscrowFor(t.Context(), "eight-b", 1, purchase)
	if err != nil || len(got) != 1 {
		t.Errorf("a purchase was refused with the waiter's own held host free: %v, %v", got, err)
	}
}

// A WAITER'S FLOOR IS ROOM IT HAS: the guard reads it as the waiter's own purchase would,
// or a buyer elsewhere is refused for a waiter that could buy.
func TestTheGuardSeesAWaitersFloorOnAHeldHost(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	floor := tier("floor", 4, 16*config.GiB)
	floor.Provider, floor.Node, floor.Reserved = config.ProviderDocker, "a", 1

	a := holdFleet(t, &now, floor)

	got, err := a.EscrowFor(t.Context(), "small-b", 1, Purchase{Protect: []string{"floor"},
		Avoid: map[string]map[string]string{"floor": {"a": "someone"}}})
	if err != nil || len(got) != 1 {
		t.Errorf("a buyer elsewhere gave %v, %v, with the waiter's floor on the held host", got, err)
	}
}

// AN ADMISSION SAYS THE TIER'S SHAPE, which the order reads to tell who could put a lease
// as large in a waiter's way.
func TestAnAdmissionSaysItsShape(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a := holdFleet(t, &now)

	wide := a.tiers["wide"]

	got, err := a.AdmissionFor(t.Context(), "wide")
	if err != nil || got.VCPU != wide.VCPU || got.Memory != wide.Memory {
		t.Errorf("the admission says %d vCPU and %d bytes, %v; want %d and %d", got.VCPU, got.Memory, err, wide.VCPU, wide.Memory)
	}
}

// A HOLD KEEPS THE ROOM ITS OWNER NEEDS, NOT THE HOST: all of it until the owner fits, and past
// that only that much, or a wide host idles for an owner of a far narrower shape. Spent as a
// bulk purchase spends it, and the guard reads it the same way.
func TestAHeldHostSellsTheRoomAboveWhatItsOwnerNeeds(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	eight := func() Purchase {
		return Purchase{Avoid: map[string]map[string]string{"small-a": {"a": "eight-a"}}}
	}

	a := holdFleet(t, &now)

	// a has 16, the owner needs 8, and small-a is 4.
	got, err := a.EscrowFor(t.Context(), "small-a", 4, eight())
	if err != nil || len(got) != 2 {
		t.Errorf("with room for its owner and two more it bought %d, %v; want 2", len(got), err)
	}

	// Now a has 8, exactly what the owner needs.
	got, err = a.EscrowFor(t.Context(), "small-a", 1, eight())
	if err != nil || len(got) != 0 {
		t.Errorf("with room only for its owner it bought %d, %v", len(got), err)
	}

	// SHORT OF THE OWNER'S SHAPE THE HOST IS CLOSED: 4 more gone, and 4 left is not for sale.
	if _, err := a.Reserve(t.Context(), "small-a"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	got, err = a.EscrowFor(t.Context(), "small-a", 1, eight())
	if err != nil || len(got) != 0 {
		t.Errorf("with the owner short of room it bought %d, %v", len(got), err)
	}

	// VCPU ALONE BINDS AS WELL: shapes of little memory, with room for all of it.
	coreOwner, coreBuyer := tier("core-owner", 8, config.GiB), tier("core-buyer", 4, config.GiB)
	coreOwner.Provider, coreBuyer.Provider = config.ProviderDocker, config.ProviderDocker
	coreBuyer.Node = "a"

	cores := holdFleet(t, &now, coreOwner, coreBuyer)

	got, err = cores.EscrowFor(t.Context(), "core-buyer", 4,
		Purchase{Avoid: map[string]map[string]string{"core-buyer": {"a": "core-owner"}}})
	if err != nil || len(got) != 2 {
		t.Errorf("with 16 vCPU and an owner of 8, a tier of 4 bought %d, %v; want 2", len(got), err)
	}

	// A HOST SHORT OF ITS OWNER COUNTS FOR NOTHING AND TAKES NOTHING FROM THE REST: the waiter's
	// one lease on b is room it has, whatever a is short of.
	waiter := tier("waiter", 4, 16*config.GiB)
	waiter.Provider = config.ProviderDocker

	// A wholly different buyer, bought only to read the guard: it succeeds only if the
	// waiter still has a lease on b once a counts for nothing.
	buyer := tier("guard-buyer", 2, 8*config.GiB)
	buyer.Provider, buyer.Node = config.ProviderDocker, "a"

	guarded := holdFleet(t, &now, waiter, buyer)

	for _, label := range []string{"small-a", "eight-a", "small-b", "eight-b"} {
		startRunning(t, guarded, label)
	}

	got, err = guarded.EscrowFor(t.Context(), "guard-buyer", 1, Purchase{Protect: []string{"waiter"},
		Avoid: map[string]map[string]string{"waiter": {"a": "wide"}}})
	if err != nil || len(got) != 1 {
		t.Errorf("with the waiter's lease on b untouched the buyer bought %d, %v", len(got), err)
	}

	// MEMORY IS ROOM TOO: an owner short of memory closes a host with vCPU to spare.
	lean := tier("lean", 2, 60*config.GiB)
	lean.Provider, lean.Node = config.ProviderDocker, "a"

	heavy := tier("heavy", 2, 8*config.GiB)
	heavy.Provider, heavy.Node = config.ProviderDocker, "a"

	other := holdFleet(t, &now, lean, heavy)

	got, err = other.EscrowFor(t.Context(), "heavy", 1, Purchase{Avoid: map[string]map[string]string{"heavy": {"a": "lean"}}})
	if err != nil || len(got) != 0 {
		t.Errorf("with 64 GiB and an owner of 60, a tier of 8 bought %d, %v", len(got), err)
	}
}

// A FLOOR MOVED BY A PURCHASE MUST NOT TAKE WHAT A HOLD KEEPS. Pack puts an unmet floor on the
// fuller host, so a purchase on the held host can bring it there, and read before the insert
// the owner's room looked safe. It is read again after, and a host where the owner did not fit
// before is not one a purchase can have cost it.
func TestAPurchaseThatMovesAFloorOntoAHeldHostIsRefused(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	floor := tier("floor", 4, 16*config.GiB)
	floor.Provider, floor.Reserved = config.ProviderDocker, 1

	a := holdFleet(t, &now, floor)

	first := startRunning(t, a, "small-a")
	startRunning(t, a, "small-a")
	startRunning(t, a, "eight-b")

	if err := a.Release(t.Context(), first.ID, first.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	holds := Purchase{Avoid: map[string]map[string]string{"small-a": {"a": "eight-a"}}}

	got, err := a.EscrowFor(t.Context(), "small-a", 1, holds)
	if err != nil || len(got) != 0 {
		t.Errorf("a purchase bringing the floor onto the held host bought %d, %v", len(got), err)
	}

	// The owner, whose room it was, still fits.
	if got, err := a.EscrowFor(t.Context(), "eight-a", 1, Purchase{}); err != nil || len(got) != 1 {
		t.Errorf("the owner bought %d, %v with the room kept", len(got), err)
	}

	// WHEREVER THE PURCHASE LANDS: with the floor on b and the owner fitting on a, the last room
	// on b sends the floor to a.
	elsewhere := holdFleet(t, &now, floor)

	for _, label := range []string{"small-a", "small-a", "eight-b", "small-b"} {
		startRunning(t, elsewhere, label)
	}

	got, err = elsewhere.EscrowFor(t.Context(), "small-b", 1, Purchase{Avoid: map[string]map[string]string{"small-b": {"a": "eight-a"}}})
	if err != nil || len(got) != 0 {
		t.Errorf("a purchase elsewhere sending the floor onto the held host bought %d, %v", len(got), err)
	}

	// A TIER BELOW ITS FLOOR IS PROMISED THE ROOM, and that read does not take it back: pinned
	// to a host held for a shape that does not fit there anyway, it buys.
	pinnedFloor := tier("pinned-floor", 4, 16*config.GiB)
	pinnedFloor.Provider, pinnedFloor.Node, pinnedFloor.Reserved = config.ProviderDocker, "a", 1

	promised := holdFleet(t, &now, pinnedFloor)

	for range 3 {
		startRunning(t, promised, "small-a")
	}

	got, err = promised.EscrowFor(t.Context(), "pinned-floor", 1,
		Purchase{Avoid: map[string]map[string]string{"pinned-floor": {"a": "eight-a"}}})
	if err != nil || len(got) != 1 {
		t.Errorf("a tier below its floor bought %d, %v on the only host it has", len(got), err)
	}
}

// A WAITER KEPT KEEPS THE ROOM IT FITS IN NOW, and only that: a purchase that would take it
// is rolled back, one that leaves it is not, and while it fits nowhere nothing is kept.
func TestAPurchaseLeavesAKeptWaiterTheRoomItFitsIn(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	two := tier("two-a", 2, 8*config.GiB)
	two.Provider, two.Node = config.ProviderDocker, "a"

	eight := tier("eight-on-a", 8, 32*config.GiB)
	eight.Provider, eight.Node = config.ProviderDocker, "a"

	a := holdFleet(t, &now, two, eight)

	keep := Purchase{Keep: []string{"eight-on-a"}}

	// a is empty: 16 free, room for two of the waiter's. Two leases of 2 leave it 12.
	got, err := a.EscrowFor(t.Context(), "two-a", 2, keep)
	if err != nil || len(got) != 2 {
		t.Fatalf("leaving the waiter room it bought %d, %v", len(got), err)
	}

	// Asked for three, it takes two: a third would leave 6, short of one of the waiter's.
	got, err = a.EscrowFor(t.Context(), "two-a", 3, keep)
	if err != nil || len(got) != 2 {
		t.Errorf("with 12 free and a waiter of 8 kept it bought %d, %v; want 2", len(got), err)
	}

	// 8 free: the waiter fits, so nothing more is for sale.
	got, err = a.EscrowFor(t.Context(), "two-a", 1, keep)
	if err != nil || len(got) != 0 {
		t.Errorf("with the waiter fitting exactly it bought %d, %v", len(got), err)
	}

	// Not kept, the same purchase goes through.
	got, err = a.EscrowFor(t.Context(), "two-a", 1, Purchase{})
	if err != nil || len(got) != 1 {
		t.Fatalf("not kept it bought %d, %v", len(got), err)
	}

	// 6 free: the waiter fits nowhere, so nothing is kept from what is left.
	got, err = a.EscrowFor(t.Context(), "two-a", 3, keep)
	if err != nil || len(got) != 3 {
		t.Errorf("with the waiter fitting nowhere it bought %d, %v; want all three", len(got), err)
	}
}
