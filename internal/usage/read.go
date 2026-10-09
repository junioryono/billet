package usage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Target is where one job's host-side counters live.
type Target struct {
	// CgroupDir is the job's own cgroup-v2 directory, created for this job, so
	// its counters start at zero when the job does.
	CgroupDir string
	// PID is the VMM's process, whose threads split guest time from the VMM's
	// own. Zero means there is no such split to make (a container).
	PID int
	// PIDStart is the start time PID had when the target was made
	// (ProcessStart). Every read checks it, so a pid the kernel has since given
	// to another process, a replacement VMM included, is never read as this
	// job's. Zero is unknown, and the threads are then not read at all.
	PIDStart uint64
	// VCPUThreadPrefix names the threads that run guest code ("fc_vcpu" for
	// Firecracker, whose vCPU threads are "fc_vcpu 0" through "fc_vcpu N").
	VCPUThreadPrefix string
	// NetDevice is the host's end of the job's network device, created for this
	// job. Empty means the job's traffic is not measured.
	NetDevice string
	// NetHostView says the device's counters are the host's view, so received
	// is what the guest sent. True for a tap, whose rx is the guest's tx.
	NetHostView bool
	// Process says the job is PID alone, with no cgroup: it is measured by the
	// process's own accounting (ProcessCounters), checked against PIDStart on
	// every read, and CgroupDir is not read.
	Process bool
}

// Sample is one reading of a job's cumulative counters. A group whose OK flag
// is false could not be read this time, and its fields are zero for that
// reason rather than measured as zero.
type Sample struct {
	CPUOK                     bool
	CPUUsage, CPUUser, CPUSys int64 // µs
	MemoryOK                  bool
	MemoryCurrent, MemoryPeak int64 // bytes
	// OOMOK says OOMKills was read. A process's own accounting has no such
	// count, so a job measured that way reports memory without it.
	OOMOK                      bool
	OOMKills                   int64
	IOOK                       bool
	DiskRead, DiskWrite        int64 // bytes
	NetOK                      bool
	NetRx, NetTx               int64 // bytes, guest view
	NetRxPackets, NetTxPackets int64
	ThreadsOK                  bool
	GuestCPU, VMMCPU           int64 // µs
	PressureOK                 bool
	CPUSome, CPUFull           int64 // µs
	MemorySome, MemoryFull     int64
	IOSome, IOFull             int64
	// ProcessEnergy is the kernel's own estimate of the energy a job's process
	// has used over its whole life, in µJ.
	ProcessEnergyOK bool
	ProcessEnergy   int64
}

// Reader reads counters under a filesystem root, "/" on a real host and a
// fixture tree in a test.
type Reader struct {
	Root string
	// Process reads a process's own accounting; nil means the platform's. A
	// test sets it, since the real one reads the running kernel.
	Process func(pid int) (ProcessCounters, error)
}

func (r Reader) path(p string) string { return filepath.Join(r.Root, p) }

func (r Reader) read(p string) (string, error) {
	b, err := os.ReadFile(r.path(p))
	return string(b), err
}

// Read takes one sample of a job's counters. It never fails as a whole: each
// group reports its own verdict, because a host without the memory controller
// still has CPU time worth reporting.
func (r Reader) Read(t Target) Sample {
	s, _ := r.readListing(t)

	return s
}

// vcpuThread names one vCPU thread: its tid, and the start time that with the
// tid names one thread, since a tid the kernel frees can be given to another.
type vcpuThread struct {
	tid   int
	start uint64
}

// readListing is Read, and the VMM's vCPU threads the sample's thread split was
// read from: nil unless ThreadsOK.
func (r Reader) readListing(t Target) (Sample, []vcpuThread) {
	var s Sample
	if t.Process {
		s.readProcess(r, t)
		return s, nil
	}
	s.readCPU(r, t)
	s.readMemory(r, t)
	s.readIO(r, t)
	s.readPressure(r, t)
	s.readNet(r, t)
	vcpus := s.readThreads(r, t)

	return s, vcpus
}

func (s *Sample) readCPU(r Reader, t Target) {
	raw, err := r.read(filepath.Join(t.CgroupDir, "cpu.stat"))
	if err != nil {
		return
	}
	stat, err := parseFlatKeyed(raw)
	if err != nil {
		return
	}
	usage, ok1 := stat["usage_usec"]
	user, ok2 := stat["user_usec"]
	sys, ok3 := stat["system_usec"]
	if !ok1 || !ok2 || !ok3 {
		return
	}
	s.CPUUsage, s.CPUUser, s.CPUSys, s.CPUOK = usage, user, sys, true
}

