package ceph

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The purge yields to the jobs on its host: before each deletion it reads how
// starved the host is for IO and, while that is past purgePressureLimit, waits
// purgeQuietPoll at a time, for at most purgeQuietMax, then deletes anyway; and a
// whole pass yields for at most purgePassYieldMax, shared by its workers, after
// which it deletes without asking.
//
// THE PURGE IS THE ONE STORAGE WORK NOTHING WAITS FOR, and it once took both OSDs
// of the reference deployment for hours while every job's IO queued behind it
// (#394, 2026-10-05: IO pressure `full` avg300 about 17%, both drives near 99%).
// Object-map makes each deletion cheap, but images created before it still
// delete slowly, and nothing about a backlog should ever outrank a running job.
// The bounds are what keep a host that is always busy from never purging: per
// deletion, so one starved moment cannot hold the pass, and per pass, so a large
// backlog on a host that never quietens is not drained at a few deletions a
// minute while jobs discard ten and the pool fills.
//
// `full avg10` is the share of the last ten seconds in which every task wanting
// IO on the host was stalled on it, read from the kernel's pressure-stall file.
// A guest's disk is a mapped RBD device on this host, so its IO waits count
// there whether the OSDs are local or not.
const (
	purgePressureLimit = 10.0
	purgeQuietPoll     = 15 * time.Second
	purgeQuietMax      = 2 * time.Minute
	purgePassYieldMax  = 10 * time.Minute
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

// yieldBudget is how long one purge pass may still spend yielding, shared by its
// workers.
type yieldBudget struct{ remaining atomic.Int64 }

func newYieldBudget(d time.Duration) *yieldBudget {
	b := &yieldBudget{}
	b.remaining.Store(int64(d))

	return b
}

// take claims d of the budget, and reports false once it is spent.
func (b *yieldBudget) take(d time.Duration) bool {
	return b.remaining.Add(-int64(d)) >= 0
}

// waitForQuietIO holds a purge deletion while the host is starved for IO, up to
// purgeQuietMax and while the pass's budget lasts; see purgePressureLimit.
func (c *Client) waitForQuietIO(ctx context.Context, budget *yieldBudget) {
	if c.ioPressure == nil {
		return
	}

	waited := time.Duration(0)
	for waited < purgeQuietMax {
		full, known := c.ioPressure()
		if !known || full < purgePressureLimit || !budget.take(purgeQuietPoll) {
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
// purge already held at saturation (#394). At 128 MiB/s this image takes about
// eleven minutes, which an upgrade that must pull one can still afford, and every
// replica of a write is well inside what an OSD drive sustains once the purge is
// not deleting objects nobody wrote; flushing every 256 MiB bounds what the kernel
// holds dirty at once.
const (
	importWriteRate  = 128 << 20
	importFlushEvery = 256 << 20
	importChunk      = 4 << 20
)

// importPace is how pacedCopy paces itself: bytes per second, bytes between
// flushes, bytes per write, and the clock and sleep it measures and waits with.
type importPace struct {
	rate       int64
	flushEvery int64
	chunk      int
	now        func() time.Time
	sleep      func(time.Duration)
}

func productionImportPace() importPace {
	return importPace{rate: importWriteRate, flushEvery: importFlushEvery, chunk: importChunk,
		now: time.Now, sleep: time.Sleep}
}

// syncWriter is what pacedCopy writes to: the mapped device, or a test's
// recorder.
type syncWriter interface {
	io.Writer
	Sync() error
}

// pacedCopy copies src to dst no faster than pace.rate, syncing dst every
// pace.flushEvery bytes so the kernel never holds more than that dirty.
//
// A ROLLING DEADLINE, NOT AN AVERAGE. Each chunk is due chunk/rate after the
// later of the previous chunk's deadline and the moment it starts, so time lost
// to a stalled read, write or sync is not banked: an average over the whole copy
// would let the chunks after a stall run unpaced until they had made it up,
// which is a burst at exactly the moment a saturated cluster recovers.
func pacedCopy(dst syncWriter, src io.Reader, pace importPace) (int64, error) {
	chunk := pace.chunk
	if chunk <= 0 {
		chunk = importChunk
	}
	buf := make([]byte, chunk)
	next := pace.now()

	var written, sinceFlush int64

	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if start := pace.now(); next.Before(start) {
				next = start
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return written, err
			}
			written += int64(n)
			sinceFlush += int64(n)

			if sinceFlush >= pace.flushEvery {
				if err := dst.Sync(); err != nil {
					return written, err
				}
				sinceFlush = 0
			}

			next = next.Add(time.Duration(float64(n) / float64(pace.rate) * float64(time.Second)))
			if wait := next.Sub(pace.now()); wait > 0 {
				pace.sleep(wait)
			}
		}

		switch {
		case errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF):
			return written, nil
		case readErr != nil:
			return written, readErr
		}
	}
}
