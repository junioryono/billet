package usage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixtureCPUs is how many processors procStat's host has, a Mac mini's.
const fixtureCPUs = 18

// procStat is a /proc/stat of fixtureCPUs processors whose busy and idle are
// given in USER_HZ ticks: what a fixture tree answers for the host's CPU on any
// platform.
func procStat(busy, idle int64) string {
	return fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\n", busy, idle) +
		strings.Repeat("cpu0 0 0 0 0 0 0 0 0 0 0\n", fixtureCPUs)
}

// meterRun is a process job (a tart VM) on a host whose power the sampler reads
// from a meter the test sets, one tick in.
type meterRun struct {
	tree       tree
	host       processHost
	watts      *float64
	wattsErr   *error
	now        *time.Time
	busy, idle *int64
	m          *Monitor
}

func newMeterRun(t *testing.T) meterRun {
	t.Helper()

	tr := newTree(t)
	tr.write("/proc/stat", procStat(1000, 9000))
	h := newProcessHost()
	watts := 20.0
	var wattsErr error
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	m := runMonitor(t, tr.root, Options{Interval: time.Second, IdleWatts: 2,
		Meter: func() (float64, error) { return watts, wattsErr },
		Now:   func() time.Time { return now }})
	m.reader.Process = h.reader.Process
	m.Tick()
	m.Start("vm", processTarget, 6)
	busy, idle := int64(1000), int64(9000)

	return meterRun{tree: tr, host: h, watts: &watts, wattsErr: &wattsErr, now: &now, busy: &busy, idle: &idle,
		m: m}
}

// advance moves one second on and ticks: the host busy for 3 s of its 18
// cpu-seconds, the VM for 1.5 s of it, and macOS's own estimate for the VM's
// process a great deal, which is not what a meter's job reports.
func (r meterRun) advance() {
	*r.now = r.now.Add(time.Second)
	*r.busy, *r.idle = *r.busy+300, *r.idle+1500
	r.tree.write("/proc/stat", procStat(*r.busy, *r.idle))
	r.host.counters.UserMicros += 1_500_000
	r.host.counters.Energy += 999_999_999
	r.m.Tick()
}

// A TART VM'S ENERGY WITH A METER IS ITS SHARE OF THE WALL: 20 W for the second
// between two readings is 20 J, of which the 18 J above the host's 2 W idle are
// shared by its 1.5 s of the host's 3 s busy, and the 2 J of idle by its 6 of
// the host's 18 cpus, never macOS's estimate for its process.
func TestAProcessJobsEnergyIsItsShareOfTheWallWithAMeter(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	r.advance()

	s := finalOf(t, r.m)
	if !s.Measured.Energy || !s.EnergyMeter || !s.EnergySplit || s.EnergyProcess {
		t.Fatalf("energy measured %v, meter %v, split %v, process %v; want the wall's share",
			s.Measured.Energy, s.EnergyMeter, s.EnergySplit, s.EnergyProcess)
	}
	if s.EnergyActive != 9_000_000 {
		t.Errorf("active energy %d µJ, want 9000000", s.EnergyActive)
	}
	if s.EnergyIdle != 666_666 {
		t.Errorf("idle energy %d µJ, want 666666", s.EnergyIdle)
	}
}

// A READING THE SAMPLER COULD NOT TAKE LEAVES ITS JOBS' ENERGY UNMEASURED, the
// interval it ends and the one after it alike, never charged as nothing drawn;
// two good readings in a row are measured.
func TestAReadingTheMeterMissedLeavesEnergyUnmeasured(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		failed   []bool
		measured bool
	}{
		"nothing missed":                    {[]bool{false, false}, true},
		"a failed reading":                  {[]bool{false, true}, false},
		"a failed reading, then a good one": {[]bool{false, true, false}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newMeterRun(t)
			for _, failed := range tc.failed {
				*r.wattsErr = nil
				if failed {
					*r.wattsErr = errors.New("the plug did not answer")
				}
				r.advance()
			}

			if s := finalOf(t, r.m); s.Measured.Energy != tc.measured {
				t.Errorf("energy measured %v (%d µJ), want %v", s.Measured.Energy, s.EnergyActive, tc.measured)
			}
		})
	}
}

// A JOB STARTED BETWEEN A FAILED READING AND THE NEXT GOOD ONE HAS ITS FIRST
// INTERVAL UNKNOWN: no reading begins it, so the good one alone cannot say what
// was drawn across it.
func TestAJobStartedDuringAStallHasItsFirstIntervalUnknown(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	*r.wattsErr = errors.New("the plug did not answer")
	r.advance()
	r.m.Start("late", processTarget, 6)
	*r.wattsErr = nil
	r.advance()

	if s, ok := r.m.Final("late"); !ok || s.Measured.Energy {
		t.Errorf("a job started during the stall measured %v (%d µJ), want its energy unmeasured",
			s.Measured.Energy, s.EnergyActive)
	}
}

// A JOB STARTED AFTER THE METER RECOVERED IS CHARGED ITS OWN SHARE ALONE: the
// energy of the stall before it is in no reading the sampler shares, so it lands
// on nobody, rather than on whoever runs when the readings resume.
func TestAJobAfterAStallIsNotChargedTheStall(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	*r.wattsErr = errors.New("the plug did not answer")
	r.advance()
	r.advance()
	*r.wattsErr = nil
	*r.watts = 200 // a burst the stall hid; none of it is the next job's.
	r.advance()
	*r.watts = 20
	r.m.Start("next", processTarget, 6)
	r.advance()

	s, ok := r.m.Final("next")
	if !ok || !s.Measured.Energy {
		t.Fatalf("the job after the stall measured %v (%v); want its own interval measured", s.Measured.Energy, ok)
	}
	// One second between readings of 200 W and 20 W is 110 J: 108 J above the 2 W
	// idle, shared with the first VM (both 1.5 s of the host's 3 s busy, by the
	// same process counters).
	if s.EnergyActive != 54_000_000 {
		t.Errorf("the job after the stall was charged %d µJ active, want 54000000", s.EnergyActive)
	}
}
