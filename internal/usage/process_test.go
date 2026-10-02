package usage

import (
	"errors"
	"testing"
	"time"
)

// refVMProcess is one tart VM's Virtualization.framework process on the
// reference deployment's Mac (macOS 27, a 6 vCPU / 24 GiB guest, 2026-09-30, read
// only), converted from its rusage_info_v6 at hw.tbfrequency 24 MHz.
var refVMProcess = ProcessCounters{
	Start:      16664085551217,
	UserMicros: 1524189381, SystemMicros: 67371613,
	Footprint: 25758733128, PeakFootprint: 25855595336,
	DiskRead: 7894355968, DiskWrite: 18492821504,
	EnergyOK: true, Energy: 7400903879,
}

// processHost is a reader whose one process answers with counters the test
// sets, under a pid the test can hand to someone else.
type processHost struct {
	counters *ProcessCounters
	err      *error
	reader   Reader
}

func newProcessHost() processHost {
	c, err := refVMProcess, error(nil)
	h := processHost{counters: &c, err: &err}
	h.reader = Reader{Process: func(pid int) (ProcessCounters, error) {
		if pid != 74207 {
			return ProcessCounters{}, errors.New("no such process")
		}

		return *h.counters, *h.err
	}}

	return h
}

var processTarget = Target{PID: 74207, PIDStart: refVMProcess.Start, Process: true}

// A PROCESS JOB IS READ FROM ITS OWN ACCOUNTING, every group from one reading,
// and the OOM count, which that accounting does not keep, is not claimed.
func TestAProcessJobIsReadFromItsOwnAccounting(t *testing.T) {
	t.Parallel()

	s := newProcessHost().reader.Read(processTarget)
	want := Sample{
		CPUOK: true, CPUUsage: 1591560994, CPUUser: 1524189381, CPUSys: 67371613,
		MemoryOK: true, MemoryCurrent: 25758733128, MemoryPeak: 25855595336,
		IOOK: true, DiskRead: 7894355968, DiskWrite: 18492821504,
		ProcessEnergyOK: true, ProcessEnergy: 7400903879,
	}
	if s != want {
		t.Errorf("read %+v,\nwant %+v", s, want)
	}
}

// A PID THAT IS NO LONGER THE JOB'S IS NOT READ: another start, an unreadable
// process, or a target with no start at all reads nothing.
func TestAProcessThatIsNotTheJobsIsNotRead(t *testing.T) {
	t.Parallel()

	h := newProcessHost()
	h.counters.Start++
	if s := h.reader.Read(processTarget); s != (Sample{}) {
		t.Errorf("a process with another start was read as the job's: %+v", s)
	}
	h = newProcessHost()
	*h.err = errors.New("gone")
	if s := h.reader.Read(processTarget); s != (Sample{}) {
		t.Errorf("an unreadable process was read: %+v", s)
	}
	unproved := processTarget
	unproved.PIDStart = 0
	if s := newProcessHost().reader.Read(unproved); s != (Sample{}) {
		t.Errorf("a target with no start was read: %+v", s)
	}
}

// ONLY A RECENT LIFETIME TOTAL IS THE JOB'S: a process whose accounting stopped
// answering long enough ago has an energy reading older than its job, which is
// could-not-tell, never a smaller total. One that missed only its final read is
// still the job's.
func TestAProcessJobsEnergyMustBeRecent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		failing  int
		measured bool
	}{{"silent for one interval", 1, true}, {"silent for three", 3, false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newProcessHost()
			now := time.Date(2026, 9, 30, 19, 0, 0, 0, time.UTC)
			m := runMonitor(t, t.TempDir(), Options{Interval: time.Second, Now: func() time.Time { return now }})
			m.reader.Process = h.reader.Process
			m.Start("vm", processTarget, 6)
			now = now.Add(time.Second)
			m.Tick()
			*h.err = errors.New("the process stopped answering")
			for range tc.failing - 1 {
				now = now.Add(time.Second)
				m.Tick()
			}
			now = now.Add(time.Second)
			sum, ok := m.Final("vm")
			if !ok || !sum.Measured.CPU {
				t.Fatalf("Final = %+v, %v; want the earlier CPU kept", sum.Measured, ok)
			}
			if sum.Measured.Energy != tc.measured {
				t.Errorf("energy measured %v after %d silent intervals, want %v", sum.Measured.Energy,
					tc.failing, tc.measured)
			}
		})
	}
}

// A PROCESS JOB'S ENERGY IS THE KERNEL'S LIFETIME TOTAL, read at the end and
// never shared out of a package counter; a kernel that keeps none leaves it
// unmeasured.
func TestAProcessJobsEnergyIsItsOwnEstimate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		energyOK bool
	}{{"estimated", true}, {"not estimated", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newProcessHost()
			h.counters.EnergyOK = tc.energyOK
			now := time.Date(2026, 9, 30, 19, 0, 0, 0, time.UTC)
			m := runMonitor(t, t.TempDir(), Options{Interval: time.Second, Now: func() time.Time { return now }})
			m.reader.Process = h.reader.Process
			m.Start("vm", processTarget, 6)
			for range 3 {
				now = now.Add(time.Second)
				h.counters.UserMicros += 900_000
				h.counters.Energy += 2_000_000
				m.Tick()
			}
			sum, ok := m.Final("vm")
			if !ok {
				t.Fatal("Final found no job")
			}
			if !sum.Measured.CPU || !sum.Measured.Memory || !sum.Measured.IO {
				t.Fatalf("the process's groups were not measured: %+v", sum.Measured)
			}
			if sum.Measured.OOM || sum.Measured.Net || sum.Measured.Threads || sum.Measured.Pressure {
				t.Errorf("groups a process does not keep were reported measured: %+v", sum.Measured)
			}
			if sum.Measured.Energy != tc.energyOK || !sum.EnergyProcess || sum.EnergySplit {
				t.Fatalf("energy measured %v (process %v, split %v), want measured %v from the process",
					sum.Measured.Energy, sum.EnergyProcess, sum.EnergySplit, tc.energyOK)
			}
			if !tc.energyOK {
				return
			}
			if want := refVMProcess.Energy + 6_000_000; sum.EnergyActive != want || sum.EnergyIdle != 0 {
				t.Errorf("energy %d active, %d idle; want the process's %d, none idle",
					sum.EnergyActive, sum.EnergyIdle, want)
			}
			if last := sum.Points[len(sum.Points)-1]; last.EnergyActive != sum.EnergyActive {
				t.Errorf("the series ends at %d µJ, not the job's %d", last.EnergyActive, sum.EnergyActive)
			}
		})
	}
}
