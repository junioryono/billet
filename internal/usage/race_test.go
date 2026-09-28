package usage

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testLimit is how long Start and Final wait for a sampler in these tests.
const testLimit = 200 * time.Millisecond

// runMonitor is a monitor whose Run is serving, ticking only when the test
// calls Tick, with a short lifecycle limit. Cleanup stops Run and proves it
// returned.
func runMonitor(t *testing.T, root string, opts Options) *Monitor {
	t.Helper()

	m := NewMonitor(root, opts)
	m.ticker, m.limit = false, testLimit
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the sampler never returned")
		}
	})

	return m
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

// A JOB FORGOTTEN WHILE A TICK READ IS NOT UPDATED BY THAT TICK, and neither is
// one started under the same key in the meantime: the read was for another job.
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
		// Run is inside this tick, so this Start cannot reach it and registers
		// the successor without a baseline.
		h.m.Start("vm", successor, 8)
	}
	h.m.Tick()

	h.m.mu.Lock()
	successorJob := h.m.jobs["vm"]
	got, points, samples := successorJob.latest.CPUUsage, successorJob.points, successorJob.samples
	h.m.mu.Unlock()
	if got != 0 || samples != 0 {
		t.Errorf("the successor holds cpu %d from %d samples, want nothing: the tick read its predecessor",
			got, samples)
	}
	for _, p := range points {
		if p.CPUUsage != 0 {
			t.Errorf("the successor's series holds %d, a reading of its predecessor", p.CPUUsage)
		}
	}
}

// A START THE SAMPLER CANNOT TAKE (it is stuck on a read) returns within the
// limit, and the job's first interval, which has no baseline, is not
// attributed.
func TestAStartDuringAStuckSamplerLeavesItsFirstIntervalUnattributed(t *testing.T) {
	h := newEnergyHost(t, 0)
	fifo := stuckJob(t, h.tr, h.target, h.m)

	stuck := tickInBackground(h.m)
	writer := waitForReader(t, fifo)
	begun := time.Now()
	h.m.Start("vm", h.target, 8)
	if took := time.Since(begun); took > testLimit+time.Second {
		t.Errorf("Start took %s against a stuck sampler, want about %s", took, testLimit)
	}
	unstick(t, writer, stuck)
	h.m.Forget("stuck")

	h.advance(10*time.Second, "69571715")
	h.m.Tick()
	if s := finalOf(t, h.m); s.Measured.Energy {
		t.Error("a job started with no baseline reported its energy as measured")
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

// RESERVED CPUS BEYOND THE HOST'S SHARE THE IDLE POOL, never multiply it, and a
// job whose CPU could not be read still holds its reservation.
func TestReservationsShareTheIdlePool(t *testing.T) {
	h := newEnergyHost(t, 73.5)
	h.m.Start("vm", h.target, 128)
	unread := h.target
	unread.CgroupDir = "/sys/fs/cgroup/unread"
	h.m.Start("unread", unread, 128)
	h.advance(10*time.Second, "69571715")
	h.m.Tick()

	s := finalOf(t, h.m)
	if !s.Measured.Energy || s.EnergyIdle != 735_000_000/2 {
		t.Errorf("a job holding half of 256 reserved CPUs got %d µJ idle (measured %v), want half of "+
			"735000000: the unreadable job still holds its reservation", s.EnergyIdle, s.Measured.Energy)
	}
}

// A GAP OF THREE INTERVALS BREAKS ENERGY FOR THE JOBS THAT LIVED THROUGH IT, for
// good, even though sampling resumed and the gap is shorter than the counter's
// wrap; a job started after the gap began is not broken by it.
func TestAGapBreaksTheJobsThatLivedThroughIt(t *testing.T) {
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

			*h.now = h.now.Add(tc.gap - time.Second)
			late := h.target
			late.CgroupDir = "/sys/fs/cgroup/late"
			h.tr.write(late.CgroupDir+"/cpu.stat", "usage_usec 0\nuser_usec 0\nsystem_usec 0\n")
			h.m.Start("late", late, 8)
			*h.now = h.now.Add(-(tc.gap - time.Second))
			h.tr.write(late.CgroupDir+"/cpu.stat", "usage_usec 1000\nuser_usec 1000\nsystem_usec 0\n")

			h.step(tc.gap, 1)
			h.m.Tick()
			h.step(10*time.Second, 2)
			h.m.Tick()

			if got := finalOf(t, h.m).Measured.Energy; got != tc.measured {
				t.Errorf("energy measured = %v after a %s gap, want %v", got, tc.gap, tc.measured)
			}
			lateSummary, ok := h.m.Final("late")
			if !ok || !lateSummary.Measured.Energy {
				t.Errorf("a job started a second before the gap ended lost its energy (found %v)", ok)
			}
		})
	}
}

