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

// AN UNPINNED RESERVATION MUST SPEND A MACHINE, and until escrow chose one it
// spent none.
//
// This is the invariant of the whole package, reached from the new direction. A
// lease for a tier that names no host used to be recorded with neither node nor
// target_node, so the deployment was charged for it and no MACHINE was. The
// fleet term therefore never shrank as work was escrowed, and billet would
// advertise the same slots over and over:
//
//	two 4 vCPU hosts, ceiling 12, a 4 vCPU tier.
//	Escrow one → the fleet still reports room for two.
//	Escrow again → three slots promised for a two-slot fleet.
//
// GitHub then assigns three jobs to machines that can hold two, which is exactly
// the overcommit the escrow exists to prevent — arrived at through the code that
// was supposed to make it stronger.
func TestEscrowSpendsAMachineEvenWhenTheTierNamesNone(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	// The ceiling allows three; the machines hold two. The fleet has to be what
	// binds, and it has to keep binding as the slots are taken.
	a := newBareAllocator(t, Limits{MaxVCPU: 12, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four})

	for _, name := range []string{"a", "b"} {
		mustRegister(t, a, NodeRegistration{
			Name: name, Provider: config.ProviderDocker, VCPU: 4, Memory: 64 * config.GiB})
	}

	if got, want := headroom(t, a, "four"), 2; got != want {
		t.Fatalf("headroom = %d, want %d — two machines, one slot each", got, want)
	}

	leases, err := a.Escrow(t.Context(), "four", 1)
	if err != nil {
		t.Fatalf("Escrow: %v", err)
	}

	if len(leases) != 1 {
		t.Fatalf("escrowed %d leases, want 1", len(leases))
	}

	if got, want := headroom(t, a, "four"), 1; got != want {
		t.Errorf("after escrowing one, headroom = %d, want %d — the reservation was charged "+
			"to the deployment and to no machine", got, want)
	}

	// EVERY LEASE NAMES ITS MACHINE. Without that the number above is a
	// coincidence of arithmetic rather than a decision anything downstream can
	// honour: Bind checks the target, and a lease with none can be bound anywhere.
	if leases[0].TargetNode == "" {
		t.Error("an escrowed lease names no host, so nothing downstream can tell where its " +
			"capacity was taken from")
	}

	// And the fleet cannot be oversold in total.
	rest, err := a.Escrow(t.Context(), "four", 5)
	if err != nil {
		t.Fatalf("Escrow the rest: %v", err)
	}

	if total := 1 + len(rest); total != 2 {
		t.Errorf("escrowed %d leases against a fleet that holds 2", total)
	}
}

// TWO LEASES MUST NOT LAND ON ONE MACHINE WHEN ANOTHER IS IDLE.
//
// Choosing at escrow is what makes the reservation mean something: whichever
// node polls first used to take the work, so a fleet with room in two places
// could pile both jobs onto one host and fail the second.
func TestEscrowSpreadsAcrossTheFleetRatherThanFillingOneHost(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four})

	for _, name := range []string{"a", "b"} {
		mustRegister(t, a, NodeRegistration{
			Name: name, Provider: config.ProviderDocker, VCPU: 4, Memory: 64 * config.GiB})
	}

	leases, err := a.Escrow(t.Context(), "four", 2)
	if err != nil {
		t.Fatalf("Escrow: %v", err)
	}

	if len(leases) != 2 {
		t.Fatalf("escrowed %d leases, want 2", len(leases))
	}

	if leases[0].TargetNode == leases[1].TargetNode {
		t.Errorf("both leases were aimed at %q while the other machine sat idle",
			leases[0].TargetNode)
	}
}

// THE PREFERENCE LIST FINALLY DECIDES SOMETHING.
//
// `providers: [firecracker, ec2]` is meant to read "the machine at home first,
// the cloud if you must". The order has been recorded and ignored since it was
// added — whichever node happened to poll first took the work — so a deployment
// that listed a cloud fallback could pay for it while the box under the desk sat
// idle. Escrow now chooses, and this is where the order is honoured.
func TestThePreferredBackendIsChosenWhenBothAreAvailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order []config.ProviderKind
		want  string
	}{
		{
			name:  "home first",
			order: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2},
			want:  "epyc-1",
		},
		{
			// The same fleet with the list reversed must go the other way, or the
			// test is only observing which host happens to sort first.
			name:  "cloud first",
			order: []config.ProviderKind{config.ProviderEC2, config.ProviderFirecracker},
			want:  "ec2-spot-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := config.Tier{
				Label: "spanning", Providers: tc.order, GuestOS: config.GuestLinux,
				VCPU: 4, Memory: 8 * config.GiB, Image: "ubuntu",
			}

			a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
				[]config.Tier{tr})

			// IDENTICAL IN EVERY OTHER RESPECT, so preference is the only thing that
			// can decide. Equal capacity means the emptiest-first rule cannot break
			// the tie, and the names sort the other way from the answer in one of
			// these cases, so the name tie-break cannot either.
			mustRegister(t, a, NodeRegistration{
				Name: "epyc-1", Provider: config.ProviderFirecracker,
				VCPU: 64, Memory: 512 * config.GiB})
			mustRegister(t, a, NodeRegistration{
				Name: "ec2-spot-1", Provider: config.ProviderEC2,
				VCPU: 64, Memory: 512 * config.GiB})

			lease, err := a.Reserve(t.Context(), "spanning")
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}

			if lease.TargetNode != tc.want {
				t.Errorf("aimed at %q, want %q — the tier's preference order was ignored",
					lease.TargetNode, tc.want)
			}
		})
	}
}

