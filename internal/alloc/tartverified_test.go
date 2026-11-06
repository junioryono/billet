package alloc

import (
	"slices"
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

// A TIER SPANNING BACKENDS IS JUDGED BY THE HOST'S, not by whether it accepts Tart at all:
// its Tart image names `@verified` and its Firecracker image does not, so an older
// Firecracker host still serves it and only the older Tart host is passed over.
func TestAVerifiedTartImageExcludesOnlyTheOlderTartHostOfAMixedTier(t *testing.T) {
	t.Parallel()

	mixed := config.Tier{
		Label: "mixed", Providers: []config.ProviderKind{config.ProviderTart, config.ProviderFirecracker},
		GuestOS: config.GuestLinux, VCPU: 2, Memory: 4 * config.GiB,
		Launch: map[config.ProviderKind]config.TierLaunch{
			config.ProviderTart:        {Image: "ghcr.io/cirruslabs/ubuntu:latest@verified"},
			config.ProviderFirecracker: {Image: "ubuntu-2404-x64@20261106"},
		},
	}

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB}, []config.Tier{mixed})

	host := func(name string, kind config.ProviderKind, wire int) nodeRow {
		return nodeRow{name: name, provider: kind, vcpu: 64, memory: 128 * config.GiB, wire: wire}
	}

	rows := []nodeRow{
		host("a-old-firecracker", config.ProviderFirecracker, TartVerifiedWireVersion-1),
		host("a-old-tart", config.ProviderTart, TartVerifiedWireVersion-1),
		host("b-new-tart", config.ProviderTart, TartVerifiedWireVersion),
	}

	var got []string
	for _, n := range a.eligibleFrom(rows, mixed, 0) {
		got = append(got, n.name)
	}

	if want := []string{"a-old-firecracker", "b-new-tart"}; !slices.Equal(got, want) {
		t.Fatalf("a mixed tier is eligible on %v, want %v", got, want)
	}
}
