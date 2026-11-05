package replay

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// THE TAIL IS NEAREST RANK, in integers: p99.99 of ten thousand and one waits
// is the ten-thousandth, where a float rounding of 0.9999*n picks another.
func TestTheTailPercentilesAreNearestRank(t *testing.T) {
	t.Parallel()

	sorted := make([]time.Duration, 10001)
	for i := range sorted {
		sorted[i] = time.Duration(i+1) * time.Second
	}

	for _, c := range []struct {
		p    int
		want time.Duration
	}{
		{5000, 5001 * time.Second}, {9000, 9001 * time.Second}, {9900, 9901 * time.Second},
		{9999, 10000 * time.Second},
	} {
		if got := percentile(sorted, c.p); got != c.want {
			t.Errorf("percentile(%d) of 10001 = %s, want %s", c.p, got, c.want)
		}
	}

	// 0.07 x 100 is 7.000000000000001 in float64, so a ceiling in floats takes
	// the eighth of a hundred where the nearest rank is the seventh.
	hundred := sorted[:100]
	if got := percentile(hundred, 700); got != 7*time.Second {
		t.Errorf("percentile(700) of 100 = %s, want the seventh, 7s", got)
	}

	if got := percentile([]time.Duration{time.Minute}, 5000); got != time.Minute {
		t.Errorf("the median of one wait is %s, want it", got)
	}
}

// A REPORT GROUPS BY THE CHARGED SHAPE, and the tail is over started jobs only.
func TestQueueWaitIsGroupedByShape(t *testing.T) {
	t.Parallel()

	start := DefaultStart
	rec := func(vcpu int, wait time.Duration, started bool) Record {
		r := Record{VCPU: vcpu, Arrival: start, QueueWait: Duration(wait)}
		if started {
			r.StartedAt = start.Add(wait)
		}

		return r
	}

	// A hundred 2 vCPU waits of 1s to 100s, given out of order.
	r := &Report{Records: []Record{rec(8, time.Minute, true), rec(8, time.Hour, false)}}
	for i := range 100 {
		r.Records = append(r.Records, rec(2, time.Duration((i*37)%100+1)*time.Second, true))
	}

	byShape := r.QueueWaitByShape()

	if two := byShape[2]; two.Count != 100 || two.P50 != 50*time.Second || two.P90 != 90*time.Second ||
		two.P95 != 95*time.Second || two.P99 != 99*time.Second || two.P9999 != 100*time.Second ||
		two.Max != 100*time.Second {
		t.Errorf("the 2 vCPU shape is %+v; want a hundred waits, p50 50s, p90 90s, p95 95s, p99 99s, "+
			"p99.99 and max 100s", two)
	}

	if eight := byShape[8]; eight.Count != 1 || eight.P9999 != time.Minute {
		t.Errorf("the 8 vCPU shape is %+v; want its one started job, the unstarted one left out", eight)
	}
}