// FILLING A MACHINE BEFORE STARTING ON THE NEXT IS THE DEFAULT, and the reason
// is which failure each choice produces.
//
// Spreading leaves every host partly used, and partly used hosts cannot hold a
// LARGE tier: six 4-vCPU jobs spread across two 16-vCPU machines leave four free
// on each, so an 8-vCPU job fits nowhere while eight vCPU sit idle in the fleet.
// A job that cannot be placed is worse than a job that shares a disk — and the
// sharing is bounded anyway, because billet escrows vCPU and memory and the
// provider enforces both per container.
//
// The tie-break by name deliberately points at the emptier host here, so a
// version that fell through to it would fail.
func TestPlacementFillsAMachineBeforeStartingTheNext(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four})

	// "a" sorts first and is the emptier of the two.
	mustRegister(t, a, NodeRegistration{
		Name: "a", Provider: config.ProviderDocker, VCPU: 16, Memory: 640 * config.GiB})
	mustRegister(t, a, NodeRegistration{
		Name: "b", Provider: config.ProviderDocker, VCPU: 8, Memory: 640 * config.GiB})

	lease, err := a.Reserve(t.Context(), "four")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if lease.TargetNode != "b" {
		t.Errorf("aimed at %q, want the fuller host b — the fleet is being spread rather "+
			"than packed, which strands capacity in fragments", lease.TargetNode)
	}
}

// AND SPREADING IS STILL AVAILABLE for a deployment that would rather have every
// job on its own spindle than fit the most work.
func TestSpreadPlacementPrefersTheEmptiestMachine(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{four}, WithPlacement(config.PlacementSpread))

	mustRegister(t, a, NodeRegistration{
		Name: "a", Provider: config.ProviderDocker, VCPU: 16, Memory: 640 * config.GiB})
	mustRegister(t, a, NodeRegistration{
		Name: "b", Provider: config.ProviderDocker, VCPU: 8, Memory: 640 * config.GiB})

	lease, err := a.Reserve(t.Context(), "four")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if lease.TargetNode != "a" {
		t.Errorf("aimed at %q, want the emptiest host a", lease.TargetNode)
	}
}

// PREFERENCE STILL OUTRANKS BOTH. The operator's provider order is an
// instruction; how to break ties among equals is a tuning knob, and a knob must
// not overrule an instruction.
func TestPlacementPolicyDoesNotOverrulePreference(t *testing.T) {
	tr := config.Tier{
		Label:     "spanning",
		Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2},
		GuestOS:   config.GuestLinux,
		VCPU:      4, Memory: 8 * config.GiB, Image: "ubuntu",
	}

	for _, policy := range []config.PlacementPolicy{
		config.PlacementPack, config.PlacementSpread,
	} {
		t.Run(string(policy), func(t *testing.T) {
			a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
				[]config.Tier{tr}, WithPlacement(policy))

			// The preferred backend is the EMPTIER host, so packing would choose the
			// fallback if the policy were allowed to win.
			mustRegister(t, a, NodeRegistration{
				Name: "epyc-1", Provider: config.ProviderFirecracker,
				VCPU: 64, Memory: 640 * config.GiB})
			mustRegister(t, a, NodeRegistration{
				Name: "ec2-spot-1", Provider: config.ProviderEC2,
				VCPU: 8, Memory: 640 * config.GiB})

			lease, err := a.Reserve(t.Context(), "spanning")
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}

			if lease.TargetNode != "epyc-1" {
				t.Errorf("aimed at %q, want the preferred backend epyc-1", lease.TargetNode)
			}
		})
	}
}

// THE SAME FLEET MUST GIVE THE SAME ANSWER, and the hazard is Go itself.
//
// Ranging a map is randomised, so a placer that walked the candidate map would
// scatter identical reservations across identical machines from run to run. That
// is not merely untidy: it cannot be reproduced from a log, it makes every
// placement test flaky in proportion to how often it is right by luck, and it
// turns "why did this job land there" into a question with no answer.
//
// The direction of the tie-break is arbitrary; that it HAS one is not.
func TestPlacementIsTheSameOnIdenticalFleets(t *testing.T) {
	four := tier("four", 4, 8*config.GiB)
	four.Provider = config.ProviderDocker

	place := func() string {
		t.Helper()

		a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
			[]config.Tier{four})

		// Indistinguishable in every respect the ordering consults, so only the
		// tie-break can decide and a map walk would answer differently each time.
		for _, name := range []string{"a", "b", "c", "d", "e"} {
			mustRegister(t, a, NodeRegistration{
				Name: name, Provider: config.ProviderDocker,
				VCPU: 8, Memory: 64 * config.GiB})
		}

		lease, err := a.Reserve(t.Context(), "four")
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}

		return lease.TargetNode
	}

	first := place()

	for range 20 {
		if got := place(); got != first {
			t.Fatalf("identical fleets placed the same reservation on %q and %q; placement "+
				"cannot be reproduced or explained", first, got)
		}
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