// stuckJob adds a job whose cpu.stat is a FIFO, so a read of it blocks, and
// returns the FIFO. Cleanup releases any reader still blocked on it, before the
// sampler is stopped.
func stuckJob(t *testing.T, tr tree, target Target, m *Monitor) string {
	t.Helper()

	stuck, fifo := stuckTarget(t, tr, target)
	m.mu.Lock()
	m.jobs["stuck"] = &job{target: stuck, first: m.opts.Now()}
	m.mu.Unlock()
	t.Cleanup(func() { releaseAnyReader(t, fifo) })

	return fifo
}

// tickInBackground runs one Tick and closes the returned channel when it has
// returned.
func tickInBackground(m *Monitor) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Tick()
	}()

	return done
}

// unstick feeds and closes the FIFO's writer and waits, bounded, for the tick.
func unstick(t *testing.T, w *os.File, ticked <-chan struct{}) {
	t.Helper()

	if _, err := w.WriteString("usage_usec 1\nuser_usec 1\nsystem_usec 0\n"); err != nil {
		t.Errorf("write the fifo: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("close the fifo: %v", err)
	}
	select {
	case <-ticked:
	case <-time.After(10 * time.Second):
		t.Fatal("the stuck tick never finished")
	}
}

// releaseAnyReader lets a reader still blocked on the FIFO finish. A FIFO with
// no reader refuses a non-blocking writer, which is how it knows there is none.
func releaseAnyReader(t *testing.T, fifo string) {
	t.Helper()

	w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	if _, err := w.WriteString("usage_usec 1\nuser_usec 1\nsystem_usec 0\n"); err != nil {
		t.Errorf("release the fifo's reader: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("close the fifo: %v", err)
	}
}

// A START THE SAMPLER TOOK BUT DID NOT FINISH IN TIME LEAVES ITS JOB PENDING,
// and a Forget in the meantime wins: when the read finally completes, the job
// is not brought back.
func TestAStartThatOutlivesItsLimitDoesNotBringAForgottenJobBack(t *testing.T) {
	tr, target := referenceVM(t)
	// A FRESH TICK AND A WIDE STALE WINDOW, so only pending can make the job's
	// energy could-not-tell.
	m := runMonitor(t, tr.root, Options{Interval: time.Minute, RAPL: true})
	m.Tick()
	stuck, fifo := stuckTarget(t, tr, target)
	t.Cleanup(func() { releaseAnyReader(t, fifo) })

	begun := time.Now()
	started := make(chan struct{})
	go func() {
		defer close(started)
		m.Start("vm", stuck, 8)
	}()
	writer := waitForReader(t, fifo)
	select {
	case <-started:
		if took := time.Since(begun); took > testLimit+time.Second {
			t.Errorf("Start took %s", took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start waited on its own stuck read")
	}

	s, ok := m.Final("vm")
	if !ok || s.Measured.Energy {
		t.Errorf("a pending job answered found %v, energy measured %v; want found and unmeasured", ok,
			s.Measured.Energy)
	}
	m.Forget("vm")

	if _, err := writer.WriteString("usage_usec 1\nuser_usec 1\nsystem_usec 0\n"); err != nil {
		t.Errorf("write the fifo: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Errorf("close the fifo: %v", err)
	}
	// A REQUEST THAT READS NOTHING, served after the stuck Start: once Run
	// answers it, the Start has finished. A Tick here would hang on the very
	// regression this test is for, reading the resurrected job's FIFO again.
	if _, answered := m.ask(request{kind: requestFinal, key: "absent"}, 10*time.Second); !answered {
		t.Fatal("the sampler never finished the stuck Start")
	}

	m.mu.Lock()
	_, back := m.jobs["vm"]
	m.mu.Unlock()
	if back {
		t.Error("a job forgotten while its Start was reading was brought back when the read finished")
	}
}

// A JOB STARTED JUST AFTER A TICK THAT THEN GOES UNSAMPLED FOR THREE INTERVALS
// has missed them as surely as one that was there before the tick.
func TestAJobStartedJustAfterATickStillMissesTheGap(t *testing.T) {
	h := newEnergyHost(t, 0)
	*h.now = h.now.Add(time.Second)
	h.m.Start("vm", h.target, 8)
	h.step(39*time.Second, 1)
	h.m.Tick()
	h.step(10*time.Second, 2)
	h.m.Tick()

	if finalOf(t, h.m).Measured.Energy {
		t.Error("a job unsampled for 39 of its first seconds reported its energy as measured")
	}
}

// A JOB STILL PENDING HOLDS ITS RESERVATION: its compute is live whether or not
// the sampler has taken its baseline, so another job is not charged its idle.
func TestAPendingJobHoldsItsReservation(t *testing.T) {
	h := newEnergyHost(t, 73.5)
	h.m.Start("vm", h.target, 128)
	h.m.mu.Lock()
	h.m.jobs["pending"] = &job{target: h.target, vcpus: 128, first: *h.now, pending: true}
	h.m.mu.Unlock()
	h.advance(10*time.Second, "69571715")
	h.m.Tick()

	s := finalOf(t, h.m)
	if !s.Measured.Energy || s.EnergyIdle != 735_000_000/2 {
		t.Errorf("idle = %d µJ (measured %v), want half of 735000000: the pending job holds the other half",
			s.EnergyIdle, s.Measured.Energy)
	}
}
