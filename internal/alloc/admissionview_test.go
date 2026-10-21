package alloc

import (
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// admissionOf reads one tier's admission, failing the test rather than the
// caller's assertion.
func admissionOf(t *testing.T, a *Allocator, label string) TierAdmission {
	t.Helper()

	view, err := a.AdmissionView(t.Context())
	if err != nil {
		t.Fatalf("AdmissionView: %v", err)
	}

	return view[label]
}

// noHeadroom proves the tier could not buy this instant, so what an assertion
// about CanGrow below reads is the model and not the allocator's own answer.
func noHeadroom(t *testing.T, a *Allocator, label string) {
	t.Helper()

	room, err := a.Headroom(t.Context(), label)
	if err != nil {
		t.Fatalf("Headroom(%s): %v", label, err)
	}

	if room != 0 {
		t.Fatalf("tier %s has %d of headroom, so this case cannot tell the eligibility rule "+
			"from the fallback that asks the allocator", label, room)
	}
}

// A LEASE STRANDED ON A HOST THAT IS GONE MUST NOT SAY THE TIER CANNOT GROW.
//
// The potential is measured on the fleet that is HERE, so comparing it with
// every open lease, including one on a machine the model no longer contains,
// counts a lease against room it was never measured against. The tier would be
// excluded from the admission order and starved of the room it can still use
// (#157).
func TestATierWithALeaseOnADepartedHostCanStillGrow(t *testing.T) {
	big := tier("big", 8, 16*config.GiB)
	small := tier("small", 4, 8*config.GiB)
	a := newBareAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{big, small})

	// One host to begin with, so the big tier's lease can only land there.
	_, leavingEpoch := mustRegister(t, a, NodeRegistration{
		Name: "leaving", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
		Incarnation: "leaving-1",
	})

	reserve(t, a, "big")

	// The second host arrives and is filled by the small tier, so nothing the
	// allocator could grant this instant can stand in for the model's answer.
	mustRegister(t, a, NodeRegistration{
		Name: "staying", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
		Incarnation: "staying-1",
	})

	reserve(t, a, "small")
	reserve(t, a, "small")

	// The host the big tier's lease is on withdraws, which takes it out of
	// placement and leaves the lease exactly where it is: outstanding, on a
	// machine the fleet no longer contains.
	if err := a.NodeWithdrawn(t.Context(), "leaving", leavingEpoch, "leaving-1"); err != nil {
		t.Fatalf("withdraw leaving: %v", err)
	}

	noHeadroom(t, a, "big")

	got := admissionOf(t, a, "big")
	if !got.CanGrow {
		t.Errorf("admission %+v: a tier whose only open lease is stranded on a host that is gone "+
			"was told it cannot grow, and would be starved of the room the small jobs on the "+
			"remaining host are about to return", got)
	}
}

// AND ITS OWN CAP IS STILL COUNTED OVER EVERY LEASE IT HOLDS.
//
// max_concurrent is enforced over all open leases, stranded or not, so a tier
// whose cap is spent on a lease the model ignores cannot buy at all. Told it
// could grow, it would hold the whole order behind a purchase the allocator
// refuses until that lease is resolved.
func TestATierAtItsCapWithAStrandedLeaseCannotGrow(t *testing.T) {
	capped := tier("capped", 4, 8*config.GiB)
	capped.MaxConcurrent = 1
	a := newBareAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{capped})

	_, leavingEpoch := mustRegister(t, a, NodeRegistration{
		Name: "leaving", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
		Incarnation: "leaving-1",
	})

	reserve(t, a, "capped")

	// A host with room to spare, which the cap makes unreachable.
	mustRegister(t, a, NodeRegistration{
		Name: "staying", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
		Incarnation: "staying-1",
	})

	if err := a.NodeWithdrawn(t.Context(), "leaving", leavingEpoch, "leaving-1"); err != nil {
		t.Fatalf("withdraw leaving: %v", err)
	}

	if got := admissionOf(t, a, "capped"); got.CanGrow {
		t.Errorf("admission %+v: a tier holding its whole max_concurrent on a departed host was "+
			"told it could grow, and would hold every other tier behind a purchase the "+
			"allocator refuses", got)
	}
}

