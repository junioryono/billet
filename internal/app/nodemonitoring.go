package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/firecracker"
	jobusage "github.com/junioryono/billet/internal/usage"
)

// firecrackerOptions are the provider options node configuration implies
// beyond the logger: with node.monitoring, the jailer is asked to account for
// memory and io where the host proves it can.
func firecrackerOptions(cfg *config.Config) []firecracker.Option {
	opts := []firecracker.Option{firecracker.WithLogger(slog.Default())}
	if cfg.Node.Monitoring != nil {
		opts = append(opts, firecracker.WithJobAccounting())
	}

	return opts
}

// nodeMonitorOptions starts the sampler node.monitoring asks for and returns
// the runner option that feeds it jobs, or none when monitoring is off. The
// sampler runs until ctx ends.
func nodeMonitorOptions(ctx context.Context, cfg *config.Config, p provider.Provider) ([]node.Option, error) {
	m := cfg.Node.Monitoring
	if m == nil {
		return nil, nil
	}
	interval, err := m.IntervalDuration()
	if err != nil {
		return nil, err
	}

	// SAID AT STARTUP, because a host that cannot account for memory or io
	// reports those groups unmeasured on every job, and the reason is here.
	switch fc, ok := p.(*firecracker.Provider); {
	case ok:
		acct := fc.Accounting()
		slog.Info("measuring each job from the host", "interval", interval, "rapl", m.RAPL,
			"idle_package_watts", m.IdlePackageWatts, "memory", acct.Memory.String(),
			"io", acct.IO.String(), "reason", acct.Reason)
	case p.Kind() == config.ProviderTart:
		slog.Info("measuring each job from the host", "interval", interval,
			"source", "each VM's own process accounting", "net", "not measured")
	default:
		slog.Info("measuring each job from the host", "interval", interval, "rapl", m.RAPL,
			"idle_package_watts", m.IdlePackageWatts)
	}

	opts := jobusage.Options{Interval: interval, RAPL: m.RAPL, IdleWatts: m.IdlePackageWatts}
	if m.Perf {
		if opts.Counters, err = counterSource(); err != nil {
			return nil, err
		}
	}
	monitor := jobusage.NewMonitor("/", opts)
	go monitor.Run(ctx)

	return []node.Option{node.WithMonitor(monitor)}, nil
}

// counterSource is the hardware counter reader node.monitoring.perf asks for.
//
// REFUSED HERE RATHER THAN IN CONFIG, on a platform that has no
// perf_event_open or a host whose group of events never counts, because a
// config is also validated on a machine that will not run it.
func counterSource() (jobusage.CounterSource, error) {
	counters, err := jobusage.HardwareCounters()
	if err != nil {
		return nil, fmt.Errorf("node.monitoring.perf is set, but this node cannot count: %w", err)
	}

	return provenCounters(counters)
}

// provenCounters is counters once they have proved they count on this host.
func provenCounters(counters jobusage.CounterSource) (jobusage.CounterSource, error) {
	if err := jobusage.ProveCounting(counters); err != nil {
		return nil, fmt.Errorf("node.monitoring.perf is set, but this node cannot count: %w", err)
	}
	slog.Info("counting each microVM's vCPU threads with the CPU's hardware counters",
		"events", "cycles, instructions, cache-references, cache-misses, branch-misses, "+
			"stalled-cycles-frontend")

	return counters, nil
}
