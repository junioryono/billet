package alloc

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestRemoteCostNodesIncludesRegisteredCloudCapacityAfterLivenessLoss(t *testing.T) {
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

	got, err := a.RemoteCostNodes(t.Context())
	if err != nil {
		t.Fatalf("RemoteCostNodes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("RemoteCostNodes returned %d nodes, want only the registered EC2 node", len(got))
	}
	if got[0].MaxVCPU != cloud.VCPU || got[0].MaxMemory != cloud.Memory ||
		len(got[0].Shapes) != 1 || got[0].Shapes[0].Type != "c7i.2xlarge" {
		t.Errorf("RemoteCostNodes = %+v, want the cloud node's registered declarations", got)
	}
}

// EVERY REMOTE BACKEND IS COSTED, NOT JUST EC2.
//
// The query behind this was scoped to `provider = 'ec2'`, so a deployment whose only
// cloud capacity was codebuild printed no cost line in `billet status` at all — and
// the absence of a cost line is exactly what a fleet that costs nothing looks like. A
// codebuild node declares ordered shapes with a price per hour for the same reason an
// ec2 node does: placement charges the first that fits.
//
// BOTH BACKENDS IN ONE LEDGER, so the assertion cannot pass by the query having been
// re-scoped to codebuild instead. And the HOST-BACKED node is asserted ABSENT, because
// its capacity is the machine somebody already owns — costing it would invent a bill.
func TestRemoteCostNodesCoversEveryRemoteBackend(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB}, nil)

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-ec2", Provider: config.ProviderEC2,
		VCPU: 8, Memory: 16 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{
			Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB,
			PriceUSDPerHour: 340_000,
		}},
	}); err != nil {
		t.Fatalf("RegisterNode(ec2): %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-codebuild", Provider: config.ProviderCodeBuild,
		VCPU: 4, Memory: 7 * config.GiB,
		EC2Shapes: []config.RemoteShape{{
			Type: "BUILD_GENERAL1_SMALL", VCPU: 2, Memory: 3 * config.GiB,
			PriceUSDPerHour: 300_000,
		}, {
			Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB,
			PriceUSDPerHour: 600_000,
		}},
	}); err != nil {
		t.Fatalf("RegisterNode(codebuild): %v", err)
	}

	if _, err := a.RegisterNode(t.Context(),
		testRegistration("home", config.ProviderFirecracker)); err != nil {
		t.Fatalf("RegisterNode(home): %v", err)
	}

	got, err := a.RemoteCostNodes(t.Context())
	if err != nil {
		t.Fatalf("RemoteCostNodes: %v", err)
	}

	// Ordered by name, so cloud-codebuild precedes cloud-ec2.
	if len(got) != 2 {
		t.Fatalf("RemoteCostNodes returned %d nodes, want both remote backends and not the "+
			"host-backed one: %+v", len(got), got)
	}

	if len(got[0].Shapes) != 2 || got[0].Shapes[0].Type != "BUILD_GENERAL1_SMALL" {
		t.Errorf("the codebuild node's shapes did not survive the round trip: %+v", got[0])
	}

	if len(got[1].Shapes) != 1 || got[1].Shapes[0].Type != "c7i.2xlarge" {
		t.Errorf("the ec2 node's shapes did not survive the round trip: %+v", got[1])
	}

	// AND THE BOUND IS NON-ZERO FOR THE CODEBUILD NODE ALONE, which is the assertion
	// the old query failed: with only codebuild registered there was nothing to report
	// and nothing said so.
	peak, err := config.RemoteFleetPeakHourlyExposure(4, 7*config.GiB,
		[]config.RemoteCostNode{got[0]})
	if err != nil {
		t.Fatalf("RemoteFleetPeakHourlyExposure: %v", err)
	}

	if peak == 0 {
		t.Error("a registered codebuild fleet bounds to $0.00/hour, which reads as compute " +
			"that is free")
	}
}

func TestRemoteCostNodesRefusesARegistrationWithoutPrices(t *testing.T) {
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

	if _, err := a.RemoteCostNodes(t.Context()); !errors.Is(err, ErrRemoteCostUnavailable) {
		t.Fatalf("RemoteCostNodes error = %v, want ErrRemoteCostUnavailable", err)
	}
}

func TestRemoteCostNodesRefusesALegacyEmptyCatalogue(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, nil)
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, site, total_vcpu, total_memory, last_seen_at, live)
			 VALUES ('old-cloud', 'ec2', '', 8, $1, '2026-08-18T00:00:00Z', 0)`,
			int64(16*config.GiB))
		return err
	}); err != nil {
		t.Fatalf("insert legacy EC2 node: %v", err)
	}

	if _, err := a.RemoteCostNodes(t.Context()); !errors.Is(err, ErrRemoteCostUnavailable) {
		t.Fatalf("RemoteCostNodes error = %v, want ErrRemoteCostUnavailable", err)
	}
}

func TestRemoteCostNodesIncludesOutstandingShapePrices(t *testing.T) {
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

	got, err := a.RemoteCostNodes(t.Context())
	if err != nil {
		t.Fatalf("RemoteCostNodes: %v", err)
	}
	if len(got) != 1 || got[0].Outstanding != 1_200_000 {
		t.Fatalf("RemoteCostNodes = %+v, want $1.20/hour outstanding", got)
	}
}
