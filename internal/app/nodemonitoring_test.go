package app

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/simulated"
	jobusage "github.com/junioryono/billet/internal/usage"
)

// node.monitoring.perf ON A PLATFORM WITHOUT perf_event_open STOPS THE NODE AT
// STARTUP, naming why, rather than running a sampler that counts nothing; where
// the platform can count, the same block starts one. Config cannot refuse it,
// because a config is also validated on a machine that will not run it.
func TestPerfStartsOnlyWhereThePlatformCanCount(t *testing.T) {
	t.Parallel()

	p, err := simulated.New("perf-test")
	if err != nil {
		t.Fatalf("simulated.New: %v", err)
	}
	cfg := &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderFirecracker, Monitoring: &config.NodeMonitoringConfig{Perf: true},
	}}

	opts, err := nodeMonitorOptions(t.Context(), cfg, p)
	if jobusage.CountersSupported {
		if err != nil || len(opts) != 1 {
			t.Fatalf("perf on a platform that can count = %d options, %v; want a sampler", len(opts), err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "node.monitoring.perf is set, but this node cannot count") {
		t.Fatalf("perf on a platform that cannot count = %d options, %v; want a refusal naming perf",
			len(opts), err)
	}

	// AND WITHOUT perf THE SAME NODE STARTS, so the refusal is perf's alone.
	cfg.Node.Monitoring.Perf = false
	if opts, err := nodeMonitorOptions(t.Context(), cfg, p); err != nil || len(opts) != 1 {
		t.Fatalf("monitoring without perf = %d options, %v; want a sampler", len(opts), err)
	}
}
