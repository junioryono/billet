package usage

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// procStat is a /proc/stat of cpus processors whose busy and idle are given in
// USER_HZ ticks: what a fixture tree answers for the host's CPU on any platform.
func procStat(busy, idle int64, cpus int) string {
	return fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\n", busy, idle) +
		strings.Repeat("cpu0 0 0 0 0 0 0 0 0 0 0\n", cpus)
}

// meterRun is a process job (a tart VM) on a host whose energy comes from a
// meter the test advances, one interval in.
type meterRun struct {
	tree    tree
	host    processHost
	wall    *Energy
	wallErr *error
	now     *time.Time
	// busy and idle are the host's /proc/stat ticks, which every advance moves
	// on, so an interval is unmeasured only for the reason a test gives it.
	busy, idle *int64
	m          *Monitor
}

func newMeterRun(t *testing.T) meterRun {
	t.Helper()

	tr := newTree(t)
	tr.write("/proc/stat", procStat(1000, 9000, 18))
	h := newProcessHost()
	wall := Energy{Microjoules: 50_000_000, MaxRange: math.MaxInt64, Epoch: 1}
	var wallErr error
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	m := runMonitor(t, tr.root, Options{Interval: time.Second, IdleWatts: 2,
		Meter: func() (Energy, error) { return wall, wallErr },
		Now:   func() time.Time { return now }})
	m.reader.Process = h.reader.Process
	m.Tick()
	m.Start("vm", processTarget, 6)

	busy, idle := int64(1000), int64(9000)

	return meterRun{tree: tr, host: h, wall: &wall, wallErr: &wallErr, now: &now, busy: &busy, idle: &idle, m: m}
}

// advance moves one second on: the host busy for 3 s of its 18 cpu-seconds,
// the VM for 1.5 s of it, the wall 20 J, and macOS's own estimate for the VM's
// process a great deal, which is not what a meter's job reports.
func (r meterRun) advance() {
	*r.now = r.now.Add(time.Second)
	*r.busy, *r.idle = *r.busy+300, *r.idle+1500
	r.tree.write("/proc/stat", procStat(*r.busy, *r.idle, 18))
	r.host.counters.UserMicros += 1_500_000
	r.host.counters.Energy += 999_999_999
	r.wall.Microjoules += 20_000_000
	r.m.Tick()
}

// A TART VM'S ENERGY WITH A METER IS ITS SHARE OF THE WALL: the 20 J above the
// host's 2 W idle shared by its 1.5 s of the host's 3 s busy, and the idle draw
// by its 6 of the host's 18 cpus, never macOS's estimate for its process.
func TestAProcessJobsEnergyIsItsShareOfTheWallWithAMeter(t *testing.T) {
	t.Parallel()

	r := newMeterRun(t)
	r.advance()

	s := finalOf(t, r.m)
	if !s.Measured.Energy || !s.EnergyMeter || !s.EnergySplit || s.EnergyProcess {
		t.Fatalf("energy measured %v, meter %v, split %v, process %v; want the wall's share",
			s.Measured.Energy, s.EnergyMeter, s.EnergySplit, s.EnergyProcess)
	}
	// active: (20 J - 2 J idle) * 1.5 / 3; idle: 2 J * 6 / 18.
	if s.EnergyActive != 9_000_000 {
		t.Errorf("active energy %d µJ, want 9000000", s.EnergyActive)
	}
	if s.EnergyIdle != 666_666 {
		t.Errorf("idle energy %d µJ, want 666666", s.EnergyIdle)
	}
}

// AN INTERVAL THE METER DID NOT COUNT IS COULD-NOT-TELL for the whole job: a
// new epoch, and a meter that answers an error, each leave its energy
// unmeasured rather than short.
func TestAnIntervalTheMeterDidNotCountLeavesEnergyUnmeasured(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		break_   func(r meterRun)
		measured bool
	}{
		// Two counted intervals in a row, so a refusal below is the break's.
		"nothing missed": {func(meterRun) {}, true},
		"a new epoch":    {func(r meterRun) { r.wall.Epoch++ }, false},
		"a failed read":  {func(r meterRun) { *r.wallErr = errors.New("the plug did not answer") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newMeterRun(t)
			r.advance()
			tc.break_(r)
			r.advance()

			if s := finalOf(t, r.m); s.Measured.Energy != tc.measured {
				t.Errorf("energy measured %v (%d µJ), want %v", s.Measured.Energy, s.EnergyActive, tc.measured)
			}
		})
	}
}
