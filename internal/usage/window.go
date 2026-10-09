package usage

import (
	"errors"
	"fmt"
	"math/bits"
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
	// series and in no uncovered interval.
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

// Window integrates the timeline over [from, to]. A window that ends before it
// begins has no length and reports nothing.
//
// THE RULE INSIDE ONE SAMPLE INTERVAL IS PROPORTIONAL SPLITTING AND NOTHING
// MORE: a cumulative column's increment between two samples is shared by time,
// rounded to the nearest unit, so a window covering a third of an interval is
// given a third of what that interval used. An interval is uncovered, never
// split, when the sampler marked the point that ends it AfterGap, or when a
// cumulative column falls across it or across the interval before it: a fall
// is a counter that was reset or misread, and the rise after a misread is its
// catch-up rather than usage. The memory level is never interpolated.
//
// GAPS ARE THE SAMPLER'S WORD, NEVER INFERRED FROM SPACING: a series the
// sampler has halved more than once holds wide old intervals beside narrow new
// ones, and any rule read from the spacing alone would call the old ones gaps.
func (tl Timeline) Window(from, to time.Time) WindowUsage {
	out := WindowUsage{Span: max(to.Sub(from), 0)}
	if out.Span == 0 || len(tl.Points) == 0 {
		return out
	}
	a := from.Sub(tl.First).Milliseconds()
	b := to.Sub(tl.First).Milliseconds()

	for _, p := range tl.Points {
		if p.OffsetMillis >= a && p.OffsetMillis <= b {
			out.Samples++
			out.MemoryPeak = max(out.MemoryPeak, p.MemoryCurrent)
		}
	}
	points := distinct(tl.Points)
	var covered int64
	for i := 1; i < len(points); i++ {
		p, next := points[i-1], points[i]
		length := next.OffsetMillis - p.OffsetMillis
		overlap := min(b, next.OffsetMillis) - max(a, p.OffsetMillis)
		if overlap <= 0 || next.AfterGap || falls(p, next) || i > 1 && falls(points[i-2], p) {
			continue
		}
		covered += overlap
		out.CPUMicros += shareOf(next.CPUUsage-p.CPUUsage, overlap, length)
		out.DiskRead += shareOf(next.DiskRead-p.DiskRead, overlap, length)
		out.DiskWrite += shareOf(next.DiskWrite-p.DiskWrite, overlap, length)
		out.NetRx += shareOf(next.NetRx-p.NetRx, overlap, length)
		out.NetTx += shareOf(next.NetTx-p.NetTx, overlap, length)
		out.EnergyActive += shareOf(next.EnergyActive-p.EnergyActive, overlap, length)
	}
	out.Covered = min(time.Duration(covered)*time.Millisecond, out.Span)

	return out
}

// falls reports whether a cumulative column Window reads is lower at next
// than at p.
func falls(p, next Point) bool {
	return next.CPUUsage < p.CPUUsage || next.DiskRead < p.DiskRead || next.DiskWrite < p.DiskWrite ||
		next.NetRx < p.NetRx || next.NetTx < p.NetTx || next.EnergyActive < p.EnergyActive
}

// shareOf is delta*part/whole rounded to the nearest unit, exact for every
// int64: the product is formed in 128 bits, and the quotient is at most delta
// because part is at most whole. delta is not negative and whole is positive.
//
// NO SUM OF SHARES OVERFLOWS EITHER: each is at most its interval's increment,
// and the increments of one column telescope to its last value less its first.
func shareOf(delta, part, whole int64) int64 {
	hi, lo := bits.Mul64(uint64(delta), uint64(part))
	q, r := bits.Div64(hi, lo, uint64(whole))
	if r >= uint64(whole)-r {
		q++
	}

	return int64(q)
}

// distinct is the points with each run sharing one offset collapsed to its
// last, marked AfterGap if any of the run was. Two samples a sampler took
// within one millisecond have no interval between them to split, so what the
// later one adds belongs to the interval before it.
func distinct(points []Point) []Point {
	out := make([]Point, 0, len(points))
	for _, p := range points {
		if n := len(out); n > 0 && out[n-1].OffsetMillis == p.OffsetMillis {
			p.AfterGap = p.AfterGap || out[n-1].AfterGap
			out[n-1] = p
			continue
		}
		out = append(out, p)
	}

	return out
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
