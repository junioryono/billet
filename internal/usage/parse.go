// Package usage measures what a job does to the host it runs on, from the
// host's side: the job's cgroup, its network device, the VMM's threads and the
// package energy counter. Nothing here is read from inside a guest, which a job
// running as root there could forge.
//
// It is a leaf: it reads files under the roots it is given and imports nothing
// of billet's, so every parser is testable against captured bytes.
package usage

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// userHZ is the unit of utime and stime in /proc/<pid>/task/<tid>/stat and of
// the /proc/stat counters. It is the kernel's USER_HZ, 100 on every Linux
// architecture billet builds for (amd64 and arm64), and not the scheduler's HZ.
const userHZ = 100

// ticksToMicros converts USER_HZ ticks to microseconds.
func ticksToMicros(ticks int64) int64 { return ticks * (1_000_000 / userHZ) }

// parseFlatKeyed reads a cgroup flat-keyed file ("key value" per line), such as
// cpu.stat, memory.stat or memory.events. Keys are read by name, never by
// position, because the kernel adds keys between versions.
func parseFlatKeyed(data string) (map[string]int64, error) {
	out := map[string]int64{}
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("usage: %q is not a key and a value", line)
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("usage: %s: %w", fields[0], err)
		}
		out[fields[0]] = v
	}

	return out, nil
}

// parseSingle reads a file holding one integer, such as memory.current.
func parseSingle(data string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(data), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("usage: %q is not an integer: %w", strings.TrimSpace(data), err)
	}

	return v, nil
}

// parseIOStat sums rbytes and wbytes over every device in a cgroup's io.stat
// ("MAJ:MIN rbytes=N wbytes=N rios=N wios=N dbytes=N dios=N" per line).
func parseIOStat(data string) (int64, int64, error) {
	var readBytes, writeBytes int64
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return 0, 0, fmt.Errorf("usage: io.stat field %q has no value", field)
			}
			if key != "rbytes" && key != "wbytes" {
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("usage: io.stat %s: %w", key, err)
			}
			if key == "rbytes" {
				readBytes += n
			} else {
				writeBytes += n
			}
		}
	}

	return readBytes, writeBytes, nil
}

// parsePressure reads the total stall time, in microseconds, from a PSI file
// ("some avg10=0.00 avg60=0.00 avg300=0.00 total=N", and a "full" line). A
// file with no full line (cpu.pressure on older kernels) reports full as zero.
func parsePressure(data string) (int64, int64, error) {
	var some, full int64
	sawSome := false
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var total int64 = -1
		for _, field := range fields[1:] {
			if value, ok := strings.CutPrefix(field, "total="); ok {
				var err error
				if total, err = strconv.ParseInt(value, 10, 64); err != nil {
					return 0, 0, fmt.Errorf("usage: pressure total: %w", err)
				}
			}
		}
		if total < 0 {
			return 0, 0, fmt.Errorf("usage: pressure line %q has no total", line)
		}
		switch fields[0] {
		case "some":
			some, sawSome = total, true
		case "full":
			full = total
		}
	}
	if !sawSome {
		return 0, 0, errors.New("usage: pressure file has no some line")
	}

	return some, full, nil
}

// parseTaskStat reads a thread's name and its user and system time in USER_HZ
// ticks from /proc/<pid>/task/<tid>/stat.
//
// THE NAME IS BETWEEN THE FIRST "(" AND THE LAST ")", because a thread name
// may itself contain spaces and parentheses ("fc_vcpu 0"); splitting the whole
// line on spaces would shift every field after it.
func parseTaskStat(data string) (string, int64, int64, error) {
	open := strings.IndexByte(data, '(')
	closing := strings.LastIndexByte(data, ')')
	if open < 0 || closing < open {
		return "", 0, 0, errors.New("usage: task stat has no (comm)")
	}
	comm := data[open+1 : closing]
	// After ")" the fields resume at field 3 (state); utime and stime are
	// fields 14 and 15, so indexes 11 and 12 here.
	rest := strings.Fields(data[closing+1:])
	if len(rest) < 13 {
		return "", 0, 0, fmt.Errorf("usage: task stat has %d fields after comm, want at least 13", len(rest))
	}
	utime, err := strconv.ParseInt(rest[11], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("usage: task utime: %w", err)
	}
	stime, err := strconv.ParseInt(rest[12], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("usage: task stime: %w", err)
	}

	return comm, utime, stime, nil
}

// parseHostCPU reads the aggregate "cpu" line of /proc/stat into busy and
// total USER_HZ ticks, and counts the per-CPU lines.
//
// BUSY EXCLUDES idle AND iowait. guest and guest_nice are already inside user
// and nice, so they are not added again.
func parseHostCPU(data string) (int64, int64, int, error) {
	var busy, total int64
	cpus := 0
	sawAggregate := false
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			cpus++
			continue
		}
		if len(fields) < 8 {
			return 0, 0, 0, fmt.Errorf("usage: /proc/stat cpu line has %d fields", len(fields))
		}
		values := make([]int64, 0, 8)
		for _, field := range fields[1:min(len(fields), 9)] {
			v, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("usage: /proc/stat cpu: %w", err)
			}
			values = append(values, v)
		}
		// user nice system idle iowait irq softirq steal
		for i, v := range values {
			total += v
			if i != 3 && i != 4 {
				busy += v
			}
		}
		sawAggregate = true
	}
	if !sawAggregate {
		return 0, 0, 0, errors.New("usage: /proc/stat has no aggregate cpu line")
	}

	return busy, total, cpus, nil
}

// energyDelta is the energy between two readings of a RAPL counter that wraps
// at maxRange, in microjoules. It assumes at most one wrap between readings,
// which the sampler guarantees by refusing a gap longer than the wrap period.
func energyDelta(prev, cur, maxRange int64) int64 {
	if cur >= prev {
		return cur - prev
	}

	return maxRange - prev + cur
}
