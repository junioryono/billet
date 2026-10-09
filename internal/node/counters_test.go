package node

import (
	"testing"

	"github.com/junioryono/billet/internal/usage"
)

// WHAT THE SAMPLER COUNTED IS WHAT THE REPORT SAYS, event by event: a counted
// zero stays zero, an uncounted event stays absent, and a job with no event
// counted reports no counters at all, which the ledger would refuse otherwise.
func TestTheReportCarriesEachCountedEventAndNoOther(t *testing.T) {
	t.Parallel()

	sum := usage.Summary{Samples: 2, Counters: &usage.Counters{
		Values:   [usage.NumEvents]int64{4000, 4840, 0, 35, 9, 777},
		Measured: [usage.NumEvents]bool{true, true, true, true, true, false},
	}}
	report, _ := jobUsageOf(sum)
	c := report.Counters
	if c == nil {
		t.Fatal("counted events did not reach the report")
	}
	for name, tc := range map[string]struct {
		got  *int64
		want int64
	}{
		"cycles": {c.Cycles, 4000}, "instructions": {c.Instructions, 4840},
		"cache references": {c.CacheReferences, 0}, "cache misses": {c.CacheMisses, 35},
		"branch misses": {c.BranchMisses, 9},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s = %v, want %d", name, tc.got, tc.want)
		}
	}
	if c.FrontendStallCycles != nil {
		t.Errorf("an uncounted event was reported as %d", *c.FrontendStallCycles)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the report does not validate: %v", err)
	}

	for name, counters := range map[string]*usage.Counters{
		"not counted":       nil,
		"no event measured": {Values: [usage.NumEvents]int64{1, 2, 3, 4, 5, 6}},
	} {
		sum.Counters = counters
		if report, _ := jobUsageOf(sum); report.Counters != nil {
			t.Errorf("%s: the report carries %+v, want no counters", name, report.Counters)
		}
	}
}