// A FLOOR ALREADY BACKED SOMEWHERE ELSE MUST NOT BE HELD AGAIN.
//
// The model packs an empty fleet, and holding a floor that is already met takes
// a host twice: once where the tier really runs, once where the model puts it.
// A tier whose only host the model then spends is reported unable to grow and
// never joins the order — the starvation this view exists to prevent.
func TestAFloorAlreadyMetDoesNotExcludeATier(t *testing.T) {
	// The floor tier fits either host, and the packer prefers the one with the
	// fewest slots for it, which is the host the large tier needs.
	floor := tier("floor", 4, 8*config.GiB)
	floor.Reserved = 1
	large := tier("large", 8, 16*config.GiB)
	large.Node = "near"
	filler := tier("filler", 4, 8*config.GiB)
	filler.Node = "near"

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{floor, large, filler})

	// The roomy host exists first, so the floor tier's own lease lands there and
	// its reservation is met away from the host the large tier is pinned to.
	mustRegister(t, a, NodeRegistration{
		Name: "roomy", Provider: config.ProviderFirecracker, VCPU: 16, Memory: 32 * config.GiB,
	})

	reserve(t, a, "floor")

	mustRegister(t, a, NodeRegistration{
		Name: "near", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
	})

	// And that host is busy, so only the model can answer for the large tier.
	reserve(t, a, "filler")
	reserve(t, a, "filler")

	noHeadroom(t, a, "large")

	if got := admissionOf(t, a, "large"); !got.CanGrow {
		t.Errorf("admission %+v: a tier was told it cannot grow because a floor already backed "+
			"on another host was held a second time against the only host it fits", got)
	}
}

// AND THE HOSTS A TIER COMPETES FOR ARE THE HOSTS IT COULD RUN ON.
//
// The placement policy considers more hosts than a shape fits. A waiter that
// claimed a host too small for it would block the tiers that host can really
// serve, for as long as it waited.
func TestTheAdmissionHostsExcludeAHostTheShapeDoesNotFit(t *testing.T) {
	large := tier("large", 8, 16*config.GiB)
	small := tier("small", 2, 4*config.GiB)
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{large, small})

	mustRegister(t, a, NodeRegistration{
		Name: "big", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
	})
	mustRegister(t, a, NodeRegistration{
		Name: "little", Provider: config.ProviderFirecracker, VCPU: 4, Memory: 8 * config.GiB,
	})

	if got := admissionOf(t, a, "large"); !slices.Equal(got.Nodes, []string{"big"}) {
		t.Errorf("the large tier competes for %v, want only the host its shape fits", got.Nodes)
	}

	if got := admissionOf(t, a, "small"); !slices.Equal(got.Nodes, []string{"big", "little"}) {
		t.Errorf("the small tier competes for %v, want both hosts", got.Nodes)
	}
}

// A WAITER THE CEILING REFUSES IS CEILING-BOUND, AND ONE ITS HOSTS REFUSE IS NOT.
//
// Two tiers pinned to different hosts under a ceiling that fits either but not
// both: the large tier's host is empty while the small tier's jobs spend the
// ceiling, so only the ceiling stands between the large tier and its host, and
// every purchase anywhere takes from it (#173's review). The small tier, whose
// own host is full, is waiting for that host, which the large tier's jobs never
// touch.
func TestAWaiterTheCeilingRefusesIsCeilingBound(t *testing.T) {
	pinnedA := tier("a", 8, 16*config.GiB)
	pinnedA.Node = "host-a"
	pinnedB := tier("b", 4, 8*config.GiB)
	pinnedB.Node = "host-b"

	fleet := func(t *testing.T, limits Limits) *Allocator {
		t.Helper()

		a := newBareAllocator(t, limits, []config.Tier{pinnedA, pinnedB})
		for _, name := range []string{"host-a", "host-b"} {
			mustRegister(t, a, NodeRegistration{
				Name: name, Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
			})
		}

		return a
	}

	// Eight vCPU over two 8 vCPU hosts, and the small tier holds all of it.
	tight := fleet(t, Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB})
	reserve(t, tight, "b")
	reserve(t, tight, "b")

	if got := admissionOf(t, tight, "a"); !got.CeilingBound {
		t.Errorf("tier a reports %+v; its host is empty and the ceiling is spent, so the "+
			"ceiling is what refuses it and every purchase anywhere takes from it", got)
	}

	if got := admissionOf(t, tight, "b"); got.CeilingBound {
		t.Errorf("tier b reports %+v; its own host is full, so it waits for that host and "+
			"not for the ceiling", got)
	}

	// The same work under a ceiling the hosts cannot reach: the hosts decide.
	roomy := fleet(t, Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB})
	reserve(t, roomy, "b")
	reserve(t, roomy, "b")

	for _, label := range []string{"a", "b"} {
		if got := admissionOf(t, roomy, label); got.CeilingBound {
			t.Errorf("tier %s reports %+v under a ceiling far above the fleet; the hosts are its limit",
				label, got)
		}
	}
}

