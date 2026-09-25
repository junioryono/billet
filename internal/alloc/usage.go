package alloc

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// UsageSourceHost is usage the node measured from outside the guest: the
// cgroup, the tap, the VMM's threads and the package energy counter. It is the
// only source today; a guest's own report would be another, and never a
// replacement for this one.
const UsageSourceHost = "host"

// The measurement groups a usage report may name as unmeasured. A group the
// node could not read reports zero AND is named, because zero is also a real
// measurement.
const (
	UsageCPU      = "cpu"
	UsageMemory   = "memory"
	UsageIO       = "io"
	UsageNet      = "net"
	UsageThreads  = "threads"
	UsagePressure = "pressure"
	UsageEnergy   = "energy"
)

var usageGroups = []string{UsageCPU, UsageMemory, UsageIO, UsageNet, UsageThreads,
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

// Measured reports whether a group was read.
func (u JobUsage) Measured(group string) bool { return !slices.Contains(u.Unmeasured, group) }

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
	switch {
	case !u.Measured(UsageEnergy):
		if u.EnergySource != "" || u.EnergyActiveMicrojoules != 0 || u.EnergyIdleMicrojoules != 0 {
			return errors.New("alloc: a usage report names energy unmeasured and reports some")
		}
	case u.EnergySource != EnergyRAPL && u.EnergySource != EnergyRAPLUnsplit:
		return fmt.Errorf("alloc: energy source %q is not one this control plane records", u.EnergySource)
	}

	return nil
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

// RecordLeaseUsage keeps what the host measured a lease's job do.
//
// FENCED ON THE EPOCH, in the same transaction, so a superseded holder cannot
// report, and a lease that has ended is refused (ErrLeaseNotFound): the node
// reports before it destroys the compute, and anything arriving after release
// is not about a lease this ledger is holding. The first report is kept.
func (a *Allocator) RecordLeaseUsage(
	ctx context.Context, leaseID string, epoch int64, usage JobUsage, series *UsageSeries,
) error {
	if err := usage.Validate(); err != nil {
		return err
	}
	if series != nil {
		if err := series.Validate(); err != nil {
			return err
		}
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		q := state.WriteQueries(tx)
		if err := q.RecordJobUsage(ctx, ledgerdb.RecordJobUsageParams{
			LeaseID: lease.ID, Node: lease.Node, RecordedAt: nowStamp(),
			Source: usage.Source, Unmeasured: strings.Join(usage.Unmeasured, ","),
			Samples: usage.Samples, IntervalMs: usage.IntervalMillis, WindowMs: usage.WindowMillis,
			CpuUserUs: usage.CPUUserMicros, CpuSystemUs: usage.CPUSystemMicros,
			GuestCpuUs: usage.GuestCPUMicros, VmmCpuUs: usage.VMMCPUMicros,
			MemoryPeakBytes: usage.MemoryPeakBytes, OomKills: usage.OOMKills,
			DiskReadBytes: usage.DiskReadBytes, DiskWriteBytes: usage.DiskWriteBytes,
			NetRxBytes: usage.NetRxBytes, NetTxBytes: usage.NetTxBytes,
			NetRxPackets: usage.NetRxPackets, NetTxPackets: usage.NetTxPackets,
			CpuSomeUs: usage.CPUSomeMicros, CpuFullUs: usage.CPUFullMicros,
			MemorySomeUs: usage.MemorySomeMicros, MemoryFullUs: usage.MemoryFullMicros,
			IoSomeUs: usage.IOSomeMicros, IoFullUs: usage.IOFullMicros,
			EnergyActiveUj: usage.EnergyActiveMicrojoules, EnergyIdleUj: usage.EnergyIdleMicrojoules,
			EnergySource: usage.EnergySource,
		}); err != nil {
			return fmt.Errorf("alloc: record the usage of lease %s: %w", leaseID, err)
		}

		if series == nil {
			return nil
		}
		if err := q.RecordJobSeries(ctx, ledgerdb.RecordJobSeriesParams{
			LeaseID: lease.ID, Codec: int64(series.Codec),
			Series: base64.StdEncoding.EncodeToString(series.Data),
		}); err != nil {
			return fmt.Errorf("alloc: record the usage series of lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// RecordedUsage is a lease's usage as the ledger kept it.
type RecordedUsage struct {
	JobUsage
	Node       string
	RecordedAt string
}

// LeaseUsage reads what the host measured a lease's job do. A lease with no
// report is ErrLeaseNotFound: nothing was measured, which is not zero.
func (a *Allocator) LeaseUsage(ctx context.Context, leaseID string) (RecordedUsage, error) {
	var out RecordedUsage
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJobUsage(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no usage was recorded for lease %s", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the usage of lease %s: %w", leaseID, err)
		}
		var unmeasured []string
		if row.Unmeasured != "" {
			unmeasured = strings.Split(row.Unmeasured, ",")
		}
		out = RecordedUsage{Node: row.Node, RecordedAt: row.RecordedAt, JobUsage: JobUsage{
			Source: row.Source, Unmeasured: unmeasured, Samples: row.Samples,
			IntervalMillis: row.IntervalMs, WindowMillis: row.WindowMs,
			CPUUserMicros: row.CpuUserUs, CPUSystemMicros: row.CpuSystemUs,
			GuestCPUMicros: row.GuestCpuUs, VMMCPUMicros: row.VmmCpuUs,
			MemoryPeakBytes: row.MemoryPeakBytes, OOMKills: row.OomKills,
			DiskReadBytes: row.DiskReadBytes, DiskWriteBytes: row.DiskWriteBytes,
			NetRxBytes: row.NetRxBytes, NetTxBytes: row.NetTxBytes,
			NetRxPackets: row.NetRxPackets, NetTxPackets: row.NetTxPackets,
			CPUSomeMicros: row.CpuSomeUs, CPUFullMicros: row.CpuFullUs,
			MemorySomeMicros: row.MemorySomeUs, MemoryFullMicros: row.MemoryFullUs,
			IOSomeMicros: row.IoSomeUs, IOFullMicros: row.IoFullUs,
			EnergyActiveMicrojoules: row.EnergyActiveUj, EnergyIdleMicrojoules: row.EnergyIdleUj,
			EnergySource: row.EnergySource,
		}}
		return nil
	})
	return out, err
}

// LeaseUsageSeries reads a lease's encoded series. A lease with none is
// ErrLeaseNotFound.
func (a *Allocator) LeaseUsageSeries(ctx context.Context, leaseID string) (UsageSeries, error) {
	var out UsageSeries
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJobSeries(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no usage series was recorded for lease %s",
				ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the usage series of lease %s: %w", leaseID, err)
		}
		data, err := base64.StdEncoding.DecodeString(row.Series)
		if err != nil {
			return fmt.Errorf("alloc: the usage series of lease %s is not base64: %w", leaseID, err)
		}
		out = UsageSeries{Codec: int(row.Codec), Data: data}
		return nil
	})
	return out, err
}
