package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A TART TIER NAMING `@verified` IS NOT PLACED ON A TART HOST TOO OLD TO READ IT, or
// placement keeps choosing a host the plane then refuses to launch on while a newer one
// sits idle. The same image without `@verified` still uses the old host, and so does a
// Firecracker tier, whose `@verified` names a generation.
func TestAVerifiedTartTierIsPlacedOnlyOnAHostThatReadsIt(t *testing.T) {
	t.Parallel()

	onTart := func(label, image string) config.Tier {
		return config.Tier{
			Label: label, Provider: config.ProviderTart, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 4 * config.GiB, Image: image,
		}
	}

	verified := onTart("verified", "ghcr.io/cirruslabs/ubuntu:latest@verified")
	plain := onTart("plain", "ghcr.io/cirruslabs/ubuntu:latest")
	generation := tier("generation", 2, 4*config.GiB)
	generation.Image = "ubuntu-2404-x64@verified"

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{verified, plain, generation})

	older := TartVerifiedWireVersion - 1

	for _, kind := range []config.ProviderKind{config.ProviderTart, config.ProviderFirecracker} {
		reg := testRegistration("a-old-"+string(kind), kind)
		reg.WireMin, reg.WireVersion, reg.WireMax = 12, older, older

		if _, err := a.RegisterNode(t.Context(), reg); err != nil {
			t.Fatalf("RegisterNode(%s): %v", reg.Name, err)
		}
	}

	if got := headroom(t, a, "verified"); got != 0 {
		t.Fatalf("verified headroom with only a Tart host at wire %d = %d, want 0", older, got)
	}

	if got := headroom(t, a, "plain"); got == 0 {
		t.Fatal("a Tart tier without @verified could not use the older Tart host")
	}

	if got := headroom(t, a, "generation"); got == 0 {
		t.Fatal("a Firecracker tier's @verified generation could not use the older Firecracker host")
	}

	// NAMED TO SORT AFTER THE OLD ONE, so a placement that still counted the old host
	// would choose it first.
	current := testRegistration("b-new-tart", config.ProviderTart)
	current.WireMin, current.WireVersion, current.WireMax = 12, TartVerifiedWireVersion, TartVerifiedWireVersion

	if _, err := a.RegisterNode(t.Context(), current); err != nil {
		t.Fatalf("RegisterNode(new): %v", err)
	}

	leases, err := a.Escrow(t.Context(), "verified", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("Escrow = %v, %v", leases, err)
	}

	if leases[0].TargetNode != "b-new-tart" {
		t.Fatalf("verified lease placed on %q, want b-new-tart", leases[0].TargetNode)
	}
}
