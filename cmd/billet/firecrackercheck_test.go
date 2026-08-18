package main

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestResize2fsIsRequiredOnlyForAnExplicitFirecrackerRootCapacity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		tier config.Tier
		want bool
	}{
		{"firecracker capacity", config.Tier{Provider: config.ProviderFirecracker,
			Disk: 20 * config.GiB}, true},
		{"firecracker backend default", config.Tier{Provider: config.ProviderFirecracker}, false},
		{"docker capacity", config.Tier{Provider: config.ProviderDocker,
			Disk: 20 * config.GiB}, false},
		{"fallback includes firecracker", config.Tier{Providers: []config.ProviderKind{
			config.ProviderDocker, config.ProviderFirecracker,
		}, Disk: 20 * config.GiB}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{Tiers: []config.Tier{tc.tier}}
			if got := needsFirecrackerRootResize(cfg); got != tc.want {
				t.Errorf("needsFirecrackerRootResize = %v, want %v", got, tc.want)
			}
		})
	}
}
