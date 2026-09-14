package alloc

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THREE CATALOGUE ENTRIES SHARE TWO REAL SLOTS. Per-entry maxima may overlap,
// but the third simultaneous launch cannot get the lease that must precede it.
func TestMacOSCatalogueOverlapDoesNotIncreaseRuntimeSlots(t *testing.T) {
	tiers := make([]config.Tier, 0, 3)
	for _, label := range []string{"mac-a", "mac-b", "mac-c"} {
		tiers = append(tiers, config.Tier{
			Label: label, Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
			Node: "mac-1", VCPU: 2, Memory: 4 * config.GiB, Image: "macos",
			MaxConcurrent: 1,
		})
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB}, tiers)
	mustRegister(t, a, NodeRegistration{
		Name: "mac-1", Provider: config.ProviderTart, VCPU: 32, Memory: 64 * config.GiB,
	})
	for i, label := range []string{"mac-a", "mac-b"} {
		lease := reserve(t, a, label)
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "mac-1"); err != nil {
			t.Fatal(err)
		}
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Reserve(t.Context(), "mac-c"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("third simultaneous launch escrow = %v, want no capacity", err)
	}
	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Leases != 2 || usage.VCPU != 4 || usage.Memory != 8*config.GiB {
		t.Fatalf("runtime usage = %+v, want two 2-vcpu / 4GiB leases", usage)
	}
}

// AN UNAVAILABLE BACKEND CANNOT BLOCK THE ADMISSION QUEUE. A busy fitting host
// remains eligible for waiting, and explicit floors remain unavailable to peers.
func TestPotentialCapacityDistinguishesMissingHostsFromBusyOnes(t *testing.T) {
	small := tier("small", 2, 4*config.GiB)
	large := tier("large", 8, 16*config.GiB)
	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, []config.Tier{small, large})
	before, err := a.PotentialCapacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before["small"] != 0 || before["large"] != 0 {
		t.Fatalf("missing hosts have potential %v", before)
	}
	mustRegister(t, a, NodeRegistration{
		Name: "host", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
	})
	reserve(t, a, "large")
	after, err := a.PotentialCapacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after["small"] != 4 || after["large"] != 1 || headroom(t, a, "small") != 0 {
		t.Fatalf("busy fleet potential = %v, want small 4 and large 1 with no free room", after)
	}
}

// AN EXPLICIT FLOOR REMAINS A GUARANTEE WHILE ARBITRATION WAITS. A hypothetical
// empty fleet cannot offer a large contender room reserved for its idle peer.
func TestPotentialCapacityKeepsExplicitFloors(t *testing.T) {
	reserved := tier("reserved", 2, 4*config.GiB)
	reserved.Reserved = 1
	large := tier("large", 8, 16*config.GiB)
	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, []config.Tier{reserved, large})
	mustRegister(t, a, NodeRegistration{
		Name: "host", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
	})
	potential, err := a.PotentialCapacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if potential["large"] != 0 || potential["reserved"] != 4 {
		t.Fatalf("potential with an explicit floor = %v, want large 0 and reserved 4", potential)
	}
	if headroom(t, a, "large") != 0 {
		t.Fatal("arbitration's eligibility read changed the explicit floor")
	}
}
