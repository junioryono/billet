package usage

import (
	"math"
	"testing"
	"time"
)

// first is the fixture series' first sample on the host's clock.
var first = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// at is a wall time this many milliseconds after the first sample.
func at(ms int64) time.Time { return first.Add(time.Duration(ms) * time.Millisecond) }

// steadySeries samples every second at the given offsets (in seconds): 0.1s
// of CPU, 1 KiB read, 2 KiB written, 3 KiB received, 4 KiB sent and 5 J per
// second, and a memory level of 100 MiB plus the second it was sampled at.
func steadySeries(seconds ...int64) Timeline {
	tl := Timeline{First: first}
	for _, s := range seconds {
		tl.Points = append(tl.Points, Point{
			OffsetMillis: s * 1000, CPUUsage: s * 100_000, DiskRead: s * 1024, DiskWrite: s * 2048,
			NetRx: s * 3072, NetTx: s * 4096, EnergyActive: s * 5_000_000,
			MemoryCurrent: 100<<20 + s,
		})
	}

	return tl
}

func secondsTo(from, to int64) []int64 {
	var out []int64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}

	return out
}

// usedPerSecond is what steadySeries uses over covered seconds.
func usedPerSecond(seconds float64) WindowUsage {
	return WindowUsage{
		CPUMicros: int64(seconds * 100_000), DiskRead: int64(seconds * 1024),
		DiskWrite: int64(seconds * 2048), NetRx: int64(seconds * 3072), NetTx: int64(seconds * 4096),
		EnergyActive: int64(seconds * 5_000_000),
	}
}

func sameIncrements(t *testing.T, name string, got, want WindowUsage) {
	t.Helper()

	if got.CPUMicros != want.CPUMicros || got.DiskRead != want.DiskRead || got.DiskWrite != want.DiskWrite ||
		got.NetRx != want.NetRx || got.NetTx != want.NetTx || got.EnergyActive != want.EnergyActive {
		t.Errorf("%s: increments = %+v, want %+v", name, got, want)
	}
}

// A WINDOW THE SERIES COVERS IS COMPLETE, and gets exactly what it used.
func TestAWindowInsideTheSeriesIsComplete(t *testing.T) {
	t.Parallel()

	w := steadySeries(secondsTo(0, 10)...).Window(at(2000), at(5000))
	if !w.Complete() || w.Span != 3*time.Second || w.Covered != 3*time.Second {
		t.Errorf("span %s covered %s, want 3s of 3s", w.Span, w.Covered)
	}
	sameIncrements(t, "2s to 5s", w, usedPerSecond(3))
	if w.Samples != 4 || w.MemoryPeak != 100<<20+5 {
		t.Errorf("samples %d peak %d, want 4 samples peaking at the 5s one", w.Samples, w.MemoryPeak)
	}
}

// A WINDOW THAT BEGAN BEFORE THE FIRST SAMPLE OR ENDED AFTER THE LAST SAYS HOW
// MUCH IS COVERED, and reports only that part, never a smaller total.
func TestAWindowPastEitherEndIsPartial(t *testing.T) {
	t.Parallel()

	tl := steadySeries(secondsTo(0, 10)...)
	before := tl.Window(at(-2000), at(3000))
	if before.Complete() || before.Span != 5*time.Second || before.Covered != 3*time.Second {
		t.Errorf("before the first sample: span %s covered %s, want 3s of 5s", before.Span, before.Covered)
	}
	sameIncrements(t, "before", before, usedPerSecond(3))

	after := tl.Window(at(8000), at(14_000))
	if after.Complete() || after.Span != 6*time.Second || after.Covered != 2*time.Second {
		t.Errorf("after the last sample: span %s covered %s, want 2s of 6s", after.Span, after.Covered)
	}
	sameIncrements(t, "after", after, usedPerSecond(2))

	outside := tl.Window(at(20_000), at(25_000))
	if outside.Covered != 0 || outside.Samples != 0 {
		t.Errorf("a window outside the series = %+v, want nothing covered", outside)
	}
}

