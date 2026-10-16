package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/usage"
	"github.com/junioryono/billet/internal/usage/flows"
)

// JobMonitor samples running jobs. *usage.Monitor is the real one.
type JobMonitor interface {
	Start(key string, target usage.Target, vcpus int)
	// Resume measures a job this process adopted, continuing its series from
	// the checkpoint the process that handed it over wrote, when there is one
	// that continues it; the error says why one was not used.
	Resume(key string, target usage.Target, vcpus int, from *usage.Checkpoint) error
	// Checkpoint is what is known of a job, for the next process.
	Checkpoint(key string) (usage.Checkpoint, bool)
	Final(key string) (usage.Summary, bool)
	Forget(key string)
}

// WithUsageCheckpoints keeps, under dir, what the sampler knew of each job a
// handover leaves running, for the next node process to continue measuring
// it from (#384). Without it a handed-over job is measured from its adoption.
func WithUsageCheckpoints(dir string) Option {
	return func(r *Runner) { r.checkpoints = dir }
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

// GuestRecorder is where what a job's guest told the node is reported: the
// allocator in process, the node client over the wire. It is the guest's own,
// unverified view, kept beside the host's measurement and never in it.
type GuestRecorder interface {
	RecordGuestReport(ctx context.Context, leaseID string, epoch int64, report alloc.GuestReport) error
}

// The runner finds its recorder by asserting its lease store, so this fails the
// build if the in-process one stops being one.
var _ GuestRecorder = (*alloc.Allocator)(nil)

// WithMonitor measures every job this runner launches, on a backend that can
// say where its counters are (provider.UsageSource).
func WithMonitor(m JobMonitor) Option {
	return func(r *Runner) { r.monitor = m }
}

// FlowWatcher follows a guest's connections by destination. *flows.Watcher is
// the real one.
type FlowWatcher interface {
	Watch(key string, mac net.HardwareAddr, leaseFile string, since time.Time)
	Final(ctx context.Context, key string, until time.Time) (flows.Result, bool, error)
	Forget(key string)
}

// WithFlows totals every job's traffic by destination, learning each guest's
// address from the DHCP leases under leaseDir (one directory per bridge).
func WithFlows(w FlowWatcher, leaseDir string) Option {
	return func(r *Runner) { r.flows, r.leaseDir = w, leaseDir }
}

// startMonitoring begins measuring a job that has just launched. Everything
// here is off the job's path: a backend that cannot say where the counters are
// costs the measurement and nothing else.
func (r *Runner) startMonitoring(ctx context.Context, lease *alloc.Lease, inst *provider.Instance,
	launchedAt time.Time,
) {
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
	r.monitor.Start(inst.Name, usageTargetOf(target), lease.VCPU)

	r.startFlows(inst.Name, target, launchedAt)
}

// usageTargetOf is where the sampler reads an instance's counters, as the
// backend said.
func usageTargetOf(target provider.UsageTarget) usage.Target {
	return usage.Target{
		CgroupDir: target.CgroupDir, PID: target.PID, PIDStart: target.PIDStart,
		VCPUThreadPrefix: target.VCPUThreadPrefix,
		NetDevice:        target.NetDevice, NetHostView: target.NetHostView,
		NetNamespace: target.NetNamespace,
		Process:      target.Process,
	}
}

// resumeMonitoring begins measuring a job this process adopted and did not
// launch, continuing its series from the checkpoint the process that handed it
// over left, when one was left and continues it (#384). Off the job's path,
// as startMonitoring is. Its traffic by destination is not followed: what it
// sent before this process looked is in no tracker this process reads.
func (r *Runner) resumeMonitoring(ctx context.Context, lease *alloc.Lease, inst *provider.Instance) {
	cp := r.takeCheckpoint(inst.Name)
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
		r.log.Warn("could not find where an adopted job's host counters are; it will not be measured",
			"runner", inst.Name, "error", err)
		return
	}
	if err := r.monitor.Resume(inst.Name, usageTargetOf(target), lease.VCPU, cp); err != nil {
		r.log.Warn("measuring an adopted job from its adoption", "runner", inst.Name, "error", err)

		return
	}
	r.log.Info("measuring an adopted job", "runner", inst.Name, "continued", cp != nil)
}

// checkpointFile is where a job's checkpoint is kept, for an instance name
// billet assigned, or false for any other.
func (r *Runner) checkpointFile(name string) (string, bool) {
	if r.checkpoints == "" || name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return "", false
	}
	if _, ours := provider.LeaseOf(name); !ours {
		return "", false
	}

	return filepath.Join(r.checkpoints, name+checkpointSuffix), true
}

