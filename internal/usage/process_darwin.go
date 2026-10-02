//go:build darwin

package usage

import (
	"bytes"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// rusageInfoV6 is struct rusage_info_v6 from <sys/resource.h>, field for
// field; only some are read. Its size is checked in a test against the 464
// bytes the macOS 26 SDK declares.
type rusageInfoV6 struct {
	UUID                [16]byte
	UserTime            uint64
	SystemTime          uint64
	PkgIdleWkups        uint64
	InterruptWkups      uint64
	Pageins             uint64
	WiredSize           uint64
	ResidentSize        uint64
	PhysFootprint       uint64
	ProcStartAbstime    uint64
	ProcExitAbstime     uint64
	ChildUserTime       uint64
	ChildSystemTime     uint64
	ChildPkgIdleWkups   uint64
	ChildInterruptWkups uint64
	ChildPageins        uint64
	ChildElapsedAbstime uint64
	DiskioBytesread     uint64
	DiskioByteswritten  uint64
	CPUTimeQOS          [7]uint64
	BilledSystemTime    uint64
	ServicedSystemTime  uint64
	LogicalWrites       uint64
	LifetimeMaxPhysFoot uint64
	Instructions        uint64
	Cycles              uint64
	BilledEnergy        uint64
	ServicedEnergy      uint64
	IntervalMaxPhysFoot uint64
	RunnableTime        uint64
	Flags               uint64
	UserPtime           uint64
	SystemPtime         uint64
	Pinstructions       uint64
	Pcycles             uint64
	EnergyNJ            uint64
	PenergyNJ           uint64
	SecureTime          uint64
	SecurePtime         uint64
	NeuralFootprint     uint64
	LifetimeMaxNeural   uint64
	IntervalMaxNeural   uint64
	Reserved            [9]uint64
}

// The proc_info(2) call that proc_pid_rusage(3) wraps: PROC_INFO_CALL_PIDRUSAGE
// with flavor RUSAGE_INFO_V6. Called directly because billet links no C.
const (
	procInfoCallPIDRusage = 9
	rusageInfoV6Flavor    = 6
)

// readProcessCounters reads pid's rusage_info_v6. It answers only for a
// process this user may inspect, which a VM the node started is.
//
// CPU TIMES ARE IN MACH ABSOLUTE UNITS, not nanoseconds: on Apple silicon the
// timebase is 24 MHz (hw.tbfrequency), so a reading taken as nanoseconds is 41.7
// times too small. Measured on macOS 26 and 27 (2026-09-30): a VM process's
// ticks over hw.tbfrequency matched `ps -o time` to the hundredth of a second.
func readProcessCounters(pid int) (ProcessCounters, error) {
	var ri rusageInfoV6
	//nolint:staticcheck // x/sys has no libSystem wrapper for proc_pid_rusage, and cgo would end the static binary.
	if _, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDRusage, uintptr(pid),
		rusageInfoV6Flavor, 0, uintptr(unsafe.Pointer(&ri)), 0); errno != 0 {
		return ProcessCounters{}, fmt.Errorf("usage: read the accounting of pid %d: %w", pid, errno)
	}
	freq, err := unix.SysctlUint64("hw.tbfrequency")
	if err != nil {
		return ProcessCounters{}, fmt.Errorf("usage: read the timebase: %w", err)
	}

	return processCountersOf(ri, freq)
}

// processCountersOf converts a reading into billet's units.
func processCountersOf(ri rusageInfoV6, tbFrequency uint64) (ProcessCounters, error) {
	if tbFrequency == 0 || ri.ProcStartAbstime == 0 {
		return ProcessCounters{}, fmt.Errorf("usage: an accounting record with timebase %d and start %d "+
			"is not one billet can read", tbFrequency, ri.ProcStartAbstime)
	}
	c := ProcessCounters{
		Start:      ri.ProcStartAbstime,
		UserMicros: ticksAt(ri.UserTime, tbFrequency), SystemMicros: ticksAt(ri.SystemTime, tbFrequency),
		Footprint: clamp(ri.PhysFootprint), PeakFootprint: clamp(max(ri.LifetimeMaxPhysFoot, ri.PhysFootprint)),
		DiskRead: clamp(ri.DiskioBytesread), DiskWrite: clamp(ri.DiskioByteswritten),
		// ZERO IS NOT AN ESTIMATE: a kernel that does not keep one leaves the field
		// zero, and a process that ran at all used some energy.
		EnergyOK: ri.EnergyNJ > 0, Energy: clamp(ri.EnergyNJ / 1000),
	}

	return c, nil
}

// The proc_info(2) call that proc_pidpath(3) wraps, and the buffer size its
// header declares (PROC_PIDPATHINFO_MAXSIZE).
const (
	procInfoCallPIDInfo   = 2
	procPIDPathInfoFlavor = 11
	procPIDPathInfoSize   = 4 * 1024
)

// ProcessPath is the executable pid is running.
func ProcessPath(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("usage: pid %d names no process", pid)
	}
	buf := make([]byte, procPIDPathInfoSize)
	//nolint:staticcheck // x/sys has no libSystem wrapper for proc_pidpath, and cgo would end the static binary.
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid),
		procPIDPathInfoFlavor, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return "", fmt.Errorf("usage: read the executable of pid %d: %w", pid, errno)
	}
	// THE CALL ANSWERS ZERO, NOT A LENGTH: proc_pidpath(3) measures the
	// NUL-terminated string itself, and so does this.
	n := bytes.IndexByte(buf, 0)
	if n < 0 {
		return "", fmt.Errorf("usage: pid %d's executable came back unterminated", pid)
	}

	return string(buf[:n]), nil
}

// ticksAt converts mach absolute ticks at freq Hz to µs, without overflowing for
// any duration a process can run.
func ticksAt(ticks, freq uint64) int64 {
	return clamp(ticks/freq*1_000_000 + ticks%freq*1_000_000/freq)
}

// clamp keeps a counter inside int64, which no real reading leaves.
func clamp(v uint64) int64 {
	if v > 1<<63-1 {
		return 1<<63 - 1
	}

	return int64(v)
}
