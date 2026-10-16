package usage

import (
	"encoding/binary"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// causedPoints is markedPoints with each interval marked by the cause given.
func causedPoints(n int, causes map[int]GapCause) []Point {
	points := markedPoints(n)
	for i, c := range causes {
		points[i].Gap = c
	}

	return points
}

// A SERIES WITH CAUSES KEEPS EVERY CAUSE, to the point (#495).
func TestASeriesKeepsWhyEachIntervalWasNotSeen(t *testing.T) {
	t.Parallel()

	causes := map[int]GapCause{}
	for i, c := range GapCauses() {
		causes[2*i+1] = c
	}
	points := causedPoints(2*len(GapCauses())+2, causes)
	start := time.Date(2026, 10, 15, 8, 50, 27, 129_000_000, time.UTC)

	data, stride, err := EncodeSeriesGaps(points, start, 1<<20)
	if err != nil || stride != 1 {
		t.Fatalf("EncodeSeriesGaps = stride %d, %v", stride, err)
	}
	tl, err := TimelineOf(SeriesCodecGaps, data)
	if err != nil {
		t.Fatalf("TimelineOf: %v", err)
	}
	if !tl.First.Equal(start) || !slices.Equal(tl.Points, points) {
		t.Errorf("the round trip changed the series: first %s, points equal %v", tl.First,
			slices.Equal(tl.Points, points))
	}

	// A PEER THAT READS ONLY THE MARK GETS EVERY GAP WITHOUT ITS CAUSE.
	marked, err := WithoutCauses(data, 1<<20)
	if err != nil {
		t.Fatalf("WithoutCauses: %v", err)
	}
	old, err := TimelineOf(SeriesCodecClocked, marked)
	if err != nil {
		t.Fatalf("TimelineOf(clocked): %v", err)
	}
	for i, p := range old.Points {
		want := GapNone
		if points[i].Gap != GapNone {
			want = GapUnspecified
		}
		if p.Gap != want {
			t.Errorf("point %d of the marked series is %v, want %v", i, p.Gap, want)
		}
	}

	// AND ONE THAT READS NO CLOCK GETS THE SAME POINTS WITHOUT EITHER.
	unclocked, err := WithoutClock(SeriesCodecGaps, data, 1<<20)
	if err != nil {
		t.Fatalf("WithoutClock: %v", err)
	}
	bare, err := DecodeSeries(unclocked)
	if err != nil {
		t.Fatalf("DecodeSeries: %v", err)
	}
	for i := range points {
		points[i].Gap = GapNone
	}
	if !slices.Equal(bare, points) {
		t.Error("the unclocked series' points differ")
	}
	if _, err := WithoutClock(SeriesCodec, unclocked, 1<<20); err == nil {
		t.Error("a series with no clock had its clock dropped")
	}
}

// A CAUSE THIS BUILD DOES NOT KNOW IS REFUSED, never read as a gap of some
// other kind, and a cause in a series that keeps only marks is not a mark.
func TestACauseThisBuildDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()

	raw := func(columns int, mark int64) []byte {
		b := binary.AppendUvarint(nil, 1)
		b = binary.AppendUvarint(b, uint64(columns))
		b = binary.AppendUvarint(b, 1)
		for range SeriesColumns {
			b = binary.AppendVarint(b, 0)
		}
		return binary.AppendVarint(b, mark)
	}
	last := int64(len(GapCauses()))
	if tl, err := DecodeSeriesGaps(encodedRaw(t, raw(caused.columns(), last))); err != nil ||
		tl.Points[0].Gap != GapSeveral {
		t.Fatalf("the last cause read back as %v, %v; want %v", tl.Points, err, GapSeveral)
	}
	if _, err := DecodeSeriesGaps(encodedRaw(t, raw(caused.columns(), last+1))); err == nil ||
		!strings.Contains(err.Error(), "not a cause this build knows") {
		t.Errorf("a cause past the last = %v, want it refused", err)
	}
	if _, err := DecodeSeriesAt(encodedRaw(t, raw(marked.columns(), int64(GapSlowRead)))); err == nil ||
		!strings.Contains(err.Error(), "not a mark") {
		t.Errorf("a cause in a marked series = %v, want it refused as no mark", err)
	}
	if got := GapCause(200).String(); got != "cause-200" {
		t.Errorf("an unknown cause is named %q", got)
	}
}

