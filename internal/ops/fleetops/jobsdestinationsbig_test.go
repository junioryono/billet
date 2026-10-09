package fleetops

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
)

// TOTALS PAST THE LARGEST COUNT ARE ORDERED, SUMMED AND COMPARED EXACTLY: a
// destination whose sent and received together pass an int64 still sorts
// above one that does not, a sum that passes it says so rather than printing a
// smaller number, and the tap's difference is the true one rather than zero.
func TestJobsShowCountsTotalsPastTheLargestInt64Exactly(t *testing.T) {
	t.Parallel()

	const most = math.MaxInt64
	dests := []alloc.JobDestination{
		{Addr: "10.0.0.1", SentBytes: most, ReceivedBytes: 1},
		{Addr: "10.0.0.2", SentBytes: most, ReceivedBytes: most},
	}
	// FIVE BEYOND THE TABLE, whose bytes and connections together pass an
	// int64 where any four of them would not.
	for i := range shownDestinations + 3 {
		dests = append(dests, alloc.JobDestination{Addr: fmt.Sprintf("10.1.0.%d", i), SentBytes: most / 4,
			Connections: most / 4})
	}
	out := renderedDestinations(t, &alloc.JobDestinations{
		Destinations: dests, Tap: &alloc.TapTotals{SentBytes: most},
	})

	if first, second := strings.Index(out, " 10.0.0.2 "), strings.Index(out, " 10.0.0.1 "); first < 0 ||
		second < 0 || first > second {
		t.Errorf("the destination with the larger total was not listed first:\n%s", out)
	}
	huge := "more than " + cli.HumanBytes(most)
	if !strings.Contains(out, "and 5 more: sent "+huge+", received 0 B, more than "+groupDigits(most)+
		" connections") {
		t.Errorf("the rest's totals past an int64 were not said as such:\n%s", out)
	}
	if !strings.Contains(out, "tap minus attributed: sent -"+huge+", received -"+huge) {
		t.Errorf("the tap's difference from totals past an int64 was not said as such:\n%s", out)
	}
}