// checkpointSuffix ends every checkpoint file's name.
const checkpointSuffix = ".usage.json"

// takeCheckpoint reads and removes the checkpoint a handover left for an
// instance, nil when there is none or it cannot be read. Removed either way,
// so one is used at most once and none outlives the job it describes.
func (r *Runner) takeCheckpoint(name string) *usage.Checkpoint {
	path, ok := r.checkpointFile(name)
	if !ok {
		return nil
	}
	data, err := regularfile.ReadFile(path, usage.MaxCheckpointBytes, regularfile.Options{NoFollow: true})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		r.log.Warn("could not remove a used handover checkpoint", "path", path, "error", rmErr)
	}
	if err != nil {
		r.log.Warn("could not read what the previous node process measured of a job; measuring it from "+
			"its adoption", "runner", name, "error", err)
		return nil
	}
	cp, err := usage.ReadCheckpoint(data)
	if err != nil {
		r.log.Warn("could not read what the previous node process measured of a job; measuring it from "+
			"its adoption", "runner", name, "error", err)
		return nil
	}

	return &cp
}

// HandOver keeps what the sampler knows of every job this process measures,
// for the next node process to continue (#384). Called once a handoff's
// withdrawal has landed, as the process leaves its compute running. Best
// effort: a job it cannot keep is measured from its adoption, and nothing
// here can fail the stop.
func (r *Runner) HandOver(context.Context) {
	if r.monitor == nil || r.checkpoints == "" {
		return
	}
	installer := durablefile.Installer{}
	if err := installer.MkdirAll(r.checkpoints, 0o700); err != nil {
		r.log.Warn("could not keep what this process measured of its jobs for the next one; they are "+
			"measured from their adoption", "dir", r.checkpoints, "error", err)
		return
	}
	kept := 0
	for _, name := range r.measuredNames() {
		cp, ok := r.monitor.Checkpoint(name)
		path, named := r.checkpointFile(name)
		if !ok || !named {
			continue
		}
		data, err := json.Marshal(cp)
		if err == nil {
			_, err = installer.Install(r.checkpoints, filepath.Base(path), 0o600, func(w io.Writer) error {
				_, err := w.Write(data)
				return err
			})
		}
		if err != nil {
			r.log.Warn("could not keep what this process measured of a job for the next one; it is "+
				"measured from its adoption", "runner", name, "error", err)
			continue
		}
		kept++
	}
	r.log.Info("kept what this process measured of the jobs it hands over", "jobs", kept)
}

// measuredNames is every instance this process measures: its running jobs and
// the adopted ones in its custody.
func (r *Runner) measuredNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var names []string
	for _, inst := range r.running {
		if inst != nil {
			names = append(names, inst.Name)
		}
	}
	for _, c := range r.custody {
		if c.measured.Load() {
			names = append(names, c.name)
		}
	}

	return names
}

// pruneCheckpoints removes the checkpoints of instances the provider no longer
// lists, which no adoption will read. Only after a List that succeeded: an
// instance it could not see may still be there.
func (r *Runner) pruneCheckpoints(instances []*provider.Instance) {
	if r.checkpoints == "" {
		return
	}
	entries, err := os.ReadDir(r.checkpoints)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.log.Warn("could not look for stale handover checkpoints", "dir", r.checkpoints, "error", err)
		}
		return
	}
	listed := make(map[string]bool, len(instances))
	for _, inst := range instances {
		listed[inst.Name+checkpointSuffix] = true
	}
	for _, e := range entries {
		name := e.Name()
		if listed[name] || !strings.HasSuffix(name, checkpointSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(r.checkpoints, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.log.Warn("could not remove a stale handover checkpoint", "file", name, "error", err)
		}
	}
}

// startFlows begins following a guest's connections, when flows are measured
// and the backend said which DHCP lease holds the guest's address.
func (r *Runner) startFlows(name string, target provider.UsageTarget, launchedAt time.Time) {
	if r.flows == nil {
		return
	}

	mac, err := net.ParseMAC(target.GuestMAC)
	if err != nil || target.Bridge == "" || filepath.Base(target.Bridge) != target.Bridge {
		r.log.Warn("could not tell which DHCP lease holds a job's address; its traffic will not be "+
			"totalled by destination", "runner", name, "mac", target.GuestMAC, "bridge", target.Bridge)

		return
	}

	r.flows.Watch(name, mac, filepath.Join(r.leaseDir, target.Bridge, "dnsmasq.leases"), launchedAt)
}

