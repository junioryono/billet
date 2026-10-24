package usage

import (
	"errors"
	"fmt"
	"os"
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

// A READING THE SAMPLER MISSED IS FOLDED INTO THE NEXT: the good reading after it
// shares the wall's energy across both intervals, by the trapezoid between the
// readings either side, and the CPU counted across both, so one slow answer from
// the plug costs no job its energy. A job that ends before a good reading follows
// has energy no reading shared, and is unmeasured.
func TestAReadingTheMeterMissedIsFoldedIntoTheNext(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		failed   []bool
		measured bool
		active   int64
	}{
		// 18 J above idle each second, the VM's half of it each time.
		"nothing missed":   {failed: []bool{false, false}, measured: true, active: 18_000_000},
		"a failed reading": {failed: []bool{false, true}},
		// 9 J for the first second, then 36 J across the two the good reading
		// spans, the VM's 3 s of the host's 6 s busy.
		"a failed reading, then a good one": {failed: []bool{false, true, false}, measured: true, active: 27_000_000},
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

			s := finalOf(t, r.m)
			if s.Measured.Energy != tc.measured {
				t.Fatalf("energy measured %v (%d µJ), want %v", s.Measured.Energy, s.EnergyActive, tc.measured)
			}
			if tc.measured && s.EnergyActive != tc.active {
				t.Errorf("active energy %d µJ, want %d", s.EnergyActive, tc.active)
			}
		})
	}
}

// A JOB STARTED BETWEEN A FAILED READING AND THE NEXT GOOD ONE IS SHARED THE SPAN
// BY ITS CPU: the good reading's span begins at the last good one, before the job,
// and the job's share is its CPU since its own start, so it is given nothing it
// did not use; its idle share is for the part of the span it existed.
func TestAJobStartedDuringAStallIsSharedTheSpanByItsCPU(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	*r.wattsErr = errors.New("the plug did not answer")
	r.advance()
	r.m.Start("late", processTarget, 6)
	*r.wattsErr = nil
	r.advance()

	s, ok := r.m.Final("late")
	if !ok || !s.Measured.Energy {
		t.Fatalf("a job started during the stall measured %v (%v), want its share", s.Measured.Energy, ok)
	}
	// 36 J above idle across the 2 s span, by its 1.5 s of the host's 6 s busy.
	if s.EnergyActive != 9_000_000 {
		t.Errorf("active energy %d µJ, want 9000000", s.EnergyActive)
	}
	// 2 W of idle for the 1 s of the 2 s span it existed, on 6 of 18 cpus.
	if s.EnergyIdle != 666_666 {
		t.Errorf("idle energy %d µJ, want 666666", s.EnergyIdle)
	}
}

