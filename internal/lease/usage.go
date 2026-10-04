package lease

import (
	"errors"
	"fmt"
	"slices"
)

// UsageSourceHost is usage the node measured from outside the guest: the
// cgroup, the tap, the VMM's threads and the package energy counter, or for a
// VM that is one process with no cgroup, that process's own accounting. It is
// the only source today; a guest's own report would be another, and never a
// replacement for this one.
const UsageSourceHost = "host"

// The measurement groups a usage report may name as unmeasured. A group the
// node could not read reports zero AND is named, because zero is also a real
// measurement.
const (
	UsageCPU    = "cpu"
	UsageMemory = "memory"
	// UsageOOM is the OOM count alone, unmeasured where the host keeps no such
	// count for the job (a process's own accounting on macOS). An unmeasured
	// memory group takes the count with it.
	UsageOOM      = "oom"
	UsageIO       = "io"
	UsageNet      = "net"
	UsageThreads  = "threads"
	UsagePressure = "pressure"
	UsageEnergy   = "energy"
)

var usageGroups = []string{UsageCPU, UsageMemory, UsageOOM, UsageIO, UsageNet, UsageThreads,
	UsagePressure, UsageEnergy}

// How a job's energy was attributed.
const (
	// EnergyRAPL is the package energy counter, split into the active energy
	// above the host's configured idle baseline (shared by CPU time) and the
	// idle baseline itself (shared by reserved vCPUs).
	EnergyRAPL = "rapl"
	// EnergyRAPLUnsplit is the package energy counter with no idle baseline
	// configured: the whole package is shared by CPU time and nothing is
	// reported as idle, which overstates what a job's work cost.
	EnergyRAPLUnsplit = "rapl-unsplit"
	// EnergyProcess is the kernel's own estimate of the energy the job's process
	// used over its life (macOS's per-process energy, for a tart VM): already
	// attributed, with no idle share to report.
	EnergyProcess = "process"
)

// UsageSeriesCodec is the one series encoding this build writes and reads.
const UsageSeriesCodec = 1

// MaxUsageSeriesBytes bounds one lease's encoded series. It is below the node
// wire's 1 MiB body limit once base64 is applied, so a report is always one
// request; a node downsamples a longer series until it fits.
const MaxUsageSeriesBytes = 512 << 10

// JobUsage is what the host measured a lease's job do, over its whole life.
// Every quantity is in the unit its name says.
type JobUsage struct {
	Source string `json:"source"`
	// Unmeasured names the groups the host could not read, whose fields are zero
	// for that reason rather than measured as zero.
	Unmeasured     []string `json:"unmeasured,omitempty"`
	Samples        int64    `json:"samples"`
	IntervalMillis int64    `json:"interval_ms"`
	WindowMillis   int64    `json:"window_ms"`

	CPUUserMicros   int64 `json:"cpu_user_us"`
	CPUSystemMicros int64 `json:"cpu_system_us"`
	// GuestCPUMicros is time the VMM's vCPU threads ran, and VMMCPUMicros the
	// time its other threads did: emulation and IO, the virtualization's cost.
	GuestCPUMicros int64 `json:"guest_cpu_us"`
	VMMCPUMicros   int64 `json:"vmm_cpu_us"`

	// MemoryPeakBytes is the cgroup's memory.peak: for a microVM, the guest
	// memory it touched plus the VMM and page cache, which does not fall when
	// the guest frees memory.
	MemoryPeakBytes int64 `json:"memory_peak_bytes"`
	OOMKills        int64 `json:"oom_kills"`

	// Disk bytes are the cgroup's io.stat: IO that missed the host's page cache,
	// plus writeback, not what the guest asked for.
	DiskReadBytes  int64 `json:"disk_read_bytes"`
	DiskWriteBytes int64 `json:"disk_write_bytes"`

	// Net fields are the guest's view: received is what reached the guest.
	NetRxBytes   int64 `json:"net_rx_bytes"`
	NetTxBytes   int64 `json:"net_tx_bytes"`
	NetRxPackets int64 `json:"net_rx_packets"`
	NetTxPackets int64 `json:"net_tx_packets"`

	CPUSomeMicros    int64 `json:"cpu_some_us"`
	CPUFullMicros    int64 `json:"cpu_full_us"`
	MemorySomeMicros int64 `json:"memory_some_us"`
	MemoryFullMicros int64 `json:"memory_full_us"`
	IOSomeMicros     int64 `json:"io_some_us"`
	IOFullMicros     int64 `json:"io_full_us"`

	EnergyActiveMicrojoules int64  `json:"energy_active_uj"`
	EnergyIdleMicrojoules   int64  `json:"energy_idle_uj"`
	EnergySource            string `json:"energy_source,omitempty"`
}

// UsageSeries is one lease's per-sample series, opaque to the ledger.
type UsageSeries struct {
	Codec int    `json:"codec"`
	Data  []byte `json:"data"`
}

