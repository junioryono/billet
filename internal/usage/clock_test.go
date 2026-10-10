package usage

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// markedPoints is a series of n one-second points whose values do not
// compress, with the gap marks given.
func markedPoints(n int, marked ...int) []Point {
	points := make([]Point, n)
	var total int64
	for i := range points {
		// AN IRREGULAR INCREMENT, so the series does not compress, and never a
		// negative one, so no counter falls.
		total += 1 + (int64(i)*7919+int64(i*i))%104729
		points[i] = Point{OffsetMillis: int64(i) * 1000, CPUUsage: total, MemoryCurrent: 1<<30 + total%4096,
			DiskRead: total * 3, NetRx: total * 5, EnergyActive: total * 11,
			AfterGap: slices.Contains(marked, i)}
	}

	return points
}

// A CLOCKED SERIES KEEPS ITS START AND ITS GAP MARKS, to the millisecond.
func TestAClockedSeriesRoundTrips(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 9, 12, 0, 0, 123_456_789, time.FixedZone("x", 3600))
	points := markedPoints(50, 7, 30)
	data, stride, err := EncodeSeriesAt(points, start, 1<<20)
	if err != nil || stride != 1 {
		t.Fatalf("EncodeSeriesAt = stride %d, %v", stride, err)
	}
	tl, err := TimelineOf(SeriesCodecClocked, data)
	if err != nil {
		t.Fatalf("TimelineOf: %v", err)
	}
	if !tl.First.Equal(start.Truncate(time.Millisecond)) {
		t.Errorf("first = %s, want %s to the millisecond", tl.First, start)
	}
	if !slices.Equal(tl.Points, points) {
		t.Errorf("the points or their marks changed in the round trip")
	}
}

// DOWNSAMPLING CARRIES A DROPPED POINT'S MARK TO THE POINT KEPT AFTER IT, whose
// interval now holds the time the sampler did not see.
func TestDownsamplingKeepsEveryGap(t *testing.T) {
	t.Parallel()

	kept := downsample(markedPoints(10, 3, 9), 4)
	var marks []int64
	for _, p := range kept {
		if p.AfterGap {
			marks = append(marks, p.OffsetMillis)
		}
	}
	if offsets := []int64{4000, 9000}; !slices.Equal(marks, offsets) {
		t.Errorf("marked offsets after stride 4 = %v, want %v", marks, offsets)
	}
	if len(kept) != 4 || kept[3].OffsetMillis != 9000 {
		t.Errorf("stride 4 of 10 points kept %d, ending at %d", len(kept), kept[len(kept)-1].OffsetMillis)
	}

	// AND THE ENCODER'S OWN CUT DOES THE SAME.
	data, stride, err := EncodeSeriesAt(markedPoints(4000, 1001), time.Now(), 8<<10)
	if err != nil || stride < 2 {
		t.Fatalf("a series cut to 8 KiB = stride %d, %v", stride, err)
	}
	tl, err := DecodeSeriesAt(data)
	if err != nil {
		t.Fatalf("DecodeSeriesAt: %v", err)
	}
	marks = nil
	for _, p := range tl.Points {
		if p.AfterGap {
			marks = append(marks, p.OffsetMillis)
		}
	}
	// THE POINT KEPT AT OR AFTER 1001s CARRIES THE MARK, and no other does.
	want := (1001000/(int64(stride)*1000) + 1) * int64(stride) * 1000
	if !slices.Equal(marks, []int64{want}) {
		t.Errorf("marked offsets after a stride-%d cut = %v, want only %d", stride, marks, want)
	}
}

// A FALL SURVIVES DOWNSAMPLING: the point kept at or after it is marked, and
// when it lands on a kept point, so is the next kept point, whose interval now
// holds the catch-up.
func TestDownsamplingKeepsEveryFall(t *testing.T) {
	t.Parallel()

	cpu := func(values ...int64) []Point {
		points := make([]Point, len(values))
		for i, v := range values {
			points[i] = Point{OffsetMillis: int64(i) * 1000, CPUUsage: v}
		}
		return points
	}
	marks := func(points []Point) []int64 {
		var out []int64
		for _, p := range points {
			if p.AfterGap {
				out = append(out, p.OffsetMillis)
			}
		}
		return out
	}

	// Falling into a dropped point (index 2 of stride 4): only the kept 4.
	if got := marks(downsample(cpu(0, 100, 50, 200, 300, 400, 500, 600, 700), 4)); !slices.Equal(got, []int64{4000}) {
		t.Errorf("a fall into a dropped point marked %v, want [4000]", got)
	}
	// Falling into a kept point (index 2 of stride 2): it and the next kept.
	if got := marks(downsample(cpu(0, 100, 50, 200, 300, 400, 500), 2)); !slices.Equal(got, []int64{2000, 4000}) {
		t.Errorf("a fall into a kept point marked %v, want [2000 4000]", got)
	}
	// AND THE WINDOW OVER THE CUT SERIES DOES NOT CALL IT COVERED.
	cut := Timeline{First: first, Points: downsample(cpu(0, 100, 50, 200, 300, 400, 500, 600, 700), 4)}
	if w := cut.Window(at(0), at(4000)); w.Covered != 0 {
		t.Errorf("an interval holding a fall covered %s", w.Covered)
	}
}