// A GAP IS UNCOVERED, NOT SPLIT: the sampler marked the point at 15s as
// ending an interval it did not see, so 5s to 15s is known only as a total.
func TestAGapInTheSeriesIsUncovered(t *testing.T) {
	t.Parallel()

	tl := steadySeries(append(secondsTo(0, 5), secondsTo(15, 20)...)...)
	tl.Points[6].AfterGap = true
	w := tl.Window(at(3000), at(17_000))
	if w.Complete() || w.Span != 14*time.Second || w.Covered != 4*time.Second {
		t.Errorf("across a gap: span %s covered %s, want 4s of 14s", w.Span, w.Covered)
	}
	sameIncrements(t, "across a gap", w, usedPerSecond(4))

	inside := tl.Window(at(7000), at(9000))
	if inside.Covered != 0 {
		t.Errorf("a window inside the gap covered %s", inside.Covered)
	}

	// ONE MARKED TICK IS A GAP however close its neighbours are.
	short := steadySeries(secondsTo(0, 5)...)
	short.Points[3].AfterGap = true
	if w := short.Window(at(1000), at(4000)); w.Covered != 2*time.Second {
		t.Errorf("a marked one-second interval: covered %s of %s, want 2s", w.Covered, w.Span)
	}
}

// A SERIES THE SAMPLER HALVED MORE THAN ONCE IS NOT A SERIES OF GAPS: wide old
// intervals beside narrow new ones are what halving leaves, and only the
// sampler's mark makes an interval a gap.
func TestADownsampledSeriesHasNoGaps(t *testing.T) {
	t.Parallel()

	// Eight-second intervals, then four, then two, then one: four halvings'
	// worth of strides, each older run coarser than the next.
	tl := steadySeries(0, 8, 16, 24, 28, 32, 34, 36, 37, 38, 39, 40)
	w := tl.Window(at(1000), at(39_500))
	if !w.Complete() {
		t.Errorf("a series of mixed strides: covered %s of %s", w.Covered, w.Span)
	}
	sameIncrements(t, "mixed strides", w, usedPerSecond(38.5))

	// A LATE TICK IS NOT A GAP EITHER.
	jittered := steadySeries(0, 1, 2, 3, 4, 5)
	jittered.Points[3].OffsetMillis = 3800
	if w := jittered.Window(at(1000), at(5000)); !w.Complete() {
		t.Errorf("a late tick: covered %s of %s", w.Covered, w.Span)
	}
}

// EACH INTERVAL IS SPLIT BY ITS OWN RATE, not the job's average: a burst
// between 2s and 3s belongs to the windows that hold it.
func TestEachIntervalIsSplitAtItsOwnRate(t *testing.T) {
	t.Parallel()

	tl := steadySeries(secondsTo(0, 6)...)
	for i := 3; i < len(tl.Points); i++ {
		tl.Points[i].CPUUsage += 5_000_000
	}
	for name, tc := range map[string]struct {
		from, to int64
		want     int64
	}{
		"before the burst":     {0, 2000, 200_000},
		"half of the burst":    {2000, 2500, 50_000 + 2_500_000},
		"across the burst":     {1500, 3500, 200_000 + 5_000_000},
		"after the burst":      {3000, 6000, 300_000},
		"a third of the burst": {2000, 2333, 33_300 + 1_665_000},
	} {
		if got := tl.Window(at(tc.from), at(tc.to)).CPUMicros; got != tc.want {
			t.Errorf("%s: cpu %dµs, want %dµs", name, got, tc.want)
		}
	}
}

