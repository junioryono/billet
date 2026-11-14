package fleetops

import (
	"bytes"
	"encoding/json"
	"html"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cloudenergy"
)

// ONLY A BOUGHT SHAPE THAT NO HOST MEASURED IS ESTIMATED, through the ledger: a
// job with energy measured keeps its source, a host-backed job with none and a
// bought job still running are not estimated, every job without measured energy
// is counted as not measured whether or not it was estimated, and the estimate is
// a source of its own that is never added to a measured one. Every form of the
// report says so.
func TestJobStatsEstimatesOnlyABoughtShapeNoHostMeasured(t *testing.T) {
	t.Parallel()

	job := func(lease, shape string, ran time.Duration, u *fixedUsage) fixedJob {
		return fixedJob{lease: lease, tier: "t", repo: "acme/api", workflow: fixedWorkflow, job: "test", run: 1,
			result: "succeeded", startedAgo: time.Hour, ran: ran, vcpu: 2, instanceType: shape, usage: u}
	}
	report := statsOver(t, statsByRepository, "", []fixedJob{
		job("bought", "c7i.large", 10*time.Minute, nil),
		job("bought-energy-unmeasured", "c7i.large", 10*time.Minute, &fixedUsage{unmeasured: alloc.UsageEnergy}),
		job("bought-measured", "c7i.large", 10*time.Minute,
			&fixedUsage{activeUJ: 1_000_000, idleUJ: 500_000, energySource: alloc.EnergyRAPL}),
		job("bought-running", "c7i.large", 0, nil),
		job("host-backed", "", 10*time.Minute, nil),
	})

	f := report.Groups[0].Current
	// Two vCPUs for ten minutes at 2.12 W each is 2,544 J a job.
	want := []statsEnergy{
		{Source: cloudenergy.Source, ActiveMicrojoules: 2 * 2_544_000_000, Jobs: 2},
		{Source: alloc.EnergyRAPL, ActiveMicrojoules: 1_000_000, IdleMicrojoules: 500_000, Jobs: 1},
	}
	if len(f.Energy) != 2 || f.Energy[0] != want[0] || f.Energy[1] != want[1] {
		t.Errorf("energy = %+v, want %+v", f.Energy, want)
	}
	if f.EnergyUnmeasured != 4 {
		t.Errorf("%d jobs have no energy measured, want the two estimated, the one still running and the "+
			"host-backed one", f.EnergyUnmeasured)
	}

	const wantText = "estimate: 5.1 kJ (not measured: each bought shape's vCPUs over its duration at an assumed " +
		"50% CPU, 0.74 to 3.50 W per vCPU, Cloud Carbon Footprint's AWS averages) over 2 jobs; rapl: 1.0 J active, " +
		"0.5 J idle (package RAPL, a model rather than a meter) over 1 jobs; not measured for 4 jobs, " +
		"2 of them estimated"
	var out bytes.Buffer
	renderJobStats(&out, report)
	if !strings.Contains(out.String(), wantText) {
		t.Errorf("the report does not say %q:\n%s", wantText, out.String())
	}

	out.Reset()
	if err := renderJobStatsPage(&out, report); err != nil {
		t.Fatalf("renderJobStatsPage: %v", err)
	}
	if !strings.Contains(out.String(), html.EscapeString(wantText)) {
		t.Errorf("the page does not say %q:\n%s", wantText, out.String())
	}

	out.Reset()
	if err := renderJobStatsJSON(&out, report); err != nil {
		t.Fatalf("renderJobStatsJSON: %v", err)
	}
	var decoded struct {
		Groups []struct {
			Current struct {
				Energy           []statsEnergyJSON `json:"energy"`
				EnergyUnmeasured int               `json:"energy_not_measured"`
			} `json:"current"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	cur := decoded.Groups[0].Current
	if cur.EnergyUnmeasured != 4 || len(cur.Energy) != 2 {
		t.Fatalf("the JSON = %+v, want two sources and 4 jobs not measured", cur)
	}
	if e := cur.Energy[0]; e.Source != cloudenergy.Source || e.ActiveJoules != "5088" || e.IdleJoules != nil ||
		e.IdleIncluded == nil || !*e.IdleIncluded || e.Jobs != 2 || e.Saturated {
		t.Errorf("the estimate in JSON = %+v, want 5,088 J over 2 jobs with idle inside it and no idle figure", e)
	}
}

// A TOTAL HELD AT ITS LARGEST IS A LOWER BOUND, never a wrapped negative: each
// job's estimate is bounded, and many at the bound still add up past an int64.
func TestAnEnergyTotalPastAnInt64IsALowerBound(t *testing.T) {
	t.Parallel()

	e := &statsEnergy{Source: cloudenergy.Source}
	e.add(math.MaxInt64-10, 0)
	if e.Saturated {
		t.Fatal("a total below the largest int64 was marked saturated")
	}
	e.add(11, 0)
	if !e.Saturated || e.ActiveMicrojoules != math.MaxInt64 || e.Jobs != 2 {
		t.Fatalf("after passing the largest int64: %+v, want it held there, saturated, over 2 jobs", e)
	}

	// The true total is MaxInt64+1 µJ, 9,223,372,036.854775808 kJ: the bound
	// claimed is rounded down from it, never up past it.
	f := &statsFigures{Energy: []statsEnergy{*e}, EnergyUnmeasured: 2}
	if text := energyText(f); !strings.HasPrefix(text, "at least estimate: 9223372036.8 kJ (") {
		t.Errorf("energyText = %q, want it worded as a lower bound rounded down", text)
	}
	for _, tc := range []struct {
		microjoules int64
		want        string
	}{{999_999_999, "999.9 J"}, {1_000_000_000, "1.0 kJ"}, {1_999_999_999, "1.9 kJ"}, {99_999, "0.0 J"}} {
		if got := joulesAtMost(tc.microjoules); got != tc.want {
			t.Errorf("joulesAtMost(%d) = %q, want %q", tc.microjoules, got, tc.want)
		}
	}
	if j := statsFiguresJSONOf(f); !j.Energy[0].Saturated || j.Energy[0].ActiveJoules != "9223372036854.775807" {
		t.Errorf("the JSON = %+v, want saturated at exactly the largest int64 of microjoules", j.Energy[0])
	}

	// BOTH FIGURES ARE EXACT, so neither is claimed past what was counted,
	// whichever of them saturated: here the active one did, and the idle one sits
	// a thousand microjoules below the largest int64.
	r := &statsEnergy{Source: alloc.EnergyRAPL}
	r.add(math.MaxInt64, math.MaxInt64-1000)
	r.add(1, 0)
	rj := statsFiguresJSONOf(&statsFigures{Energy: []statsEnergy{*r}}).Energy[0]
	if rj.ActiveJoules != "9223372036854.775807" || rj.IdleJoules == nil || *rj.IdleJoules != "9223372036854.774807" {
		t.Errorf("the JSON = active %s, idle %v; want each exactly what was counted", rj.ActiveJoules, rj.IdleJoules)
	}
	for _, tc := range []struct {
		microjoules int64
		want        json.Number
	}{{0, "0"}, {1, "0.000001"}, {8480, "0.00848"}, {5_088_000_000, "5088"}, {1_500_000, "1.5"}} {
		if got := joulesNumber(tc.microjoules); got != tc.want {
			t.Errorf("joulesNumber(%d) = %s, want %s", tc.microjoules, got, tc.want)
		}
	}
}

// boughtRecord is a job that ran ninety seconds to GitHub's result on four vCPUs
// of a bought shape, its lease archived a minute later.
func boughtRecord(shape string) alloc.JobRecord {
	start := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	return alloc.JobRecord{LeaseID: "l1", Tier: "t", InstanceType: shape, VCPU: 4,
		StartedAt: start.Format(time.RFC3339Nano), ResultAt: start.Add(90 * time.Second).Format(time.RFC3339Nano),
		FinishedAt: start.Add(150 * time.Second).Format(time.RFC3339Nano)}
}

// raplUsage is a host's report that measured the job's energy.
var raplUsage = &alloc.RecordedUsage{Node: "n1", JobUsage: alloc.JobUsage{
	Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000,
	EnergySource: alloc.EnergyRAPL, EnergyActiveMicrojoules: 7_000_000,
}}

// noEnergyUsage is a host's report that named energy unmeasured.
var noEnergyUsage = &alloc.RecordedUsage{Node: "n1", JobUsage: alloc.JobUsage{
	Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000, Unmeasured: []string{alloc.UsageEnergy},
}}

// `billet jobs show`, ITS JSON AND ITS PAGE ESTIMATE THE SAME JOBS AS THE STATS
// DO, over the job's own duration to GitHub's result, word the estimate as one,
// and estimate nothing a host measured or that bought nothing. Four vCPUs for
// ninety seconds at 2.12 W each is 763.2 J.
func TestJobsShowEstimatesOnlyABoughtShapeNoHostMeasured(t *testing.T) {
	t.Parallel()

	const estimated = "763.2 J, estimated, not measured: 4 vCPU for 1m30s at an assumed 50% CPU"
	with := func(change func(*alloc.JobRecord)) alloc.JobRecord {
		r := boughtRecord("c7i.xlarge")
		change(&r)
		return r
	}
	for _, tc := range []struct {
		name     string
		rec      alloc.JobRecord
		measured *alloc.RecordedUsage
		want     bool
		// line is the label the estimate is on in the text report.
		line string
	}{
		{"bought, no report", boughtRecord("c7i.xlarge"), nil, true, "energy     "},
		{"bought, a report that measured no energy", boughtRecord("c7i.xlarge"), noEnergyUsage, true, "estimated  "},
		{"bought, a result and the lease not yet archived", with(func(r *alloc.JobRecord) { r.FinishedAt = "" }),
			nil, true, "energy     "},
		{"host-backed, no report", boughtRecord(""), nil, false, ""},
		{"bought, energy measured", boughtRecord("c7i.xlarge"), raplUsage, false, ""},
		{"bought, no result yet", with(func(r *alloc.JobRecord) { r.ResultAt = "" }), nil, false, ""},
	} {
		var text bytes.Buffer
		renderJob(&text, tc.rec, tc.measured, nil)
		if got := strings.Contains(text.String(), estimated); got != tc.want {
			t.Errorf("%s: jobs show estimates %v, want %v:\n%s", tc.name, got, tc.want, text.String())
		}
		if tc.want && !strings.Contains(text.String(), tc.line+estimated) {
			t.Errorf("%s: the estimate is not on its line %q:\n%s", tc.name, tc.line, text.String())
		}
		if tc.measured == noEnergyUsage && !strings.Contains(text.String(), "energy     not measured") {
			t.Errorf("%s: the report's own energy no longer says not measured:\n%s", tc.name, text.String())
		}

		var js bytes.Buffer
		if err := renderJobJSON(&js, tc.rec, tc.measured, nil, "", nil, jobEgress{}, jobTests{}, nil); err != nil {
			t.Fatalf("%s: renderJobJSON: %v", tc.name, err)
		}
		var decoded struct {
			Estimate *energyEstimateJSON `json:"energy_estimate"`
			Usage    *struct {
				Measured map[string]bool `json:"measured"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
			t.Fatalf("%s: not JSON: %v", tc.name, err)
		}
		switch e := decoded.Estimate; {
		case !tc.want && e != nil:
			t.Errorf("%s: energy_estimate = %+v, want null", tc.name, e)
		case tc.want && (e == nil || e.Joules != 763.2 || e.VCPU != 4 || e.DurationSeconds != 90 ||
			e.Utilization != 0.5 || e.MinWattsPerVCPU != 0.74 || e.MaxWattsPerVCPU != 3.5 ||
			!strings.HasPrefix(e.Method, "estimated, not measured")):
			t.Errorf("%s: energy_estimate = %+v, want 763.2 J for 4 vCPU over 90 s at 50%%", tc.name, e)
		}
		if tc.measured == noEnergyUsage && (decoded.Usage == nil || decoded.Usage.Measured[alloc.UsageEnergy]) {
			t.Errorf("%s: the usage in JSON says energy was measured: %+v", tc.name, decoded.Usage)
		}

		var page bytes.Buffer
		if err := renderJobPage(&page, jobPageOf(tc.rec, tc.measured, jobSteps{none: "x"}, nil, "no series")); err != nil {
			t.Fatalf("%s: renderJobPage: %v", tc.name, err)
		}
		if got := strings.Contains(page.String(), html.EscapeString(estimated)); got != tc.want {
			t.Errorf("%s: the page estimates %v, want %v", tc.name, got, tc.want)
		}
	}
}

