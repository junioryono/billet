package fleetops

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cloudenergy"
)

// ONLY A BOUGHT SHAPE THAT NO HOST MEASURED IS ESTIMATED, through the ledger: a
// job with energy measured keeps its source, a host-backed job with none and a
// bought job still running are not measured, and the estimate is a source of
// its own that is never added to a measured one.
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
	if f.EnergyUnmeasured != 2 {
		t.Errorf("%d jobs have no energy, want the bought one still running and the host-backed one",
			f.EnergyUnmeasured)
	}

	var out bytes.Buffer
	renderJobStats(&out, report)
	if want := "estimate: 5.1 kJ (not measured: each bought shape's vCPUs over its duration at an assumed 50% " +
		"CPU, 0.74 to 3.50 W per vCPU, Cloud Carbon Footprint's AWS averages) over 2 jobs; rapl: 1.0 J active, " +
		"0.5 J idle (package RAPL, a model rather than a meter) over 1 jobs; not measured for 2 jobs"; !strings.Contains(out.String(), want) {
		t.Errorf("the report does not say %q:\n%s", want, out.String())
	}

	figures := statsFiguresJSONOf(&f)
	estimate := figures.Energy[0]
	if estimate.Source != cloudenergy.Source || estimate.ActiveJoules != 5088 || estimate.IdleJoules != nil ||
		estimate.IdleIncluded == nil || !*estimate.IdleIncluded || estimate.Jobs != 2 {
		t.Errorf("the estimate in JSON = %+v, want 5,088 J over 2 jobs with idle inside it and no idle figure",
			estimate)
	}
}

// boughtRecord is a job that ran ninety seconds on four vCPUs of a bought shape.
func boughtRecord(shape string) alloc.JobRecord {
	start := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	return alloc.JobRecord{LeaseID: "l1", Tier: "t", InstanceType: shape, VCPU: 4,
		StartedAt: start.Format(time.RFC3339Nano), FinishedAt: start.Add(90 * time.Second).Format(time.RFC3339Nano)}
}

// raplUsage is a host's report that measured the job's energy.
var raplUsage = &alloc.RecordedUsage{Node: "n1", JobUsage: alloc.JobUsage{
	Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000,
	EnergySource: alloc.EnergyRAPL, EnergyActiveMicrojoules: 7_000_000,
}}

// `billet jobs show`, ITS JSON AND ITS PAGE ESTIMATE THE SAME JOBS, word the
// estimate as one, and estimate nothing a host measured or that bought nothing.
// Four vCPUs for ninety seconds at 2.12 W each is 763.2 J.
func TestJobsShowEstimatesOnlyABoughtShapeNoHostMeasured(t *testing.T) {
	t.Parallel()

	const estimated = "763.2 J, estimated, not measured: 4 vCPU for 1m30s at an assumed 50% CPU"
	for _, tc := range []struct {
		name     string
		rec      alloc.JobRecord
		measured *alloc.RecordedUsage
		want     bool
	}{
		{"bought, no report", boughtRecord("c7i.xlarge"), nil, true},
		{"host-backed, no report", boughtRecord(""), nil, false},
		{"bought, energy measured", boughtRecord("c7i.xlarge"), raplUsage, false},
		{"bought, still running", func() alloc.JobRecord {
			r := boughtRecord("c7i.xlarge")
			r.FinishedAt = ""
			return r
		}(), nil, false},
	} {
		var text bytes.Buffer
		renderJob(&text, tc.rec, tc.measured, nil)
		if got := strings.Contains(text.String(), estimated); got != tc.want {
			t.Errorf("%s: jobs show estimates %v, want %v:\n%s", tc.name, got, tc.want, text.String())
		}
		if tc.want && !strings.Contains(text.String(), "energy     "+estimated) {
			t.Errorf("%s: the estimate is not on the energy line:\n%s", tc.name, text.String())
		}

		var js bytes.Buffer
		if err := renderJobJSON(&js, tc.rec, tc.measured, nil, "", nil, jobEgress{}, jobTests{}, nil); err != nil {
			t.Fatalf("%s: renderJobJSON: %v", tc.name, err)
		}
		var decoded struct {
			Estimate *energyEstimateJSON `json:"energy_estimate"`
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

		page := jobPageOf(tc.rec, tc.measured, jobSteps{none: "x"}, nil, "no series")
		var onPage bool
		for _, g := range page.Usage.Groups {
			onPage = onPage || strings.Contains(g.Value, estimated)
		}
		if onPage != tc.want {
			t.Errorf("%s: the page estimates %v, want %v: %+v", tc.name, onPage, tc.want, page.Usage.Groups)
		}
	}
}

// A REPORT THAT NAMED ENERGY UNMEASURED ON A BOUGHT SHAPE is estimated too, on a
// line of its own beside the report's own "not measured", so neither hides the
// other.
func TestJobsShowEstimatesBesideAReportThatMeasuredNoEnergy(t *testing.T) {
	t.Parallel()

	noEnergy := &alloc.RecordedUsage{Node: "n1", JobUsage: alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000, Unmeasured: []string{alloc.UsageEnergy},
	}}
	var out bytes.Buffer
	renderJob(&out, boughtRecord("c7i.xlarge"), noEnergy, nil)
	for _, want := range []string{"energy     not measured", "estimated  763.2 J, estimated, not measured"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("jobs show does not say %q:\n%s", want, out.String())
		}
	}
}
