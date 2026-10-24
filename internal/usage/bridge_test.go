package usage

import (
	"testing"
	"time"
)

// A HOST-WIDE READ THAT FAILED IS FOLDED INTO THE NEXT TICK: the package counter,
// the host's busy time and the job's CPU are lifetime totals, so a tick between
// two good ones whose package or host read failed changes nothing the job is
// given. It gets exactly what one tick across the whole span gives it.
func TestAFailedHostWideReadIsFoldedIntoTheNextTick(t *testing.T) {
	t.Parallel()

	control := newEnergyHost(t, 73.5)
	control.m.Start("vm", control.target, 8)
	control.advance(10*time.Second, "69571715")
	control.m.Tick()
	want := finalOf(t, control.m)
	if !want.Measured.Energy || want.EnergyActive <= 0 {
		t.Fatalf("the control measured %v (%d µJ); want a share", want.Measured.Energy, want.EnergyActive)
	}

	for name, fail := range map[string]string{
		"the package counter":  raplZone + "/energy_uj",
		"the host's busy time": "/proc/stat",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newEnergyHost(t, 73.5)
			h.m.Start("vm", h.target, 8)
			h.tr.remove(fail)
			*h.now = h.now.Add(5 * time.Second)
			h.m.Tick()
			// advance writes back the failed file with the span's totals.
			h.advance(5*time.Second, "69571715")
			h.m.Tick()

			got := finalOf(t, h.m)
			if !got.Measured.Energy {
				t.Fatalf("a failed read of %s left energy unmeasured", name)
			}
			if got.EnergyActive != want.EnergyActive || got.EnergyIdle != want.EnergyIdle {
				t.Errorf("with %s missed for one tick the job got %d µJ active and %d idle, want %d and %d",
					name, got.EnergyActive, got.EnergyIdle, want.EnergyActive, want.EnergyIdle)
			}
		})
	}
}

// A JOB THAT ENDS BEFORE THE READ IS FOLDED IN IS UNMEASURED, never given less
// than it drew: the interval it lived through was shared with nobody.
func TestAJobEndingBeforeAFailedReadIsFoldedInIsUnmeasured(t *testing.T) {
	t.Parallel()

	h := newEnergyHost(t, 73.5)
	h.m.Start("vm", h.target, 8)
	h.advance(10*time.Second, "69571715")
	h.m.Tick()
	h.tr.remove(raplZone + "/energy_uj")
	*h.now = h.now.Add(time.Second)
	h.m.Tick()

	if s := finalOf(t, h.m); s.Measured.Energy {
		t.Errorf("a job that ended inside an unshared interval measured %d µJ, want it unmeasured", s.EnergyActive)
	}
}