// AN ESTIMATE TOO SMALL FOR joules' ONE DECIMAL keeps its figures, so it never
// reads as nothing.
func TestASmallEstimateNeverReadsAsZero(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		microjoules int64
		want        string
	}{{2120, "0.00212 J"}, {49_999, "0.05 J"}, {50_000, "0.1 J"}, {763_200_000, "763.2 J"}} {
		if got := estimateJoules(tc.microjoules); got != tc.want {
			t.Errorf("estimateJoules(%d) = %q, want %q", tc.microjoules, got, tc.want)
		}
	}
}

// A SMALL ESTIMATE IS NEVER ZERO IN ANY REPORT: four vCPUs for a millisecond is
// 8,480 µJ, which every rendering of the stats and of jobs show keeps.
func TestASmallEstimateIsNeverZeroInAnyReport(t *testing.T) {
	t.Parallel()

	const small = "0.00848 J"
	report := statsOver(t, statsByRepository, "", []fixedJob{{lease: "tiny", tier: "t", repo: "acme/api",
		workflow: fixedWorkflow, job: "test", run: 1, result: "succeeded", startedAgo: time.Hour,
		ran: time.Millisecond, vcpu: 4, instanceType: "c7i.xlarge"}})
	var out bytes.Buffer
	renderJobStats(&out, report)
	if !strings.Contains(out.String(), "estimate: "+small+" (") {
		t.Errorf("the stats do not say %s:\n%s", small, out.String())
	}
	out.Reset()
	if err := renderJobStatsPage(&out, report); err != nil {
		t.Fatalf("renderJobStatsPage: %v", err)
	}
	if !strings.Contains(out.String(), "estimate: "+small+" (") {
		t.Errorf("the stats page does not say %s", small)
	}
	if e := statsFiguresJSONOf(&report.Groups[0].Current).Energy; len(e) != 1 || e[0].ActiveJoules != "0.00848" {
		t.Errorf("the stats JSON = %+v, want 0.00848 J", e)
	}

	rec := boughtRecord("c7i.xlarge")
	start, err := time.Parse(time.RFC3339Nano, rec.StartedAt)
	if err != nil {
		t.Fatalf("the record's start: %v", err)
	}
	rec.ResultAt = start.Add(time.Millisecond).Format(time.RFC3339Nano)
	const shown = small + ", estimated, not measured: 4 vCPU for 1ms"
	var text bytes.Buffer
	renderJob(&text, rec, nil, nil)
	if !strings.Contains(text.String(), shown) {
		t.Errorf("jobs show does not say %q:\n%s", shown, text.String())
	}
	var page bytes.Buffer
	if err := renderJobPage(&page, jobPageOf(rec, nil, jobSteps{none: "x"}, nil, "no series")); err != nil {
		t.Fatalf("renderJobPage: %v", err)
	}
	if !strings.Contains(page.String(), html.EscapeString(shown)) {
		t.Errorf("the job page does not say %q", shown)
	}
	var js bytes.Buffer
	if err := renderJobJSON(&js, rec, nil, nil, "", nil, jobEgress{}, jobTests{}, nil); err != nil {
		t.Fatalf("renderJobJSON: %v", err)
	}
	var decoded struct {
		Estimate *energyEstimateJSON `json:"energy_estimate"`
	}
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil || decoded.Estimate == nil ||
		decoded.Estimate.Joules != 0.00848 {
		t.Errorf("the JSON estimate = %+v, %v; want 0.00848 J", decoded.Estimate, err)
	}
}