// TWO SAMPLES AT ONE OFFSET LOSE NOTHING: what the second adds belongs to the
// interval before it, and the window covering both gets it all.
func TestSamplesAtOneOffsetLoseNothing(t *testing.T) {
	t.Parallel()

	tl := Timeline{First: first, Points: []Point{
		{OffsetMillis: 0}, {OffsetMillis: 1000, CPUUsage: 100}, {OffsetMillis: 1000, CPUUsage: 200},
		{OffsetMillis: 2000, CPUUsage: 300},
	}}
	if w := tl.Window(at(0), at(2000)); !w.Complete() || w.CPUMicros != 300 || w.Samples != 4 {
		t.Errorf("a duplicated offset = %+v, want all 300µs over four samples", w)
	}
	if w := tl.Window(at(0), at(1000)); w.CPUMicros != 200 {
		t.Errorf("the interval ending at the duplicate got %dµs, want both samples' 200", w.CPUMicros)
	}

	tl.Points[1].AfterGap = true
	if w := tl.Window(at(0), at(1000)); w.Covered != 0 {
		t.Errorf("a gap mark on the earlier duplicate was lost: covered %s", w.Covered)
	}

	// A FALL BETWEEN TWO SAMPLES AT ONE OFFSET IS STILL A FALL: the interval
	// into it and the catch-up after it are uncovered, never read as no usage.
	fallen := Timeline{First: first, Points: []Point{
		{OffsetMillis: 0}, {OffsetMillis: 1000, CPUUsage: 100}, {OffsetMillis: 1000, CPUUsage: 0},
		{OffsetMillis: 2000, CPUUsage: 200}, {OffsetMillis: 3000, CPUUsage: 300},
	}}
	if w := fallen.Window(at(0), at(3000)); w.Covered != time.Second || w.CPUMicros != 100 {
		t.Errorf("a fall inside a duplicated offset = covered %s, cpu %dµs; want 1s and 100µs",
			w.Covered, w.CPUMicros)
	}
}

// A COUNTER THAT FALLS IS NOT USAGE, AND NEITHER IS ITS CATCH-UP: in every
// cumulative column, the interval it falls across (1s to 2s) and the one after
// it (2s to 3s) are uncovered, and the intervals either side are counted
// exactly.
func TestACounterThatFallsIsUncovered(t *testing.T) {
	t.Parallel()

	for name, misread := range map[string]func(*Point){
		"cpu":        func(p *Point) { p.CPUUsage = 0 },
		"disk read":  func(p *Point) { p.DiskRead = 0 },
		"disk write": func(p *Point) { p.DiskWrite = 0 },
		"received":   func(p *Point) { p.NetRx = 0 },
		"sent":       func(p *Point) { p.NetTx = 0 },
		"energy":     func(p *Point) { p.EnergyActive = 0 },
	} {
		tl := steadySeries(secondsTo(0, 4)...)
		misread(&tl.Points[2])
		w := tl.Window(at(0), at(4000))
		if w.Covered != 2*time.Second {
			t.Errorf("%s falling: covered %s, want the 2s either side", name, w.Covered)
		}
		sameIncrements(t, name+" falling", w, usedPerSecond(2))
		if got := tl.Window(at(2000), at(3000)); got.Covered != 0 {
			t.Errorf("%s falling: the catch-up interval covered %s", name, got.Covered)
		}
	}
}

// A TOTAL THAT WOULD NOT FIT IS UNCOVERED, NEVER WRAPPED: increments either
// side of a reset do not telescope, so two that each fit can sum past MaxInt64.
func TestATotalPastMaxInt64IsUncovered(t *testing.T) {
	t.Parallel()

	tl := Timeline{First: first, Points: []Point{{OffsetMillis: 0},
		{OffsetMillis: 1000, DiskRead: math.MaxInt64}, {OffsetMillis: 2000},
		{OffsetMillis: 3000}, {OffsetMillis: 4000, DiskRead: math.MaxInt64}}}
	w := tl.Window(at(0), at(4000))
	if w.DiskRead != math.MaxInt64 || w.Covered != time.Second {
		t.Errorf("two MaxInt64 increments across a reset = read %d over %s, want one of them over 1s",
			w.DiskRead, w.Covered)
	}
}

