package fleetops

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
)

// A DRAIN NAMES WHAT A LEASE IS DOING, NOT THE PHASE IT IS STUCK AT: a pool runner's
// lease stays at launching through its whole job (#795), so the line an operator
// reads says whether its job started.
func TestADrainSaysARunningJobIsRunning(t *testing.T) {
	t.Parallel()

	summary := OutstandingSummary(alloc.Quiescence{Sealed: true, Outstanding: []alloc.Outstanding{
		{ID: "a", Tier: "linux-8", Node: "epyc-1", Phase: alloc.PhaseLaunching, RunID: "41", JobStarted: true},
		{ID: "b", Tier: "linux-4", Node: "epyc-1", Phase: alloc.PhaseLaunching, RunID: "42"},
		{ID: "c", Tier: "linux-2", Node: "epyc-1", Phase: alloc.PhaseCustody, RunID: "43"},
	}})

	for _, want := range []string{
		"\n  linux-8 running a job run 41 on epyc-1",
		"\n  linux-4 launched, waiting for its job run 42 on epyc-1",
		"\n  linux-2 in custody run 43 on epyc-1",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary lacks %q:\n%s", want, summary)
		}
	}

	if strings.Contains(summary, string(alloc.PhaseLaunching)) {
		t.Errorf("the summary still names a lease by the phase it never leaves:\n%s", summary)
	}
}
