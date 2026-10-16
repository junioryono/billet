package usage

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// handedOver is a checkpoint of the reference VM after a start and three
// ticks a second apart, as the process handing it over wrote it and the next
// read it back, with the clock both share.
func handedOver(t *testing.T) (tree, Target, Checkpoint, *time.Time) {
	t.Helper()

	tr, target := referenceVM(t)
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	a := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
	a.Start("vm", target, 8)
	for range 3 {
		now = now.Add(time.Second)
		a.Tick()
	}
	cp, ok := a.Checkpoint("vm")
	if !ok {
		t.Fatal("no checkpoint of a measured job")
	}
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadCheckpoint(data)
	if err != nil {
		t.Fatalf("ReadCheckpoint: %v", err)
	}

	return tr, target, back, &now
}

// A JOB HANDED OVER KEEPS ITS SERIES (#384): the next process continues it from
// the checkpoint, every sample the first took is still there, and the time
// between the two processes is one interval marked handover, which no window
// counts and the observer hears.
func TestACheckpointCarriesTheSeriesAcrossAHandover(t *testing.T) {
	tr, target, cp, now := handedOver(t)
	start := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

	*now = now.Add(5 * time.Second)
	o := &recordingObserver{}
	b := runMonitor(t, tr.root, Options{Interval: time.Second, Observer: o, Now: func() time.Time { return *now }})
	if err := b.Resume("vm", target, 8, &cp); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for range 2 {
		*now = now.Add(time.Second)
		b.Tick()
	}
	s, ok := b.Final("vm")
	if !ok {
		t.Fatal("no summary")
	}

	if !s.First.Equal(start) {
		t.Errorf("the series begins at %s, want the first process's %s", s.First, start)
	}
	var offsets []int64
	var gaps []GapCause
	for _, p := range s.Points {
		offsets = append(offsets, p.OffsetMillis)
		gaps = append(gaps, p.Gap)
	}
	if want := []int64{0, 1000, 2000, 3000, 8000, 9000, 10000, 10000}; !slices.Equal(offsets, want) {
		t.Errorf("offsets = %v, want %v", offsets, want)
	}
	if want := []GapCause{GapNone, GapNone, GapNone, GapNone, GapHandover, GapNone, GapNone, GapNone}; !slices.Equal(gaps, want) {
		t.Errorf("marks = %v, want only the handover's", gaps)
	}
	if s.Samples != 8 {
		t.Errorf("%d samples, want the first process's 4 and the second's 4, its final read among them", s.Samples)
	}
	tl := Timeline{First: s.First, Points: s.Points}
	if w := tl.Window(start, start.Add(3*time.Second)); !w.Complete() {
		t.Errorf("the first process's time is covered %s of %s", w.Covered, w.Span)
	}
	if got := tl.Gaps(); len(got) != 1 || got[0].Cause != GapHandover || got[0].Length() != 5*time.Second {
		t.Errorf("gaps = %+v, want the five seconds between the processes", got)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !slices.Equal(o.gaps, []string{"handover"}) {
		t.Errorf("the observer heard %v, want the handover", o.gaps)
	}
}

// A RESUME WHOSE BASELINE COULD NOT READ A GROUP KEEPS THE GAP OPEN: no sample
// holding the old process's value at the resume's time is kept, and the
// interval from the last sample before the handover to the first after it
// that read everything is not counted.
func TestAResumeWhoseBaselineFailedKeepsTheGapOpen(t *testing.T) {
	tr, target, cp, now := handedOver(t)
	start := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

	*now = now.Add(5 * time.Second)
	tr.remove(refCgroup + "/cpu.stat")
	b := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return *now }})
	if err := b.Resume("vm", target, 8, &cp); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 99999999", 1))
	*now = now.Add(time.Second)
	b.Tick()
	s, ok := b.Final("vm")
	if !ok {
		t.Fatal("no summary")
	}
	var offsets []int64
	for _, p := range s.Points {
		offsets = append(offsets, p.OffsetMillis)
	}
	if want := []int64{0, 1000, 2000, 3000, 9000, 9000}; !slices.Equal(offsets, want) {
		t.Fatalf("offsets = %v, want no sample at the resume (8000)", offsets)
	}
	if s.Points[4].Gap == GapNone {
		t.Errorf("the first sample after the handover is unmarked: %+v", s.Points[4])
	}
	tl := Timeline{First: s.First, Points: s.Points}
	if w := tl.Window(start.Add(3*time.Second), start.Add(9*time.Second)); w.Covered != 0 || w.CPUMicros != 0 {
		t.Errorf("the handover and the failed baseline were covered %s with %d µs", w.Covered, w.CPUMicros)
	}
}