// UTILIZATION AND EMPTY HOSTS ARE INTEGRATED OVER THE SPAN from the ledger's
// own charges: one host, two 4 vCPU charges, the second of two hours idle.
func TestUtilizationAndEmptyHostTimeAreIntegratedFromTheCharges(t *testing.T) {
	t.Parallel()

	// THE ROOM IS THE CEILING when it is below what the hosts hold: 12 vCPU and
	// 48 GiB of the hosts' 16 and 64.
	start := DefaultStart
	fleet := Fleet{
		Hosts: []Host{{Name: "only", VCPU: 8, Memory: 32 * config.GiB},
			{Name: "spare", VCPU: 8, Memory: 32 * config.GiB}},
		MaxVCPU: 12, MaxMemory: 48 * config.GiB,
	}

	// 0h-1h: 8 vCPU on "only"; 1h-2h: nothing; 2h-3h: 4 vCPU on "only". The
	// span is the three hours from the first charge to the last end.
	r := &Report{Fleet: fleet, Records: []Record{
		chargeAt(start, start.Add(time.Hour)),
		chargeAt(start, start.Add(time.Hour)),
		chargeAt(start.Add(2*time.Hour), start.Add(3*time.Hour)),
	}}

	vcpu, memory := r.MeanUtilization()
	if math.Abs(vcpu-12.0/36) > 1e-9 || math.Abs(memory-24.0/144) > 1e-9 {
		t.Errorf("mean utilization %.4f vCPU, %.4f memory; want 12 of 36 vCPU-hours, 24 of 144 GiB-hours",
			vcpu, memory)
	}

	// "only" was empty from 1h to 2h, "spare" for all three.
	if got := r.EmptyHostHours(); math.Abs(got-4) > 1e-9 {
		t.Errorf("%.3f empty host-hours; want 4", got)
	}

	points := r.Utilization(time.Hour)
	if len(points) != 3 || math.Abs(points[0].VCPU-8.0/12) > 1e-9 || points[1].VCPU != 0 ||
		math.Abs(points[2].VCPU-4.0/12) > 1e-9 || !points[1].From.Equal(start.Add(time.Hour)) {
		t.Errorf("hourly utilization %+v; want 8, 0 and 4 of 12 vCPU", points)
	}

	// A HORIZON ENDS THE SPAN: an hour and a half in, the first hour full and
	// the half after it empty.
	r.Horizon = start.Add(90 * time.Minute)
	if vcpu, _ := r.MeanUtilization(); math.Abs(vcpu-8.0/18) > 1e-9 {
		t.Errorf("to the horizon, mean utilization %.4f; want 8 of 18 vCPU-hours", vcpu)
	}
}

// A COMPLETION DELIVERED AND NEVER ARCHIVED IS LOST, whatever the horizon and
// however the ledger dated the start: judged from what the scripted GitHub
// sent, never from a deadline rebuilt from the ledger's times.
func TestADeliveredCompletionTheLedgerNeverSawIsLost(t *testing.T) {
	t.Parallel()

	start := DefaultStart
	r := &Report{Horizon: start.Add(time.Minute), Records: []Record{
		// Delivered, never archived: lost, though its start was dated a
		// nanosecond late and a rebuilt deadline would fall past the horizon.
		{Seq: 1, StartedAt: start.Add(time.Nanosecond)},
		// Running at the horizon, its completion not yet delivered.
		{Seq: 2, StartedAt: start},
		// Delivered and archived.
		{Seq: 3, StartedAt: start, FinishedAt: start.Add(time.Minute)},
	}}

	r.noteLostCompletions(map[int64]bool{1: true, 3: true})

	if len(r.LostCompletions) != 1 || r.LostCompletions[0] != 1 {
		t.Errorf("lost completions %v; want job 1 alone", r.LostCompletions)
	}
}

// A COMPARISON REFUSES A REPORT THAT CANNOT SPEAK FOR ITS REPLAY, each reason
// on its own, and a horizon excuses only the jobs it cut short.
func TestAComparisonRefusesAnUnspeakableReport(t *testing.T) {
	t.Parallel()

	horizon := DefaultStart.Add(time.Hour)

	for _, c := range []struct {
		name   string
		report Report
		want   string
	}{
		{"sound", Report{}, ""},
		{"overcommit", Report{Violations: []string{"host-1 over by 2 vCPU"}}, "overcommit"},
		{"missing", Report{Missing: []int64{4}}, "no row for jobs [4]"},
		{"unstarted without a horizon", Report{Unstarted: []int64{5}}, "with no horizon"},
		{"unfinished without a horizon", Report{Unfinished: []int64{6}}, "with no horizon"},
		{"cut short by the horizon", Report{Horizon: horizon, Unstarted: []int64{5}, Unfinished: []int64{6}}, ""},
		{"lost at the horizon", Report{Horizon: horizon, Unfinished: []int64{6}, LostCompletions: []int64{6}},
			"jobs [6] were completed"},
		{"start lost at the horizon", Report{Horizon: horizon, Unstarted: []int64{7}, LostStarts: []int64{7}},
			"jobs [7] were started"},
		// The specific cause is named before the general one.
		{"lost with no horizon", Report{Unfinished: []int64{6}, LostCompletions: []int64{6}},
			"jobs [6] were completed"},
	} {
		got := c.report.unspeakable()
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: unspeakable says %q; want %q", c.name, got, c.want)
		}
	}
}

