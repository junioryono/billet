package fleetops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// ranHistory runs jobs through the real allocator on a clock it moves, each
// bought and assigned a second before its start, named, busy at its start and
// archived after its runtime, and returns their leases in order. A job names
// its tier; the ledger knows billet-2vcpu, which the jobs config declares, and
// billet-4vcpu, which it does not.
func ranHistory(t *testing.T, stateDir string, jobs []ranHistoryJob) []string {
	t.Helper()

	seedLedger(t, stateDir)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the ledger: %v", err)
		}
	}()

	var now time.Time

	small := config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}
	larger := small
	larger.Label, larger.VCPU, larger.Memory = "billet-4vcpu", 4, 16*config.GiB

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{small, larger},
		alloc.WithClock(func() time.Time { return now }), alloc.WithLeaseTTL(90*24*time.Hour))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	leases := make([]string, 0, len(jobs))

	for i, j := range jobs {
		now = j.start.Add(-time.Second)

		lease, err := a.Reserve(t.Context(), cmpOr(j.tier, small.Label))
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, int64(100+i)); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		if err := a.RecordJobIdentity(t.Context(), lease.ID, alloc.HistoryJob{
			JobID: fmt.Sprintf("job-%d", i), Owner: "acme", Repository: "api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", Name: j.name,
		}); err != nil {
			t.Fatalf("RecordJobIdentity: %v", err)
		}

		now = j.start
		for _, phase := range []alloc.Phase{alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy} {
			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
				t.Fatalf("Advance to %s: %v", phase, err)
			}
		}

		now = j.start.Add(j.runtime)
		if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}

		leases = append(leases, lease.ID)
	}

	return leases
}

func cmpOr(s, otherwise string) string {
	if s == "" {
		return otherwise
	}

	return s
}

type ranHistoryJob struct {
	name    string
	start   time.Time
	runtime time.Duration
	tier    string
}

// runtimeHistory is three 6m runs of a job three days ago, one 3m job of
// another tier then, and a day ago three 1m runs of the job inside an hour
// followed by one 8m run later in that hour.
//
// THE LAST IS PREDICTED FROM THE HOUR IT STARTED IN, so from the three 6m runs
// alone: the three 1m runs finished after that hour began. A prediction made
// at its own start would have learned them too, and said 1m.
func runtimeHistory(t *testing.T, stateDir string) []string {
	t.Helper()

	earlier := time.Now().Add(-72 * time.Hour).Truncate(time.Hour)
	later := time.Now().Add(-24 * time.Hour).Truncate(time.Hour)

	return ranHistory(t, stateDir, []ranHistoryJob{
		{name: "build (ubuntu)", start: earlier.Add(time.Minute), runtime: 6 * time.Minute},
		{name: "build (ubuntu)", start: earlier.Add(10 * time.Minute), runtime: 6 * time.Minute},
		{name: "build (ubuntu)", start: earlier.Add(20 * time.Minute), runtime: 6 * time.Minute},
		{name: "lint", start: earlier.Add(30 * time.Minute), runtime: 3 * time.Minute, tier: "billet-4vcpu"},
		{name: "build (ubuntu)", start: later.Add(10 * time.Second), runtime: time.Minute},
		{name: "build (ubuntu)", start: later.Add(90 * time.Second), runtime: time.Minute},
		{name: "build (ubuntu)", start: later.Add(3 * time.Minute), runtime: time.Minute},
		{name: "build (ubuntu)", start: later.Add(5 * time.Minute), runtime: 8 * time.Minute},
	})
}