// A PLANE THAT READS ONLY THE OLD CODEC GETS THE SAME POINTS WITHOUT THE CLOCK.
func TestWithoutClockIsTheSameSeries(t *testing.T) {
	t.Parallel()

	points := markedPoints(20, 4)
	data, _, err := EncodeSeriesAt(points, time.Now(), 1<<20)
	if err != nil {
		t.Fatalf("EncodeSeriesAt: %v", err)
	}
	old, err := WithoutClock(data, 1<<20)
	if err != nil {
		t.Fatalf("WithoutClock: %v", err)
	}
	got, err := DecodeSeries(old)
	if err != nil {
		t.Fatalf("DecodeSeries: %v", err)
	}
	points[4].AfterGap = false
	if !slices.Equal(got, points) {
		t.Errorf("the old codec's points differ from the clocked series'")
	}
	if _, err := TimelineOf(SeriesCodec, old); !errors.Is(err, ErrNoClock) {
		t.Errorf("an unclocked series = %v, want ErrNoClock", err)
	}
}

// A CLOCKED SERIES THE ENCODER COULD NOT HAVE WRITTEN IS REFUSED.
func TestAHostileClockedSeriesIsRefused(t *testing.T) {
	t.Parallel()

	raw := func(start uint64, mark int64) []byte {
		b := binary.AppendUvarint(nil, start)
		b = binary.AppendUvarint(b, uint64(len(clockedColumns)))
		b = binary.AppendUvarint(b, 1)
		for range SeriesColumns {
			b = binary.AppendVarint(b, 0)
		}
		return binary.AppendVarint(b, mark)
	}
	if _, err := DecodeSeriesAt(encodedRaw(t, raw(1, 1))); err != nil {
		t.Fatalf("a well-formed one-point series was refused: %v", err)
	}
	unclocked, _, err := EncodeSeries(markedPoints(3), 1<<20)
	if err != nil {
		t.Fatalf("EncodeSeries: %v", err)
	}
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"a mark that is not one": {encodedRaw(t, raw(1, 2)), "is not a mark"},
		"no start":               {encodedRaw(t, raw(0, 0)), "no valid start"},
		"the old codec":          {unclocked, "columns"},
	} {
		if _, err := DecodeSeriesAt(tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s = %v, want it refused saying %q", name, err, tc.want)
		}
	}
	if _, _, err := EncodeSeriesAt(markedPoints(3), time.Time{}, 1<<20); err == nil {
		t.Error("a series with no start was encoded as clocked")
	}
}

// THE SAMPLER MARKS WHAT IT DID NOT SEE: a read of a group it had read before
// that failed keeps no point and marks the next one, as does a tick it missed;
// an ordinary tick marks nothing, and the summary says when the series began.
func TestTheSamplerMarksWhatItDidNotSee(t *testing.T) {
	tr, target := referenceVM(t)
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	now := start
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	tick := func(after time.Duration) {
		now = now.Add(after)
		m.Tick()
	}

	tick(time.Second)
	tr.remove(refCgroup + "/cpu.stat")
	tick(time.Second)
	tr.write(refCgroup+"/cpu.stat", refCPUStat)
	tick(time.Second)
	tick(time.Second)
	tick(3 * time.Second)
	tick(time.Second)

	s, ok := m.Final("vm")
	if !ok {
		t.Fatal("no summary")
	}
	if !s.First.Equal(start) {
		t.Errorf("the series began at %s, want %s", s.First, start)
	}
	type mark struct {
		offset int64
		gap    bool
	}
	var got []mark
	for _, p := range s.Points {
		got = append(got, mark{p.OffsetMillis, p.AfterGap})
	}
	want := []mark{{0, false}, {1000, false}, {3000, true}, {4000, false}, {7000, true}, {8000, false},
		{8000, false}}
	if !slices.Equal(got, want) {
		t.Errorf("points = %v,\nwant %v", got, want)
	}
}

