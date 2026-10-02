package usage

import "fmt"

// ProcessCounters is one process's own accounting, as the kernel keeps it for
// the process's whole life: what a job is measured by when it is one process
// and has no cgroup of its own (a Virtualization.framework VM on macOS).
type ProcessCounters struct {
	// Start names the process together with its pid: two processes that held the
	// same pid never have the same start.
	Start uint64
	// UserMicros and SystemMicros are the CPU time the process has run.
	UserMicros, SystemMicros int64
	// Footprint is the memory the process is charged for now, and PeakFootprint
	// the most it has ever been charged for.
	Footprint, PeakFootprint int64
	// DiskRead and DiskWrite are the bytes the process moved to and from
	// storage, below any cache.
	DiskRead, DiskWrite int64
	// EnergyOK says the kernel estimates this process's energy, and Energy is
	// that estimate in µJ.
	EnergyOK bool
	Energy   int64
}

// processCounters reads pid's accounting through the seam a test sets, or the
// platform's own.
func (r Reader) processCounters(pid int) (ProcessCounters, error) {
	if r.Process != nil {
		return r.Process(pid)
	}

	return readProcessCounters(pid)
}

// ProcessCounters reads pid's own accounting.
func (r Reader) ProcessCounters(pid int) (ProcessCounters, error) {
	if pid <= 0 {
		return ProcessCounters{}, fmt.Errorf("usage: pid %d names no process", pid)
	}

	return r.processCounters(pid)
}

// readProcess samples a job that is one process. Every group comes from one
// reading, so they agree with each other, and the reading is discarded unless
// it is of the process the target names.
//
// A TARGET WITH NO START READS NOTHING through the same comparison: a real
// process never has start zero, since processCountersOf refuses one.
func (s *Sample) readProcess(r Reader, t Target) {
	c, err := r.ProcessCounters(t.PID)
	if err != nil || c.Start != t.PIDStart {
		return
	}
	s.CPUUsage, s.CPUUser, s.CPUSys, s.CPUOK = c.UserMicros+c.SystemMicros, c.UserMicros, c.SystemMicros, true
	s.MemoryCurrent, s.MemoryPeak, s.MemoryOK = c.Footprint, c.PeakFootprint, true
	s.DiskRead, s.DiskWrite, s.IOOK = c.DiskRead, c.DiskWrite, true
	if c.EnergyOK {
		s.ProcessEnergy, s.ProcessEnergyOK = c.Energy, true
	}
}
