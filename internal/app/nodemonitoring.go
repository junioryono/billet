package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/firecracker"
	jobusage "github.com/junioryono/billet/internal/usage"
	"github.com/junioryono/billet/internal/usage/flows"
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

	sampling := jobusage.Options{Interval: interval, RAPL: m.RAPL, IdleWatts: m.IdlePackageWatts}
	if m.Perf {
		if sampling.Counters, err = counterSource(); err != nil {
			return nil, err
		}
	}
	monitor := jobusage.NewMonitor("/", sampling)
	go monitor.Run(ctx)

	opts := []node.Option{node.WithMonitor(monitor)}

	if m.Flows {
		watcher, err := startFlows(ctx, "/", interval)
		if err != nil {
			return nil, err
		}

		opts = append(opts, node.WithFlows(watcher, config.DHCPLeaseDir))
	}

	return opts, nil
}

// startFlows starts following guests' connections from the host's connection
// tracker, refusing a host (its /proc under root) that does not count and stamp
// them: asked for flows
// and unable to measure them, a node that started anyway would report every
// job's destinations as unmeasured with nobody told why.
func startFlows(ctx context.Context, root string, interval time.Duration) (*flows.Watcher, error) {
	if err := flows.Ready(root); err != nil {
		return nil, fmt.Errorf("node.monitoring.flows is set, but %w; set both with "+
			"`sysctl -w net.netfilter.nf_conntrack_acct=1 net.netfilter.nf_conntrack_timestamp=1` "+
			"(and persist them under /etc/sysctl.d), or remove flows", err)
	}

	acct := flows.NewAccountant()
	tracker := flows.NewTracker(slog.Default())
	watcher := flows.NewWatcher(acct, tracker, config.DHCPLeaseTime, slog.Default())

	go tracker.Run(ctx, acct)
	go watcher.Run(ctx, interval)

	slog.Info("totalling each job's traffic by destination from the connection tracker",
		"leases", config.DHCPLeaseDir)

	return watcher, nil
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
