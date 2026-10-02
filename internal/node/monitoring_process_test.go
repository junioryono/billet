package node

import (
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/usage"
)

// A JOB MEASURED BY ITS PROCESS IS REPORTED AS ONE: its energy is the
// process's own, with no idle share, its OOM count is not claimed, and what it
// reports validates.
func TestAProcessMeasuredJobIsReportedAsOne(t *testing.T) {
	t.Parallel()

	u, _ := jobUsageOf(usage.Summary{
		Samples: 3, Interval: time.Second, Window: 3 * time.Second,
		Latest:     usage.Sample{CPUUser: 5, CPUSys: 1, MemoryPeak: 7, OOMKills: 2, DiskRead: 3},
		MemoryPeak: 9, EnergyProcess: true, EnergyActive: 11,
		Measured: usage.Measured{CPU: true, Memory: true, IO: true, Energy: true},
	})
	if u.EnergySource != alloc.EnergyProcess || u.EnergyActiveMicrojoules != 11 || u.EnergyIdleMicrojoules != 0 {
		t.Errorf("energy %d active, %d idle from %q; want the process's 11 µJ", u.EnergyActiveMicrojoules,
			u.EnergyIdleMicrojoules, u.EnergySource)
	}
	if !slices.Contains(u.Unmeasured, alloc.UsageOOM) || u.OOMKills != 0 {
		t.Errorf("an OOM count nobody read was reported: %d, unmeasured %v", u.OOMKills, u.Unmeasured)
	}
	if err := u.Validate(); err != nil {
		t.Errorf("the report does not validate: %v", err)
	}

	// A cgroup's count is read with its memory, and reported.
	u, _ = jobUsageOf(usage.Summary{
		Samples: 1, Interval: time.Second, Latest: usage.Sample{OOMKills: 2},
		Measured: usage.Measured{Memory: true, OOM: true},
	})
	if u.OOMKills != 2 || slices.Contains(u.Unmeasured, alloc.UsageOOM) {
		t.Errorf("a read OOM count came out as %d, unmeasured %v", u.OOMKills, u.Unmeasured)
	}
}
