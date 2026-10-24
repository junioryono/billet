package usage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A FOLDED TICK KEEPS NO POINT IN THE JOB'S SERIES: the recovery's point spans
// both intervals and is marked unseen, so the series never shows the failed
// second drawing nothing and the next drawing both.
func TestAFoldedTickKeepsNoPointInTheSeries(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	r.advance()
	*r.wattsErr = errors.New("the plug did not answer")
	r.advance()
	*r.wattsErr = nil
	r.advance()

	s := finalOf(t, r.m)
	if !s.Measured.Energy {
		t.Fatal("the folded span was not measured")
	}
	var at3 *Point
	for i, p := range s.Points {
		if p.OffsetMillis == 2000 {
			t.Errorf("the series kept a point at the folded tick: %+v", p)
		}
		if p.OffsetMillis == 3000 && at3 == nil {
			at3 = &s.Points[i]
		}
	}
	if at3 == nil || at3.Gap != GapFailedRead {
		t.Errorf("the recovery's point is %+v, want one marked %v", at3, GapFailedRead)
	}
}

// WHAT A FOLDED TICK'S READS SAW IS KEPT: a job counter that fell, or a host that
// changed its processor count, during the span leaves its energy unknown even
// when the recovery's reads look continuous with the span's start.
func TestAFoldedTickKeepsWhatItsReadsSaw(t *testing.T) {
	t.Parallel()

	for name, during := range map[string]func(r meterRun){
		"the job's counter fell": func(r meterRun) {
			r.host.counters.UserMicros -= 5_000_000
			*r.now = r.now.Add(time.Second)
			*r.busy, *r.idle = *r.busy+300, *r.idle+1500
			r.tree.write("/proc/stat", procStat(*r.busy, *r.idle))
			r.m.Tick()
			// THE RECOVERY LOOKS CONTINUOUS with the span's start: 2 s more than
			// the baseline, within the host's 6 s busy, so only the fall the
			// folded tick saw tells.
			r.host.counters.UserMicros += 5_500_000
		},
		// THE FALL IS AFTER THE FOLDED TICK: it read 2 s on, the recovery reads
		// 1 s back from that, still 1 s past the baseline.
		"the job's counter fell after the folded tick": func(r meterRun) {
			r.host.counters.UserMicros += 2_000_000
			*r.now = r.now.Add(time.Second)
			*r.busy, *r.idle = *r.busy+300, *r.idle+1500
			r.tree.write("/proc/stat", procStat(*r.busy, *r.idle))
			r.m.Tick()
			r.host.counters.UserMicros -= 2_500_000
		},
		"the host's busy time fell after the folded tick": func(r meterRun) {
			*r.now = r.now.Add(time.Second)
			*r.busy, *r.idle = *r.busy+600, *r.idle+1200
			r.tree.write("/proc/stat", procStat(*r.busy, *r.idle))
			r.host.counters.UserMicros += 1_500_000
			r.m.Tick()
			*r.busy -= 400
		},
		"the host's processor count changed": func(r meterRun) {
			*r.now = r.now.Add(time.Second)
			*r.busy, *r.idle = *r.busy+300, *r.idle+1500
			r.tree.write("/proc/stat", fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\n", *r.busy, *r.idle)+
				strings.Repeat("cpu0 0 0 0 0 0 0 0 0 0 0\n", fixtureCPUs-1))
			r.host.counters.UserMicros += 1_500_000
			r.m.Tick()
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newMeterRun(t)
			*r.wattsErr = errors.New("the plug did not answer")
			during(r)
			*r.wattsErr = nil
			r.advance()

			if s := finalOf(t, r.m); s.Measured.Energy {
				t.Errorf("energy across a span whose reads saw %s measured %d µJ, want it unmeasured", name,
					s.EnergyActive)
			}
		})
	}
}

// A JOB STARTED LATE IN A PACKAGE SPAN IS FOLDED BY ITS OWN SPAN: started two
// intervals after the last good read, through a failed read at the third, it is
// shared the fourth's recovery, as a job started late in an unbroken span is.
func TestAJobStartedLateInAPackageSpanIsFoldedByItsOwnSpan(t *testing.T) {
	t.Parallel()

	h := newEnergyHost(t, 0)
	*h.now = h.now.Add(20 * time.Second)
	h.m.Start("vm", h.target, 8)
	h.tr.remove(raplZone + "/energy_uj")
	*h.now = h.now.Add(10 * time.Second)
	h.m.Tick()
	h.advance(10*time.Second, "69571715")
	h.m.Tick()

	if s := finalOf(t, h.m); !s.Measured.Energy || s.EnergyActive <= 0 {
		t.Errorf("a job started late in a package span measured %v (%d µJ), want its share",
			s.Measured.Energy, s.EnergyActive)
	}
}

// A FAILURE THAT OUTLASTS THE SPAN KEEPS THE SERIES: once no reading can fold the
// span, the energy is broken and the job's points are kept as ever, so a host
// whose package counter never answers still records the job's CPU.
func TestAFailureThatOutlastsTheSpanKeepsTheSeries(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	*r.wattsErr = errors.New("the plug did not answer")
	for range 5 {
		r.advance()
	}

	s := finalOf(t, r.m)
	if s.Measured.Energy {
		t.Errorf("energy across five failed readings measured %d µJ, want it unmeasured", s.EnergyActive)
	}
	kept := 0
	for _, p := range s.Points {
		if p.OffsetMillis >= 3000 && p.OffsetMillis <= 5000 {
			kept++
		}
	}
	if kept < 3 {
		t.Errorf("the series kept %d of the points from 3 s to 5 s, want them all once the span cannot fold: %+v",
			kept, s.Points)
	}
}
