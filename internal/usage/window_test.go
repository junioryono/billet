package usage

import (
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

// A GAP IS UNCOVERED, NOT SPLIT: the sampler saw nothing between 5s and 15s.
func TestAGapInTheSeriesIsUncovered(t *testing.T) {
	t.Parallel()

	tl := steadySeries(append(secondsTo(0, 5), secondsTo(15, 20)...)...)
	w := tl.Window(at(3000), at(17_000))
	if w.Complete() || w.Span != 14*time.Second || w.Covered != 4*time.Second {
		t.Errorf("across a gap: span %s covered %s, want 4s of 14s", w.Span, w.Covered)
	}
	sameIncrements(t, "across a gap", w, usedPerSecond(4))

	inside := tl.Window(at(7000), at(9000))
	if inside.Covered != 0 {
		t.Errorf("a window inside the gap covered %s", inside.Covered)
	}
}

// A DOWNSAMPLED SERIES IS NOT A SERIES OF GAPS: its usual spacing is its
// stride, and the shorter last interval downsampling keeps is not a gap either.
func TestADownsampledSeriesHasNoGaps(t *testing.T) {
	t.Parallel()

	tl := steadySeries(0, 4, 8, 12, 16, 18)
	w := tl.Window(at(2000), at(17_000))
	if !w.Complete() {
		t.Errorf("a stride-4 series: covered %s of %s", w.Covered, w.Span)
	}
	sameIncrements(t, "stride 4", w, usedPerSecond(15))

	// A LATE TICK IS NOT A GAP EITHER: 1.8s against a usual 1s is within twice it.
	jittered := steadySeries(0, 1, 2, 3, 4, 5)
	jittered.Points[3].OffsetMillis = 3800
	if w := jittered.Window(at(1000), at(5000)); !w.Complete() {
		t.Errorf("a late tick: covered %s of %s", w.Covered, w.Span)
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
