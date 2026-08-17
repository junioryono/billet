package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
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
