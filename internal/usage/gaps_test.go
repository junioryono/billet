package usage

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// THE CAUSE NUMBERS ARE FROZEN: they are what a stored series holds, so a
// renumbering would rename every gap already in a ledger. The numbers and the
// names are written out here rather than read from the constants, and a series
// written by hand with a number decodes to its name.
func TestTheGapCauseNumbersAreFrozen(t *testing.T) {
	t.Parallel()

	frozen := []string{"none", "unspecified", "missed_tick", "slow_read", "failed_read", "late_first_read",
		"counter_fell", "final_energy", "sampler_stuck", "handover", "several"}
	for n, name := range frozen {
		if got := GapCause(n).String(); got != name {
			t.Errorf("cause %d is named %q, want %q", n, got, name)
		}
	}
	if got := len(GapCauses()); got != len(frozen)-1 {
		t.Errorf("%d causes, want %d: a new cause needs a wire version and a line here", got, len(frozen)-1)
	}

	raw := binary.AppendUvarint(nil, 1)
	raw = binary.AppendUvarint(raw, uint64(len(SeriesColumns)+1))
	raw = binary.AppendUvarint(raw, 2)
	for range SeriesColumns {
		raw = binary.AppendVarint(raw, 0)
		raw = binary.AppendVarint(raw, 0)
	}
	raw = binary.AppendVarint(raw, 0)
	raw = binary.AppendVarint(raw, 4)
	tl, err := DecodeSeriesGaps(encodedRaw(t, raw))
	if err != nil || tl.Points[1].Gap.String() != "failed_read" {
		t.Errorf("a series holding cause 4 read back as %+v (%v), want failed_read", tl.Points, err)
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
			if g.Length() <= 0 || g.Cause == GapNone || i > 0 && (gaps[i-1].To.After(g.From) ||
				gaps[i-1].To.Equal(g.From) && gaps[i-1].Cause == g.Cause) {
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

// A FALL AND A GROUP FIRST READ AT THE END ARE HEARD TOO: the interval a
// counter fell into and the catch-up after it, which every reader's settle
// leaves uncovered, and the earlier intervals a final read's first sight of a
// group marks.
func TestTheObserverHearsFallsAndALateFinalRead(t *testing.T) {
	tr, target := referenceVM(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	o := &recordingObserver{}
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Observer: o, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	tick := func() {
		now = now.Add(time.Second)
		m.Tick()
	}
	tick()
	tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 1", 1))
	tick()
	tr.write(refCgroup+"/cpu.stat", refCPUStat)
	tick()
	tick()

	o.mu.Lock()
	falls := slices.Clone(o.gaps)
	o.mu.Unlock()
	if want := []string{"counter_fell", "counter_fell"}; !slices.Equal(falls, want) {
		t.Errorf("a fall and its catch-up were observed as %v, want %v", falls, want)
	}

	tr2, target2 := referenceVM(t)
	tr2.remove(refCgroup + "/cpu.stat")
	o2 := &recordingObserver{}
	m2 := runMonitor(t, tr2.root, Options{Interval: time.Second, Observer: o2, Now: func() time.Time { return now }})
	m2.Start("vm", target2, 8)
	for range 3 {
		now = now.Add(time.Second)
		m2.Tick()
	}
	tr2.write(refCgroup+"/cpu.stat", refCPUStat)
	now = now.Add(time.Second)
	if _, ok := m2.Final("vm"); !ok {
		t.Fatal("no summary")
	}
	o2.mu.Lock()
	defer o2.mu.Unlock()
	if want := []string{"late_first_read", "late_first_read", "late_first_read"}; !slices.Equal(o2.gaps, want) {
		t.Errorf("a group first read at the final read was observed as %v, want %v", o2.gaps, want)
	}
}

// A READ WRITEBACK KEPT MOVING KEEPS ITS SAMPLE AND IS TOLD (#495): through a
// real tick, the point is kept and seen, its disk written is the lower bound,
// the observer hears one bounded read, and the job's series has no gap.
func TestABoundedReadKeepsItsSampleThroughATick(t *testing.T) {
	tr, target := referenceVM(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	o := &recordingObserver{}
	m := NewMonitor(tr.root, Options{Interval: time.Second, Observer: o, Now: func() time.Time { return now }})
	m.ticker, m.limit = false, testLimit
	var moving atomic.Bool
	var written atomic.Int64
	written.Store(4 << 20)
	m.reader.ReadFile = func(name string) ([]byte, error) {
		if !moving.Load() || !strings.HasSuffix(name, "/io.stat") {
			return os.ReadFile(name)
		}
		// The reference VM's io.stat, its first device's written bytes moving on
		// every read as writeback would move them.
		return fmt.Appendf(nil, "251:16 rbytes=1048576 wbytes=%d rios=10 wios=40 dbytes=0 dios=0\n"+
			"251:32 rbytes=2097152 wbytes=0 rios=5 wios=0 dbytes=0 dios=0\n", written.Add(4096)), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	m.Start("vm", target, 8)
	tick := func() {
		now = now.Add(time.Second)
		m.Tick()
	}
	tick()
	moving.Store(true)
	tick()
	moving.Store(false)
	tick()

	s, ok := m.Final("vm")
	if !ok {
		t.Fatal("no summary")
	}
	if !s.Measured.IO {
		t.Error("io reads as unmeasured")
	}
	if len(s.Points) != 5 {
		t.Fatalf("the series holds %d points, want 5: the bounded tick's sample was dropped", len(s.Points))
	}
	for i, p := range s.Points {
		if p.Gap != GapNone {
			t.Errorf("point %d is marked %v", i, p.Gap)
		}
	}
	if s.Points[2].DiskWrite <= s.Points[1].DiskWrite {
		t.Errorf("the bounded sample's written %d is not above the one before (%d)", s.Points[2].DiskWrite,
			s.Points[1].DiskWrite)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.bounded != 1 || len(o.gaps) != 0 {
		t.Errorf("observed %d bounded reads and gaps %v, want one and none", o.bounded, o.gaps)
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
