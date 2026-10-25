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
		"complete   no: flows may have been missed, so every byte and connection total here is a lower bound",
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

// WHAT GOT NOTHING BACK IS NAMED, beside the table and beyond it: a column for
// the destinations listed, the counts in the rest's and other's totals, and a
// line naming every destination with an unanswered connection, the most
// first, including one the table leaves out because it moved next to nothing,
// which is the destination a stalled job was waiting on.
func TestJobsShowNamesWhatGotNothingBack(t *testing.T) {
	t.Parallel()

	dests := manyDestinations(25)
	dests[24].Unanswered = 1
	dests = append(dests, alloc.JobDestination{Addr: "10.9.9.9", SentBytes: 120, Connections: 6, Unanswered: 6})
	out := renderedDestinations(t, &alloc.JobDestinations{
		Destinations:      dests,
		Other:             alloc.JobDestination{SentBytes: 2 << 10, Connections: 4, Unanswered: 3},
		UnansweredCounted: true,
	})

	if !strings.Contains(out, "connections  unanswered\n") {
		t.Errorf("the table has no unanswered column:\n%s", out)
	}
	if !strings.HasSuffix(lineContaining(t, out, " 10.0.0.25 "), " 1") {
		t.Errorf("the largest destination's row does not end in its one unanswered connection:\n%s", out)
	}
	if strings.Contains(out, "\n"+strings.Repeat(" ", 11)+"10.9.9.9 ") {
		t.Errorf("the destination that moved least was listed in the table:\n%s", out)
	}
	for label, want := range map[string]string{
		"other": "sent 2.0 KiB, received 0 B, 4 connections, 3 unanswered (beyond the 256",
		"unanswered": "10 connections got nothing back (a SYN nothing answered, or a datagram, a DNS query " +
			"among them, with no reply): 10.9.9.9 6, 10.0.0.25 1, other 3",
	} {
		if line := lineStarting(t, out, label); !strings.Contains(line, want) {
			t.Errorf("the %s line reads %q, want it to say %q", label, line, want)
		}
	}
	if !strings.Contains(out, "and 6 more: sent 120 B, received 15.0 KiB, 11 connections, 6 unanswered") {
		t.Errorf("the rest's totals do not carry their unanswered count:\n%s", out)
	}
}

// A REPORT THAT COUNTED AND FOUND NOTHING SAYS NONE; ONE THAT DID NOT COUNT
// SAYS THAT, with no column and no count in any total, since a zero from a
// node that never counted would clear a destination nothing was measured
// against.
func TestJobsShowTellsNoneUnansweredFromNotCounted(t *testing.T) {
	t.Parallel()

	counted := renderedDestinations(t, &alloc.JobDestinations{Destinations: manyDestinations(2),
		UnansweredCounted: true})
	if line := lineStarting(t, counted, "unanswered"); !strings.HasSuffix(line,
		"none: every connection the guest sent on got something back") {
		t.Errorf("a report that counted none unanswered reads %q", line)
	}

	uncounted := renderedDestinations(t, &alloc.JobDestinations{Destinations: manyDestinations(2)})
	if line := lineStarting(t, uncounted, "unanswered"); !strings.Contains(line, "not counted") ||
		strings.Contains(line, "none") {
		t.Errorf("a report that did not count reads %q", line)
	}
	if strings.Contains(uncounted, "unanswered\n") || strings.Contains(uncounted, " unanswered (beyond") {
		t.Errorf("a report that did not count printed an unanswered column or count:\n%s", uncounted)
	}
}

// EVERY DESTINATION THAT GOT NOTHING BACK IS NAMED, however many there are,
// since one left out of this line may be in no table either; and a result
// that may have missed flows proves no "none", and its count is what it saw,
// which a missed reply can make an over-count as well as a missed flow an
// under-count.
func TestJobsShowNamesEveryStalledDestinationAndQualifiesAnIncompleteOne(t *testing.T) {
	t.Parallel()

	dests := manyDestinations(shownDestinations + 5)
	for i := range dests {
		dests[i].Unanswered = 1
	}
	out := renderedDestinations(t, &alloc.JobDestinations{Destinations: dests, UnansweredCounted: true,
		Incomplete: true})
	line := lineStarting(t, out, "unanswered")
	for i := 1; i <= len(dests); i++ {
		if !strings.Contains(line, fmt.Sprintf("10.0.0.%d 1", i)) {
			t.Errorf("10.0.0.%d, which got nothing back once, is not named: %q", i, line)
		}
	}
	if !strings.HasPrefix(line, "unanswered 25 connections got nothing back") ||
		!strings.HasSuffix(line, "so these are what was seen, not a bound") {
		t.Errorf("an incomplete result's count is not said as what was seen: %q", line)
	}

	none := renderedDestinations(t, &alloc.JobDestinations{Destinations: manyDestinations(2),
		UnansweredCounted: true, Incomplete: true})
	if line := lineStarting(t, none, "unanswered"); !strings.HasPrefix(line, "unanswered none observed, but flows "+
		"may have been missed") || strings.Contains(line, "every connection the guest sent on got something back") {
		t.Errorf("an incomplete result that saw none unanswered reads %q", line)
	}
}