// finalFlows takes what a job's connections came to, counting none that
// started after until, when the job's destroy began, and says it in the node's
// log beside the tap's own totals. It returns what the usage report carries:
// nil when the flows were not totalled, which the ledger keeps as not totalled
// rather than as a job that sent nothing.
func (r *Runner) finalFlows(ctx context.Context, name string, until time.Time, sum usage.Summary,
	sampled bool,
) *alloc.JobDestinations {
	if r.flows == nil {
		return nil
	}

	res, measured, err := r.flows.Final(context.WithoutCancel(ctx), name, until)
	if !measured {
		r.log.Warn("a job's traffic was not totalled by destination", "runner", name, "error", err)

		return nil
	}

	c := compareWithTap(res, sum, sampled)

	attrs := []any{"runner", name, "destinations", len(res.Destinations), "sent", c.sent,
		"received", c.received, "incomplete", res.Incomplete, "error", err}

	if c.tapKnown {
		attrs = append(attrs, "tap_sent", c.tapSent, "tap_received", c.tapReceived,
			"tap_minus_attributed_sent", c.tapSent-int64(c.sent),
			"tap_minus_attributed_received", c.tapReceived-int64(c.received))
	}

	r.log.Info("a job's traffic by destination", attrs...)

	return jobDestinationsOf(res, c)
}

// jobDestinationsOf is the report's destinations: each one as the tracker
// totalled it, largest first, the rest under Other, and the tap's totals when
// they were read and are not negative.
//
// A TOTAL TOO LARGE FOR THE LEDGER IS KEPT AT ITS LARGEST AND THE RESULT MARKED
// INCOMPLETE, so it reads as the lower bound it then is; and traffic to an
// address that is not one (which the tracker never reports) is counted under
// Other rather than lost.
func jobDestinationsOf(res flows.Result, c tapComparison) *alloc.JobDestinations {
	out := &alloc.JobDestinations{Incomplete: res.Incomplete}
	clamp := func(v uint64) int64 {
		if v > math.MaxInt64 {
			out.Incomplete = true
			return math.MaxInt64
		}
		return int64(v)
	}
	add := func(a, b int64) int64 {
		if a > math.MaxInt64-b {
			out.Incomplete = true
			return math.MaxInt64
		}
		return a + b
	}
	other := alloc.JobDestination{SentBytes: clamp(res.Other.Sent), ReceivedBytes: clamp(res.Other.Received),
		Connections: clamp(res.Other.Connections)}
	for _, d := range res.Destinations {
		dest := alloc.JobDestination{Addr: d.Addr.String(), SentBytes: clamp(d.Sent),
			ReceivedBytes: clamp(d.Received), Connections: clamp(d.Connections)}
		if !d.Addr.IsValid() || len(out.Destinations) == alloc.MaxJobDestinations {
			other.SentBytes = add(other.SentBytes, dest.SentBytes)
			other.ReceivedBytes = add(other.ReceivedBytes, dest.ReceivedBytes)
			other.Connections = add(other.Connections, dest.Connections)

			continue
		}
		out.Destinations = append(out.Destinations, dest)
	}
	out.Other = other
	if c.tapKnown && c.tapSent >= 0 && c.tapReceived >= 0 {
		out.Tap = &alloc.TapTotals{SentBytes: c.tapSent, ReceivedBytes: c.tapReceived}
	}

	return out
}

// tapComparison is what a job's destinations attributed, beside its tap's own
// totals.
//
// A COMPARISON OF TWO COUNTERS, NOT A MEASUREMENT OF WHAT WAS MISSED. The tap
// sees every byte the guest sent or received and cannot be forged by it, so a
// large positive difference says traffic no destination accounts for: a flow
// missed, merged into an entry an earlier guest left, or never tracked (ARP,
// and DHCP before the guest has its address). But the tap is read before the
// destroy and the flows after it, so replies that arrive in between are
// attributed without reaching the tap, and its last reading can be an older
// one when the final read failed. The tap counts each skb with its 14-byte
// Ethernet header, which is taken off; a VLAN tag or a non-IP payload is not.
type tapComparison struct {
	sent, received       uint64
	tapKnown             bool
	tapSent, tapReceived int64
}