// A READ IS STALE ONLY FOR A GROUP THE SERIES HOLDS THAT WAS READ BEFORE AND
// FAILED NOW: each such group alone makes it so, and a group never read, or
// one the series does not hold, does not.
func TestAStaleReadIsOneOfASeriesGroupReadBefore(t *testing.T) {
	t.Parallel()

	everything := seen{cpu: true, memory: true, io: true, net: true, threads: true, pressure: true,
		processEnergy: true}
	read := Sample{CPUOK: true, MemoryOK: true, IOOK: true, NetOK: true, ThreadsOK: true, PressureOK: true,
		ProcessEnergyOK: true}
	if unread(read, everything, true) {
		t.Fatal("a sample that read every group was stale")
	}
	for name, fail := range map[string]func(*Sample){
		"cpu": func(s *Sample) { s.CPUOK = false }, "memory": func(s *Sample) { s.MemoryOK = false },
		"io": func(s *Sample) { s.IOOK = false }, "net": func(s *Sample) { s.NetOK = false },
		"threads":        func(s *Sample) { s.ThreadsOK = false },
		"process energy": func(s *Sample) { s.ProcessEnergyOK = false },
	} {
		s := read
		fail(&s)
		if !unread(s, everything, true) {
			t.Errorf("a failed %s read was not stale", name)
		}
		if unread(s, seen{}, true) {
			t.Errorf("a failed %s read of a group never read was stale", name)
		}
	}
	pressure := read
	pressure.PressureOK = false
	if unread(pressure, everything, true) {
		t.Error("a failed pressure read was stale, though the series holds no pressure")
	}
	energy := read
	energy.ProcessEnergyOK = false
	if unread(energy, everything, false) {
		t.Error("a failed process-energy read was stale for a job not measured by its process")
	}
}