// EVERY INT64 SPLITS EXACTLY: no float rounds a large counter, and none
// overflows.
func TestLargeCountersSplitExactly(t *testing.T) {
	t.Parallel()

	const odd = 1<<53 + 1
	tl := Timeline{First: first, Points: []Point{{OffsetMillis: 0},
		{OffsetMillis: 1000, CPUUsage: odd, DiskRead: math.MaxInt64}}}
	whole := tl.Window(at(0), at(1000))
	if whole.CPUMicros != odd || whole.DiskRead != math.MaxInt64 {
		t.Errorf("a whole interval = cpu %d read %d, want %d and %d", whole.CPUMicros, whole.DiskRead,
			int64(odd), int64(math.MaxInt64))
	}
	if half := tl.Window(at(0), at(500)); half.DiskRead != math.MaxInt64/2+1 {
		t.Errorf("half of MaxInt64 = %d, want it rounded to %d", half.DiskRead, int64(math.MaxInt64/2+1))
	}
}

// A STEP SHORTER THAN ONE SAMPLE GETS ITS SHARE OF THE INTERVAL AROUND IT BY
// TIME, says no sample was taken inside it, and claims no memory level.
func TestAWindowInsideOneIntervalIsSplitByTime(t *testing.T) {
	t.Parallel()

	w := steadySeries(secondsTo(0, 10)...).Window(at(2250), at(2750))
	if !w.Complete() || w.Covered != 500*time.Millisecond {
		t.Errorf("half a second inside one interval: covered %s of %s", w.Covered, w.Span)
	}
	sameIncrements(t, "half an interval", w, usedPerSecond(0.5))
	if w.Samples != 0 || w.MemoryPeak != 0 {
		t.Errorf("samples %d peak %d, want none and no level", w.Samples, w.MemoryPeak)
	}
}

// A ZERO-LENGTH OR BACKWARDS WINDOW HAS NO LENGTH AND IS GIVEN NOTHING.
func TestAWindowWithNoLengthIsGivenNothing(t *testing.T) {
	t.Parallel()

	tl := steadySeries(secondsTo(0, 10)...)
	for name, w := range map[string]WindowUsage{
		"zero length": tl.Window(at(3000), at(3000)),
		"backwards":   tl.Window(at(5000), at(3000)),
	} {
		if w != (WindowUsage{}) || w.Complete() {
			t.Errorf("%s = %+v, want nothing", name, w)
		}
	}
	if w := (Timeline{First: first}).Window(at(0), at(1000)); w.Covered != 0 || w.Span != time.Second {
		t.Errorf("an empty series = %+v, want a second with nothing covered", w)
	}
}

// STEPS ARE INDEPENDENT WINDOWS: asked in any order, each gets the same answer.
func TestStepsInAnyOrderGetTheSameWindows(t *testing.T) {
	t.Parallel()

	tl := steadySeries(secondsTo(0, 30)...)
	windows := [][2]int64{{0, 4000}, {4000, 4000}, {4000, 19_500}, {19_500, 31_000}, {2500, 2700}}
	inOrder := make([]WindowUsage, len(windows))
	for i, w := range windows {
		inOrder[i] = tl.Window(at(w[0]), at(w[1]))
	}
	for i := len(windows) - 1; i >= 0; i-- {
		if got := tl.Window(at(windows[i][0]), at(windows[i][1])); got != inOrder[i] {
			t.Errorf("window %v asked last = %+v, asked first = %+v", windows[i], got, inOrder[i])
		}
	}
	if total := inOrder[0].CPUMicros + inOrder[2].CPUMicros + inOrder[3].CPUMicros; total != 3_000_000 {
		t.Errorf("three windows tiling the series used %dµs, want the series' 3s", total)
	}
}
