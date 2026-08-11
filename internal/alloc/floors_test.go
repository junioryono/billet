package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A FLOOR IS A PROMISE, AND PLACEMENT CAN BREAK IT WITHOUT ANYTHING MISBEHAVING.
//
// `reserved: 2` says "always keep room for two of this tier". Deducting it from
// one deployment-wide pool was a real guarantee while capacity WAS one pool: if
// the fleet had the vCPU, the fleet could run them.
//
// It stops being a guarantee the moment room lives on particular machines. Two
// hosts with room for one each satisfy a floor of two in aggregate and satisfy it
// on neither. Another tier takes one slot on each, the arithmetic is content —
// the deployment still has "enough" vCPU — and the reserved tier now cannot place
// a single instance. Nothing overcommitted, nothing errored, and the promise is
// simply gone.
//
// So the floor is checked where it can still be kept: at placement, against the
// fleet as it would be afterwards.
func TestAFloorSurvivesAnotherTierTakingTheFleet(t *testing.T) {
	reserved := tier("reserved", 4, 8*config.GiB)
	reserved.Provider = config.ProviderDocker
	reserved.Reserved = 2

	greedy := tier("greedy", 4, 8*config.GiB)
	greedy.Provider = config.ProviderDocker

	// A ceiling far above the fleet, so only the machines can bind.
	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{reserved, greedy})

	// Two machines, one slot each. Exactly the reserved tier's floor.
	for _, name := range []string{"a", "b"} {
		mustRegister(t, a, NodeRegistration{
			Name: name, Provider: config.ProviderDocker, VCPU: 4, Memory: 64 * config.GiB})
	}

	// The greedy tier may have NOTHING: every slot it could take is the last one
	// keeping the floor placeable.
	if got := headroom(t, a, "greedy"); got != 0 {
		t.Errorf("the unreserved tier was offered %d slots, which would leave the reserved "+
			"tier unable to place its floor of %d", got, reserved.Reserved)
	}

	// And the reserved tier can still take what was promised to it.
	if got, want := headroom(t, a, "reserved"), 2; got != want {
		t.Errorf("the reserved tier can place %d of its own floor of %d", got, want)
	}
}

// A FLOOR ALREADY MET STOPS HOLDING ANYTHING BACK, which is what keeps a
// reservation a floor rather than a quota.
//
// Once the reserved tier is actually holding its two slots, it has what it was
// promised and competes for the rest on equal terms. A version that kept
// deducting would idle capacity permanently in the name of work that has already
// arrived.
func TestAFloorAlreadyHeldNoLongerBlocksAnyone(t *testing.T) {
	reserved := tier("reserved", 4, 8*config.GiB)
	reserved.Provider = config.ProviderDocker
	reserved.Reserved = 1

	other := tier("other", 4, 8*config.GiB)
	other.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{reserved, other})

	// One machine with room for two.
	mustRegister(t, a, NodeRegistration{
		Name: "only", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})

	// While the floor is unmet, the other tier may take only the spare slot.
	if got, want := headroom(t, a, "other"), 1; got != want {
		t.Fatalf("with the floor unmet the other tier was offered %d, want %d", got, want)
	}

	if _, err := a.Reserve(t.Context(), "reserved"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The floor is met, so it holds nothing back — but the machine is now half
	// used, so what is left is one slot either way. The point is that the tier is
	// no longer being charged for a promise that has been kept.
	if got, want := headroom(t, a, "other"), 1; got != want {
		t.Errorf("with the floor met the other tier was offered %d, want %d", got, want)
	}
}