// A FINAL READ THAT FAILS MARKS THE LAST INTERVAL UNSEEN: the final point holds
// the group's last value at a time it was not read.
func TestAFailedFinalReadMarksTheLastInterval(t *testing.T) {
	tr, target := referenceVM(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	now = now.Add(time.Second)
	m.Tick()
	tr.remove(refCgroup + "/cpu.stat")
	now = now.Add(time.Second)

	s, ok := m.Final("vm")
	if !ok || len(s.Points) != 3 {
		t.Fatalf("summary %v with %d points, want three", ok, len(s.Points))
	}
	if s.Points[1].AfterGap || !s.Points[2].AfterGap {
		t.Errorf("marks = %v, %v; want only the final point's interval unseen", s.Points[1].AfterGap,
			s.Points[2].AfterGap)
	}
}

// A GROUP READ FOR THE FIRST TIME LATE MAKES EVERYTHING BEFORE IT UNSEEN:
// every earlier point held the group's zero, so an earlier step would show it
// doing nothing and the interval that reads it first would be given all it did
// before.
func TestAGroupReadLateLeavesNothingBeforeItSeen(t *testing.T) {
	tr, target := referenceVM(t)
	tr.remove(refCgroup + "/cpu.stat")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	for range 2 {
		now = now.Add(time.Second)
		m.Tick()
	}
	tr.write(refCgroup+"/cpu.stat", refCPUStat)
	now = now.Add(time.Second)
	m.Tick()
	now = now.Add(time.Second)
	m.Tick()

	s, ok := m.Final("vm")
	if !ok || !s.Measured.CPU {
		t.Fatalf("summary %v, cpu measured %v", ok, s.Measured.CPU)
	}
	var marks []bool
	for _, p := range s.Points {
		marks = append(marks, p.AfterGap)
	}
	if want := []bool{false, true, true, true, false, false}; !slices.Equal(marks, want) {
		t.Errorf("marks = %v, want every interval to the first cpu read unseen and none after: %v", marks, want)
	}
}

// AN OFFSET NO DURATION HOLDS IS REFUSED, never wrapped into a time it was not.
func TestAnOffsetPastADurationIsRefused(t *testing.T) {
	t.Parallel()

	points := []Point{{OffsetMillis: 0}, {OffsetMillis: maxOffsetMillis + 1}}
	data, _, err := EncodeSeries(points, 1<<20)
	if err != nil {
		t.Fatalf("EncodeSeries: %v", err)
	}
	if _, err := DecodeSeries(data); err == nil || !strings.Contains(err.Error(), "past any offset") {
		t.Errorf("an offset past a duration = %v, want it refused", err)
	}
	points[1].OffsetMillis = maxOffsetMillis
	if data, _, err = EncodeSeries(points, 1<<20); err != nil {
		t.Fatalf("EncodeSeries: %v", err)
	}
	if _, err := DecodeSeries(data); err != nil {
		t.Errorf("the largest offset a duration holds was refused: %v", err)
	}
}

// AND A GROUP FIRST READ BY THE FINAL SAMPLE DOES THE SAME.
func TestAGroupFirstReadAtTheEndLeavesNothingBeforeItSeen(t *testing.T) {
	tr, target := referenceVM(t)
	tr.remove(refCgroup + "/cpu.stat")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	now = now.Add(time.Second)
	m.Tick()
	tr.write(refCgroup+"/cpu.stat", refCPUStat)
	now = now.Add(time.Second)

	s, ok := m.Final("vm")
	if !ok || !s.Measured.CPU {
		t.Fatalf("summary %v, cpu measured %v", ok, s.Measured.CPU)
	}
	var marks []bool
	for _, p := range s.Points {
		marks = append(marks, p.AfterGap)
	}
	if want := []bool{false, true, true}; !slices.Equal(marks, want) {
		t.Errorf("marks = %v, want %v", marks, want)
	}
}

// A READ THAT STALLS FOR AN INTERVAL KEEPS NO POINT, and its counters are not
// backdated to the tick's start: the interval before the stall keeps only what
// was read before it, and the one through it is unseen.
func TestAStalledReadIsNotBackdated(t *testing.T) {
	tr, target := referenceVM(t)
	var mu sync.Mutex
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var queue []time.Time
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if len(queue) > 0 {
			clock, queue = queue[0], queue[1:]
		}
		return clock
	}
	then := func(times ...time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		for _, d := range times {
			queue = append(queue, clock.Add(d))
		}
	}
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: now})
	m.Start("vm", target, 8)
	// An ordinary tick at 1s: its start, the job's read beginning and ending.
	then(time.Second, time.Second, time.Second)
	m.Tick()
	// A tick at 2s whose read of the job stalls until 4s.
	then(time.Second, time.Second, 3*time.Second)
	m.Tick()
	then(time.Second, time.Second, time.Second)
	m.Tick()
	// A read that takes half an interval holds the time it ended.
	then(time.Second, time.Second, 1500*time.Millisecond)
	m.Tick()

	s, ok := m.Final("vm")
	if !ok {
		t.Fatal("no summary")
	}
	type mark struct {
		offset int64
		gap    bool
	}
	var got []mark
	for _, p := range s.Points {
		got = append(got, mark{p.OffsetMillis, p.AfterGap})
	}
	if want := []mark{{0, false}, {1000, false}, {5000, true}, {6500, false}, {6500, false}}; !slices.Equal(got, want) {
		t.Errorf("points = %v,\nwant %v", got, want)
	}
}

// WITH ENERGY SHARED FROM THE PACKAGE, THE FINAL INTERVAL IS UNSEEN: the final
// read takes no package reading, so its point repeats the last tick's energy.
func TestTheFinalIntervalIsUnseenWhenEnergyComesFromThePackage(t *testing.T) {
	for _, rapl := range []bool{false, true} {
		tr, target := referenceVM(t)
		now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		m := runMonitor(t, tr.root, Options{Interval: time.Second, RAPL: rapl, Now: func() time.Time { return now }})
		m.Start("vm", target, 8)
		now = now.Add(time.Second)
		m.Tick()
		now = now.Add(time.Second)

		s, ok := m.Final("vm")
		if !ok || len(s.Points) != 3 {
			t.Fatalf("rapl %v: summary %v with %d points", rapl, ok, len(s.Points))
		}
		if got := s.Points[2].AfterGap; got != rapl {
			t.Errorf("rapl %v: the final interval unseen = %v", rapl, got)
		}
	}
}

// A SERIES THAT BEGAN CENTURIES FROM THE WINDOW COVERS NONE OF IT: the window's
// ends saturate, and nothing wraps into a plausible overlap.
func TestASeriesFarFromTheWindowCoversNoneOfIt(t *testing.T) {
	t.Parallel()

	for _, start := range []time.Time{
		time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		tl := Timeline{First: start, Points: []Point{{OffsetMillis: 0}, {OffsetMillis: 1000, CPUUsage: math.MaxInt64},
			{OffsetMillis: 2000, CPUUsage: math.MaxInt64}}}
		w := tl.Window(first, first.Add(time.Second))
		if w.Covered != 0 || w.CPUMicros != 0 || w.Samples != 0 {
			t.Errorf("a series beginning %s = %+v, want nothing covered", start, w)
		}
	}
}

