package alloc

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func ec2AccountingAllocator(t *testing.T) *Allocator {
	t.Helper()

	tier := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier})

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 16, Memory: 64 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB},
		},
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return a
}

// A cloud node whose catalogue cannot satisfy the tier is not eligible. This is
// enforced from registration data, so it also works when node and server use
// different config files.
func TestEC2PlacementRequiresARegisteredShapeThatFits(t *testing.T) {
	tier := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 8, Memory: 16 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier})

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{
			Type: "too-small", VCPU: 4, Memory: 8 * config.GiB,
		}},
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if got := headroom(t, a, "cloud"); got != 0 {
		t.Fatalf("Headroom = %d, want 0 when no registered shape fits", got)
	}
	if _, err := a.Reserve(t.Context(), "cloud"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Reserve = %v, want ErrNoCapacity", err)
	}
}

// The deployment ceiling is shared even when each EC2 node has room of its own.
// Charging only the tier request here would advertise eight jobs instead of two.
func TestEC2ShapesCountAgainstTheDeploymentCeilingAcrossNodes(t *testing.T) {
	tier := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier})

	for _, name := range []string{"cloud-1", "cloud-2"} {
		_, err := a.RegisterNode(t.Context(), NodeRegistration{
			Name: name, Provider: config.ProviderEC2,
			VCPU: 16, Memory: 32 * config.GiB,
			EC2Shapes: []config.EC2InstanceType{{
				Type: "eight", VCPU: 8, Memory: 16 * config.GiB,
			}},
		})
		if err != nil {
			t.Fatalf("RegisterNode(%s): %v", name, err)
		}
	}

	if got := headroom(t, a, "cloud"); got != 2 {
		t.Fatalf("Headroom = %d, want 2 shapes under the shared deployment ceiling", got)
	}
}

// Changing the ordered catalogue changes both placement and fallback. Existing
// escrow therefore pins it just as existing work pins a node's provider and site.
func TestEC2ShapeCatalogueCannotChangeUnderOutstandingEscrow(t *testing.T) {
	a := ec2AccountingAllocator(t)
	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	changed := NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 16, Memory: 64 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{
			Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB,
		}},
	}
	if _, err := a.RegisterNode(t.Context(), changed); err == nil ||
		!strings.Contains(err.Error(), "outstanding") {
		t.Fatalf("shape catalogue changed under escrow: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), changed); err != nil {
		t.Fatalf("idle node could not change shape catalogue: %v", err)
	}
}

// The issue-47 example: a 2-vCPU tier backed by an 8-vCPU shape and a 16-vCPU
// node may advertise two jobs, not eight.
func TestEC2EscrowChargesTheSelectedShape(t *testing.T) {
	a := ec2AccountingAllocator(t)

	if got := headroom(t, a, "cloud"); got != 2 {
		t.Fatalf("Headroom = %d, want 2 purchasable shapes", got)
	}

	leases, err := a.Escrow(t.Context(), "cloud", 8)
	if err != nil {
		t.Fatalf("Escrow: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("Escrow returned %d leases, want 2", len(leases))
	}

	for _, lease := range leases {
		if lease.VCPU != 8 || lease.Memory != 16*config.GiB || lease.InstanceType != "eight" {
			t.Errorf("charged lease %+v, want the selected eight-vCPU shape", lease)
		}
		if lease.RequestedVCPU != 2 || lease.RequestedMemory != 4*config.GiB {
			t.Errorf("lease forgot the tier request: %+v", lease)
		}
	}
}

// A larger fallback is authorised only if the real purchase still fits. The
// first lease cannot grow while the second 8-vCPU shape holds the rest of the
// node, then can grow after that reservation is released.
func TestEC2FallbackResizesInsideTheBudget(t *testing.T) {
	a := ec2AccountingAllocator(t)
	leases, err := a.Escrow(t.Context(), "cloud", 2)
	if err != nil || len(leases) != 2 {
		t.Fatalf("Escrow = %d leases, %v; want 2", len(leases), err)
	}

	lease := leases[0]
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "cloud-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	err = a.Resize(t.Context(), lease.ID, lease.Epoch, "sixteen", 16, 32*config.GiB)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("fallback beyond the budget returned %v, want ErrNoCapacity", err)
	}

	if err := a.Release(t.Context(), leases[1].ID, leases[1].Epoch, PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.Resize(t.Context(), lease.ID, lease.Epoch,
		"sixteen", 16, 32*config.GiB); err != nil {
		t.Fatalf("Resize after capacity returned: %v", err)
	}

	got, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if got.VCPU != 16 || got.Memory != 32*config.GiB || got.InstanceType != "sixteen" {
		t.Errorf("resized lease = %+v, want the sixteen-vCPU shape", got)
	}
}

func TestEC2FallbackPreservesAnotherTiersFloor(t *testing.T) {
	tiers := []config.Tier{
		{Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test"},
		{Label: "reserved", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
			VCPU: 8, Memory: 16 * config.GiB, Reserved: 1, Image: "ami-test"},
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, tiers)
	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 16, Memory: 32 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB},
		},
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "cloud-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	err = a.Resize(t.Context(), lease.ID, lease.Epoch, "sixteen", 16, 32*config.GiB)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("fallback consumed reserved capacity: %v", err)
	}
}

func TestLegacyEC2CatalogueInitializesWithOutstandingWork(t *testing.T) {
	a := ec2AccountingAllocator(t)
	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET ec2_shapes = '' WHERE name = 'cloud-1'`)

		return err
	}); err != nil {
		t.Fatalf("simulate migration 19: %v", err)
	}

	reg := NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 16, Memory: 64 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB},
		},
	}
	if _, err := a.RegisterNode(t.Context(), reg); err != nil {
		t.Fatalf("first post-migration registration with lease %s: %v", lease.ID, err)
	}
}
