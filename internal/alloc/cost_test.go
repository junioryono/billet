package alloc

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestEC2CostNodesIncludesRegisteredCloudCapacityAfterLivenessLoss(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, nil)
	cloud := NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 8, Memory: 16 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{
			Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB,
			PriceUSDPerHour: 340_000,
		}},
	}
	epoch, err := a.RegisterNode(t.Context(), cloud)
	if err != nil {
		t.Fatalf("RegisterNode(cloud): %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), testRegistration("home", config.ProviderFirecracker)); err != nil {
		t.Fatalf("RegisterNode(home): %v", err)
	}
	if err := a.NodeGone(t.Context(), cloud.Name, epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	got, err := a.EC2CostNodes(t.Context())
	if err != nil {
		t.Fatalf("EC2CostNodes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("EC2CostNodes returned %d nodes, want only the registered EC2 node", len(got))
	}
	if got[0].MaxVCPU != cloud.VCPU || got[0].MaxMemory != cloud.Memory ||
		len(got[0].InstanceTypes) != 1 || got[0].InstanceTypes[0].Type != "c7i.2xlarge" {
		t.Errorf("EC2CostNodes = %+v, want the cloud node's registered declarations", got)
	}
}

func TestEC2CostNodesRefusesARegistrationWithoutPrices(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, nil)
	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "old-cloud", Provider: config.ProviderEC2,
		VCPU: 8, Memory: 16 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{
			Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB,
		}},
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if _, err := a.EC2CostNodes(t.Context()); !errors.Is(err, ErrEC2CostUnavailable) {
		t.Fatalf("EC2CostNodes error = %v, want ErrEC2CostUnavailable", err)
	}
}

func TestEC2CostNodesRefusesALegacyEmptyCatalogue(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, nil)
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, site, total_vcpu, total_memory, last_seen_at, live)
			 VALUES ('old-cloud', 'ec2', '', 8, ?, '2026-08-18T00:00:00Z', 0)`,
			int64(16*config.GiB))
		return err
	}); err != nil {
		t.Fatalf("insert legacy EC2 node: %v", err)
	}

	if _, err := a.EC2CostNodes(t.Context()); !errors.Is(err, ErrEC2CostUnavailable) {
		t.Fatalf("EC2CostNodes error = %v, want ErrEC2CostUnavailable", err)
	}
}

func TestEC2CostNodesIncludesOutstandingShapePrices(t *testing.T) {
	tier := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{tier})
	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 16, Memory: 32 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{
			Type: "shape", VCPU: 8, Memory: 16 * config.GiB, PriceUSDPerHour: 600_000,
		}},
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	for range 2 {
		if _, err := a.Reserve(t.Context(), tier.Label); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	}

	got, err := a.EC2CostNodes(t.Context())
	if err != nil {
		t.Fatalf("EC2CostNodes: %v", err)
	}
	if len(got) != 1 || got[0].Outstanding != 1_200_000 {
		t.Fatalf("EC2CostNodes = %+v, want $1.20/hour outstanding", got)
	}
}