// A JOB'S REPORT PRINTS ITS RUNTIME BESIDE WHAT WAS PREDICTED WHEN IT STARTED,
// learned from the jobs that had finished before the hour it started in, and
// the JSON record carries the same.
func TestJobsShowPrintsTheRuntimeBesideItsPrediction(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)

	leases := runtimeHistory(t, stateDir)
	last := leases[len(leases)-1]

	env, out := bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, last}); err != nil {
		t.Fatalf("billet jobs show: %v", err)
	}
	if want := "runtime    ran 8m0s; predicted 6m0s (p90 6m0s) from 3 runs of this job, and it ran 2m0s " +
		"longer\n"; !strings.Contains(out.String(), want) {
		t.Errorf("the report does not say %q:\n%s", want, out)
	}

	env, out = bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, leases[0]}); err != nil {
		t.Fatalf("billet jobs show: %v", err)
	}
	if want := "runtime    ran 6m0s; predicted 10m0s, the default, with nothing learned, and it ran 4m0s " +
		"shorter\n"; !strings.Contains(out.String(), want) {
		t.Errorf("the first job's report does not say %q:\n%s", want, out)
	}

	env, out = bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, "--json", last}); err != nil {
		t.Fatalf("billet jobs show --json: %v", err)
	}

	var rec jobJSON
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("decode the record: %v\n%s", err, out)
	}
	p := rec.Prediction
	if p == nil || p.MedianSeconds != 360 || p.P90Seconds != 360 || p.Samples != 3 ||
		p.Level != string(alloc.RuntimeByJob) || p.RuntimeSeconds == nil || *p.RuntimeSeconds != 480 ||
		p.ErrorSeconds == nil || *p.ErrorSeconds != 120 || rec.PredictionUnavailable != "" {
		t.Errorf("the record's prediction is %+v; want 360s from 3 runs of the job, a 480s runtime, "+
			"120s over\n%s", p, out)
	}
}

// A PREDICTION THAT CANNOT BE READ IS SAID, AND THE REPORT IS STILL PRINTED.
func TestJobsShowSaysWhyAJobWasNotPredicted(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)

	leases := runtimeHistory(t, stateDir)

	a, closeDB, err := ControlPlaneAllocator(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer closeDB()

	unreadable := func(time.Time) (*alloc.RuntimePriors, error) {
		return nil, errors.New("the history could not be read")
	}

	var out bytes.Buffer
	if _, err := showJob(t.Context(), &out, a, unreadable, cfg, leases[0], false, false, nil, false); err != nil {
		t.Fatalf("a job whose prediction could not be read was not shown: %v", err)
	}
	if want := "runtime    ran 6m0s; not predicted: the history could not be read\n"; !strings.Contains(
		out.String(), want) {
		t.Errorf("the report does not say %q:\n%s", want, out.String())
	}

	out.Reset()
	if _, err := showJob(t.Context(), &out, a, unreadable, cfg, leases[0], true, false, nil, false); err != nil {
		t.Fatalf("a job whose prediction could not be read was not shown as JSON: %v", err)
	}

	var rec jobJSON
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("decode the record: %v\n%s", err, out.String())
	}
	if rec.Prediction != nil || rec.PredictionUnavailable != "the history could not be read" {
		t.Errorf("the record says prediction %+v, unavailable %q; want none, and why", rec.Prediction,
			rec.PredictionUnavailable)
	}
}

// THE RUNTIME LINE SAYS WHERE THE JOB IS AND WHERE ITS PREDICTION CAME FROM.
func TestTheRuntimeLineSaysWhatItKnows(t *testing.T) {
	name := alloc.RuntimeEstimate{Median: 5 * time.Minute, P90: 9 * time.Minute, Samples: 4,
		Level: alloc.RuntimeByName}
	tier := alloc.RuntimeEstimate{Median: 5 * time.Minute, P90: 9 * time.Minute, Samples: 12,
		Level: alloc.RuntimeByTier}

	for _, c := range []struct {
		rec  alloc.JobRecord
		want string
	}{
		{alloc.JobRecord{}, "not started"},
		{alloc.JobRecord{StartedAt: "2026-10-20T12:00:00Z", Predicted: &name},
			"still running; predicted 5m0s (p90 9m0s) from 4 runs of this job across its matrix"},
		{alloc.JobRecord{StartedAt: "2026-10-20T12:00:00Z", FinishedAt: "2026-10-20T12:05:00Z", Predicted: &tier},
			"ran 5m0s; predicted 5m0s (p90 9m0s) from 12 jobs of its tier, and it ran exactly that"},
		{alloc.JobRecord{StartedAt: "2026-10-20T12:00:00Z", FinishedAt: "2026-10-20T12:05:00Z"},
			"ran 5m0s; not predicted"},
	} {
		if got := runtimeLine(c.rec); got != c.want {
			t.Errorf("runtimeLine(%+v) = %q; want %q", c.rec, got, c.want)
		}
	}
}