// A COMPARISON MEASURES EACH POLICY OVER ITS OWN SPAN, AND SAYS WHICH: the
// same share of the room over a longer span is printed with that span.
func TestAComparisonPrintsEachPolicysSpan(t *testing.T) {
	t.Parallel()

	start := DefaultStart
	fleet := Fleet{Hosts: []Host{{Name: "only", VCPU: 8, Memory: 32 * config.GiB}}, MaxVCPU: 8,
		MaxMemory: 32 * config.GiB}

	// 4 of 8 vCPU and 8 of 32 GiB: one policy for an hour, the other for two.
	// A replay that charged nothing has no span and no shares.
	soon := &Report{Fleet: fleet, Records: []Record{chargeAt(start, start.Add(time.Hour))}}
	late := &Report{Fleet: fleet, Records: []Record{chargeAt(start, start.Add(2*time.Hour))}}
	idle := &Report{Fleet: fleet}

	table := Comparison{Policies: []Policy{{Name: "soon"}, {Name: "late"}, {Name: "idle"}},
		Reports: []*Report{soon, late, idle}}.Markdown()

	if !strings.Contains(table, "| soon | 0 | 0 | 0 | 50% | 25% | 0.0 | 1.00 |") ||
		!strings.Contains(table, "| late | 0 | 0 | 0 | 50% | 25% | 0.0 | 2.00 |") ||
		!strings.Contains(table, "| idle | 0 | 0 | 0 | — | — | — | — |") {
		t.Errorf("the table is\n%s\nwant each policy's figures over its own span, one hour and two, and none "+
			"for a replay that charged nothing", table)
	}
}

// A SHAPE THAT STARTED NOTHING SHOWS NO WAITS, and its backlog at the horizon
// is in the table beside the waits it leaves out.
func TestTheComparisonTableShowsAStarvedShapesBacklog(t *testing.T) {
	t.Parallel()

	horizon := DefaultStart.Add(time.Hour)
	fleet := Fleet{Tiers: []TierShape{{Label: "billet-4vcpu", VCPU: 4}, {Label: "billet-16vcpu", VCPU: 16}}}

	wide := Arrival{Seq: 2, At: DefaultStart.Add(10 * time.Minute), Tier: "billet-16vcpu"}
	starved := &Report{Fleet: fleet, Horizon: horizon, StillQueued: []int64{2},
		queued: map[int64]Arrival{2: wide},
		Records: []Record{{Seq: 1, Tier: "billet-4vcpu", VCPU: 4, Arrival: DefaultStart,
			StartedAt: DefaultStart.Add(time.Minute), QueueWait: Duration(time.Minute)}}}

	table := Comparison{Policies: []Policy{{Name: "fill"}}, Reports: []*Report{starved}}.Markdown()

	if !strings.Contains(table, "| 16 vCPU | fill | 0 | — | — | — | — | — | 1 | 50m0s |") ||
		!strings.Contains(table, "| 4 vCPU | fill | 1 | 1m0s |") {
		t.Errorf("the table is\n%s\nwant the 16 vCPU shape with no waits and one job queued for 50m", table)
	}
}