// A WAITER WHOSE HOSTS ARE FULL IS HOST-BOUND, WHEREVER THE CEILING SITS (#187).
//
// A Linux host contributing 120 vCPU and a Mac contributing 18, under a ceiling
// of 136 vCPU and 540 GiB: just below what the hosts hold (138, 544), which the
// rule this replaced read as "the ceiling is one pot" for every tier. A macOS
// tier waiting for one of the Mac's two VM slots has plenty of ceiling left;
// what refuses it is the slot limit, and no room freed on the Linux host can
// ever reach it, so it must not be ceiling-bound and hold the Linux tiers.
func TestAWaiterForAMacSlotIsNotCeilingBound(t *testing.T) {
	mac := func(label string) config.Tier {
		return config.Tier{
			Label: label, Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
			Node: "mac", VCPU: 4, Memory: 8 * config.GiB, Image: "macos",
		}
	}

	linux := tier("linux", 2, 8*config.GiB)
	a := newBareAllocator(t, Limits{MaxVCPU: 136, MaxMemory: 540 * config.GiB},
		[]config.Tier{mac("mac-a"), mac("mac-b"), linux})

	mustRegister(t, a, NodeRegistration{
		Name: "linux-host", Provider: config.ProviderFirecracker, VCPU: 120, Memory: 480 * config.GiB,
	})
	mustRegister(t, a, NodeRegistration{
		Name: "mac", Provider: config.ProviderTart, VCPU: 18, Memory: 64 * config.GiB,
	})

	// The Mac's two slots, held by the other macOS tier.
	reserve(t, a, "mac-a")
	reserve(t, a, "mac-a")
	noHeadroom(t, a, "mac-b")

	got := admissionOf(t, a, "mac-b")
	if !got.CanGrow {
		t.Fatalf("tier mac-b reports %+v; it can grow once the other tier's jobs end, so this "+
			"case would not reach the order at all", got)
	}

	if got.CeilingBound {
		t.Errorf("tier mac-b reports %+v; 128 vCPU of ceiling is left and the Mac's slot limit "+
			"is what refuses it, so it would hold every Linux tier behind room it cannot use", got)
	}
}

// A WAITER AT ITS OWN CAP IS NOT CEILING-BOUND, whatever the ceiling says: its
// cap refuses it first, and a purchase elsewhere takes nothing it could use.
func TestATierAtItsCapIsNotCeilingBound(t *testing.T) {
	capped := tier("capped", 4, 8*config.GiB)
	capped.MaxConcurrent = 1
	other := tier("other", 4, 8*config.GiB)

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB},
		[]config.Tier{capped, other})
	mustRegister(t, a, NodeRegistration{
		Name: "host", Provider: config.ProviderFirecracker, VCPU: 16, Memory: 32 * config.GiB,
	})

	reserve(t, a, "capped")
	reserve(t, a, "other")

	if got := admissionOf(t, a, "capped"); got.CeilingBound {
		t.Errorf("tier capped reports %+v; it holds its whole max_concurrent, so the ceiling "+
			"is not what refuses it", got)
	}

	if got := admissionOf(t, a, "other"); !got.CeilingBound {
		t.Errorf("tier other reports %+v; its host has room and the ceiling is spent", got)
	}
}

// AND A CHARGE STRANDED OFF THE FLEET IS SPENT CEILING.
//
// A lease on a host the fleet no longer contains holds its shape against the
// deployment until something proves its compute gone, and no live host can use
// what it holds. The waiter it starves has a host with room and no ceiling,
// which is exactly ceiling-bound, with nothing special done for the stranded
// charge.
func TestAChargeOnADepartedHostMakesAWaiterCeilingBound(t *testing.T) {
	big := tier("big", 8, 16*config.GiB)
	small := tier("small", 4, 8*config.GiB)

	// A ceiling equal to what the two surviving hosts hold: by itself, not a
	// constraint either tier meets before its host.
	fleet := func(t *testing.T) (*Allocator, int64) {
		t.Helper()

		a := newBareAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
			[]config.Tier{big, small})

		_, epoch := mustRegister(t, a, NodeRegistration{
			Name: "leaving", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
			Incarnation: "leaving-1",
		})

		return a, epoch
	}

	survivors := func(t *testing.T, a *Allocator) {
		t.Helper()

		for _, name := range []string{"host-a", "host-b"} {
			mustRegister(t, a, NodeRegistration{
				Name: name, Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
			})
		}
	}

	// The same host leaves in both cases and the same small job runs on a
	// survivor; only the lease left behind on the departed host differs.
	quiet, quietEpoch := fleet(t)
	survivors(t, quiet)

	if err := quiet.NodeWithdrawn(t.Context(), "leaving", quietEpoch, "leaving-1"); err != nil {
		t.Fatalf("withdraw leaving: %v", err)
	}

	reserve(t, quiet, "small")

	if got := admissionOf(t, quiet, "big"); got.CeilingBound {
		t.Fatalf("tier big reports %+v with twelve of the sixteen vCPU free; this case cannot "+
			"tell a stranded charge from the ceiling itself", got)
	}

	// The same fleet, with eight vCPU of it charged to a lease on the host that
	// has gone: half the ceiling is spent where no tier can reach it.
	stranded, epoch := fleet(t)

	reserve(t, stranded, "big")
	survivors(t, stranded)

	if err := stranded.NodeWithdrawn(t.Context(), "leaving", epoch, "leaving-1"); err != nil {
		t.Fatalf("withdraw leaving: %v", err)
	}

	reserve(t, stranded, "small")

	if got := admissionOf(t, stranded, "big"); !got.CeilingBound {
		t.Errorf("tier big reports %+v; a host of its has room, and the eight vCPU it needs are "+
			"held by a lease on a host that is gone and the small job, so the ceiling refuses it", got)
	}
}
