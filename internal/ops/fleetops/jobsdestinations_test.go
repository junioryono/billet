package fleetops

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
)

// manyDestinations is n destinations, the i-th with i KiB received, given in
// the order a node would never send them, so the report's order is its own.
func manyDestinations(n int) []alloc.JobDestination {
	out := make([]alloc.JobDestination, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, alloc.JobDestination{Addr: fmt.Sprintf("10.0.0.%d", i), ReceivedBytes: int64(i) << 10,
			Connections: 1})
	}

	return out
}

// renderedDestinations renders a measured job carrying destinations.
func renderedDestinations(t *testing.T, d *alloc.JobDestinations) string {
	t.Helper()

	usage := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000,
		Unmeasured: []string{alloc.UsageEnergy}, Destinations: d,
	}}
	var out bytes.Buffer
	renderJob(&out, alloc.JobRecord{LeaseID: "l1", Tier: "t"}, usage)

	return out.String()
}

// THROUGH THE LEDGER AND THE COMMAND: the twenty largest destinations by total
// bytes, largest first, the rest counted and totalled on one line, the traffic
// beyond the ones the node kept by name, an incomplete result said plainly,
// and the tap beside the destinations, labelled as the comparison it is.
func TestJobsShowPrintsWhereAJobsTrafficWent(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	usage := &alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000,
		Unmeasured: []string{alloc.UsageEnergy}, NetTxBytes: 1 << 20, NetTxPackets: 10,
		Destinations: &alloc.JobDestinations{
			Destinations: manyDestinations(25),
			Other:        alloc.JobDestination{SentBytes: 2 << 10, ReceivedBytes: 3 << 10, Connections: 4},
			Incomplete:   true,
			Tap:          &alloc.TapTotals{SentBytes: 10 << 10, ReceivedBytes: 300 << 10},
		},
	}
	lease := seedMeasuredJob(t, stateDir, 77, usage)

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})

	if !strings.Contains(out, "destinations 25 by total bytes") {
		t.Errorf("the report does not count the destinations:\n%s", out)
	}
	shown := 0
	last := -1
	for i := 25; i >= 1; i-- {
		at := strings.Index(out, fmt.Sprintf(" 10.0.0.%d ", i))
		if i <= 5 {
			if at >= 0 {
				t.Errorf("10.0.0.%d, the %d-th largest, was listed past the first twenty:\n%s", i, 26-i, out)
			}
			continue
		}
		if at < 0 {
			t.Errorf("10.0.0.%d, among the twenty largest, was not listed:\n%s", i, out)
			continue
		}
		if at < last {
			t.Errorf("10.0.0.%d was listed before a larger destination:\n%s", i, out)
		}
		last, shown = at, shown+1
	}
	if shown != 20 {
		t.Errorf("%d destinations were listed, want 20", shown)
	}
	for _, want := range []string{
		"and 5 more: sent 0 B, received 15.0 KiB, 5 connections",
		"other      sent 2.0 KiB, received 3.0 KiB, 4 connections (beyond the 256 destinations the node keeps by name)",
		"complete   no: flows may have been missed, so every total here is a lower bound",
		// 325 KiB received across the named destinations and 3 KiB beyond them.
		"tap        sent 10.0 KiB, received 300.0 KiB; tap minus attributed: sent +8.0 KiB, received -28.0 KiB",
		"(a comparison of two counters read a moment apart, not a measurement of what was missed)",
		"25.0 KiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// A JOB WHOSE FLOWS WERE NOT TOTALLED SAYS SO, and prints no table, total or
// verdict that would read as a job that sent nothing.
func TestJobsShowSaysWhenDestinationsWereNotMeasured(t *testing.T) {
	t.Parallel()

	out := renderedDestinations(t, nil)
	if !strings.Contains(out, "\ndestinations not measured") {
		t.Errorf("a job whose flows were not totalled did not say so:\n%s", out)
	}
	for _, label := range []string{"other ", "complete ", "tap "} {
		if strings.Contains(out, "\n"+label) {
			t.Errorf("a job whose flows were not totalled printed a %q line:\n%s", label, out)
		}
	}
}

// A COMPLETE RESULT WITH NOTHING IN IT IS A JOB THAT SENT NOTHING, said as
// such; and with no tap reading there is nothing to compare it with, which the
// report says rather than printing the destinations' sum as the difference.
func TestJobsShowPrintsAnEmptyCompleteResultAndNoTap(t *testing.T) {
	t.Parallel()

	out := renderedDestinations(t, &alloc.JobDestinations{})
	for label, want := range map[string]string{
		"complete": "yes",
		"other":    "sent 0 B, received 0 B, 0 connections",
		"tap":      "not read, so there is nothing to compare the destinations with",
	} {
		if line := lineStarting(t, out, label); !strings.HasSuffix(line, want) &&
			!strings.Contains(line, want) {
			t.Errorf("the %s line reads %q, want it to say %q", label, line, want)
		}
	}
	if !strings.Contains(out, "destinations 0 by total bytes") || strings.Contains(out, "connections\n") {
		t.Errorf("an empty result printed a table:\n%s", out)
	}
	if strings.Contains(out, "more:") {
		t.Errorf("an empty result counted destinations beyond the table:\n%s", out)
	}
}

// TWENTY OR FEWER ARE ALL LISTED, AND NONE IS COUNTED AS MORE.
func TestJobsShowListsEveryDestinationUpToTwenty(t *testing.T) {
	t.Parallel()

	out := renderedDestinations(t, &alloc.JobDestinations{Destinations: manyDestinations(20)})
	for i := 1; i <= 20; i++ {
		if !strings.Contains(out, fmt.Sprintf(" 10.0.0.%d ", i)) {
			t.Errorf("10.0.0.%d was not listed:\n%s", i, out)
		}
	}
	if strings.Contains(out, "more:") {
		t.Errorf("twenty destinations were reported as more than the table:\n%s", out)
	}
}
