package main

import (
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
		if err := printEC2Cost(cfg); err != nil {
			t.Fatalf("printEC2Cost: %v", err)
		}
	})
	for _, want := range []string{"<= $0.34/hour", "$248.2/month", "declared shape prices"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q: %s", want, out)
		}
	}
}

func TestEC2FleetCostStatesItsDeploymentBoundAndScope(t *testing.T) {
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
		if err := printEC2FleetCost(t.Context(), a, cfg); err != nil {
			t.Fatalf("printEC2FleetCost: %v", err)
		}
	})
	for _, want := range []string{
		"<= $0.6/hour", "$438/month", "across 2 registered node(s)", "declared shape prices",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q: %s", want, out)
		}
	}
}