func (s *Sample) readMemory(r Reader, t Target) {
	current, err := r.read(filepath.Join(t.CgroupDir, "memory.current"))
	if err != nil {
		return
	}
	cur, err := parseSingle(current)
	if err != nil {
		return
	}
	// memory.peak is 5.19+; where it is absent the sampler's own maximum of
	// memory.current stands in, which misses a spike between samples.
	peak := cur
	if raw, err := r.read(filepath.Join(t.CgroupDir, "memory.peak")); err == nil {
		if p, err := parseSingle(raw); err == nil {
			peak = p
		}
	}
	events, err := r.read(filepath.Join(t.CgroupDir, "memory.events"))
	if err != nil {
		return
	}
	ev, err := parseFlatKeyed(events)
	if err != nil {
		return
	}
	s.MemoryCurrent, s.MemoryPeak, s.OOMKills, s.MemoryOK, s.OOMOK = cur, peak, ev["oom_kill"], true, true
}

func (s *Sample) readIO(r Reader, t Target) {
	raw, err := r.read(filepath.Join(t.CgroupDir, "io.stat"))
	if err != nil {
		return
	}
	rb, wb, err := parseIOStat(raw)
	if err != nil {
		return
	}
	s.DiskRead, s.DiskWrite, s.IOOK = rb, wb, true
}

func (s *Sample) readPressure(r Reader, t Target) {
	var values [3][2]int64
	for i, name := range []string{"cpu.pressure", "memory.pressure", "io.pressure"} {
		raw, err := r.read(filepath.Join(t.CgroupDir, name))
		if err != nil {
			return
		}
		if values[i][0], values[i][1], err = parsePressure(raw); err != nil {
			return
		}
	}
	s.CPUSome, s.CPUFull = values[0][0], values[0][1]
	s.MemorySome, s.MemoryFull = values[1][0], values[1][1]
	s.IOSome, s.IOFull = values[2][0], values[2][1]
	s.PressureOK = true
}

func (s *Sample) readNet(r Reader, t Target) {
	if t.NetDevice == "" || strings.ContainsAny(t.NetDevice, "/.") {
		return
	}
	var v [4]int64
	for i, name := range []string{"rx_bytes", "tx_bytes", "rx_packets", "tx_packets"} {
		raw, err := r.read("/sys/class/net/" + t.NetDevice + "/statistics/" + name)
		if err != nil {
			return
		}
		if v[i], err = parseSingle(raw); err != nil {
			return
		}
	}
	// A TAP COUNTS FROM THE HOST'S SIDE: what the host transmitted on it is
	// what the guest received.
	if t.NetHostView {
		v[0], v[1], v[2], v[3] = v[1], v[0], v[3], v[2]
	}
	s.NetRx, s.NetTx, s.NetRxPackets, s.NetTxPackets, s.NetOK = v[0], v[1], v[2], v[3], true
}

// readThreads splits a VMM's CPU time into its vCPU threads (guest code) and
// everything else (the event loop, the API thread: emulation and IO).
func (s *Sample) readThreads(r Reader, t Target) []vcpuThread {
	if t.PID <= 0 || t.VCPUThreadPrefix == "" || t.PIDStart == 0 {
		return nil
	}
	guest, vmm, vcpus, err := r.threadTimes(t.PID, t.VCPUThreadPrefix)
	if err != nil {
		return nil
	}
	// CHECKED AFTER THE READ, which covers a pid reused before it and during it:
	// either way the process holding the pid now is not the one recorded, and
	// what was read is discarded.
	if start, err := r.ProcessStart(t.PID); err != nil || start != t.PIDStart {
		return nil
	}
	s.GuestCPU, s.VMMCPU, s.ThreadsOK = guest, vmm, true

	return vcpus
}

