package guestagent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// maxProcesses bounds the processes one walk reads. A guest holding more is one
// whose processes the agent could not tell, rather than one it reads partly.
const maxProcesses = 1 << 15

// The device and interface whose totals a Sample carries: a Firecracker guest's
// root disk and its one network interface.
const (
	guestDisk      = "vda"
	guestInterface = "eth0"
)

// procKey names one process across walks: a pid the kernel reuses starts at
// another time.
type procKey struct {
	pid   int
	start uint64
}

// procCounters is what one process had used by one walk.
type procCounters struct {
	cpu, read, written int64
}

// collector reads the guest's totals and processes from a /proc tree and holds
// what it read until the sender cuts a batch. What it holds is bounded by one
// batch: a reading that would pass a batch's bound is not taken, and the next one
// taken covers the time since the last, because every figure but a level is a
// counter. The last place of each kind is the final reading's alone, so a job that
// ends while the node is not taking batches still has its end read.
type collector struct {
	proc fs.FS

	samples    []guestreport.Sample
	lastSample *guestreport.Sample

	processes  []guestreport.ProcessSample
	lastProcAt int64
	// base is every process the last kept walk read, and baseBusy the guest's
	// busy CPU after it; the next walk is measured from them.
	base     map[procKey]procCounters
	baseBusy int64
	haveBase bool

	// agentCPU is the agent's own CPU, once haveAgentCPU says it was read.
	agentCPU     int64
	haveAgentCPU bool

	log *limitedLog
}

// room is how many readings of a kind the collector holds: one fewer than a batch
// takes, unless this is the final reading.
func room(bound int, final bool) int {
	if final {
		return bound
	}

	return bound - 1
}

func newCollector(proc fs.FS, log *limitedLog) *collector {
	return &collector{proc: proc, log: log}
}

// sample reads the guest's totals at now and keeps them, unless the pending
// samples have no room or the reading goes back on the one kept before it (a clock
// stepped back, a counter the kernel moved back), which the node would refuse in a
// merge.
func (c *collector) sample(now time.Time, final bool) {
	if len(c.samples) >= room(guestreport.MaxBatchSamples, final) {
		return
	}

	s, err := c.readSample(now)
	if err != nil {
		c.log.printf("totals", "the guest's totals were not read, so no sample is sent for now: %v", err)

		return
	}

	if c.lastSample != nil && !follows(&s, c.lastSample) {
		c.log.printf("totals-back", "the guest's clock or a counter went back; the sample is not sent")

		return
	}

	c.samples = append(c.samples, s)
	c.lastSample = &c.samples[len(c.samples)-1]
}

// follows reports whether s comes after prev: a later time, and no counter lower.
func follows(s, prev *guestreport.Sample) bool {
	return s.AtMillis > prev.AtMillis &&
		s.CPUBusyTicks >= prev.CPUBusyTicks && s.CPUTotalTicks >= prev.CPUTotalTicks &&
		s.DiskReadBytes >= prev.DiskReadBytes && s.DiskWriteBytes >= prev.DiskWriteBytes &&
		s.NetRxBytes >= prev.NetRxBytes && s.NetTxBytes >= prev.NetTxBytes
}

func (c *collector) readSample(now time.Time) (guestreport.Sample, error) {
	s := guestreport.Sample{AtMillis: now.UnixMilli()}

	stat, err := readProc(c.proc, "stat", totalsFileLimit)
	if err != nil {
		return s, fmt.Errorf("stat: %w", err)
	}

	if s.CPUBusyTicks, s.CPUTotalTicks, err = cpuTicks(stat); err != nil {
		return s, err
	}

	meminfo, err := readProc(c.proc, "meminfo", totalsFileLimit)
	if err != nil {
		return s, fmt.Errorf("meminfo: %w", err)
	}

	if s.MemUsedBytes, s.MemAvailableBytes, s.MemCachedBytes, err = memory(meminfo); err != nil {
		return s, err
	}

	diskstats, err := readProc(c.proc, "diskstats", totalsFileLimit)
	if err != nil {
		return s, fmt.Errorf("diskstats: %w", err)
	}

	if s.DiskReadBytes, s.DiskWriteBytes, err = diskBytes(diskstats, guestDisk); err != nil {
		return s, err
	}

	dev, err := readProc(c.proc, "net/dev", totalsFileLimit)
	if err != nil {
		return s, fmt.Errorf("net/dev: %w", err)
	}

	if s.NetRxBytes, s.NetTxBytes, err = netBytes(dev, guestInterface); err != nil {
		return s, err
	}

	if s.AtMillis <= 0 || s.AtMillis > guestreport.MaxValue || s.CPUTotalTicks > guestreport.MaxValue {
		return s, fmt.Errorf("%w: the clock or the CPU total is past what a report carries", errCouldNotTell)
	}

	return s, nil
}