// THE REPORT GRADES EVERY JOB AGAINST WHAT WAS KNOWN BEFORE IT, per tier.
func TestJobsRuntimesGradesEachTiersPredictions(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	runtimeHistory(t, stateDir)

	report := runtimesJSON(t, cfg, "--tier", "billet-2vcpu")

	// The three 6m jobs against the 10m default (off by 4m each, within 2x);
	// the three 1m jobs against the 6m runs (off by 5m each, not within 2x);
	// the 8m job against the 6m runs (off by 2m, within 2x).
	if len(report.Tiers) != 1 {
		t.Fatalf("the report has %d tiers, want only the one asked for:\n%+v", len(report.Tiers), report)
	}
	got := report.Tiers[0]
	if got.Tier != "billet-2vcpu" || got.Jobs != 7 || got.Within2x != 4 || got.MedianAbsErrorSeconds != 240 ||
		got.Levels["default"] != 3 || got.Levels["job"] != 4 || report.Partial {
		t.Errorf("the report is %+v; want 7 jobs, 4 within 2x, a 240s median error, 3 from the "+
			"default and 4 from the job", got)
	}

	if all := runtimesJSON(t, cfg); len(all.Tiers) != 2 {
		t.Errorf("with no --tier the report has %+v; want both tiers the ledger graded", all.Tiers)
	}

	// A NARROWER WINDOW STILL LEARNS FROM THE FORTNIGHT BEFORE IT: graded from
	// two days back, only the day-old jobs are graded, predicted from the runs
	// before the window.
	narrow := runtimesJSON(t, cfg, "--tier", "billet-2vcpu", "--since", "48h")
	if len(narrow.Tiers) != 1 || narrow.Tiers[0].Jobs != 4 || narrow.Tiers[0].Levels["job"] != 4 ||
		narrow.Tiers[0].MedianAbsErrorSeconds != 300 {
		t.Errorf("graded from two days back, the report is %+v; want the four day-old jobs, predicted from "+
			"the job's history, a 300s median error", narrow.Tiers)
	}

	env, out := bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"runtimes", "--config", cfg, "--tier", "billet-2vcpu"}); err != nil {
		t.Fatalf("billet jobs runtimes: %v", err)
	}
	for _, want := range []string{
		"billet-2vcpu: 7 jobs, median error 4m0s, 57% within 2x of the prediction",
		"  predicted from the job's own history: 4",
		"  predicted from nothing learned (the 10m0s default): 3",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the text report does not say %q:\n%s", want, out)
		}
	}

	env, out = bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"runtimes", "--config", cfg, "--since", "1h"}); err != nil {
		t.Fatalf("billet jobs runtimes --since 1h: %v", err)
	}
	if want := "no job started and finished in the window"; !strings.Contains(out.String(), want) {
		t.Errorf("a window with no job does not say %q:\n%s", want, out)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--tier", "billet-typo"}, `--tier "billet-typo" is not a tier`},
		{[]string{"--since", "0s"}, "--since must be positive"},
	} {
		err := Jobs(t.Context(), env, append([]string{"runtimes", "--config", cfg}, c.args...))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("billet jobs runtimes %v gave %v; want it refused saying %q", c.args, err, c.want)
		}
	}
}

// runtimesJSON runs `billet jobs runtimes --json` with args and decodes it.
func runtimesJSON(t *testing.T, cfg string, args ...string) runtimeReportJSON {
	t.Helper()

	env, out := bufferedEnv()
	if err := Jobs(t.Context(), env, append([]string{"runtimes", "--config", cfg, "--json"}, args...)); err != nil {
		t.Fatalf("billet jobs runtimes %v: %v", args, err)
	}

	var report runtimeReportJSON
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode the report: %v\n%s", err, out)
	}

	return report
}
