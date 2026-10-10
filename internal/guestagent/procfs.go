package guestagent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/guestreport"
)

// userHZ is the unit of every CPU figure /proc reports: Linux's USER_HZ, which is
// 100 on every architecture whatever the kernel's own tick rate.
const userHZ = 100

// The bytes one read of a /proc file may take. The totals' files grow with the
// guest's CPUs, disks and interfaces, which a job can add; a process's files do not.
const (
	totalsFileLimit  = 1 << 20
	processFileLimit = 16 << 10
)

// errCouldNotTell is a figure the agent could not read. A Sample or a ProcessSample
// carries no way to say one of its numbers is unknown, so a reading that hits this
// is not sent at all: a zero would be a claim.
var errCouldNotTell = errors.New("could not tell")

// errGone is a process that exited while the agent read it.
var errGone = errors.New("the process exited")

// readProc is one /proc file, at most limit bytes. A file longer than that is
// could-not-tell, never cut.
func readProc(proc fs.FS, name string, limit int64) ([]byte, error) {
	f, err := proc.Open(name)
	if err != nil {
		return nil, procError(err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, procError(err)
	}

	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: longer than %d bytes", errCouldNotTell, limit)
	}

	return data, nil
}

// procError sorts a failed read: a file that is not there, or a process the kernel
// no longer has (ESRCH from a read of one that exited after the open), is errGone;
// anything else, a refused read included, is could-not-tell.
func procError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return errGone
	}

	return fmt.Errorf("%w: %w", errCouldNotTell, err)
}

// cpuTicks is /proc/stat's cpu line as a Sample carries it: busy is every column but
// idle and iowait, total every column that counts time. guest and guest_nice are
// already inside user and nice, so adding them would count that time twice.
func cpuTicks(stat []byte) (int64, int64, error) {
	line, ok := lineWithPrefix(stat, "cpu ")
	if !ok {
		return 0, 0, fmt.Errorf("%w: /proc/stat has no cpu line", errCouldNotTell)
	}

	fields := strings.Fields(line)[1:]
	// user nice system idle iowait irq softirq steal: what a 6.1 kernel writes,
	// and every kernel since 2.6.11.
	if len(fields) < 8 {
		return 0, 0, fmt.Errorf("%w: /proc/stat's cpu line has %d columns", errCouldNotTell, len(fields))
	}

	var busy, total int64

	for i, f := range fields[:8] {
		v, err := counter(f)
		if err != nil {
			return 0, 0, err
		}

		total += v
		if i != 3 && i != 4 {
			busy += v
		}
	}

	return busy, total, nil
}

// memory is /proc/meminfo's three levels, in bytes: MemTotal less MemAvailable,
// MemAvailable, and Cached.
func memory(meminfo []byte) (int64, int64, int64, error) {
	values := map[string]int64{}

	for line := range strings.Lines(string(meminfo)) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || (key != "MemTotal" && key != "MemAvailable" && key != "Cached") {
			continue
		}

		v, err := kilobytes(rest)
		if err != nil {
			return 0, 0, 0, err
		}

		values[key] = v
	}

	total, okTotal := values["MemTotal"]
	available, okAvailable := values["MemAvailable"]
	cached, okCached := values["Cached"]

	if !okTotal || !okAvailable || !okCached {
		return 0, 0, 0, fmt.Errorf("%w: /proc/meminfo lacks MemTotal, MemAvailable or Cached", errCouldNotTell)
	}

	if available > total {
		return 0, 0, 0, fmt.Errorf("%w: /proc/meminfo has more available than in total", errCouldNotTell)
	}

	return total - available, available, cached, nil
}

// sectorBytes is the unit of /proc/diskstats' sector counts, whatever the device's
// own sector size.
const sectorBytes = 512

// diskBytes is the bytes device has read and written, from /proc/diskstats.
func diskBytes(diskstats []byte, device string) (int64, int64, error) {
	for line := range strings.Lines(string(diskstats)) {
		fields := strings.Fields(line)
		// major minor name reads merged sectors_read ms writes merged sectors_written
		if len(fields) < 10 || fields[2] != device {
			continue
		}

		read, err := bytesOf(fields[5], sectorBytes)
		if err != nil {
			return 0, 0, err
		}

		written, err := bytesOf(fields[9], sectorBytes)
		if err != nil {
			return 0, 0, err
		}

		return read, written, nil
	}

	return 0, 0, fmt.Errorf("%w: /proc/diskstats has no %s", errCouldNotTell, device)
}

// netBytes is the bytes iface has received and sent, from /proc/net/dev.
func netBytes(dev []byte, iface string) (int64, int64, error) {
	for line := range strings.Lines(string(dev)) {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != iface {
			continue
		}

		fields := strings.Fields(rest)
		// eight receive columns, bytes first, then eight transmit columns.
		if len(fields) < 16 {
			return 0, 0, fmt.Errorf("%w: /proc/net/dev's %s line has %d columns", errCouldNotTell, iface, len(fields))
		}

		rx, err := counter(fields[0])
		if err != nil {
			return 0, 0, err
		}

		tx, err := counter(fields[8])
		if err != nil {
			return 0, 0, err
		}

		return rx, tx, nil
	}

	return 0, 0, fmt.Errorf("%w: /proc/net/dev has no %s", errCouldNotTell, iface)
}

