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

// AND A CEILING SMALLER THAN THE FLEET IS ONE POT EVERY TIER BUYS FROM.
//
// Asking whether the ceiling cuts one tier's own total is not the question: two
// tiers can each fit under it alone and still not fit together, which is exactly
// when a stream of the small one starves the large one on a host it never
// touches.
func TestTheAdmissionSaysWhenTheDeploymentCeilingIsShared(t *testing.T) {
	pinnedA := tier("a", 8, 16*config.GiB)
	pinnedA.Node = "host-a"
	pinnedB := tier("b", 4, 8*config.GiB)
	pinnedB.Node = "host-b"

	hosts := func(t *testing.T, a *Allocator) {
		t.Helper()

		for _, name := range []string{"host-a", "host-b"} {
			mustRegister(t, a, NodeRegistration{
				Name: name, Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
			})
		}
	}

	// Eight vCPU over two 8 vCPU hosts: each tier fits under the ceiling on its
	// own host, and the two of them do not fit at once.
	tight := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB},
		[]config.Tier{pinnedA, pinnedB})
	hosts(t, tight)

	for _, label := range []string{"a", "b"} {
		if got := admissionOf(t, tight, label); !got.CeilingShared {
			t.Errorf("tier %s reports %+v; the deployment ceiling is smaller than the hosts it "+
				"caps, so a tier on another host is buying from the same pot", label, got)
		}
	}

	// With a ceiling the hosts cannot reach, the hosts are the limit again.
	roomy := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB},
		[]config.Tier{pinnedA, pinnedB})
	hosts(t, roomy)

	if got := admissionOf(t, roomy, "a"); got.CeilingShared {
		t.Errorf("tier a reports %+v under a ceiling far above the fleet; the hosts are its limit", got)
	}
}

// AND A CHARGE STRANDED OFF THE FLEET IS SPENT CEILING.
//
// A lease on a host the fleet no longer contains holds its shape against the
// deployment until something proves its compute gone, and no live host can use
// what it holds. Measured against the ceiling's face value, a deployment whose
// ceiling exactly matches its hosts reports the ceiling as nobody's constraint
// while that charge is making it everybody's.
func TestAChargeOnADepartedHostMakesTheCeilingShared(t *testing.T) {
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

	// The same host leaves in both cases, so the surviving fleet is the same
	// sixteen vCPU; only the lease left behind on it differs.
	quiet, quietEpoch := fleet(t)
	survivors(t, quiet)

	if err := quiet.NodeWithdrawn(t.Context(), "leaving", quietEpoch, "leaving-1"); err != nil {
		t.Fatalf("withdraw leaving: %v", err)
	}

	if got := admissionOf(t, quiet, "small"); got.CeilingShared {
		t.Fatalf("tier small reports %+v on a fleet its ceiling exactly matches; this case "+
			"cannot tell a stranded charge from the ceiling itself", got)
	}

	// The same fleet, with eight vCPU of it charged to a lease on the host that
	// has gone: half the ceiling is spent where no tier can reach it.
	stranded, epoch := fleet(t)

	reserve(t, stranded, "big")
	survivors(t, stranded)

	if err := stranded.NodeWithdrawn(t.Context(), "leaving", epoch, "leaving-1"); err != nil {
		t.Fatalf("withdraw leaving: %v", err)
	}

	for _, label := range []string{"big", "small"} {
		if got := admissionOf(t, stranded, label); !got.CeilingShared {
			t.Errorf("tier %s reports %+v; eight of the sixteen vCPU the deployment may spend "+
				"are held by a lease on a host that is gone, so both tiers are buying what is "+
				"left of one pot", label, got)
		}
	}
}
