package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A JOB ON A BOUGHT SHAPE IS ONE FROM ITS ASSIGNMENT, not only from its archive:
// the history row the assignment opens names the shape the lease bought and its
// price, read from the lease row, so a report that estimates such a job's energy
// or counts its cost does not skip it while its teardown runs. A host-backed
// lease, which buys nothing, names neither.
func TestAJobsBoughtShapeIsInItsHistoryFromItsAssignment(t *testing.T) {
	t.Parallel()

	cloud := config.Tier{
		Label: "cloud-arm", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-arm", Arch: config.ArchARM64,
	}
	local := config.Tier{
		Label: "local", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ubuntu:24.04", Arch: config.ArchX64,
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{cloud, local})
	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 16, Memory: 64 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "c7g.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchARM64, PriceUSDPerHour: 290_000},
		},
		Architectures: []config.Arch{config.ArchARM64},
	}); err != nil {
		t.Fatalf("RegisterNode cloud-1: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "host-1", Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB,
		Architectures: []config.Arch{config.ArchX64},
	}); err != nil {
		t.Fatalf("RegisterNode host-1: %v", err)
	}

	for _, tc := range []struct {
		tier, wantShape string
		wantPrice       bool
	}{
		{"cloud-arm", "c7g.2xlarge", true},
		{"local", "", false},
	} {
		leases, err := a.Escrow(t.Context(), tc.tier, 1)
		if err != nil || len(leases) != 1 {
			t.Fatalf("Escrow %s = %v, %v", tc.tier, leases, err)
		}
		l := leases[0]
		if err := a.Assign(t.Context(), l.ID, l.Epoch, 7, 1); err != nil {
			t.Fatalf("Assign %s: %v", tc.tier, err)
		}

		rec, err := a.Job(t.Context(), l.ID)
		if err != nil {
			t.Fatalf("Job %s: %v", tc.tier, err)
		}
		if rec.InstanceType != tc.wantShape {
			t.Errorf("%s: the assigned job's history names shape %q, want %q", tc.tier, rec.InstanceType,
				tc.wantShape)
		}
		placed, err := a.HistoryPlacement(t.Context(), l.ID)
		if err != nil {
			t.Fatalf("HistoryPlacement %s: %v", tc.tier, err)
		}
		if placed.InstanceType != tc.wantShape || (placed.PriceUSDPerHour == 290_000) != tc.wantPrice ||
			(!tc.wantPrice && placed.PriceUSDPerHour != 0) {
			t.Errorf("%s: the assigned job's history = %+v, want shape %q and the price only for a bought one",
				tc.tier, placed, tc.wantShape)
		}
	}
}