// pfKthread is the kernel's PF_KTHREAD flag in /proc/<pid>/stat's flags field.
const pfKthread = 0x00200000

// pidStat is what the agent reads from /proc/<pid>/stat.
type pidStat struct {
	comm string
	// state is the one-letter state; Z and X are a process that has exited and
	// not been reaped, which has no memory.
	state byte
	flags uint64
	// cpu is utime plus stime, in ticks: what the process's threads used,
	// those that exited included, and not its reaped children.
	cpu int64
	// start is the process's start in ticks since boot, which tells a reused pid
	// from the process that had it before.
	start uint64
}

// parsePIDStat reads /proc/<pid>/stat. The comm runs from the first '(' to the
// LAST ')', because a comm can hold spaces and parentheses of its own and every
// field after it is a number or a state letter.
func parsePIDStat(data []byte) (pidStat, error) {
	open := bytes.IndexByte(data, '(')
	closing := bytes.LastIndexByte(data, ')')

	if open < 0 || closing < open {
		return pidStat{}, fmt.Errorf("%w: a stat line without its comm", errCouldNotTell)
	}

	fields := strings.Fields(string(data[closing+1:]))
	// state(3) ... utime(14) stime(15) ... starttime(22): index i is field i+3.
	if len(fields) < 20 || len(fields[0]) != 1 {
		return pidStat{}, fmt.Errorf("%w: a stat line with %d fields after its comm", errCouldNotTell, len(fields))
	}

	flags, err := strconv.ParseUint(fields[6], 10, 64)
	if err != nil {
		return pidStat{}, fmt.Errorf("%w: a stat line's flags", errCouldNotTell)
	}

	utime, err := counter(fields[11])
	if err != nil {
		return pidStat{}, err
	}

	stime, err := counter(fields[12])
	if err != nil {
		return pidStat{}, err
	}

	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return pidStat{}, fmt.Errorf("%w: a stat line's start time", errCouldNotTell)
	}

	return pidStat{
		comm:  string(data[open+1 : closing]),
		state: fields[0][0],
		flags: flags,
		cpu:   utime + stime,
		start: start,
	}, nil
}

// residentBytes is /proc/<pid>/status's VmRSS. A kernel thread and a process that
// has exited have no VmRSS line and no memory of their own, which is zero; any
// other process without one is could-not-tell.
func residentBytes(status []byte, st pidStat) (int64, error) {
	for line := range strings.Lines(string(status)) {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			return kilobytes(rest)
		}
	}

	if st.flags&pfKthread != 0 || st.state == 'Z' || st.state == 'X' {
		return 0, nil
	}

	return 0, fmt.Errorf("%w: a process's status has no VmRSS", errCouldNotTell)
}

// ioBytes is /proc/<pid>/io's read_bytes and write_bytes: what the process caused
// to be read from and written to storage, not what it passed through read(2).
func ioBytes(data []byte) (int64, int64, error) {
	var read, written int64

	found := 0

	for line := range strings.Lines(string(data)) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || (key != "read_bytes" && key != "write_bytes") {
			continue
		}

		v, err := counter(strings.TrimSpace(rest))
		if err != nil {
			return 0, 0, err
		}

		if key == "read_bytes" {
			read = v
		} else {
			written = v
		}

		found++
	}

	if found != 2 {
		return 0, 0, fmt.Errorf("%w: a process's io lacks read_bytes or write_bytes", errCouldNotTell)
	}

	return read, written, nil
}

// counter is a non-negative decimal no larger than guestreport.MaxValue, which is
// the largest a report carries.
func counter(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > guestreport.MaxValue {
		return 0, fmt.Errorf("%w: a figure that is not a count", errCouldNotTell)
	}

	return v, nil
}

// bytesOf is a count of units, in bytes.
func bytesOf(s string, unit int64) (int64, error) {
	v, err := counter(s)
	if err != nil {
		return 0, err
	}

	if v > guestreport.MaxValue/unit {
		return 0, fmt.Errorf("%w: a figure past what a report carries", errCouldNotTell)
	}

	return v * unit, nil
}

// kilobytes is a "<n> kB" value of meminfo or status, in bytes.
func kilobytes(s string) (int64, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 || fields[1] != "kB" {
		return 0, fmt.Errorf("%w: a size that is not in kB", errCouldNotTell)
	}

	return bytesOf(fields[0], 1024)
}

// lineWithPrefix is the first line of data that begins with prefix.
func lineWithPrefix(data []byte, prefix string) (string, bool) {
	for line := range strings.Lines(string(data)) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line), true
		}
	}

	return "", false
}