func compareWithTap(res flows.Result, sum usage.Summary, sampled bool) tapComparison {
	c := tapComparison{sent: res.Other.Sent, received: res.Other.Received}
	for _, d := range res.Destinations {
		c.sent, c.received = c.sent+d.Sent, c.received+d.Received
	}

	if tap := sum.Latest; sampled && sum.Measured.Net {
		c.tapKnown = true
		c.tapSent = tap.NetTx - ethernetHeader*tap.NetTxPackets
		c.tapReceived = tap.NetRx - ethernetHeader*tap.NetRxPackets
	}

	return c
}

// ethernetHeader is what a tap counts for each packet beyond the IP packet
// conntrack counts.
const ethernetHeader = 14

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

	if r.flows != nil {
		r.flows.Forget(name)
	}
}

// reportUsage sends a job's usage to the ledger, fenced on the lease's epoch.
// The lease must still be live: the plane releases it only after the destroy
// this runs inside of returns.
func (r *Runner) reportUsage(ctx context.Context, lease *alloc.Lease, name string, sum usage.Summary,
	destinations *alloc.JobDestinations,
) {
	rec, ok := r.alloc.(UsageRecorder)
	if !ok || lease == nil {
		return
	}
	report, series := jobUsageOf(sum)
	report.Destinations = destinations
	logGaps(r.log, lease.ID, name, sum)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageReportLimit)
	defer cancel()

	if err := rec.RecordLeaseUsage(ctx, lease.ID, lease.Epoch, report, series); err != nil {
		r.log.Warn("could not report what a job did to the host; the job is unaffected",
			"runner", name, "lease", lease.ID, "error", err)
	}
}

// logGaps says in the node's log how much of a job's series the sampler did not
// see, by cause, beside the report that carries it: the series says which
// intervals, and this is what an operator searching the node's journal for a
// lease finds (#495). A series it saw whole says nothing.
func logGaps(log *slog.Logger, leaseID, name string, sum usage.Summary) {
	gaps := usage.Timeline{First: sum.First, Points: sum.Points}.Gaps()
	if len(gaps) == 0 {
		return
	}
	unseen := map[usage.GapCause]time.Duration{}
	var total time.Duration
	for _, g := range gaps {
		unseen[g.Cause] += g.Length()
		total += g.Length()
	}
	attrs := []any{"runner", name, "lease", leaseID, "gaps", len(gaps), "unseen", total.Round(time.Millisecond)}
	for _, c := range usage.GapCauses() {
		if d, ok := unseen[c]; ok {
			attrs = append(attrs, c.String(), d.Round(time.Millisecond))
		}
	}
	log.Info("a job's series has intervals the sampler did not see", attrs...)
}

// heldUsage is an adopted job's last sample, taken before its compute goes,
// and when it was taken.
type heldUsage struct {
	sum usage.Summary
	has bool
	at  time.Time
}

// heldFinal takes the last sample of an adopted job in custody, before its
// compute is destroyed or as it is found gone; nothing for an entry that owes
// no report.
func (r *Runner) heldFinal(c *custody) heldUsage {
	if !c.measured.Load() {
		return heldUsage{}
	}
	sum, has := r.finalUsage(c.name)

	return heldUsage{sum: sum, has: has, at: time.Now()}
}

// settleHeldUsage reports, once, what an adopted job in custody did and what
// its guest said, as its compute goes and while its lease is still held
// (#384): the path that ends an adopted job used to report neither, so a job
// that ran across a handover, or was adopted again when its own node
// re-registered, had no usage at all. current says the entry's epoch is the
// ledger's; a lease that has gone takes no report. Either way the job stops
// being measured.
func (r *Runner) settleHeldUsage(ctx context.Context, c *custody, h heldUsage, current bool) {
	if !c.measured.CompareAndSwap(true, false) {
		return
	}
	if current {
		lease := &alloc.Lease{ID: c.leaseID, Epoch: c.epoch.Load()}
		if h.has {
			r.reportUsage(ctx, lease, c.name, h.sum, r.finalFlows(ctx, c.name, h.at, h.sum, h.has))
		}
		r.reportGuest(ctx, lease, c.name)
	}
	r.forgetMonitoring(c.name)
}