// A FLOOR NOTHING CAN EVER KEEP MUST NOT FREEZE THE FLEET.
//
// A tier reserved for more than the machines could ever hold is a configuration
// mistake, and the harm of reading it literally is total: every other tier would
// be refused forever, waiting for room that cannot exist. Billet keeps what it
// can and lets the rest compete.
func TestAnImpossibleFloorDoesNotStopEveryoneElse(t *testing.T) {
	impossible := tier("impossible", 4, 8*config.GiB)
	impossible.Provider = config.ProviderDocker
	impossible.Reserved = 99

	other := tier("other", 4, 8*config.GiB)
	other.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{impossible, other})

	mustRegister(t, a, NodeRegistration{
		Name: "only", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})

	// The floor takes what the fleet has; the rest of the reservation is simply
	// not keepable, and pretending otherwise would idle the machine forever.
	if got := headroom(t, a, "other"); got != 0 {
		t.Errorf("other = %d; the impossible floor should still hold what the fleet has", got)
	}

	if got := headroom(t, a, "impossible"); got == 0 {
		t.Error("the reserved tier cannot place anything, so nobody can use this fleet at all")
	}
}

// A FLOOR IS KEPT WHERE ITS OWN TIER COULD RUN, and holding it anywhere else is
// wrong in both directions at once.
//
// A macOS tier's reservation belongs on the Mac. Held against the machines the
// ASKING tier happens to use, it denies those machines room to protect a floor
// that could never be kept there, and leaves the Mac — the only host that
// matters to it — untouched. The reserved tier is no safer and everyone else is
// poorer.
func TestAFloorIsHeldOnTheMachinesItsOwnTierCouldUse(t *testing.T) {
	mac := config.Tier{
		Label: "macos", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
		Node: "mac-mini-1", VCPU: 4, Memory: 8 * config.GiB, Image: "macos-26",
	}
	mac.Reserved = 2

	linux := tier("linux", 4, 8*config.GiB)
	linux.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{mac, linux})

	// The Mac, which is the only host the reserved tier can use.
	mustRegister(t, a, NodeRegistration{
		Name: "mac-mini-1", Provider: config.ProviderTart, VCPU: 64, Memory: 512 * config.GiB})

	// A docker box with room for exactly two of the linux tier, and no business
	// keeping a macOS reservation.
	mustRegister(t, a, NodeRegistration{
		Name: "docker-box", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})

	if got, want := headroom(t, a, "linux"), 2; got != want {
		t.Errorf("the linux tier was offered %d of its %d slots; a macOS floor is being kept "+
			"on a machine that cannot boot macOS", got, want)
	}
}

// A FLOOR NOBODY CAN KEEP MUST NOT SPEND THE DEPLOYMENT'S CEILING.
//
// The fleet term holds floors on the machines that could keep them, which is
// where the promise lives. The GLOBAL term was also deducting every unmet floor
// from the deployment ceiling, without asking whether any machine could serve
// it — so a reservation on a tier with no suitable host anywhere took the
// ceiling away from tiers that were perfectly placeable.
//
// A Tart tier reserving two slots on a fleet of one Docker box is the clean
// case: nothing can ever keep that floor, and the Docker tier is the only thing
// that can run at all. Deducting it twice — once impossibly, once against a
// machine it cannot use — left an entirely healthy fleet advertising nothing.
func TestAFloorWithNoSuitableMachineDoesNotSpendTheCeiling(t *testing.T) {
	tart := config.Tier{
		Label: "tart-only", Provider: config.ProviderTart, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 8 * config.GiB, Image: "ubuntu-arm64",
	}
	tart.Reserved = 2

	docker := tier("docker", 4, 8*config.GiB)
	docker.Provider = config.ProviderDocker

	// A ceiling with room for exactly two of the docker tier.
	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 4000 * config.GiB},
		[]config.Tier{tart, docker})

	// The only machine cannot run the reserved tier at all.
	mustRegister(t, a, NodeRegistration{
		Name: "docker-box", Provider: config.ProviderDocker, VCPU: 64, Memory: 512 * config.GiB})

	if got, want := headroom(t, a, "docker"), 2; got != want {
		t.Errorf("the docker tier was offered %d of %d; a floor no machine can keep is "+
			"still taking the deployment's ceiling away from a tier that can run", got, want)
	}
}
