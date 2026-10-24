package node

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/usage"
)

// A JOB WHOSE ENERGY CAME FROM A METER IS REPORTED AS THE WALL'S, with its idle
// share, even when its host also keeps a per-process estimate, and what it
// reports validates.
func TestAMeterMeasuredJobIsReportedAsTheWalls(t *testing.T) {
	t.Parallel()

	u, _ := jobUsageOf(usage.Summary{
		Samples: 3, Interval: time.Second, Window: 3 * time.Second,
		EnergyMeter: true, EnergySplit: true, EnergyActive: 9_000_000, EnergyIdle: 666_666,
		Measured: usage.Measured{CPU: true, Energy: true},
	})
	if u.EnergySource != alloc.EnergyMeter || u.EnergyActiveMicrojoules != 9_000_000 ||
		u.EnergyIdleMicrojoules != 666_666 {
		t.Errorf("energy %d active, %d idle from %q; want the meter's 9000000 and 666666", u.EnergyActiveMicrojoules,
			u.EnergyIdleMicrojoules, u.EnergySource)
	}
	if err := u.Validate(); err != nil {
		t.Errorf("the report does not validate: %v", err)
	}
}
