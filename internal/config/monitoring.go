package config

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// NodeMonitoringConfig turns on per-job measurement on a node: each lease's
// cgroup, its network device, the VMM's threads and, optionally, the package
// energy counter, sampled for the whole life of the lease and reported to the
// control plane before the compute is destroyed.
//
// ABSENT MEANS OFF, and off is exactly the node it always was: no sampler, and
// a Firecracker launch with the same jailer arguments as before.
type NodeMonitoringConfig struct {
	// Interval is how often each lease is sampled, a Go duration string.
	// Empty means DefaultMonitoringInterval.
	Interval string `yaml:"interval,omitempty"`
	// RAPL reads the package energy counter
	// (/sys/class/powercap/intel-rapl:0/energy_uj) and attributes it to jobs by
	// CPU time. It needs root, which a node is.
	RAPL bool `yaml:"rapl,omitempty"`
	// IdlePackageWatts is the host's package power with no jobs running,
	// measured by the operator. With it, energy above the baseline is shared by
	// CPU time and the baseline by reserved vCPUs; without it the whole package
	// is shared by CPU time and reported as unsplit. Requires rapl.
	IdlePackageWatts float64 `yaml:"idle_package_watts,omitempty"`
}

const (
	// DefaultMonitoringInterval samples once a second.
	DefaultMonitoringInterval = time.Second
	// MinMonitoringInterval and MaxMonitoringInterval bound the interval. The
	// maximum is set by RAPL: the reference host's counter wraps after about 65
	// kJ, four minutes at 280 W, and a sampler must read it at least once per
	// wrap to know how many times it wrapped.
	MinMonitoringInterval = 250 * time.Millisecond
	MaxMonitoringInterval = 30 * time.Second
	// maxIdlePackageWatts is far above any socket's idle draw; it exists to
	// refuse a unit mistake (milliwatts) rather than to model hardware.
	maxIdlePackageWatts = 2000
)

// IntervalDuration is the sampling interval, with the default applied.
func (m *NodeMonitoringConfig) IntervalDuration() (time.Duration, error) {
	if m == nil || strings.TrimSpace(m.Interval) == "" {
		return DefaultMonitoringInterval, nil
	}

	d, err := time.ParseDuration(strings.TrimSpace(m.Interval))
	if err != nil {
		return 0, fmt.Errorf("node.monitoring.interval %q is not a duration (write e.g. 1s or 500ms)",
			m.Interval)
	}
	if d < MinMonitoringInterval || d > MaxMonitoringInterval {
		return 0, fmt.Errorf("node.monitoring.interval %s is outside %s to %s", d,
			MinMonitoringInterval, MaxMonitoringInterval)
	}

	return d, nil
}

// validateMonitoringNode refuses a monitoring block this node could not honour.
//
// REFUSED RATHER THAN IGNORED on a backend with no host-side view of the job:
// ec2 and codebuild run it on someone else's machine and tart's guest lives in
// a process billet does not yet read, so the block would promise numbers that
// never arrive.
func (c *Config) validateMonitoringNode() []error {
	m := c.Node.Monitoring
	if m == nil {
		return nil
	}

	var errs []error
	if c.Node.Provider != ProviderFirecracker && c.Node.Provider != ProviderDocker {
		errs = append(errs, fmt.Errorf("node.monitoring is set but this node's provider is %s, "+
			"and only firecracker and docker jobs can be measured from the host", c.Node.Provider))
	}
	if _, err := m.IntervalDuration(); err != nil {
		errs = append(errs, err)
	}
	switch w := m.IdlePackageWatts; {
	case math.IsNaN(w) || math.IsInf(w, 0) || w < 0 || w > maxIdlePackageWatts:
		errs = append(errs, fmt.Errorf("node.monitoring.idle_package_watts %v is not a package "+
			"power between 0 and %d watts", w, maxIdlePackageWatts))
	case w > 0 && !m.RAPL:
		errs = append(errs, errors.New("node.monitoring.idle_package_watts is set but rapl is not, "+
			"and the baseline only splits the energy RAPL measures"))
	}

	return errs
}
