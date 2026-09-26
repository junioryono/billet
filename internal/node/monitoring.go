package node

import (
	"context"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// JobMonitor samples running jobs. *usage.Monitor is the real one.
type JobMonitor interface {
	Start(key string, target usage.Target, vcpus int)
	Final(key string) (usage.Summary, bool)
	Forget(key string)
}

// UsageRecorder is where a job's usage is reported: the allocator in process,
// the node client over the wire. A ledger without it measures nothing.
type UsageRecorder interface {
	RecordLeaseUsage(ctx context.Context, leaseID string, epoch int64,
		usage alloc.JobUsage, series *alloc.UsageSeries) error
}

// usageReportLimit bounds one usage report, which sits on the destroy path: a
// slow control plane costs a measurement, never a teardown.
const usageReportLimit = 2 * time.Second

// WithMonitor measures every job this runner launches, on a backend that can
// say where its counters are (provider.UsageSource).
func WithMonitor(m JobMonitor) Option {
	return func(r *Runner) { r.monitor = m }
}

// startMonitoring begins measuring a job that has just launched. Everything
// here is off the job's path: a backend that cannot say where the counters are
// costs the measurement and nothing else.
func (r *Runner) startMonitoring(ctx context.Context, lease *alloc.Lease, inst *provider.Instance) {
	if r.monitor == nil {
		return
	}
	src, ok := r.provider.(provider.UsageSource)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageReportLimit)
	defer cancel()

	target, err := src.UsageTarget(ctx, inst.ID)
	if err != nil {
		r.log.Warn("could not find where a job's host counters are; it will not be measured",
			"runner", inst.Name, "error", err)
		return
	}
	r.monitor.Start(inst.Name, usage.Target{
		CgroupDir: target.CgroupDir, PID: target.PID, VCPUThreadPrefix: target.VCPUThreadPrefix,
		NetDevice: target.NetDevice, NetHostView: target.NetHostView,
	}, lease.VCPU)
}

// finalUsage takes a job's last sample. It is called before the compute is
// destroyed, because its cgroup and threads go with it.
func (r *Runner) finalUsage(name string) (usage.Summary, bool) {
	if r.monitor == nil {
		return usage.Summary{}, false
	}

	return r.monitor.Final(name)
}

// forgetMonitoring stops measuring a job whose compute is gone or no longer
// this process's.
func (r *Runner) forgetMonitoring(name string) {
	if r.monitor != nil {
		r.monitor.Forget(name)
	}
}

// reportUsage sends a job's usage to the ledger, fenced on the lease's epoch.
// The lease must still be live: the plane releases it only after the destroy
// this runs inside of returns.
func (r *Runner) reportUsage(ctx context.Context, lease *alloc.Lease, name string, sum usage.Summary) {
	rec, ok := r.alloc.(UsageRecorder)
	if !ok || lease == nil {
		return
	}
	report, series := jobUsageOf(sum)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageReportLimit)
	defer cancel()

	if err := rec.RecordLeaseUsage(ctx, lease.ID, lease.Epoch, report, series); err != nil {
		r.log.Warn("could not report what a job did to the host; the job is unaffected",
			"runner", name, "lease", lease.ID, "error", err)
	}
}

// jobUsageOf turns what the sampler kept into the report the ledger stores.
func jobUsageOf(sum usage.Summary) (alloc.JobUsage, *alloc.UsageSeries) {
	s := sum.Latest
	u := alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: max(sum.Samples, 1),
		IntervalMillis: max(sum.Interval.Milliseconds(), 1), WindowMillis: sum.Window.Milliseconds(),
		CPUUserMicros: s.CPUUser, CPUSystemMicros: s.CPUSys,
		GuestCPUMicros: s.GuestCPU, VMMCPUMicros: s.VMMCPU,
		MemoryPeakBytes: sum.MemoryPeak, OOMKills: s.OOMKills,
		DiskReadBytes: s.DiskRead, DiskWriteBytes: s.DiskWrite,
		NetRxBytes: s.NetRx, NetTxBytes: s.NetTx, NetRxPackets: s.NetRxPackets, NetTxPackets: s.NetTxPackets,
		CPUSomeMicros: s.CPUSome, CPUFullMicros: s.CPUFull,
		MemorySomeMicros: s.MemorySome, MemoryFullMicros: s.MemoryFull,
		IOSomeMicros: s.IOSome, IOFullMicros: s.IOFull,
	}
	for group, measured := range map[string]bool{
		alloc.UsageCPU: sum.Measured.CPU, alloc.UsageMemory: sum.Measured.Memory,
		alloc.UsageIO: sum.Measured.IO, alloc.UsageNet: sum.Measured.Net,
		alloc.UsageThreads: sum.Measured.Threads, alloc.UsagePressure: sum.Measured.Pressure,
		alloc.UsageEnergy: sum.Measured.Energy,
	} {
		if !measured {
			u.Unmeasured = append(u.Unmeasured, group)
		}
	}
	slices.Sort(u.Unmeasured)
	if sum.Measured.Energy {
		u.EnergyActiveMicrojoules, u.EnergyIdleMicrojoules = sum.EnergyActive, sum.EnergyIdle
		u.EnergySource = alloc.EnergyRAPLUnsplit
		if sum.EnergySplit {
			u.EnergySource = alloc.EnergyRAPL
		}
	}
	if len(sum.Points) == 0 {
		return u, nil
	}
	data, _, err := usage.EncodeSeries(sum.Points, alloc.MaxUsageSeriesBytes)
	if err != nil {
		return u, nil
	}

	return u, &alloc.UsageSeries{Codec: usage.SeriesCodec, Data: data}
}
