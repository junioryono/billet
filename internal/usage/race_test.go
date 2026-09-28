package usage

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A TICK THAT READS AFTER A FINAL'S CLOCK WINS, and the final series still
// never goes back in time: the decoder refuses one that does, which would make
// the stored series unreadable.
func TestAFinalOvertakenByATickKeepsTheSeriesInOrder(t *testing.T) {
	h := newEnergyHost(t, 0)
	h.m.Start("vm", h.target, 8)
	h.advance(10*time.Second, "69571715")
	h.m.Tick()

	*h.now = h.now.Add(time.Second)
	h.m.afterFinalRead = func() {
		h.m.afterFinalRead = nil
		h.advance(time.Second, "70000000")
		h.m.Tick()
	}
	s := finalOf(t, h.m)
	if s.Latest.CPUUsage != 70_000_000 {
		t.Errorf("cpu = %d, want the overtaking tick's 70000000, not the final's older read", s.Latest.CPUUsage)
	}
	data, _, err := EncodeSeries(s.Points, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSeries(data); err != nil {
		t.Errorf("the final series does not decode: %v", err)
	}
}

// A JOB FORGOTTEN AND A NEW ONE STARTED UNDER THE SAME KEY WHILE A TICK READ IS
// A DIFFERENT JOB: the old job's counters are not applied to it.
func TestATickDoesNotApplyOneJobsReadToItsSuccessor(t *testing.T) {
	h := newEnergyHost(t, 0)
	h.m.Start("vm", h.target, 8)

	successor := h.target
	successor.CgroupDir = "/sys/fs/cgroup/successor"
	h.tr.write(successor.CgroupDir+"/cpu.stat", "usage_usec 5\nuser_usec 5\nsystem_usec 0\n")
	h.advance(10*time.Second, "69571715")
	h.m.afterTickRead = func() {
		h.m.afterTickRead = nil
		h.m.Forget("vm")
		h.m.Start("vm", successor, 8)
	}
	h.m.Tick()

	// READ STRAIGHT AFTER THE TICK: a Final would re-read the successor's own
	// counters and cover up what the tick applied.
	h.m.mu.Lock()
	successorJob := h.m.jobs["vm"]
	got, points := successorJob.latest.CPUUsage, successorJob.points
	h.m.mu.Unlock()
	if got != 5 {
		t.Errorf("the successor's cpu = %d, want its own 5, not its predecessor's", got)
	}
	for _, p := range points {
		if p.CPUUsage != 5 {
			t.Errorf("the successor's series holds %d, a reading of its predecessor", p.CPUUsage)
		}
	}
}

// A START OVERTAKEN BY A TICK READS AGAIN, so its baseline pairs with the
// host's; overtaken every time, its first interval is not attributed.
func TestAStartOvertakenByATickReadsItsBaselineAgain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overtakes int
		measured  bool
	}{
		{"once", 1, true},
		{"every attempt", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEnergyHost(t, 0)
			left := tc.overtakes
			h.m.afterStartRead = func() {
				if left == 0 {
					return
				}
				left--
				*h.now = h.now.Add(time.Second)
				h.m.Tick()
			}
			h.m.Start("vm", h.target, 8)
			h.m.afterStartRead = nil
			h.advance(10*time.Second, "69571715")
			h.m.Tick()

			if got := finalOf(t, h.m).Measured.Energy; got != tc.measured {
				t.Errorf("energy measured = %v, want %v", got, tc.measured)
			}
		})
	}
}

// A CHANGE IN THE HOST'S CPU COUNT BREAKS THE INTERVAL rather than dividing
// the idle pool by a denominator the interval did not have.
func TestAChangedCPUCountIsNotAttributed(t *testing.T) {
	h := newEnergyHost(t, 73.5)
	h.m.Start("vm", h.target, 8)
	h.advance(10*time.Second, "69571715")
	h.tr.write("/proc/stat", refProcStatAfter+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 63))
	h.m.Tick()

	if finalOf(t, h.m).Measured.Energy {
		t.Error("energy across a change from 128 to 64 CPUs was reported as measured")
	}
}

// RESERVED CPUS BEYOND THE HOST'S SHARE THE IDLE POOL, never multiply it.
func TestOverReservedCPUsShareTheIdlePool(t *testing.T) {
	h := newEnergyHost(t, 73.5)
	h.m.Start("vm", h.target, 256)
	h.advance(10*time.Second, "69571715")
	h.m.Tick()

	s := finalOf(t, h.m)
	if !s.Measured.Energy || s.EnergyIdle != 735_000_000 {
		t.Errorf("a job reserving twice the host got %d µJ idle (measured %v), want the whole 735000000",
			s.EnergyIdle, s.Measured.Energy)
	}
}

// step moves the host by the reference interval's amounts again, n intervals
// after the reference: busy ticks, package energy and the job's CPU all grow,
// so an interval built from it is consistent unless something else is wrong.
func (h energyHost) step(d time.Duration, n int64) {
	*h.now = h.now.Add(d)
	user := 728214069 + n*15353
	h.tr.write("/proc/stat", fmt.Sprintf("cpu  %d 39636 158013305 %d 62999128 0 4781732 0 688798719 927\n",
		user, 37269744359+n*107845)+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 128))
	h.tr.write(raplZone+"/energy_uj", fmt.Sprintf("%d\n", 39826460727+n*1_000_000_000))
	h.tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613",
		fmt.Sprintf("usage_usec %d", 69571715+n*5_000_000), 1))
}

// A GAP OF THREE INTERVALS BREAKS ENERGY FOR GOOD, even though sampling resumed
// before the job ended and the gap is shorter than the counter's wrap. Every
// interval moves the counters, so the gap is the only thing wrong.
func TestAGapIsRememberedAfterSamplingResumes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gap      time.Duration
		measured bool
	}{
		{"two intervals", 20 * time.Second, true},
		{"three intervals", 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEnergyHost(t, 0)
			h.m.Start("vm", h.target, 8)
			h.advance(10*time.Second, "69571715")
			h.m.Tick()
			h.step(tc.gap, 1)
			h.m.Tick()
			h.step(10*time.Second, 2)
			h.m.Tick()

			if got := finalOf(t, h.m).Measured.Energy; got != tc.measured {
				t.Errorf("energy measured = %v after a %s gap, want %v", got, tc.gap, tc.measured)
			}
		})
	}
}
