package fleetops

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
)

// A DRAIN NAMES WHAT A LEASE IS DOING, NOT THE PHASE IT IS STUCK AT: a pool runner's
// lease stays at launching through its whole job and after it (#795), so the line an
// operator reads comes from what billet heard of the job, and says no more than that.
func TestADrainSaysWhatEachLeasesJobIsDoing(t *testing.T) {
	t.Parallel()

	summary := OutstandingSummary(alloc.Quiescence{Sealed: true, Outstanding: []alloc.Outstanding{
		{ID: "a", Tier: "linux-8", Node: "epyc-1", Phase: alloc.PhaseLaunching, RunID: "41", JobStarted: true},
		{ID: "b", Tier: "linux-4", Node: "epyc-1", Phase: alloc.PhaseLaunching, RunID: "42"},
		{
			ID: "c", Tier: "linux-2", Node: "epyc-1", Phase: alloc.PhaseLaunching, RunID: "43",
			JobStarted: true, JobFinished: true,
		},
		{ID: "d", Tier: "linux-1", Node: "epyc-1", Phase: alloc.PhaseCustody, RunID: "44"},
	}})

	for _, want := range []string{
		"\n  linux-8 job started, no result recorded run 41 on epyc-1",
		"\n  linux-4 launching or running, no job start recorded run 42 on epyc-1",
		"\n  linux-2 job finished, lease still open run 43 on epyc-1",
		"\n  linux-1 in custody run 44 on epyc-1",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary lacks %q:\n%s", want, summary)
		}
	}
}
