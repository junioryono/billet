package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
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
	// Flows totals each job's traffic by destination from the host's
	// connection tracker, which needs net.netfilter.nf_conntrack_acct and
	// nf_conntrack_timestamp set to 1; a node turns either on when it reads 0,
	// and refuses to start when the tracker is not loaded or a switch cannot be
	// read or set. Firecracker only: the guest's address is
	// learned from the DHCP lease its bridge's dnsmasq granted it.
	Flows bool `yaml:"flows,omitempty"`
	// Perf counts each microVM's vCPU threads with the CPU's hardware counters
	// (perf_event_open, guest mode included): cycles, instructions, cache
	// references and misses, branch misses and frontend stall cycles. Off
	// unless true; firecracker only, and Linux only, which a node checks at
	// startup since this file may be validated elsewhere.
	Perf bool `yaml:"perf,omitempty"`
	// Guest asks each firecracker guest to start billet's in-VM agent and
	// accepts what it reports on the guest-facing cache service, keeping it with
	// the job as the guest's own unverified view. Off unless true; it needs
	// node.cache, whose listener and per-guest bearer the agent reports through.
	Guest bool `yaml:"guest,omitempty"`
	// Meter reads the whole host's power at the wall from the smart plug it is
	// plugged into, and shares that energy among jobs in place of RAPL's: above
	// its idle draw by CPU time, the idle draw by reserved vCPUs. It is the one
	// energy source a Mac has. Exclusive with rapl.
	Meter *MeterConfig `yaml:"meter,omitempty"`
}

// MeterConfig names the power meter a host is plugged into.
type MeterConfig struct {
	// Shelly is the base address of a Shelly Gen2 or later plug (Plus, Pro,
	// Gen3, Gen4), such as http://192.168.1.246, read through its local RPC API.
	// It has no authentication; a plug with authentication enabled is refused
	// when read.
	Shelly string `yaml:"shelly"`
	// Switch is the plug's switch (output) id; 0, the default, on a
	// single-outlet plug.
	Switch int `yaml:"switch,omitempty"`
	// IdleWatts is the host's power at the wall with no jobs running, measured
	// by the operator from the same plug. Required: energy at the wall is mostly
	// the machine idling, and with no baseline every joule of it would be
	// charged to whatever jobs ran.
	IdleWatts float64 `yaml:"idle_watts"`
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
	// DHCPLeaseDir is where the host role's dnsmasq keeps each guest bridge's
	// leases, one directory per bridge (billet-dnsmasq@<bridge>), and
	// DHCPLeaseTime the lease length its dhcp-range grants.
	DHCPLeaseDir  = "/var/lib/billet-dnsmasq"
	DHCPLeaseTime = time.Hour
	// maxIdlePackageWatts is far above any socket's idle draw; it exists to
	// refuse a unit mistake (milliwatts) rather than to model hardware.
	maxIdlePackageWatts = 2000
	// maxMeterSwitch bounds a plug's switch id; a Shelly Pro has at most four.
	maxMeterSwitch = 15
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
// ec2 and codebuild run it on someone else's machine, so the block would
// promise numbers that never arrive. A tart VM is measured by its process's own
// accounting, which carries its own energy estimate, so RAPL (a Linux counter)
// and its idle baseline are refused there too.
func (c *Config) validateMonitoringNode() []error {
	m := c.Node.Monitoring
	if m == nil {
		return nil
	}

	var errs []error
	// ONLY A VMM HAS vCPU THREADS TO COUNT. A docker job's threads are the
	// workload's own, and a tart VM runs on a host without perf_event_open.
	if m.Perf && c.Node.Provider != ProviderFirecracker {
		errs = append(errs, fmt.Errorf("node.monitoring.perf counts a microVM's vCPU threads, and "+
			"this node's provider is %s; only firecracker can be counted", c.Node.Provider))
	}
	switch c.Node.Provider {
	case ProviderFirecracker, ProviderDocker:
	case ProviderTart:
		if m.RAPL || m.IdlePackageWatts != 0 {
			errs = append(errs, errors.New("node.monitoring.rapl and idle_package_watts are Linux "+
				"package counters, and a tart VM's energy is macOS's own estimate for its process, or "+
				"the wall's with node.monitoring.meter"))
		}
	default:
		errs = append(errs, fmt.Errorf("node.monitoring is set but this node's provider is %s, "+
			"and only firecracker, docker and tart jobs can be measured from the host", c.Node.Provider))
	}
	if m.Flows && c.Node.Provider != ProviderFirecracker {
		errs = append(errs, fmt.Errorf("node.monitoring.flows is set but this node's provider is %s, "+
			"and only a firecracker guest's address is known to the host (from its bridge's DHCP lease)",
			c.Node.Provider))
	}
	// THE AGENT REPORTS THROUGH THE GUEST CACHE SERVICE, with the bearer it mints
	// per guest, and only a firecracker guest is told to start it; anywhere else
	// the switch would promise reports that never arrive.
	if m.Guest && c.Node.Provider != ProviderFirecracker {
		errs = append(errs, fmt.Errorf("node.monitoring.guest is set but this node's provider is %s, "+
			"and only a firecracker guest is told to start billet's in-VM agent", c.Node.Provider))
	}
	if m.Guest && c.Node.Cache == nil {
		errs = append(errs, errors.New("node.monitoring.guest is set but node.cache is not, and the "+
			"in-VM agent reports through the guest cache service node.cache starts"))
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
	if m.Meter != nil {
		errs = append(errs, m.Meter.validate(m.RAPL)...)
	}

	return errs
}

// validate refuses a meter this node could not read, or one beside the package
// counter.
//
// ONE ENERGY SOURCE A HOST: the wall includes the package, so sharing both
// would charge every job twice.
func (m *MeterConfig) validate(rapl bool) []error {
	var errs []error
	if rapl {
		errs = append(errs, errors.New("node.monitoring.meter and rapl are both set, and a host's "+
			"energy has one source: the wall includes the package, so keep one"))
	}

	// THE ADDRESS IS NEVER REPEATED IN A REFUSAL: one refused for carrying
	// credentials would otherwise put them in the node's log.
	u, err := url.Parse(m.Shelly)
	switch {
	case strings.TrimSpace(m.Shelly) == "":
		errs = append(errs, errors.New("node.monitoring.meter.shelly is empty; name the plug's base "+
			"address, such as http://192.168.1.246"))
	case m.Shelly != strings.TrimSpace(m.Shelly):
		errs = append(errs, errors.New("node.monitoring.meter.shelly has spaces around it, which the "+
			"plug's address cannot have"))
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		errs = append(errs, errors.New("node.monitoring.meter.shelly is not an http or https "+
			"address with a host"))
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
		errs = append(errs, errors.New("node.monitoring.meter.shelly names more than the plug: "+
			"give its base address alone, with no credentials, path or query"))
	}
	if m.Switch < 0 || m.Switch > maxMeterSwitch {
		errs = append(errs, fmt.Errorf("node.monitoring.meter.switch %d is outside 0 to %d",
			m.Switch, maxMeterSwitch))
	}
	if w := m.IdleWatts; math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 || w > maxIdlePackageWatts {
		errs = append(errs, fmt.Errorf("node.monitoring.meter.idle_watts %v is not a host's idle "+
			"power above 0 and up to %d watts; measure it from the plug with no jobs running", w,
			maxIdlePackageWatts))
	}

	return errs
}
