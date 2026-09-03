package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func TestEC2CostStatesItsBoundAndSource(t *testing.T) {
	cfg := &config.Config{
		Server: &config.ServerConfig{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		Node: &config.NodeConfig{
			Provider:  config.ProviderEC2,
			MaxVCPU:   8,
			MaxMemory: 16 * config.GiB,
			EC2: &config.EC2Config{InstanceTypes: []config.EC2InstanceType{
				{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB,
					PriceUSDPerHour: 340_000},
			}},
		},
	}

	out := capture(t, func() {
		if err := printRemoteCost(cfg); err != nil {
			t.Fatalf("printRemoteCost: %v", err)
		}
	})
	for _, want := range []string{"<= $0.34/hour", "$248.2/month", "declared shape prices"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q: %s", want, out)
		}
	}
}

func TestRemoteFleetCostStatesItsDeploymentBoundAndScope(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	for _, price := range []config.USDPerHour{340_000, 600_000} {
		_, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
			Name: "cloud-" + price.Decimal(), Provider: config.ProviderEC2,
			VCPU: 8, Memory: 16 * config.GiB,
			EC2Shapes: []config.EC2InstanceType{{
				Type: "shape-" + price.Decimal(), VCPU: 8, Memory: 16 * config.GiB,
				PriceUSDPerHour: price,
			}},
		})
		if err != nil {
			t.Fatalf("RegisterNode: %v", err)
		}
	}
	cfg := &config.Config{Server: &config.ServerConfig{
		MaxVCPU: 8, MaxMemory: 16 * config.GiB,
	}}

	out := capture(t, func() {
		if err := printRemoteFleetCost(t.Context(), a, cfg); err != nil {
			t.Fatalf("printRemoteFleetCost: %v", err)
		}
	})
	for _, want := range []string{
		"<= $0.6/hour", "$438/month", "across 2 registered remote node(s)",
		"declared shape prices",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q: %s", want, out)
		}
	}
}

// A CODEBUILD FLEET IS COSTED IN `billet status`, and a deployment whose only cloud
// capacity is codebuild used to print nothing here at all.
//
// The query behind the line was scoped to `provider = 'ec2'`, so the absence of a cost
// line was indistinguishable from a fleet that costs nothing. This drives the printer
// rather than the allocator, because the property is "status says so": the allocator's
// own coverage is asserted one layer down, and a helper test would stay green with the
// call deleted from cmdStatus.
func TestRemoteFleetCostCoversACodeBuildOnlyDeployment(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "cloud-codebuild", Provider: config.ProviderCodeBuild,
		VCPU: 8, Memory: 16 * config.GiB,
		EC2Shapes: []config.RemoteShape{{
			Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB,
			PriceUSDPerHour: 600_000,
		}},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{
		MaxVCPU: 8, MaxMemory: 16 * config.GiB,
	}}

	out := capture(t, func() {
		if err := printRemoteFleetCost(t.Context(), a, cfg); err != nil {
			t.Fatalf("printRemoteFleetCost: %v", err)
		}
	})

	if out == "" {
		t.Fatal("a deployment whose only cloud capacity is codebuild printed no cost line at " +
			"all, which reads as compute that is free")
	}

	for _, want := range []string{"cloud peak", "across 1 registered remote node(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q: %s", want, out)
		}
	}
}

func TestRemoteFleetCostLeavesStatusUsableForARetiredLegacyNode(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, site, total_vcpu, total_memory, last_seen_at, live)
			 VALUES ('retired-cloud', 'ec2', '', 8, ?, '2026-08-18T00:00:00Z', 0)`,
			int64(16*config.GiB))

		return err
	}); err != nil {
		t.Fatalf("insert legacy EC2 node: %v", err)
	}
	cfg := &config.Config{Server: &config.ServerConfig{
		MaxVCPU: 8, MaxMemory: 16 * config.GiB,
	}}

	out := capture(t, func() {
		if err := printRemoteFleetCost(t.Context(), a, cfg); err != nil {
			t.Fatalf("printRemoteFleetCost made status fail: %v", err)
		}
	})
	for _, want := range []string{"cloud peak  unavailable", "retired-cloud"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q: %s", want, out)
		}
	}
}