// walkProcesses walks the guest's processes at now and keeps what each did since the
// last kept walk, by name. The first walk only sets the base. A walk that could
// not read every live process, or that the pending process samples have no room
// for, keeps nothing and leaves the base where it was, so the next kept walk
// covers the time since it.
func (c *collector) walkProcesses(now time.Time, final bool) {
	if len(c.processes) >= room(guestreport.MaxBatchProcessSamples, final) {
		return
	}

	at := now.UnixMilli()
	if c.haveBase && at <= c.lastProcAt {
		c.log.printf("processes-back", "the guest's clock went back; no process sample is sent for now")

		return
	}

	current, rows, sum, err := c.walk()
	if err != nil {
		c.log.printf("processes", "the guest's processes were not all read, so no process sample is sent for now: %v", err)

		return
	}

	// The guest's busy CPU is read AFTER the walk, so it includes everything the
	// processes it read had used when they were read.
	stat, err := readProc(c.proc, "stat", totalsFileLimit)
	if err != nil {
		c.log.printf("processes", "the guest's CPU was not read, so no process sample is sent for now: stat: %v", err)

		return
	}

	busy, _, err := cpuTicks(stat)
	if err != nil {
		c.log.printf("processes", "the guest's CPU was not read, so no process sample is sent for now: %v", err)

		return
	}

	if !c.haveBase {
		c.base, c.baseBusy, c.haveBase, c.lastProcAt = current, busy, true, at

		return
	}

	kept, other, err := guestreport.Fold(rows)
	if err != nil {
		c.log.printf("processes", "the guest's processes did not fold into a sample: %v", err)

		return
	}

	// THE RESIDUAL is the busy CPU no live process was charged with: what a process
	// that exited since the last walk used, and the interrupts no process is
	// charged with. The walk and the stat read are not one instant, so it can come
	// out a little below zero, which is no CPU.
	residual := max(busy-c.baseBusy-sum, 0)

	c.processes = append(c.processes, guestreport.ProcessSample{
		AtMillis: at, Rows: kept, Other: other, ResidualCPUTicks: residual,
	})
	c.base, c.baseBusy, c.lastProcAt = current, busy, at
}

// walk reads every process: what each has used, and one row per process of what it
// used since the base, with the sum of their CPU. A process that exited while it
// was read is left out, and what it used goes to the residual.
func (c *collector) walk() (map[procKey]procCounters, []guestreport.ProcessRow, int64, error) {
	pids, err := c.pids()
	if err != nil {
		return nil, nil, 0, err
	}

	current := make(map[procKey]procCounters, len(pids))
	rows := make([]guestreport.ProcessRow, 0, len(pids))

	var sum int64

	for _, pid := range pids {
		key, counters, row, err := c.readProcess(pid)
		if errors.Is(err, errGone) {
			continue
		}

		if err != nil {
			return nil, nil, 0, err
		}

		current[key] = counters

		prev, seen := c.base[key]
		if seen {
			if counters.cpu < prev.cpu || counters.read < prev.read || counters.written < prev.written {
				return nil, nil, 0, fmt.Errorf("%w: a process's counters went back", errCouldNotTell)
			}

			row.CPUTicks, row.ReadBytes, row.WriteBytes =
				counters.cpu-prev.cpu, counters.read-prev.read, counters.written-prev.written
		}

		sum += row.CPUTicks
		rows = append(rows, row)
	}

	return current, rows, sum, nil
}

