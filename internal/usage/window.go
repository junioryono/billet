package usage

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// Timeline is a series placed on wall time: its points, and the wall time of
// the first one on the host's clock. Every later point is an offset from that
// first one, so a window of wall time from another clock lines up with the
// series only as well as the two clocks agree.
type Timeline struct {
	First  time.Time
	Points []Point
}

// WindowUsage is what a timeline says about one window of wall time.
//
// EVERY QUANTITY IS OVER Covered, NOT Span. A window the series does not
// fully cover (it began before the first sample, ended after the last, or
// spans a gap) reports what the covered part used and how much was covered;
// a reader that printed it as the window's total would print a smaller number
// than the job used.
type WindowUsage struct {
	// Span is the window's length, and Covered how much of it lies inside the
	// series and outside every gap.
	Span, Covered time.Duration
	// Samples counts the samples taken inside the window. With none, every
	// quantity is a share of the one interval around the window, split by time.
	Samples int
	// CPUMicros, the byte counts and EnergyActive are increments of the
	// series' cumulative columns over the covered time.
	CPUMicros           int64
	DiskRead, DiskWrite int64
	NetRx, NetTx        int64
	EnergyActive        int64
	// MemoryPeak is the highest memory level sampled inside the window, zero
	// when Samples is zero: a level between two samples is not known.
	MemoryPeak int64
}

// Complete reports whether the series covers the whole of a window with a
// length.
func (w WindowUsage) Complete() bool { return w.Span > 0 && w.Covered == w.Span }

// GapFactor is how many times the series' usual spacing two consecutive
// samples may lie apart before the interval between them is a gap: time the
// sampler did not see, whose increment is known only as a total. The usual
// spacing is the median, because a downsampled series spaces its points by the
// stride it kept and a stalled sampler is the exception.
const GapFactor = 2

// Window integrates the timeline over [from, to]. A window that ends before it
// begins has no length and reports nothing.
//
// THE RULE INSIDE ONE SAMPLE INTERVAL IS PROPORTIONAL SPLITTING AND NOTHING
// MORE: a cumulative column's increment between two samples is shared by time,
// so a window covering a third of an interval is given a third of what that
// interval used. Nothing is interpolated across a gap, which is uncovered
// instead, and the memory level is never interpolated at all.
func (tl Timeline) Window(from, to time.Time) WindowUsage {
	out := WindowUsage{Span: max(to.Sub(from), 0)}
	if out.Span == 0 || len(tl.Points) == 0 {
		return out
	}
	a := from.Sub(tl.First).Milliseconds()
	b := to.Sub(tl.First).Milliseconds()
	limit := GapFactor * tl.usualSpacing()

	var covered int64
	var cpu, read, write, rx, tx, energy float64
	for i, p := range tl.Points {
		if p.OffsetMillis >= a && p.OffsetMillis <= b {
			out.Samples++
			out.MemoryPeak = max(out.MemoryPeak, p.MemoryCurrent)
		}
		if i == len(tl.Points)-1 {
			break
		}
		next := tl.Points[i+1]
		length := next.OffsetMillis - p.OffsetMillis
		if length <= 0 || length > limit {
			continue
		}
		overlap := min(b, next.OffsetMillis) - max(a, p.OffsetMillis)
		if overlap <= 0 {
			continue
		}
		covered += overlap
		share := float64(overlap) / float64(length)
		cpu += share * float64(next.CPUUsage-p.CPUUsage)
		read += share * float64(next.DiskRead-p.DiskRead)
		write += share * float64(next.DiskWrite-p.DiskWrite)
		rx += share * float64(next.NetRx-p.NetRx)
		tx += share * float64(next.NetTx-p.NetTx)
		energy += share * float64(next.EnergyActive-p.EnergyActive)
	}
	out.Covered = min(time.Duration(covered)*time.Millisecond, out.Span)
	round := func(v float64) int64 { return int64(math.Round(v)) }
	out.CPUMicros, out.DiskRead, out.DiskWrite = round(cpu), round(read), round(write)
	out.NetRx, out.NetTx, out.EnergyActive = round(rx), round(tx), round(energy)

	return out
}

// usualSpacing is the median distance between consecutive samples, ignoring
// two taken at the same offset.
func (tl Timeline) usualSpacing() int64 {
	var spacings []int64
	for i := 1; i < len(tl.Points); i++ {
		if d := tl.Points[i].OffsetMillis - tl.Points[i-1].OffsetMillis; d > 0 {
			spacings = append(spacings, d)
		}
	}
	if len(spacings) == 0 {
		return 0
	}
	slices.Sort(spacings)

	return spacings[len(spacings)/2]
}

// ErrNoClock says a stored series records no wall time for its first sample,
// so its points are offsets from a moment nothing kept and no window of wall
// time can be placed on them.
var ErrNoClock = errors.New("usage: the series records no start on the host's clock")

// TimelineOf reads a stored series as a timeline. A series of SeriesCodec is
// ErrNoClock.
func TimelineOf(codec int, data []byte) (Timeline, error) {
	if codec == SeriesCodec {
		if _, err := DecodeSeries(data); err != nil {
			return Timeline{}, err
		}

		return Timeline{}, ErrNoClock
	}

	return Timeline{}, fmt.Errorf("usage: series codec %d is not one this build reads", codec)
}