// A CARRIED JOB WHOSE BASELINE THE SAMPLER NEVER TOOK STILL MARKS THE
// HANDOVER: it stays pending, and its final read must not give the old
// process's last sample's interval everything that happened since.
func TestACarriedJobLeftPendingStillMarksTheHandover(t *testing.T) {
	tr, target, cp, now := handedOver(t)

	*now = now.Add(5 * time.Second)
	b := NewMonitor(tr.root, Options{Interval: time.Second, Now: func() time.Time { return *now }})
	b.ticker, b.limit = false, testLimit
	var block atomic.Bool
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	b.afterTickRead = func() {
		if block.Load() {
			entered <- struct{}{}
			<-release
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	block.Store(true)
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		b.Tick()
	}()
	// THE SAMPLER IS BUSY inside that tick, so the resume cannot reach it and
	// the job stays pending with the first process's samples.
	<-entered
	if err := b.Resume("vm", target, 8, &cp); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// AND A STOP NOW CAN STILL HAND IT ON, as it was carried.
	again, ok := b.Checkpoint("vm")
	if !ok || !slices.Equal(again.Series, cp.Series) || again.Unseen != GapHandover {
		t.Errorf("a carried job awaiting its baseline was checkpointed %v (unseen %v), want its series as "+
			"carried with the handover marked", ok, again.Unseen)
	}
	close(release)
	<-ticked

	s, ok := b.Final("vm")
	if !ok || len(s.Points) == 0 {
		t.Fatalf("summary %v with %d points", ok, len(s.Points))
	}
	if last := s.Points[len(s.Points)-1]; last.Gap != GapHandover {
		t.Errorf("the last sample, after the handover, is marked %v, want %v", last.Gap, GapHandover)
	}
}

// A CHECKPOINT OF OTHER COMPUTE, OR SAMPLED AT ANOTHER INTERVAL, IS NOT USED:
// the job is measured from now and the error says why.
func TestACheckpointThatDoesNotContinueTheJobIsNotUsed(t *testing.T) {
	for name, tc := range map[string]struct {
		change   func(*Target, *time.Duration)
		why      string
		interval time.Duration
	}{
		"another vmm":      {change: func(t *Target, _ *time.Duration) { t.PIDStart++ }, why: "other compute"},
		"another cgroup":   {change: func(t *Target, _ *time.Duration) { t.CgroupDir += "-2" }, why: "other compute"},
		"another interval": {change: func(_ *Target, d *time.Duration) { *d = 2 * time.Second }, why: "sampled every"},
	} {
		t.Run(name, func(t *testing.T) {
			tr, target, cp, now := handedOver(t)
			interval := time.Second
			tc.change(&target, &interval)
			resumedAt := *now

			b := runMonitor(t, tr.root, Options{Interval: interval, Now: func() time.Time { return *now }})
			err := b.Resume("vm", target, 8, &cp)
			if err == nil || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("Resume = %v, want it refused saying %q", err, tc.why)
			}
			s, ok := b.Final("vm")
			if !ok || !s.First.Equal(resumedAt) {
				t.Errorf("the series begins at %s, want the resume's %s", s.First, resumedAt)
			}
		})
	}
}