// isVCPUThread reports whether v is still one of the target VMM's vCPU
// threads: listed under the VMM with the start time it was listed with, named
// as one, and the VMM still the process the target recorded. A thread or a VMM
// that is gone, or a tid now naming another thread, is a no; a read that fails
// otherwise, or a stat it cannot parse, is an error, because it cannot tell.
func (r Reader) isVCPUThread(t Target, v vcpuThread) (bool, error) {
	raw, err := r.read(fmt.Sprintf("/proc/%d/task/%d/stat", t.PID, v.tid))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	comm, _, _, err := parseTaskStat(raw)
	if err != nil {
		return false, err
	}
	threadStart, err := parseStartTime(raw)
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(comm, t.VCPUThreadPrefix) || threadStart != v.start {
		return false, nil
	}
	start, err := r.ProcessStart(t.PID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return start == t.PIDStart, nil
}

// ProcessStart is the start time of pid, which with the pid names one process.
func (r Reader) ProcessStart(pid int) (uint64, error) {
	raw, err := r.read(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}

	return parseStartTime(raw)
}

func (r Reader) threadTimes(pid int, vcpuPrefix string) (int64, int64, []vcpuThread, error) {
	dir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(r.path(dir))
	if err != nil {
		return 0, 0, nil, err
	}
	var guest, vmm int64
	var vcpus []vcpuThread
	for _, entry := range entries {
		raw, err := r.read(filepath.Join(dir, entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			// A thread that exited between the listing and the read.
			continue
		}
		if err != nil {
			return 0, 0, nil, err
		}
		comm, utime, stime, err := parseTaskStat(raw)
		if err != nil {
			return 0, 0, nil, err
		}
		ticks := utime + stime
		if !strings.HasPrefix(comm, vcpuPrefix) {
			vmm += ticks
			continue
		}
		tid, err := strconv.Atoi(entry.Name())
		if err != nil {
			return 0, 0, nil, fmt.Errorf("usage: task %q is not a thread id", entry.Name())
		}
		start, err := parseStartTime(raw)
		if err != nil {
			return 0, 0, nil, err
		}
		guest += ticks
		vcpus = append(vcpus, vcpuThread{tid: tid, start: start})
	}
	if len(vcpus) == 0 {
		return 0, 0, nil, fmt.Errorf("usage: pid %d has no %q thread, so it is not the VMM", pid, vcpuPrefix)
	}

	return ticksToMicros(guest), ticksToMicros(vmm), vcpus, nil
}

// HostCPU is the host's aggregate CPU counters from /proc/stat, in µs.
type HostCPU struct {
	Busy, Total int64
	CPUs        int
}

// ReadHostCPU reads the host's CPU counters.
func (r Reader) ReadHostCPU() (HostCPU, error) {
	raw, err := r.read("/proc/stat")
	if err != nil {
		return HostCPU{}, err
	}
	busy, total, cpus, err := parseHostCPU(raw)
	if err != nil {
		return HostCPU{}, err
	}

	return HostCPU{Busy: ticksToMicros(busy), Total: ticksToMicros(total), CPUs: cpus}, nil
}

// raplZone is the package zone. The core subzone (intel-rapl:0:0) is never
// read: on the reference host (AMD EPYC 7763, 2026-09-25) it read 0.6 W while
// turbostat's CorWatt read 4.2 W for all cores, which fits AMD's per-core
// energy register reporting a single core.
const raplZone = "/sys/class/powercap/intel-rapl:0"

// secondPackage is the zone a second socket's package would have. Energy from
// package 0 shared by CPU time counted on every socket would charge one
// socket's energy to jobs running on another, so a host with a second package
// reports energy as could-not-tell instead.
const secondPackage = "/sys/class/powercap/intel-rapl:1"

// Energy is one reading of the package energy counter.
type Energy struct {
	Microjoules, MaxRange int64
}

// ReadEnergy reads the package energy counter and its wrap point.
func (r Reader) ReadEnergy() (Energy, error) {
	switch _, err := os.Stat(r.path(secondPackage)); {
	case err == nil:
		return Energy{}, errors.New("usage: this host has more than one package, and energy " +
			"is attributed from package 0 alone")
	case !errors.Is(err, os.ErrNotExist):
		return Energy{}, fmt.Errorf("usage: could not tell how many packages this host has: %w", err)
	}
	rawMax, err := r.read(filepath.Join(raplZone, "max_energy_range_uj"))
	if err != nil {
		return Energy{}, err
	}
	maxRange, err := parseSingle(rawMax)
	if err != nil {
		return Energy{}, err
	}
	rawNow, err := r.read(filepath.Join(raplZone, "energy_uj"))
	if err != nil {
		return Energy{}, err
	}
	now, err := parseSingle(rawNow)
	if err != nil {
		return Energy{}, err
	}
	if maxRange <= 0 || now < 0 || now > maxRange {
		return Energy{}, fmt.Errorf("usage: energy %d outside its range %d", now, maxRange)
	}

	return Energy{Microjoules: now, MaxRange: maxRange}, nil
}