// HALVING KEEPS EACH GAP'S CAUSE, and one interval made of gaps of different
// causes says several.
func TestHalvingKeepsEachGapsCause(t *testing.T) {
	t.Parallel()

	kept := downsample(causedPoints(9, map[int]GapCause{1: GapFailedRead, 2: GapFailedRead, 5: GapSlowRead,
		6: GapHandover}), 4)
	var got []GapCause
	for _, p := range kept {
		got = append(got, p.Gap)
	}
	if want := []GapCause{GapNone, GapFailedRead, GapSeveral}; !slices.Equal(got, want) {
		t.Errorf("causes after stride 4 = %v, want %v", got, want)
	}
}

// GAPS ARE WHAT WINDOW DOES NOT COVER: on random series with repeated offsets,
// marked intervals and counters that fall, what the whole series' window
// covers and the length of its gaps add up to the series' span, and no gap
// overlaps a covered instant.
func TestGapsAreWhatTheWindowLeavesUncovered(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(9, 5))
	causes := GapCauses()
	for c := range 20_000 {
		tl := Timeline{First: first}
		var cum Point
		for range 1 + rng.IntN(40) {
			if rng.IntN(4) != 0 {
				cum.OffsetMillis += int64(rng.IntN(3000))
			}
			step := func(v int64) int64 {
				if rng.IntN(12) == 0 {
					return max(v-int64(rng.IntN(5000)), 0)
				}
				return v + int64(rng.IntN(5000))
			}
			cum.CPUUsage, cum.DiskRead, cum.DiskWrite = step(cum.CPUUsage), step(cum.DiskRead), step(cum.DiskWrite)
			cum.NetRx, cum.NetTx, cum.EnergyActive = step(cum.NetRx), step(cum.NetTx), step(cum.EnergyActive)
			cum.Gap = GapNone
			if rng.IntN(8) == 0 {
				cum.Gap = causes[rng.IntN(len(causes))]
			}
			tl.Points = append(tl.Points, cum)
		}
		from, to := at(tl.Points[0].OffsetMillis), at(cum.OffsetMillis)
		w := tl.Window(from, to)
		var unseen time.Duration
		gaps := tl.Gaps()
		for i, g := range gaps {
			if g.Length() <= 0 || g.Cause == GapNone || i > 0 && !gaps[i-1].To.Before(g.From) {
				t.Fatalf("case %d: gap %d is %+v after %+v", c, i, g, gaps[max(i-1, 0)])
			}
			if inside := tl.Window(g.From, g.To); inside.Covered != 0 {
				t.Fatalf("case %d: the window covers %s of the gap %+v", c, inside.Covered, g)
			}
			unseen += g.Length()
		}
		if w.Covered+unseen != to.Sub(from) {
			t.Fatalf("case %d: covered %s and gaps %s, want the span %s\n%+v", c, w.Covered, unseen,
				to.Sub(from), tl.Points)
		}
	}
}

// A TIMELINE'S GAPS NAME THE TIMES AND THE CAUSE: run 15's idle job, whose one
// dropped sample left two seconds of its load window unseen.
func TestAGapIsWhenAndWhy(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 15, 8, 50, 27, 129_000_000, time.UTC)
	tl := Timeline{First: start, Points: []Point{{OffsetMillis: 0}, {OffsetMillis: 68_164},
		{OffsetMillis: 70_162, Gap: GapFailedRead}, {OffsetMillis: 71_164}}}
	gaps := tl.Gaps()
	want := []Gap{{From: start.Add(68_164 * time.Millisecond), To: start.Add(70_162 * time.Millisecond),
		Cause: GapFailedRead}}
	if !slices.Equal(gaps, want) {
		t.Fatalf("gaps = %+v, want %+v", gaps, want)
	}
	if gaps[0].Length() != 1998*time.Millisecond {
		t.Errorf("the gap lasted %s, want 1.998s", gaps[0].Length())
	}
}