// A RESUMED JOB COUNTS NO HARDWARE COUNTERS AND IS GIVEN NO PACKAGE ENERGY:
// what happened before this process took it was counted by nobody here. The
// same host started instead counts both, which is what makes this a test of
// the resume.
func TestAResumedJobCountsNoCountersAndNoEnergy(t *testing.T) {
	src := newFakeCounters()
	_, target, m := countedVM(t, src)
	if err := m.Resume("vm", target, 8, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	src.set(refVCPUs, reading(10*time.Second, 10*time.Second, 1000, 1210, 300, 9, 4, 120))
	m.Tick()
	if s, ok := m.Final("vm"); !ok || s.Counters != nil {
		t.Errorf("a resumed job's summary carries counters %+v", s.Counters)
	}

	for _, resume := range []bool{false, true} {
		tr, target := referenceVM(t)
		tr.write("/proc/stat", refProcStatBefore+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
		tr.write(raplZone+"/max_energy_range_uj", "65532610987\n")
		tr.write(raplZone+"/energy_uj", "38419855704\n")
		now := time.Date(2026, 9, 25, 22, 50, 0, 0, time.UTC)
		m := runMonitor(t, tr.root, Options{Interval: 30 * time.Second, RAPL: true, IdleWatts: 73.5,
			Now: func() time.Time { return now }})
		m.Tick()
		if resume {
			if err := m.Resume("vm", target, 8, nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
		} else {
			m.Start("vm", target, 8)
		}
		now = now.Add(10 * time.Second)
		tr.write("/proc/stat", refProcStatAfter+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
		tr.write(raplZone+"/energy_uj", "39826460727\n")
		tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 69571715", 1))
		m.Tick()
		s, ok := m.Final("vm")
		if !ok || s.Measured.Energy == resume {
			t.Errorf("resumed %v: energy measured %v", resume, s.Measured.Energy)
		}
	}
}

// RESUMING A JOB THIS PROCESS ALREADY MEASURES CHANGES NOTHING: recovery runs
// at every registration and finds this process's own jobs too.
func TestResumingAJobAlreadyMeasuredChangesNothing(t *testing.T) {
	tr, target := referenceVM(t)
	start := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	now := start
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	now = now.Add(time.Second)
	m.Tick()
	if err := m.Resume("vm", target, 8, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	now = now.Add(time.Second)
	m.Tick()
	s, ok := m.Final("vm")
	if !ok || !s.First.Equal(start) || len(s.Points) != 4 {
		t.Errorf("a job resumed while measured begins %s with %d points, want %s and 4", s.First, len(s.Points),
			start)
	}
}

// A CHECKPOINT BILLET DID NOT WRITE IS REFUSED, whole: another schema, a field
// this build does not know, something after it, a series that does not
// decode, no samples, a cause past the last, and one past its bound.
func TestAMalformedCheckpointIsRefused(t *testing.T) {
	_, _, cp, _ := handedOver(t)
	good, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	spoil := func(f func(*Checkpoint)) []byte {
		c := cp
		f(&c)
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, data := range map[string][]byte{
		"another schema":   spoil(func(c *Checkpoint) { c.Schema = 2 }),
		"an unknown field": []byte(strings.Replace(string(good), `{"schema"`, `{"extra":1,"schema"`, 1)),
		"trailing data":    append(slices.Clone(good), []byte(` {}`)...),
		"a broken series":  spoil(func(c *Checkpoint) { c.Series = []byte{1, 2, 3} }),
		"no samples":       spoil(func(c *Checkpoint) { c.Samples = 0 }),
		"no interval":      spoil(func(c *Checkpoint) { c.IntervalMillis = 0 }),
		"an unknown cause": spoil(func(c *Checkpoint) { c.Unseen = gapCauses }),
		"past its bound":   make([]byte, MaxCheckpointBytes+1),
	} {
		if _, err := ReadCheckpoint(data); err == nil {
			t.Errorf("%s: a checkpoint was read", name)
		}
	}
	if _, err := ReadCheckpoint(good); err != nil {
		t.Errorf("the checkpoint itself was refused: %v", err)
	}
}