// readProcess is one process's stat, status and io, never its cmdline or environ,
// which can hold a secret. Its row carries everything the process has used; walk
// makes that what it used since the base.
func (c *collector) readProcess(pid int) (procKey, procCounters, guestreport.ProcessRow, error) {
	dir := strconv.Itoa(pid)

	statData, err := readProc(c.proc, dir+"/stat", processFileLimit)
	if err != nil {
		return procKey{}, procCounters{}, guestreport.ProcessRow{}, err
	}

	st, err := parsePIDStat(statData)
	if err != nil {
		return procKey{}, procCounters{}, guestreport.ProcessRow{}, err
	}

	status, err := readProc(c.proc, dir+"/status", processFileLimit)
	if err != nil {
		return procKey{}, procCounters{}, guestreport.ProcessRow{}, err
	}

	rss, err := residentBytes(status, st)
	if err != nil {
		return procKey{}, procCounters{}, guestreport.ProcessRow{}, err
	}

	ioData, err := readProc(c.proc, dir+"/io", processFileLimit)
	if err != nil {
		return procKey{}, procCounters{}, guestreport.ProcessRow{}, err
	}

	read, written, err := ioBytes(ioData)
	if err != nil {
		return procKey{}, procCounters{}, guestreport.ProcessRow{}, err
	}

	counters := procCounters{cpu: st.cpu, read: read, written: written}

	return procKey{pid: pid, start: st.start}, counters, guestreport.ProcessRow{
		Name: textOrMark(guestreport.Clean(st.comm, guestreport.MaxNameBytes)),
		ProcessUsage: guestreport.ProcessUsage{
			Procs: 1, CPUTicks: st.cpu, RSSBytes: rss, ReadBytes: read, WriteBytes: written,
		},
	}, nil
}

// pids lists the processes in the tree, at most maxProcesses of them.
func (c *collector) pids() ([]int, error) {
	dir, err := c.proc.Open(".")
	if err != nil {
		return nil, fmt.Errorf("%w: list the processes: %w", errCouldNotTell, err)
	}
	defer dir.Close()

	lister, ok := dir.(fs.ReadDirFile)
	if !ok {
		return nil, fmt.Errorf("%w: the process tree cannot be listed", errCouldNotTell)
	}

	var pids []int

	for {
		entries, err := lister.ReadDir(256)
		for _, e := range entries {
			pid, parseErr := strconv.Atoi(e.Name())
			if parseErr != nil || pid <= 0 || !e.IsDir() {
				continue
			}

			if len(pids) == maxProcesses {
				return nil, fmt.Errorf("%w: more than %d processes", errCouldNotTell, maxProcesses)
			}

			pids = append(pids, pid)
		}

		if errors.Is(err, io.EOF) || (err == nil && len(entries) == 0) {
			return pids, nil
		}

		if err != nil {
			return nil, fmt.Errorf("%w: list the processes: %w", errCouldNotTell, err)
		}
	}
}

// readAgentCPU is the CPU the agent has used since it started, from its own stat.
// The figure only rises; a failed read keeps the last one read, which is still a
// true figure for the time it was read, and before any was read the figure is
// unknown, which haveAgentCPU says.
func (c *collector) readAgentCPU() {
	data, err := readProc(c.proc, "self/stat", processFileLimit)
	if err == nil {
		var st pidStat

		if st, err = parsePIDStat(data); err == nil {
			c.agentCPU, c.haveAgentCPU = max(c.agentCPU, st.cpu), true

			return
		}
	}

	c.log.printf("self", "the agent's own CPU was not read; the last figure read is sent: %v", err)
}

// take hands over everything pending, which the batch being cut now carries.
func (c *collector) take() ([]guestreport.Sample, []guestreport.ProcessSample) {
	samples, processes := c.samples, c.processes
	c.samples, c.processes = nil, nil

	// lastSample pointed into the slice just handed over; it stays the sample the
	// next one must follow.
	if c.lastSample != nil {
		last := *c.lastSample
		c.lastSample = &last
	}

	return samples, processes
}

// textOrMark is text Clean left empty, which the schema refuses, as a mark that
// says the name was there and could not be shown.
func textOrMark(s string) string {
	if s == "" {
		return "?"
	}

	return s
}
