package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A PROMISE BILLET CANNOT KEEP IS THE ONE THIS CHANGES.
//
// The budget said the deployment had room; it did not say any single machine
// did. With a 64 vCPU box and an 8 vCPU Mac mini and a ceiling of 120, billet
// would escrow 120 vCPU of work, advertise it to GitHub, and then be unable to
// place most of it — every one of those jobs assigned to a fleet that cannot run
// them.
//
// What a tier may advertise is now the smaller of the deployment's ceiling and
// what the machines can actually hold.
func TestATierAdvertisesOnlyWhatItsHostsCanHold(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	// A ceiling far above the fleet, so only the fleet can produce these numbers.
	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four})

	if got := headroom(t, a, "four"); got != 0 {
		t.Fatalf("a deployment with no machines advertised %d; there is nowhere to put it", got)
	}

	// Two hosts, three slots between them: 8 vCPU fits two, 4 vCPU fits one.
	mustRegister(t, a, NodeRegistration{
		Name: "big", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})
	mustRegister(t, a, NodeRegistration{
		Name: "small", Provider: config.ProviderDocker, VCPU: 4, Memory: 64 * config.GiB})

	if got, want := headroom(t, a, "four"), 3; got != want {
		t.Errorf("headroom = %d, want %d — the sum of what each machine can take", got, want)
	}
}

// THE DEPLOYMENT CEILING STILL BINDS, and it is the half that keeps a one-box
// install behaving exactly as it did.
//
// The reference host detects 128 threads while its config says 120. Without the
// ceiling the fleet term would win and billet would quietly start advertising
// eight more than the operator allowed, overcommitting the machine.
func TestTheDeploymentCeilingStillBindsBelowTheFleet(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	// Room for two, on a machine with room for far more.
	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four})

	mustRegister(t, a, NodeRegistration{
		Name: "huge", Provider: config.ProviderDocker, VCPU: 1000, Memory: 4000 * config.GiB})

	if got, want := headroom(t, a, "four"), 2; got != want {
		t.Errorf("headroom = %d, want %d — the deployment ceiling stopped binding", got, want)
	}
}

// A FLEET THAT CANNOT SERVE THIS TIER IS NOT A FLEET FOR IT. Every filter that
// makes a host ineligible has to reach the advertised number, or billet accepts
// work for a machine that could never take it.
func TestATierWithNoSuitableHostAdvertisesNothing(t *testing.T) {
	tart := config.Tier{
		Label: "arm", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 8 * config.GiB, Image: "ubuntu-arm64",
	}

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{tart})

	// A large machine that runs the wrong backend entirely.
	mustRegister(t, a, NodeRegistration{
		Name: "docker-box", Provider: config.ProviderDocker, VCPU: 512, Memory: 1000 * config.GiB})

	if got := headroom(t, a, "arm"); got != 0 {
		t.Errorf("headroom = %d, want 0 — the only machine cannot run this tier", got)
	}
}

// A HOST THE PLANE HAS GIVEN UP ON STOPS BACKING ADVERTISEMENTS, which is the
// whole reason liveness reached the ledger.
func TestAHostThatIsGoneStopsCountingTowardsWhatIsAdvertised(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four})

	_, epoch := mustRegister(t, a, NodeRegistration{
		Name: "only", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})

	if got, want := headroom(t, a, "four"), 2; got != want {
		t.Fatalf("headroom = %d, want %d before the host went away", got, want)
	}

	if err := a.NodeGone(t.Context(), "only", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if got := headroom(t, a, "four"); got != 0 {
		t.Errorf("headroom = %d, want 0 — a machine nothing can reach is still being "+
			"advertised, and GitHub will assign against it", got)
	}
}

// SINGLE-NODE BEHAVIOUR IS UNCHANGED, which is the property that makes this
// landable on its own.
//
// One host holding the whole budget is the deployment billet has today. Its
// arithmetic must come out exactly where it did before capacity was a per-machine
// question, or every existing install changes underneath its operator.
func TestOneHostHoldingTheWholeBudgetBehavesAsBefore(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	const budget = 12

	a := newBareAllocator(t, Limits{MaxVCPU: budget, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four})

	mustRegister(t, a, NodeRegistration{
		Name: "the-box", Provider: config.ProviderDocker,
		VCPU: budget, Memory: 4000 * config.GiB})

	// budget/tier = 3, which is what the global-only arithmetic produced.
	if got, want := headroom(t, a, "four"), budget/4; got != want {
		t.Fatalf("headroom = %d, want %d", got, want)
	}

	// And it still shrinks as work is taken, one for one.
	for taken := 1; taken <= budget/4; taken++ {
		if _, err := a.Reserve(t.Context(), "four"); err != nil {
			t.Fatalf("Reserve %d: %v", taken, err)
		}

		if got, want := headroom(t, a, "four"), budget/4-taken; got != want {
			t.Errorf("after %d reserved, headroom = %d, want %d", taken, got, want)
		}
	}
}
