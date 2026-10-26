package alloc

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// archTier is a two-vCPU Firecracker tier of arch, or of none.
func archTier(label string, arch config.Arch) config.Tier {
	return config.Tier{
		Label: label, Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ubuntu-2404-" + string(arch), Arch: arch,
	}
}

// registerArchNode registers an eight-vCPU Firecracker host reporting archs.
func registerArchNode(t *testing.T, a *Allocator, name string, archs ...config.Arch) {
	t.Helper()

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: name, Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 32 * config.GiB, Architectures: archs,
	}); err != nil {
		t.Fatalf("RegisterNode %s: %v", name, err)
	}
}

// targets escrows every lease a tier can have and says where each was placed.
func targets(t *testing.T, a *Allocator, tier string) []string {
	t.Helper()

	leases, err := a.Escrow(t.Context(), tier, 100)
	if err != nil {
		t.Fatalf("Escrow %s: %v", tier, err)
	}

	out := make([]string, 0, len(leases))
	for _, l := range leases {
		out = append(out, l.TargetNode)
	}

	return out
}

// A TIER THAT NAMES AN ARCHITECTURE IS PLACED ONLY ON NODES THAT REPORT IT, and a
// node that reports none, from before the wire that carries it, serves only tiers
// that name none (ADR-022).
func TestATierIsPlacedOnlyOnNodesReportingItsArchitecture(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB},
		[]config.Tier{archTier("intel", config.ArchX64), archTier("graviton", config.ArchARM64)})

	registerArchNode(t, a, "amd-1", config.ArchX64)
	registerArchNode(t, a, "arm-1", config.ArchARM64)
	registerArchNode(t, a, "old-1")

	for tier, want := range map[string]string{"intel": "amd-1", "graviton": "arm-1"} {
		got := targets(t, a, tier)
		if len(got) != 4 {
			t.Errorf("%s was escrowed %d leases, want the 4 its one node holds: %v", tier, len(got), got)
		}

		for _, node := range got {
			if node != want {
				t.Errorf("%s was placed on %s, want only %s", tier, node, want)
			}
		}
	}
}

// AND A CATALOGUE THAT NAMES NONE PLACES AS IT ALWAYS DID, on a node of any
// architecture and on one that reports none.
func TestATierNamingNoArchitectureIsPlacedOnAnyNode(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB},
		[]config.Tier{archTier("any", "")})

	registerArchNode(t, a, "amd-1", config.ArchX64)
	registerArchNode(t, a, "arm-1", config.ArchARM64)
	registerArchNode(t, a, "old-1")

	seen := map[string]int{}
	for _, node := range targets(t, a, "any") {
		seen[node]++
	}

	if seen["amd-1"] != 4 || seen["arm-1"] != 4 || seen["old-1"] != 4 {
		t.Errorf("a tier naming no architecture was placed %v, want 4 on each node", seen)
	}
}

// AN EC2 NODE IS BOUGHT ONLY A SHAPE OF THE TIER'S ARCHITECTURE, though a shape of
// another comes first in its order and fits.
func TestAnEC2TierIsBoughtOnlyAShapeOfItsArchitecture(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label: "cloud-arm", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-arm", Arch: config.ArchARM64,
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{tier})

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 16, Memory: 64 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchX64},
			{Type: "c7g.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchARM64},
			{Type: "unsaid", VCPU: 8, Memory: 16 * config.GiB},
		},
		Architectures: []config.Arch{config.ArchX64, config.ArchARM64},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	leases, err := a.Escrow(t.Context(), "cloud-arm", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("Escrow = %v, %v", leases, err)
	}

	if got := leases[0].InstanceType; got != "c7g.2xlarge" {
		t.Errorf("an arm64 tier was charged shape %q, want c7g.2xlarge", got)
	}
}

// A REPORT PLACEMENT COULD BE MISLED BY IS REFUSED, and a node that reports none is
// accepted as it always was.
func TestANodesArchitectureReportIsHeldToItsProvider(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, nil)

	arm := []config.EC2InstanceType{{Type: "c7g.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchARM64}}

	for _, tc := range []struct {
		name    string
		reg     NodeRegistration
		wantErr string
	}{
		{name: "a host reporting none", reg: NodeRegistration{Provider: config.ProviderFirecracker}},
		{name: "a host reporting its own", reg: NodeRegistration{Provider: config.ProviderFirecracker,
			Architectures: []config.Arch{config.ArchARM64}}},
		{name: "a host reporting two", reg: NodeRegistration{Provider: config.ProviderFirecracker,
			Architectures: []config.Arch{config.ArchARM64, config.ArchX64}},
			wantErr: "it launches one, its host's or its environment's"},
		{name: "a spelling nothing writes", reg: NodeRegistration{Provider: config.ProviderFirecracker,
			Architectures: []config.Arch{"aarch64"}}, wantErr: `architecture "aarch64" is not one`},
		{name: "an EC2 node reporting its shapes'", reg: NodeRegistration{Provider: config.ProviderEC2,
			EC2Shapes: arm, Architectures: []config.Arch{config.ArchARM64}}},
		{name: "an EC2 node reporting more than its shapes", reg: NodeRegistration{
			Provider: config.ProviderEC2, EC2Shapes: arm,
			Architectures: []config.Arch{config.ArchARM64, config.ArchX64}},
			wantErr: "an EC2 node launches what its shapes run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := tc.reg
			reg.Name = strings.ReplaceAll(tc.name, " ", "-")
			reg.VCPU, reg.Memory = 8, 32*config.GiB

			_, err := a.RegisterNode(t.Context(), reg)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("answered %v, want %q", err, tc.wantErr)
			}
		})
	}
}
