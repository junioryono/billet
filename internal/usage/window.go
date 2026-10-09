package usage

import (
	"errors"
	"fmt"
	"math"
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
	points, fell := distinct(tl.Points)
	var covered int64
	var sums [6]int64
	for i := 1; i < len(points); i++ {
		p, next := points[i-1], points[i]
		length := next.OffsetMillis - p.OffsetMillis
		overlap := min(b, next.OffsetMillis) - max(a, p.OffsetMillis)
		if overlap <= 0 || next.AfterGap || fell[i] || fell[i-1] {
			continue
		}
		before, after := p.cumulative(), next.cumulative()
		var shares [6]int64
		fits := true
		for c := range shares {
			shares[c] = shareOf(after[c]-before[c], overlap, length)
			fits = fits && shares[c] <= math.MaxInt64-sums[c]
		}
		// A TOTAL THAT WOULD NOT FIT IS UNCOVERED, never wrapped: the increments
		// telescope only between resets, and the fall rule leaves counters on
		// both sides of a reset in the window.
		if !fits {
			continue
		}
		covered += overlap
		for c := range shares {
			sums[c] += shares[c]
		}
	}
	out.Covered = min(time.Duration(covered)*time.Millisecond, out.Span)
	out.CPUMicros, out.DiskRead, out.DiskWrite = sums[0], sums[1], sums[2]
	out.NetRx, out.NetTx, out.EnergyActive = sums[3], sums[4], sums[5]

	return out
}

// cumulative is the point's cumulative columns Window reads, in the order it
// sums them.
func (p Point) cumulative() [6]int64 {
	return [6]int64{p.CPUUsage, p.DiskRead, p.DiskWrite, p.NetRx, p.NetTx, p.EnergyActive}
}

// falls reports whether a cumulative column Window reads is lower at next
// than at p. The interval it falls across and the one after it are uncovered.
func falls(p, next Point) bool {
	before, after := p.cumulative(), next.cumulative()
	for c := range before {
		if after[c] < before[c] {
			return true
		}
	}

	return false
}

// shareOf is delta*part/whole rounded to the nearest unit, exact for every
// int64: the product is formed in 128 bits, and the quotient is at most delta
// because part is at most whole. delta is not negative and whole is positive.
func shareOf(delta, part, whole int64) int64 {
	hi, lo := bits.Mul64(uint64(delta), uint64(part))
	q, r := bits.Div64(hi, lo, uint64(whole))
	if r >= uint64(whole)-r {
		q++
	}

	return int64(q)
}

// distinct is the points with each run sharing one offset collapsed to its
// last, marked AfterGap if any of the run was, and for each the fall
// evidence of the interval that ends at it: whether a counter fell between any
// two consecutive samples from the one before that interval to the end of the
// run. Two samples a sampler took within one millisecond have no interval
// between them to split, so what the later one adds belongs to the interval
// before it, and so does a fall between them, which collapsing would hide.
func distinct(points []Point) ([]Point, []bool) {
	out := make([]Point, 0, len(points))
	fell := make([]bool, 0, len(points))
	for i, p := range points {
		dropped := i > 0 && falls(points[i-1], p)
		if n := len(out); n > 0 && out[n-1].OffsetMillis == p.OffsetMillis {
			p.AfterGap = p.AfterGap || out[n-1].AfterGap
			out[n-1] = p
			fell[n-1] = fell[n-1] || dropped
			continue
		}
		out = append(out, p)
		fell = append(fell, dropped)
	}

	return out, fell
}

// ErrNoClock says a stored series records no wall time for its first sample,
// so its points are offsets from a moment nothing kept and no window of wall
// time can be placed on them.
var ErrNoClock = errors.New("usage: the series records no start on the host's clock")

// TimelineOf reads a stored series as a timeline. A series of SeriesCodec,
// which a node or a control plane older than SeriesCodecClocked writes, is
// ErrNoClock.
func TimelineOf(codec int, data []byte) (Timeline, error) {
	switch codec {
	case SeriesCodecClocked:
		return DecodeSeriesAt(data)
	case SeriesCodec:
		if _, err := DecodeSeries(data); err != nil {
			return Timeline{}, err
		}

		return Timeline{}, ErrNoClock
	}

	return Timeline{}, fmt.Errorf("usage: series codec %d is not one this build reads", codec)
}
