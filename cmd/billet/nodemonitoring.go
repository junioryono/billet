package main

import (
	"context"
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
	if fc, ok := p.(*firecracker.Provider); ok {
		acct := fc.Accounting()
		slog.Info("measuring each job from the host", "interval", interval, "rapl", m.RAPL,
			"idle_package_watts", m.IdlePackageWatts, "memory", acct.Memory.String(),
			"io", acct.IO.String(), "reason", acct.Reason)
	} else {
		slog.Info("measuring each job from the host", "interval", interval, "rapl", m.RAPL,
			"idle_package_watts", m.IdlePackageWatts)
	}

	monitor := jobusage.NewMonitor("/", jobusage.Options{Interval: interval, RAPL: m.RAPL,
		IdleWatts: m.IdlePackageWatts})
	go monitor.Run(ctx)

	return []node.Option{node.WithMonitor(monitor)}, nil
}
