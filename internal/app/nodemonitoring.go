package app

import (
	"context"
	"log/slog"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/firecracker"
	jobusage "github.com/junioryono/billet/internal/usage"
)

// FirecrackerOptions are the provider options node configuration implies
// beyond the logger: with node.monitoring, the jailer is asked to account for
// memory and io where the host proves it can. billet check builds its provider
// with them too, so it refuses what the node would.
func FirecrackerOptions(cfg *config.Config) []firecracker.Option {
	opts := []firecracker.Option{firecracker.WithLogger(slog.Default())}
	if cfg.Node.Monitoring != nil {
		opts = append(opts, firecracker.WithJobAccounting())
	}

	return opts
}

// requireJobAccounting refuses a Firecracker node whose node.monitoring the
// host cannot honour: a controller the host did not prove is never asked of
// the jailer, so its group would be recorded as unmeasured on every job. A
// provider built without node.monitoring, and every other backend, refuses
// nothing.
func requireJobAccounting(p provider.Provider) error {
	fc, ok := p.(*firecracker.Provider)
	if !ok {
		return nil
	}

	return fc.RequireJobAccounting()
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

	// SAID AT STARTUP, so the log names what each job's cgroup accounts for;
	// requireJobAccounting has already refused a host that proved less.
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

	monitor := jobusage.NewMonitor("/", jobusage.Options{Interval: interval, RAPL: m.RAPL,
		IdleWatts: m.IdlePackageWatts})
	go monitor.Run(ctx)

	return []node.Option{node.WithMonitor(monitor)}, nil
}