// recordingObserver keeps what the sampler told it.
type recordingObserver struct {
	mu      sync.Mutex
	ticks   []time.Duration
	gaps    []string
	bounded int
}

func (o *recordingObserver) TickTook(d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ticks = append(o.ticks, d)
}

func (o *recordingObserver) Gap(c string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gaps = append(o.gaps, c)
}

func (o *recordingObserver) IOBounded() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bounded++
}

// THE OBSERVER HEARS EVERY TICK AND EVERY INTERVAL MARKED UNSEEN, with its
// cause, once: a failed read, the tick it missed, and the intervals a group
// read late leaves behind it.
func TestTheObserverHearsEachTickAndEachGap(t *testing.T) {
	tr, target := referenceVM(t)
	tr.remove(refCgroup + "/cpu.stat")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	o := &recordingObserver{}
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Observer: o, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	tick := func(after time.Duration) {
		now = now.Add(after)
		m.Tick()
	}

	tick(time.Second)
	tick(time.Second)
	tr.write(refCgroup+"/cpu.stat", refCPUStat)
	tick(time.Second) // cpu read late: the two intervals before it, and this one
	tr.remove(refCgroup + "/cpu.stat")
	tick(time.Second) // a failed read: no point kept
	tr.write(refCgroup+"/cpu.stat", refCPUStat)
	tick(time.Second) // the point after it, marked for the failed read
	tick(3 * time.Second)

	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.ticks) != 6 {
		t.Errorf("the observer heard %d ticks, want 6", len(o.ticks))
	}
	want := []string{"late_first_read", "late_first_read", "late_first_read", "failed_read", "missed_tick"}
	if !slices.Equal(o.gaps, want) {
		t.Errorf("gaps observed = %v, want %v", o.gaps, want)
	}
	if o.bounded != 0 {
		t.Errorf("%d bounded io reads observed of a cgroup whose io.stat never moved", o.bounded)
	}
}

// A READ WHOSE DISK WRITTEN IS A LOWER BOUND IS TOLD TO THE OBSERVER, and only
// one that measured io.
func TestABoundedReadIsObserved(t *testing.T) {
	t.Parallel()

	o := &recordingObserver{}
	m := NewMonitor(t.TempDir(), Options{Interval: time.Second, Observer: o})
	m.readBounded(Sample{IOOK: true, IOBounded: true})
	m.readBounded(Sample{IOOK: true})
	m.readBounded(Sample{IOBounded: true})
	if o.bounded != 1 {
		t.Errorf("observed %d bounded reads, want 1", o.bounded)
	}
}

// THE SERIES DECODER NEVER PANICS, and anything it reads it writes back the
// same.
func FuzzDecodeSeriesGaps(f *testing.F) {
	for _, points := range [][]Point{
		markedPoints(3),
		causedPoints(6, map[int]GapCause{1: GapFailedRead, 4: GapHandover}),
		{{OffsetMillis: 0}, {OffsetMillis: 1000, Gap: GapSeveral}},
	} {
		data, _, err := EncodeSeriesGaps(points, time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), 1<<20)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		tl, err := DecodeSeriesGaps(data)
		if err != nil {
			return
		}
		again, _, err := EncodeSeriesGaps(tl.Points, tl.First, 1<<30)
		if err != nil {
			if len(tl.Points) == 0 {
				return
			}
			t.Fatalf("a series that decoded did not encode: %v", err)
		}
		back, err := DecodeSeriesGaps(again)
		if err != nil || !back.First.Equal(tl.First) || !slices.Equal(back.Points, tl.Points) {
			t.Fatalf("the series did not survive a second round trip: %v", err)
		}
		for _, p := range tl.Points {
			if p.Gap >= gapCauses {
				t.Fatalf("a cause past the last was read: %d", p.Gap)
			}
		}
	})
}
