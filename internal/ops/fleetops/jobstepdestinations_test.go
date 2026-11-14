package fleetops

import (
	"bytes"
	"html"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/github"
)

// t0 is a step's start on a whole second, as GitHub gives it.
var t0 = time.Unix(1_794_650_000, 0).UTC()

// fiveDestinations is a job that talked to five named destinations and beyond
// them, its connections split by the second each opened.
func fiveDestinations() *alloc.JobDestinations {
	d := &alloc.JobDestinations{OpeningSeconds: 1}
	for i, addr := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"} {
		d.Destinations = append(d.Destinations, alloc.JobDestination{Addr: addr, SentBytes: int64(100 * (5 - i)),
			ReceivedBytes: 1000, Connections: 2})
	}
	d.Other = alloc.JobDestination{SentBytes: 1, ReceivedBytes: 2, Connections: 1}
	at := func(s int64) int64 { return t0.Unix() + s }
	d.Openings = []alloc.DestinationOpening{
		// The step is [0, 60): 10.0.0.1 opens once inside it and once after.
		{Ordinal: 0, Start: at(0), SentBytes: 400, ReceivedBytes: 900, Connections: 1},
		{Ordinal: 0, Start: at(60), SentBytes: 100, ReceivedBytes: 100, Connections: 1},
		{Ordinal: 1, Start: at(59), SentBytes: 400, ReceivedBytes: 1000, Connections: 2},
		{Ordinal: 2, Start: at(10), SentBytes: 300, ReceivedBytes: 1000, Connections: 2},
		{Ordinal: 3, Start: at(-1), SentBytes: 200, ReceivedBytes: 1000, Connections: 2},
		{Ordinal: 4, Start: at(30), SentBytes: 100, ReceivedBytes: 1000, Connections: 2},
		{Ordinal: alloc.MaxJobDestinations, Start: at(30), SentBytes: 1, ReceivedBytes: 2, Connections: 1},
	}

	return d
}

// A STEP'S DESTINATIONS ARE THE CONNECTIONS THAT OPENED BETWEEN ITS START AND ITS
// END, largest first: one opened the second before the step or at the second it
// ends is another step's, the three largest are named, and the rest, the traffic
// beyond the named destinations among them, are counted and totalled.
func TestAStepsDestinationsAreTheConnectionsThatOpenedInIt(t *testing.T) {
	t.Parallel()

	got := stepDestinations(fiveDestinations(), t0, t0.Add(time.Minute))
	want := "10.0.0.2 (sent 400 B, received 1000 B, 2 connections); " +
		"10.0.0.1 (sent 400 B, received 900 B, 1 connection); " +
		"10.0.0.3 (sent 300 B, received 1000 B, 2 connections); " +
		"and 2 more (sent 101 B, received 1002 B, 3 connections)"
	if got != want {
		t.Errorf("stepDestinations =\n%q\nwant\n%q", got, want)
	}

	// The second after the step, and only 10.0.0.1's second connection is there.
	if got := stepDestinations(fiveDestinations(), t0.Add(time.Minute), t0.Add(2*time.Minute)); got !=
		"10.0.0.1 (sent 100 B, received 100 B, 1 connection)" {
		t.Errorf("the next step's destinations = %q", got)
	}
	// A step that opened nothing says so, never nothing at all.
	if got := stepDestinations(fiveDestinations(), t0.Add(5*time.Minute), t0.Add(6*time.Minute)); got !=
		"no connection opened in this step" {
		t.Errorf("a quiet step's destinations = %q", got)
	}
}

// A JOB NOT SPLIT BY TIME SAYS NOTHING PER STEP, and neither does a step with no
// length; a split wider than a second says how wide; and traffic beyond the named
// destinations is named as that, never as an address.
func TestAStepsDestinationsSayWhatTheyCannotTell(t *testing.T) {
	t.Parallel()

	unsplit := fiveDestinations().WithoutOpenings()
	for name, got := range map[string]string{
		"no destinations":     stepDestinations(nil, t0, t0.Add(time.Minute)),
		"not split by time":   stepDestinations(&unsplit, t0, t0.Add(time.Minute)),
		"a step of no length": stepDestinations(fiveDestinations(), t0, t0),
	} {
		if got != "" {
			t.Errorf("%s: %q, want nothing said", name, got)
		}
	}

	wide := &alloc.JobDestinations{OpeningSeconds: 4,
		Other:    alloc.JobDestination{SentBytes: 9, ReceivedBytes: 9, Connections: 1},
		Openings: []alloc.DestinationOpening{{Ordinal: alloc.MaxJobDestinations, Start: t0.Unix(), SentBytes: 9, ReceivedBytes: 9, Connections: 1}}}
	if got := stepDestinations(wide, t0, t0.Add(time.Minute)); got !=
		"to within 4s, traffic beyond the named destinations (sent 9 B, received 9 B, 1 connection)" {
		t.Errorf("a wide split's destinations = %q", got)
	}

	// Totals at the largest int64 are held there, never wrapped.
	big := &alloc.JobDestinations{OpeningSeconds: 1,
		Destinations: []alloc.JobDestination{{Addr: "10.0.0.1", SentBytes: math.MaxInt64, Connections: 2}},
		Openings: []alloc.DestinationOpening{
			{Ordinal: 0, Start: t0.Unix(), SentBytes: math.MaxInt64, Connections: 1},
			{Ordinal: 0, Start: t0.Unix() + 1, SentBytes: math.MaxInt64, Connections: 1},
		}}
	if got := stepDestinations(big, t0, t0.Add(time.Minute)); !strings.HasPrefix(got, "10.0.0.1 (sent 8.0 EiB") {
		t.Errorf("a step whose bytes pass an int64 = %q, want them held at the largest", got)
	}
}

// `billet jobs show` AND ITS PAGE PUT A STEP'S DESTINATIONS UNDER IT, with or
// without a series, since they need only the step's times and the split.
func TestJobsShowPutsAStepsDestinationsUnderIt(t *testing.T) {
	t.Parallel()

	measured := &alloc.RecordedUsage{Node: "n1", JobUsage: alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000, Unmeasured: []string{alloc.UsageEnergy},
		Destinations: fiveDestinations(),
	}}
	step := github.JobStep{Number: 3, Name: "Build", Conclusion: "success", StartedAt: t0,
		CompletedAt: t0.Add(time.Minute)}
	const want = stepDestinationsLabel + ": 10.0.0.2 (sent 400 B"

	var out bytes.Buffer
	renderSteps(&out, jobSteps{runner: "billet-l1", measured: measured, unaligned: "no series",
		job: github.WorkflowJob{ID: 7, Name: "test", Steps: []github.JobStep{step}}})
	if !strings.Contains(out.String(), want) {
		t.Errorf("jobs show does not put the step's destinations under it:\n%s", out.String())
	}

	row := stepRow(step, nil, measured)
	if !strings.HasPrefix(row.Destinations, "10.0.0.2 (sent 400 B") {
		t.Errorf("the page's step row has destinations %q", row.Destinations)
	}
	var page bytes.Buffer
	if err := renderJobPage(&page, jobPageOf(alloc.JobRecord{LeaseID: "l1"}, measured,
		jobSteps{runner: "billet-l1", measured: measured,
			job: github.WorkflowJob{ID: 7, Name: "test", Steps: []github.JobStep{step}}}, nil, "no series")); err != nil {
		t.Fatalf("renderJobPage: %v", err)
	}
	if !strings.Contains(page.String(), html.EscapeString("Destinations, by when each connection opened: 10.0.0.2")) {
		t.Errorf("the page does not put the step's destinations under it")
	}
}