// THE PAGE WORDS COMPLETENESS AND WHAT GOT NOTHING BACK AS THE TEXT REPORT
// DOES, for each shape the count can take, so neither says a bound or a "none"
// the other does not.
func TestThePageWordsUnansweredAsTheTextReportDoes(t *testing.T) {
	t.Parallel()

	stalled := manyDestinations(3)
	stalled[0].Unanswered = 1
	late := timedDestinations(3)
	late[1].SlowReplies, late[1].SlowestReplyMillis = 1, 2300
	for name, d := range map[string]*alloc.JobDestinations{
		"counted, incomplete":     {Destinations: stalled, UnansweredCounted: true, Incomplete: true},
		"counted, none, complete": {Destinations: manyDestinations(3), UnansweredCounted: true},
		"counted, none, incomplete": {Destinations: manyDestinations(3), UnansweredCounted: true,
			Incomplete: true},
		"not counted": {Destinations: stalled[1:]},
		"timed, one late, incomplete": {Destinations: late, UnansweredCounted: true, RepliesTimed: true,
			Incomplete: true},
		"timed, none late": {Destinations: timedDestinations(2), UnansweredCounted: true, RepliesTimed: true},
	} {
		text := renderedDestinations(t, d)
		page := destinationsSection(alloc.JobRecord{}, &alloc.RecordedUsage{JobUsage: alloc.JobUsage{Destinations: d}})
		if page.Timed != d.RepliesTimed {
			t.Errorf("%s: the page draws the slowest-reply column %v, want %v", name, page.Timed, d.RepliesTimed)
		}
		for label, said := range map[string]string{"complete": page.Complete, "unanswered": page.Unanswered,
			"slow": page.Slow} {
			if line := lineStarting(t, text, label); line != fmt.Sprintf("%-11s%s", label, said) {
				t.Errorf("%s: the page's %s says %q and the text report %q", name, label, said, line)
			}
		}
		if page.Counted != d.UnansweredCounted {
			t.Errorf("%s: the page draws the unanswered column %v, want %v", name, page.Counted, d.UnansweredCounted)
		}
	}
}

// timedDestinations is manyDestinations with each connection answered and its
// first reply timed at 30 ms.
func timedDestinations(n int) []alloc.JobDestination {
	dests := manyDestinations(n)
	for i := range dests {
		dests[i].TimedReplies, dests[i].SlowestReplyMillis = 1, 30
	}

	return dests
}

// WHAT ANSWERED LATE IS NAMED, every destination with a slow first reply by
// name, the slowest first, beside the table's slowest-reply column: the
// resolver that answered after a second, even when it moved too little to be
// listed by bytes. Answered connections left untimed are counted, a report
// that did not time replies says so and draws no column, and one that timed
// them and found none late says none.
func TestJobsShowNamesWhatAnsweredLate(t *testing.T) {
	t.Parallel()

	dests := timedDestinations(25)
	dests[24].SlowReplies, dests[24].SlowestReplyMillis = 1, 1400
	dests = append(dests, alloc.JobDestination{Addr: "172.31.1.1", SentBytes: 80, Connections: 3,
		TimedReplies: 2, SlowReplies: 2, SlowestReplyMillis: 4200})
	out := renderedDestinations(t, &alloc.JobDestinations{Destinations: dests, UnansweredCounted: true,
		RepliesTimed: true, Other: alloc.JobDestination{Connections: 2, TimedReplies: 2, SlowReplies: 1,
			SlowestReplyMillis: 1100}})

	if !strings.Contains(out, "connections  unanswered  slowest reply\n") {
		t.Errorf("the table has no slowest-reply column:\n%s", out)
	}
	if !strings.HasSuffix(lineContaining(t, out, " 10.0.0.25 "), " 1.4 s") ||
		!strings.HasSuffix(lineContaining(t, out, " 10.0.0.24 "), " 30 ms") {
		t.Errorf("the rows do not end in their slowest reply:\n%s", out)
	}
	want := "4 of 29 timed replies came after more than 1 s, TCP's first retransmission (a resolver that " +
		"answered late, or an uplink slow enough that the guest sent its SYN again): 172.31.1.1 2 " +
		"(slowest 4.2 s), 10.0.0.25 1 (slowest 1.4 s), other 1 (slowest 1.1 s); 1 answered connections " +
		"were not timed, so their waits are not known"
	if line := lineStarting(t, out, "slow"); line != "slow       "+want {
		t.Errorf("the slow line reads %q,\nwant %q", line, "slow       "+want)
	}

	none := renderedDestinations(t, &alloc.JobDestinations{Destinations: timedDestinations(2),
		UnansweredCounted: true, RepliesTimed: true})
	if line := lineStarting(t, none, "slow"); !strings.HasSuffix(line, "none: each of 2 timed replies came within 1 s") {
		t.Errorf("a report that timed and found nothing late reads %q", line)
	}

	// WITHOUT THE UNANSWERED COUNTS no connection is known answered, so none is
	// said to have been left untimed.
	uncounted := timedDestinations(2)
	uncounted[0].TimedReplies = 0
	if line := lineStarting(t, renderedDestinations(t, &alloc.JobDestinations{Destinations: uncounted,
		RepliesTimed: true}), "slow"); strings.Contains(line, "not timed") {
		t.Errorf("a report that did not count unanswered connections claims untimed answered ones: %q", line)
	}

	untimed := renderedDestinations(t, &alloc.JobDestinations{Destinations: manyDestinations(2),
		UnansweredCounted: true})
	if line := lineStarting(t, untimed, "slow"); !strings.Contains(line, "not timed") ||
		strings.Contains(line, "none") {
		t.Errorf("a report that did not time replies reads %q", line)
	}
	if strings.Contains(untimed, "slowest reply") {
		t.Errorf("a report that did not time replies drew the column:\n%s", untimed)
	}
}

// lineContaining is the report's line holding s.
func lineContaining(t *testing.T, report, s string) string {
	t.Helper()

	for line := range strings.SplitSeq(report, "\n") {
		if strings.Contains(line, s) {
			return line
		}
	}
	t.Fatalf("no line holds %q in:\n%s", s, report)

	return ""
}