// Measured reports whether a group was read. The OOM count is read only with
// the memory it belongs to.
func (u JobUsage) Measured(group string) bool {
	if group == UsageOOM && !u.Measured(UsageMemory) {
		return false
	}

	return !slices.Contains(u.Unmeasured, group)
}

// Validate refuses a report the ledger could not keep faithfully.
func (u JobUsage) Validate() error {
	if u.Source != UsageSourceHost {
		return fmt.Errorf("alloc: usage source %q is not one this control plane records", u.Source)
	}
	if u.Samples < 1 || u.IntervalMillis < 1 || u.WindowMillis < 0 {
		return fmt.Errorf("alloc: a usage report needs at least one sample and an interval "+
			"(samples %d, interval %dms, window %dms)", u.Samples, u.IntervalMillis, u.WindowMillis)
	}
	seen := map[string]bool{}
	for _, group := range u.Unmeasured {
		if !slices.Contains(usageGroups, group) {
			return fmt.Errorf("alloc: %q is not a usage group this control plane records", group)
		}
		if seen[group] {
			return fmt.Errorf("alloc: usage group %q is named unmeasured twice", group)
		}
		seen[group] = true
	}
	for name, v := range map[string]int64{
		"cpu_user_us": u.CPUUserMicros, "cpu_system_us": u.CPUSystemMicros,
		"guest_cpu_us": u.GuestCPUMicros, "vmm_cpu_us": u.VMMCPUMicros,
		"memory_peak_bytes": u.MemoryPeakBytes, "oom_kills": u.OOMKills,
		"disk_read_bytes": u.DiskReadBytes, "disk_write_bytes": u.DiskWriteBytes,
		"net_rx_bytes": u.NetRxBytes, "net_tx_bytes": u.NetTxBytes,
		"net_rx_packets": u.NetRxPackets, "net_tx_packets": u.NetTxPackets,
		"cpu_some_us": u.CPUSomeMicros, "cpu_full_us": u.CPUFullMicros,
		"memory_some_us": u.MemorySomeMicros, "memory_full_us": u.MemoryFullMicros,
		"io_some_us": u.IOSomeMicros, "io_full_us": u.IOFullMicros,
		"energy_active_uj": u.EnergyActiveMicrojoules, "energy_idle_uj": u.EnergyIdleMicrojoules,
	} {
		if v < 0 {
			return fmt.Errorf("alloc: usage %s is negative (%d)", name, v)
		}
	}
	for group, fields := range u.groupFields() {
		if u.Measured(group) {
			continue
		}
		for _, v := range fields {
			if v != 0 {
				return fmt.Errorf("alloc: a usage report names %s unmeasured and reports some", group)
			}
		}
	}
	switch {
	case !u.Measured(UsageEnergy):
		if u.EnergySource != "" {
			return errors.New("alloc: a usage report names energy unmeasured and gives it a source")
		}
	case u.EnergySource == EnergyRAPLUnsplit || u.EnergySource == EnergyProcess:
		if u.EnergyIdleMicrojoules != 0 {
			return fmt.Errorf("alloc: %s energy has no idle share, so it cannot report one", u.EnergySource)
		}
	case u.EnergySource != EnergyRAPL:
		return fmt.Errorf("alloc: energy source %q is not one this control plane records", u.EnergySource)
	}

	return nil
}

// groupFields maps each measurement group to the quantities it owns, which are
// zero whenever the group is unmeasured.
func (u JobUsage) groupFields() map[string][]int64 {
	return map[string][]int64{
		UsageCPU:     {u.CPUUserMicros, u.CPUSystemMicros},
		UsageThreads: {u.GuestCPUMicros, u.VMMCPUMicros},
		UsageMemory:  {u.MemoryPeakBytes, u.OOMKills},
		UsageOOM:     {u.OOMKills},
		UsageIO:      {u.DiskReadBytes, u.DiskWriteBytes},
		UsageNet:     {u.NetRxBytes, u.NetTxBytes, u.NetRxPackets, u.NetTxPackets},
		UsagePressure: {u.CPUSomeMicros, u.CPUFullMicros, u.MemorySomeMicros, u.MemoryFullMicros,
			u.IOSomeMicros, u.IOFullMicros},
		UsageEnergy: {u.EnergyActiveMicrojoules, u.EnergyIdleMicrojoules},
	}
}

// Validate refuses a series this build cannot read back or the wire could not
// carry.
func (s UsageSeries) Validate() error {
	if s.Codec != UsageSeriesCodec {
		return fmt.Errorf("alloc: usage series codec %d is not one this control plane records", s.Codec)
	}
	if len(s.Data) == 0 || len(s.Data) > MaxUsageSeriesBytes {
		return fmt.Errorf("alloc: a usage series is %d bytes, and must be 1 to %d",
			len(s.Data), MaxUsageSeriesBytes)
	}

	return nil
}