// HALVING KEEPS A FALL BETWEEN TWO SAMPLES AT ONE OFFSET: the interval into
// it and the catch-up after it stay uncovered once the point that fell is
// dropped.
func TestHalvingKeepsAFallInsideOneOffset(t *testing.T) {
	t.Parallel()

	points := []Point{{OffsetMillis: 0}, {OffsetMillis: 1000, CPUUsage: 100}, {OffsetMillis: 2000, CPUUsage: 200},
		{OffsetMillis: 2000, CPUUsage: 0}, {OffsetMillis: 3000, CPUUsage: 300}}
	whole := Timeline{First: first, Points: points}
	cut := Timeline{First: first, Points: downsample(points, 2)}
	for name, tl := range map[string]Timeline{"whole": whole, "halved": cut} {
		if w := tl.Window(at(1000), at(3000)); w.Covered != 0 {
			t.Errorf("%s: the fall and its catch-up covered %s with %dµs", name, w.Covered, w.CPUMicros)
		}
	}
}

// queueClock answers each call with the next queued time, and the last one
// once the queue is empty.
type queueClock struct {
	mu    sync.Mutex
	now   time.Time
	queue []time.Time
}

func (c *queueClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) > 0 {
		c.now, c.queue = c.queue[0], c.queue[1:]
	}

	return c.now
}

// then queues times this far after the clock's current one.
func (c *queueClock) then(after ...time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range after {
		c.queue = append(c.queue, c.now.Add(d))
	}
}

// lastMark is whether the final point of a summary is marked unseen.
func lastMark(t *testing.T, m *Monitor) bool {
	t.Helper()

	s, ok := m.Final("vm")
	if !ok || len(s.Points) == 0 {
		t.Fatalf("summary %v with %d points", ok, len(s.Points))
	}

	return s.Points[len(s.Points)-1].AfterGap
}

// THE START AND THE FINAL READ ARE DATED BY THEIR END, and one that takes a
// whole interval leaves its interval unseen.
func TestALifecycleReadThatStallsIsUnseen(t *testing.T) {
	tr, target := referenceVM(t)
	clock := &queueClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: clock.Now})
	// Start's placeholder, then the baseline read beginning and ending 2s later.
	clock.then(0, 0, 2*time.Second)
	m.Start("vm", target, 8)
	clock.then(time.Second, time.Second, time.Second)
	m.Tick()
	s, ok := m.Final("vm")
	if !ok || !s.First.Equal(time.Date(2026, 10, 9, 12, 0, 2, 0, time.UTC)) || !s.Points[1].AfterGap {
		t.Errorf("a stalled baseline: first %s, marks %v; want the read's end and the first interval unseen",
			s.First, s.Points)
	}

	tr2, target2 := referenceVM(t)
	clock2 := &queueClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	m2 := runMonitor(t, tr2.root, Options{Interval: time.Second, Now: clock2.Now})
	m2.Start("vm", target2, 8)
	clock2.then(time.Second, time.Second, time.Second)
	m2.Tick()
	// The read takes 1.2s, ending 1.7s after the last point: within the gap
	// rule, so only the stall can mark it.
	clock2.then(500*time.Millisecond, 1700*time.Millisecond)
	if !lastMark(t, m2) {
		t.Error("a final read that stalled for two intervals was not marked")
	}
	if !lastMark(t, m2) {
		t.Error("the stalled final read was forgotten by the Final a failed destroy's retry asks for")
	}
}

// A FINAL READ THAT FAILED OR SAW A COUNTER FALL IS REMEMBERED: the Final a
// failed destroy's retry asks for still marks the interval, though that read
// succeeds.
func TestAFinalReadsEvidenceOutlivesItsSummary(t *testing.T) {
	for name, spoil := range map[string]func(tree){
		"a failed read": func(tr tree) { tr.remove(refCgroup + "/cpu.stat") },
		"a counter fall": func(tr tree) {
			tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 1", 1))
		},
	} {
		tr, target := referenceVM(t)
		now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
		m.Start("vm", target, 8)
		now = now.Add(time.Second)
		m.Tick()
		spoil(tr)
		now = now.Add(time.Second)
		if !lastMark(t, m) {
			t.Errorf("%s: the final interval was not marked", name)
		}
		tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 99999999", 1))
		now = now.Add(time.Second)
		if !lastMark(t, m) {
			t.Errorf("%s: a retry's Final forgot the interval the first one could not see", name)
		}
	}
}