// A START DELIVERED AND NEVER RECORDED IS LOST, not still queued: neither in
// the backlog the percentiles leave out nor in its count by shape.
func TestADeliveredStartTheLedgerNeverSawIsLost(t *testing.T) {
	t.Parallel()

	horizon := DefaultStart.Add(time.Hour)
	// The lost job arrived first, so counting it as queued would make it the
	// oldest.
	trace := Trace{Arrivals: []Arrival{
		{Seq: 1, At: DefaultStart.Add(-30 * time.Minute), Tier: "billet-4vcpu"},
		{Seq: 2, At: DefaultStart, Tier: "billet-4vcpu"},
		{Seq: 3, At: DefaultStart, Tier: "billet-4vcpu"},
		{Seq: 4, At: DefaultStart, Tier: "billet-4vcpu"},
	}}
	r := &Report{Fleet: Fleet{Tiers: []TierShape{{Label: "billet-4vcpu", VCPU: 4}}}, Horizon: horizon,
		Records: []Record{{Seq: 1}, {Seq: 2}}}

	// 1: recorded, its start delivered and never written. 2: recorded, its
	// runner still booting. 3: held by the scripted GitHub alone. 4: started
	// and never recorded at all, which is a job the ledger lost.
	r.classify(trace, horizon, map[int64]bool{1: true, 2: true}, map[int64]bool{2: true, 3: true},
		map[int64]bool{1: true, 4: true})

	if len(r.LostStarts) != 1 || r.LostStarts[0] != 1 || !slices.Equal(r.StillQueued, []int64{2, 3}) ||
		r.QueuedByShape()[4] != (Backlog{Count: 2, Oldest: time.Hour}) || r.LongestQueued != time.Hour ||
		len(r.Missing) != 1 || r.Missing[0] != 4 {
		t.Errorf("lost starts %v, still queued %v (%v by shape, the oldest %s), missing %v; want job 1 lost, 2 "+
			"and 3 queued for an hour and 4 missing", r.LostStarts, r.StillQueued, r.QueuedByShape(),
			r.LongestQueued, r.Missing)
	}
}

// THE SUMMARY SAYS WHAT WAS LOST, WHAT THE HORIZON LEFT QUEUED, WHAT WAS
// REFUSED AND WHAT WAS CHARGED, and the utilization CSV is the same figures by
// bucket.
func TestTheSummaryAndTheUtilizationSayWhatTheReportHolds(t *testing.T) {
	t.Parallel()

	start := DefaultStart
	horizon := start.Add(time.Hour)
	r := &Report{
		Fleet: Fleet{Hosts: []Host{{Name: "only", VCPU: 8, Memory: 32 * config.GiB}}, MaxVCPU: 8,
			MaxMemory: 32 * config.GiB},
		Horizon: horizon, StillQueued: []int64{5}, LongestQueued: 10 * time.Minute, LaterArrivals: 2,
		LostStarts: []int64{7}, Declined: map[string]int{"billet-4vcpu": 3},
		Records: []Record{{Seq: 1, Tier: "billet-4vcpu", Node: "only", VCPU: 4, Memory: 8 * config.GiB,
			Arrival: start, ChargedFrom: start, StartedAt: start.Add(time.Minute), QueueWait: Duration(time.Minute),
			FinishedAt: horizon}},
	}

	summary := r.Summary()
	for _, want := range []string{
		"LOST: the scripted GitHub delivered the start of [7] and the completion of []",
		"horizon " + horizon.Format(time.RFC3339) + ": 1 jobs still queued, recorded or held by the scripted " +
			"GitHub (the oldest for 10m0s), kept out of every percentile; 2 arrive after it",
		"tier billet-4vcpu: 1 jobs, queue wait",
		"; 3 jobs refused at least once",
		"shape 4 vCPU: 1 jobs, queue wait",
		"utilization (charged, escrow included): 50% of the room's vCPU, 25% of its memory; 0.0 host-hours empty",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary lacks %q:\n%s", want, summary)
		}
	}

	var csv strings.Builder
	if err := r.WriteUtilization(&csv, 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	row := start.Format(time.RFC3339) + ",0.5000,0.2500\n"
	if want := "from,vcpu,memory\n" + row + start.Add(30*time.Minute).Format(time.RFC3339) + ",0.5000,0.2500\n"; csv.String() != want {
		t.Errorf("the utilization CSV is\n%s\nwant\n%s", csv.String(), want)
	}
}
