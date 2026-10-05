package ceph

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// The purge yields to the jobs on its host: before each deletion it reads how
// starved the host is for IO and, while that is past purgePressureLimit, waits
// purgeQuietPoll at a time, for at most purgeQuietMax, then deletes anyway.
//
// THE PURGE IS THE ONE STORAGE WORK NOTHING WAITS FOR, and it once took both OSDs
// of the reference deployment for hours while every job's IO queued behind it
// (#394, 2026-10-05: IO pressure `full` avg300 about 17%, both drives near 99%).
// Object-map makes each deletion cheap, but images created before it still
// delete slowly, and nothing about a backlog should ever outrank a running job.
// The bound is what keeps a host that is always busy from never purging: a
// deletion still runs at least every purgeQuietMax per worker, so the trash
// cannot grow without limit while the pool fills.
//
// `full avg10` is the share of the last ten seconds in which every task wanting
// IO on the host was stalled on it, read from the kernel's pressure-stall file.
// A guest's disk is a mapped RBD device on this host, so its IO waits count
// there whether the OSDs are local or not.
const (
	purgePressureLimit = 10.0
	purgeQuietPoll     = 15 * time.Second
	purgeQuietMax      = 2 * time.Minute
)

// hostIOPressurePath is the kernel's pressure-stall file for IO.
const hostIOPressurePath = "/proc/pressure/io"

// readHostIOPressure reports the host's IO `full avg10`, and false when it cannot
// be read: a kernel without pressure-stall information, or a host that is not
// Linux. Could-not-tell paces nothing, because the purge must still run where
// the signal does not exist.
func readHostIOPressure() (float64, bool) {
	file, err := os.Open(hostIOPressurePath)
	if err != nil {
		return 0, false
	}
	defer func() { _ = file.Close() }()

	return parseIOPressure(bufio.NewScanner(file))
}

// parseIOPressure reads the avg10 of the `full` line of a pressure-stall file.
func parseIOPressure(lines *bufio.Scanner) (float64, bool) {
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) == 0 || fields[0] != "full" {
			continue
		}
		for _, field := range fields[1:] {
			value, found := strings.CutPrefix(field, "avg10=")
			if !found {
				continue
			}
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || parsed < 0 {
				return 0, false
			}

			return parsed, true
		}
	}

	return 0, false
}

// waitForQuietIO holds a purge deletion while the host is starved for IO, up to
// purgeQuietMax; see purgePressureLimit.
func (c *Client) waitForQuietIO(ctx context.Context) {
	if c.ioPressure == nil {
		return
	}

	waited := time.Duration(0)
	for waited < purgeQuietMax {
		full, known := c.ioPressure()
		if !known || full < purgePressureLimit {
			return
		}
		if !c.pause(ctx, purgeQuietPoll) {
			return
		}
		waited += purgeQuietPoll
	}
}

// sleepFor waits d or until ctx ends, and reports whether it waited the whole time.
func sleepFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// The image import writes the guest image at importWriteRate, flushing every
// importFlushEvery bytes.
//
// AN UNPACED IMPORT IS AN IO STORM. The guest image is 80 GiB, written in full
// through a mapped device, and copied without a bound it went into the page cache
// as fast as the source could be read and was written back in bursts: the image
// refresh on 2026-10-04 peaked at 140 GB of memory and landed on a cluster the
// purge already held at saturation (#394). A refresh is background work with a
// week between runs; at 64 MiB/s this image takes about 21 minutes, and flushing
// every 256 MiB bounds what the kernel holds dirty at once.
const (
	importWriteRate  = 64 << 20
	importFlushEvery = 256 << 20
)

// importPace is how writeImage paces itself: bytes per second, bytes between
// flushes, and the clock and sleep it measures and waits with.
type importPace struct {
	rate       int64
	flushEvery int64
	now        func() time.Time
	sleep      func(time.Duration)
}

func productionImportPace() importPace {
	return importPace{rate: importWriteRate, flushEvery: importFlushEvery, now: time.Now, sleep: time.Sleep}
}