// reportGuest sends what a job's guest told the node to the ledger, fenced on the
// lease's epoch, while the lease is still held. The lease is the runner's own for
// the instance, and the report is the one the instance's session kept, found by
// the bearer the guest sent; nothing the guest wrote names either.
//
// BOUNDED AND NEVER FATAL, as the usage report is: a control plane that cannot
// take it costs the guest's view, never the teardown. One whose wire predates
// guest reports takes none, and the client drops it as sent.
func (r *Runner) reportGuest(ctx context.Context, lease *alloc.Lease, name string) {
	rec, ok := r.alloc.(GuestRecorder)
	if !ok || r.cache == nil || lease == nil {
		return
	}

	takeCtx, cancelTake := context.WithTimeout(context.WithoutCancel(ctx), guestReportLimit)
	report, ok := r.cache.TakeGuestReport(takeCtx, name)
	cancelTake()

	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guestReportLimit)
	defer cancel()

	if err := rec.RecordGuestReport(ctx, lease.ID, lease.Epoch, report); err != nil {
		r.log.Warn("could not report what a job's guest said; the job is unaffected",
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
		MemoryPeakBytes: sum.MemoryPeak,
		DiskReadBytes:   s.DiskRead, DiskWriteBytes: s.DiskWrite,
		NetRxBytes: s.NetRx, NetTxBytes: s.NetTx, NetRxPackets: s.NetRxPackets, NetTxPackets: s.NetTxPackets,
		CPUSomeMicros: s.CPUSome, CPUFullMicros: s.CPUFull,
		MemorySomeMicros: s.MemorySome, MemoryFullMicros: s.MemoryFull,
		IOSomeMicros: s.IOSome, IOFullMicros: s.IOFull,
	}
	for group, measured := range map[string]bool{
		alloc.UsageCPU: sum.Measured.CPU, alloc.UsageMemory: sum.Measured.Memory,
		alloc.UsageOOM: sum.Measured.OOM,
		alloc.UsageIO:  sum.Measured.IO, alloc.UsageNet: sum.Measured.Net,
		alloc.UsageThreads: sum.Measured.Threads, alloc.UsagePressure: sum.Measured.Pressure,
		alloc.UsageEnergy: sum.Measured.Energy,
	} {
		if !measured {
			u.Unmeasured = append(u.Unmeasured, group)
		}
	}
	slices.Sort(u.Unmeasured)
	if sum.Measured.Memory && sum.Measured.OOM {
		u.OOMKills = s.OOMKills
	}
	if sum.Measured.Energy {
		u.EnergyActiveMicrojoules, u.EnergyIdleMicrojoules = sum.EnergyActive, sum.EnergyIdle
		switch {
		case sum.EnergyProcess:
			u.EnergySource = alloc.EnergyProcess
		case sum.EnergySplit:
			u.EnergySource = alloc.EnergyRAPL
		default:
			u.EnergySource = alloc.EnergyRAPLUnsplit
		}
	}
	u.Counters = jobCountersOf(sum.Counters)
	if len(sum.Points) == 0 {
		return u, nil
	}
	// CLOCKED WHENEVER THE SAMPLER SAYS WHEN IT BEGAN, which is what lets an
	// operator place a step's window on the series, and with each unseen
	// interval's cause; the node client re-encodes it without the causes, or
	// without the clock, for a plane too old to keep them.
	if !sum.First.IsZero() {
		data, _, err := usage.EncodeSeriesGaps(sum.Points, sum.First, alloc.MaxUsageSeriesBytes)
		if err == nil {
			return u, &alloc.UsageSeries{Codec: usage.SeriesCodecGaps, Data: data}
		}
	}
	data, _, err := usage.EncodeSeries(sum.Points, alloc.MaxUsageSeriesBytes)
	if err != nil {
		return u, nil
	}

	return u, &alloc.UsageSeries{Codec: usage.SeriesCodec, Data: data}
}

// jobCountersOf is the report's hardware counters: each event the sampler
// counted, and none at all when it counted no event, since the ledger reads a
// block of nothing as no block.
func jobCountersOf(c *usage.Counters) *alloc.JobCounters {
	if c == nil {
		return nil
	}
	value := func(e usage.Event) *int64 {
		if !c.Measured[e] {
			return nil
		}
		v := c.Values[e]
		return &v
	}
	out := alloc.JobCounters{
		Cycles: value(usage.Cycles), Instructions: value(usage.Instructions),
		CacheReferences: value(usage.CacheReferences), CacheMisses: value(usage.CacheMisses),
		BranchMisses: value(usage.BranchMisses), FrontendStallCycles: value(usage.FrontendStalls),
	}
	if out == (alloc.JobCounters{}) {
		return nil
	}

	return &out
}