// A STALL OF THREE INTERVALS OR MORE IS NOT BRIDGED: the trapezoid across it is a
// guess, so every job sampled through it is unmeasured.
func TestAStallOfThreeIntervalsIsNotBridged(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	*r.wattsErr = errors.New("the plug did not answer")
	r.advance()
	r.advance()
	*r.wattsErr = nil
	r.advance()

	if s := finalOf(t, r.m); s.Measured.Energy {
		t.Errorf("energy across a 3 s stall measured %d µJ, want it unmeasured", s.EnergyActive)
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

// THE WALL'S ENERGY IS SHARED OVER THE INTERVAL BETWEEN THE PLUG'S ANSWERS, the
// one the CPU is read across just after each, however long the plug takes: an
// answer that comes 400 ms later than the last makes that interval 1.4 s, of
// both the wall's 20 W and the host's 2 W idle. The host and the VM do their
// work while the plug is answering, so only CPU read after it sees any.
func TestTheWallIsSharedOverTheIntervalItsReadingsSpan(t *testing.T) {
	t.Parallel()

	tr := newTree(t)
	tr.write("/proc/stat", procStat(1000, 9000))
	h := newProcessHost()
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	var answering func()
	m := runMonitor(t, tr.root, Options{Interval: time.Second, IdleWatts: 2,
		Meter: func() (float64, error) {
			if answering != nil {
				answering()
			}

			return 20, nil
		},
		Now: func() time.Time { return now }})
	m.reader.Process = h.reader.Process
	m.Tick()
	m.Start("vm", processTarget, 6)

	now = now.Add(time.Second)
	answering = func() {
		now = now.Add(400 * time.Millisecond)
		tr.write("/proc/stat", procStat(1300, 10500))
		h.counters.UserMicros += 1_500_000
	}
	m.Tick()

	s := finalOf(t, m)
	if !s.Measured.Energy {
		t.Fatal("energy across a slow answer was not measured")
	}
	// (20 W - 2 W idle) * 1.4 s * 1.5 / 3.
	if s.EnergyActive != 12_600_000 {
		t.Errorf("active energy %d µJ, want 12600000 over the 1.4 s the readings span", s.EnergyActive)
	}
	// 2 W * 1.4 s * 6 / 18.
	if s.EnergyIdle != 933_333 {
		t.Errorf("idle energy %d µJ, want 933333 over the 1.4 s the readings span", s.EnergyIdle)
	}
}

// A JOB WHOSE CPU IS READ WELL AFTER THE HOST'S IS NOT SHARED THE INTERVAL: its
// CPU runs past the one the plug and the host were read across, whether its own
// read was slow or it waited behind another job's. Its other usage is measured
// as ever; a CPU read a little after the host's, as every job's is, is shared.
func TestAJobReadLongAfterTheHostIsNotSharedTheInterval(t *testing.T) {
	t.Parallel()

	for late, measured := range map[time.Duration]bool{
		50 * time.Millisecond:  true,
		300 * time.Millisecond: false,
	} {
		tr := newTree(t)
		tr.write("/proc/stat", procStat(1000, 9000))
		now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
		m := runMonitor(t, tr.root, Options{Interval: time.Second, IdleWatts: 2,
			Meter: func() (float64, error) { return 20, nil },
			Now:   func() time.Time { return now }})
		// TWO VMS, AND WHICHEVER IS READ FIRST IN A TICK TAKES late: the second
		// answers at once, after waiting behind it.
		cpu := map[int]int64{1: 0, 2: 0}
		var delayed bool
		m.reader.Process = func(pid int) (ProcessCounters, error) {
			c := refVMProcess
			c.UserMicros, c.SystemMicros = cpu[pid], 0
			if !delayed {
				delayed = true
				now = now.Add(late)
			}

			return c, nil
		}
		m.Tick()
		m.Start("vm1", Target{PID: 1, PIDStart: refVMProcess.Start, Process: true}, 6)
		m.Start("vm2", Target{PID: 2, PIDStart: refVMProcess.Start, Process: true}, 6)

		now = now.Add(time.Second)
		tr.write("/proc/stat", procStat(1300, 10500))
		cpu[1], cpu[2] = 1_000_000, 1_000_000
		delayed = false
		m.Tick()

		for _, key := range []string{"vm1", "vm2"} {
			s, ok := m.Final(key)
			if !ok || !s.Measured.CPU {
				t.Fatalf("%s's CPU was not measured (%v)", key, ok)
			}
			if s.Measured.Energy != measured {
				t.Errorf("with the first read %s after the host's, %s measured energy %v (%d µJ), want %v",
					late, key, s.Measured.Energy, s.EnergyActive, measured)
			}
		}
	}
}

// A JOB WHOSE START THE SAMPLER NEVER TOOK, busy past the limit on a plug that
// would not answer, is given its baseline by the next tick and sampled from
// then, its energy unknown: it is never left pending, unsampled, for the rest of
// its life.
func TestAStartTheSamplerNeverTookIsBaselinedByTheNextTick(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.m.opts.Meter = func() (float64, error) {
		entered <- struct{}{}
		<-release

		return 20, nil
	}
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		r.m.Tick()
	}()
	<-entered
	r.m.limit = 10 * time.Millisecond
	r.m.Start("late", processTarget, 6)
	r.m.limit = lifecycleLimit
	close(release)
	<-ticked

	r.m.opts.Meter = func() (float64, error) { return 20, nil }
	r.advance()
	r.advance()

	// The baseline, the two ticks after it and the final read: a job left
	// pending is read by its final alone, beside its placeholder.
	s, ok := r.m.Final("late")
	if !ok || len(s.Points) != 4 {
		t.Fatalf("a start the sampler never took kept %d points (%v); want 4, sampled by the ticks after",
			len(s.Points), ok)
	}
	if s.Measured.Energy {
		t.Errorf("a start the sampler never took measured %d µJ; want its energy unknown", s.EnergyActive)
	}
}

// TWO GOOD READINGS FAR APART LEAVE THE GAP UNKNOWN: a job started a second
// before the second reading is not given a share of the ten seconds the
// trapezoid would guess at.
func TestALongGapBetweenGoodReadingsIsUnknown(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	*r.now = r.now.Add(9 * time.Second)
	r.m.Start("late", processTarget, 6)
	r.advance()

	if s, ok := r.m.Final("late"); !ok || s.Measured.Energy {
		t.Errorf("a job started near the end of a 10 s gap measured %v (%d µJ), want its energy unmeasured",
			s.Measured.Energy, s.EnergyActive)
	}
}

// A CGROUP JOB IS JUDGED BY WHEN ITS CPU WAS READ: a VM whose cpu.stat answers
// 300 ms after the host's is not shared the interval, and one whose memory,
// read after its CPU, answers as late still is.
func TestACgroupJobIsJudgedByWhenItsCPUWasRead(t *testing.T) {
	t.Parallel()

	for file, measured := range map[string]bool{"/cpu.stat": false, "/memory.current": true} {
		tr, target := referenceVM(t)
		tr.write("/proc/stat", procStat(1000, 9000))
		now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
		m := runMonitor(t, tr.root, Options{Interval: time.Second, IdleWatts: 2,
			Meter: func() (float64, error) { return 20, nil },
			Now:   func() time.Time { return now }})
		var slow bool
		m.reader.ReadFile = func(name string) ([]byte, error) {
			if slow && strings.HasSuffix(name, file) {
				now = now.Add(300 * time.Millisecond)
			}

			return os.ReadFile(name)
		}
		m.Tick()
		m.Start("vm", target, 6)

		now = now.Add(time.Second)
		tr.write("/proc/stat", procStat(1300, 10500))
		tr.write(refCgroup+"/cpu.stat",
			strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 63338613", 1))
		slow = true
		m.Tick()
		slow = false

		if s := finalOf(t, m); s.Measured.Energy != measured {
			t.Errorf("a VM whose %s answered 300 ms late measured energy %v (%d µJ), want %v",
				file, s.Measured.Energy, s.EnergyActive, measured)
		}
	}
}

// A BASELINE WHOSE LATER GROUPS TAKE THREE INTERVALS LEAVES THE GAP UNKNOWN: the
// job's CPU was read at its start, and the time to the next tick is counted from
// then, not from when its memory answered. RAPL's, since a meter refuses so long
// a gap between its own readings anyway; a baseline that is only a little slow
// is measured.
func TestASlowBaselineDoesNotHideAGap(t *testing.T) {
	t.Parallel()

	for slow, measured := range map[time.Duration]bool{
		100 * time.Millisecond: true,
		31 * time.Second:       false,
	} {
		h := newEnergyHost(t, 73.5)
		var slowing bool
		h.m.reader.ReadFile = func(name string) ([]byte, error) {
			if slowing && strings.HasSuffix(name, "/memory.current") {
				*h.now = h.now.Add(slow)
			}

			return os.ReadFile(name)
		}
		slowing = true
		h.m.Start("vm", h.target, 8)
		slowing = false
		h.advance(100*time.Millisecond, "69571715")
		h.m.Tick()

		s := finalOf(t, h.m)
		if s.Measured.Energy != measured {
			t.Errorf("a job whose baseline's memory took %s measured energy %v (%d µJ), want %v",
				slow, s.Measured.Energy, s.EnergyActive, measured)
		}
		// 73.5 W of idle for the 0.2 s since the baseline's CPU, 8 of 128 cpus;
		// counted from when its memory answered it would be half that.
		if measured && s.EnergyIdle != 918_750 {
			t.Errorf("a job whose baseline's memory took %s holds %d µJ of idle, want 918750", slow, s.EnergyIdle)
		}
	}
}
