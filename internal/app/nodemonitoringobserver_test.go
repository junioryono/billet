package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/simulated"
)

// tickCounter is an observer that counts the sampler's ticks.
type tickCounter struct{ ticks atomic.Int32 }

func (c *tickCounter) TickTook(time.Duration) { c.ticks.Add(1) }
func (c *tickCounter) Gap(string)             {}
func (c *tickCounter) IOBounded()             {}

// THE SAMPLER node.monitoring STARTS REPORTS TO THE OBSERVER IT IS GIVEN, the
// one Node.Run hands it from the metrics endpoint: its first tick, which it
// takes as it starts, reaches it.
func TestTheSamplerReportsToTheObserverItIsGiven(t *testing.T) {
	t.Parallel()

	p, err := simulated.New("observer-test")
	if err != nil {
		t.Fatalf("simulated.New: %v", err)
	}
	cfg := &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderFirecracker, Monitoring: &config.NodeMonitoringConfig{Interval: "250ms"},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	observer := &tickCounter{}
	if _, err := nodeMonitorOptions(ctx, cfg, p, observer); err != nil {
		t.Fatalf("nodeMonitorOptions: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for observer.ticks.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the sampler ticked and its observer never heard")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
