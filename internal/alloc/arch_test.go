package alloc

import (
	"errors"
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
			// DECLARING NONE, AND BEFORE THE arm64 ONE, so a filter that kept a shape
			// it cannot tell about would buy this.
			{Type: "unsaid", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "c7g.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchARM64},
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

// A CODEBUILD TIER NAMING AN ARCHITECTURE IS PLACED BY ITS NODE'S ENVIRONMENT: a
// compute type names none, so the per-shape rule is EC2's alone, or every
// CodeBuild tier would place nowhere once a catalogue named arch.
func TestACodeBuildTierIsPlacedByItsEnvironmentsArchitecture(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label: "cb-arm", Provider: config.ProviderCodeBuild, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 7 * config.GiB, Image: "img", Arch: config.ArchARM64,
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, []config.Tier{tier})

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cb-1", Provider: config.ProviderCodeBuild, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: codeBuildShapes(), Architectures: []config.Arch{config.ArchARM64},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if got := targets(t, a, "cb-arm"); len(got) == 0 {
		t.Error("a CodeBuild tier of its environment's architecture was placed nowhere")
	}
}

// A HOST DOES NOT DROP AN ARCHITECTURE WHILE WORK IS OUTSTANDING ON IT, and adding
// one, or reporting none, as a node rolled back to an older wire does, is free.
func TestAHostMayNotDropAnArchitectureUnderOutstandingWork(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB},
		[]config.Tier{archTier("intel", config.ArchX64)})

	registerArchNode(t, a, "host-1", config.ArchX64)

	if got := targets(t, a, "intel"); len(got) == 0 {
		t.Fatal("nothing was escrowed on the host")
	}

	reg := func(archs ...config.Arch) error {
		_, err := a.RegisterNode(t.Context(), NodeRegistration{
			Name: "host-1", Provider: config.ProviderFirecracker,
			VCPU: 8, Memory: 32 * config.GiB, Architectures: archs,
		})

		return err
	}

	if err := reg(config.ArchARM64); err == nil || !strings.Contains(err.Error(), "now does not launch x64") {
		t.Errorf("a host that dropped x64 under escrowed work was answered %v", err)
	}

	if err := reg(); err != nil {
		t.Errorf("a host reporting none, as an older wire does, was refused: %v", err)
	}

	if err := reg(config.ArchX64); err != nil {
		t.Errorf("the same architecture again was refused: %v", err)
	}
}

// A FALLBACK DOES NOT CROSS THE TIER'S ARCHITECTURE, and is refused as no capacity,
// so the node tries its next shape rather than giving the launch up.
func TestAnEC2FallbackDoesNotCrossItsTiersArchitecture(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label: "cloud-arm", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-arm", Arch: config.ArchARM64,
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{tier})

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "c7g.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchARM64},
			{Type: "c7i.4xlarge", VCPU: 16, Memory: 32 * config.GiB, Arch: config.ArchX64},
			{Type: "unsaid", VCPU: 16, Memory: 32 * config.GiB},
			{Type: "c7g.4xlarge", VCPU: 16, Memory: 32 * config.GiB, Arch: config.ArchARM64},
		},
		Architectures: []config.Arch{config.ArchARM64, config.ArchX64},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	leases, err := a.Escrow(t.Context(), "cloud-arm", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("Escrow = %v, %v", leases, err)
	}

	l := leases[0]
	if err := a.Assign(t.Context(), l.ID, l.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.Bind(t.Context(), l.ID, l.Epoch, "cloud-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := a.Advance(t.Context(), l.ID, l.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	for _, shape := range []string{"c7i.4xlarge", "unsaid"} {
		if err := a.Resize(t.Context(), l.ID, l.Epoch, shape, 16, 32*config.GiB); !errors.Is(err, ErrNoCapacity) {
			t.Errorf("a fallback to %s was answered %v, want ErrNoCapacity", shape, err)
		}
	}

	if err := a.Resize(t.Context(), l.ID, l.Epoch, "c7g.4xlarge", 16, 32*config.GiB); err != nil {
		t.Errorf("a fallback of the tier's architecture was refused: %v", err)
	}
}

// A CATALOGUE BUILT IN CODE IS HELD TO THE ARCHITECTURE RULE TOO.
func TestTheAllocatorRefusesACatalogueNamingArchOnSomeTiers(t *testing.T) {
	t.Parallel()

	db := openTestLedger(t)

	_, err := New(db, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{archTier("intel", config.ArchX64), archTier("any", "")})
	if err == nil || !strings.Contains(err.Error(), "arch is named on 1 of 2 tiers") {
		t.Fatalf("New = %v, want the all-or-none refusal", err)
	}
}
