package usage

import (
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// markedPoints is a series of n one-second points whose values do not
// compress, with the gap marks given.
func markedPoints(n int, marked ...int) []Point {
	points := make([]Point, n)
	for i := range points {
		v := int64(i)*7919 + int64(i*i)%104729
		points[i] = Point{OffsetMillis: int64(i) * 1000, CPUUsage: int64(i)*100_000 + v%1000,
			MemoryCurrent: 1<<30 + v, DiskRead: v * 3, NetRx: v * 5, EnergyActive: v * 11,
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
	if w := tl.Window(tl.First, tl.First.Add(4000*time.Second)); w.Complete() {
		t.Errorf("the mark at 1001s was lost in a stride-%d cut", stride)
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
