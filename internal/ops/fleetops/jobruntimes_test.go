package fleetops

import (
	"encoding/json"
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
// archived after its runtime, and returns their leases in order.
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

	tier := config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier},
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

		lease, err := a.Reserve(t.Context(), tier.Label)
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

type ranHistoryJob struct {
	name    string
	start   time.Time
	runtime time.Duration
}

// runtimeHistory is three 6m runs of a job three days ago, then one 8m run of
// it a day ago: the first three predicted from nothing learned, the last from
// them.
func runtimeHistory(t *testing.T, stateDir string) []string {
	t.Helper()

	earlier := time.Now().Add(-72 * time.Hour).Truncate(time.Hour)
	later := time.Now().Add(-24 * time.Hour).Truncate(time.Hour)

	return ranHistory(t, stateDir, []ranHistoryJob{
		{"build (ubuntu)", earlier.Add(time.Minute), 6 * time.Minute},
		{"build (ubuntu)", earlier.Add(10 * time.Minute), 6 * time.Minute},
		{"build (ubuntu)", earlier.Add(20 * time.Minute), 6 * time.Minute},
		{"build (ubuntu)", later.Add(5 * time.Minute), 8 * time.Minute},
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

	env, out := bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, leases[3]}); err != nil {
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
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, "--json", leases[3]}); err != nil {
		t.Fatalf("billet jobs show --json: %v", err)
	}

	var rec jobJSON
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("decode the record: %v\n%s", err, out)
	}
	p := rec.Prediction
	if p == nil || p.MedianSeconds != 360 || p.Samples != 3 || p.Level != string(alloc.RuntimeByJob) ||
		p.RuntimeSeconds == nil || *p.RuntimeSeconds != 480 || p.ErrorSeconds == nil || *p.ErrorSeconds != 120 {
		t.Errorf("the record's prediction is %+v; want 360s from 3 runs of the job, a 480s runtime, "+
			"120s over\n%s", p, out)
	}
}

// THE REPORT GRADES EVERY JOB AGAINST WHAT WAS KNOWN BEFORE IT, per tier.
func TestJobsRuntimesGradesEachTiersPredictions(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	runtimeHistory(t, stateDir)

	env, out := bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"runtimes", "--config", cfg, "--json"}); err != nil {
		t.Fatalf("billet jobs runtimes: %v", err)
	}

	var report runtimeReportJSON
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode the report: %v\n%s", err, out)
	}

	// The three 6m jobs against the 10m default (off by 4m each, within 2x),
	// then the 8m job against their 6m (off by 2m, within 2x).
	if len(report.Tiers) != 1 {
		t.Fatalf("the report has %d tiers, want one:\n%s", len(report.Tiers), out)
	}
	got := report.Tiers[0]
	if got.Tier != "billet-2vcpu" || got.Jobs != 4 || got.Within2x != 4 || got.MedianAbsErrorSeconds != 240 ||
		got.Levels["default"] != 3 || got.Levels["job"] != 1 || report.Partial {
		t.Errorf("the report is %+v; want 4 jobs, 4 within 2x, a 240s median error, 3 from the "+
			"default and 1 from the job\n%s", got, out)
	}

	// A NARROWER WINDOW STILL LEARNS FROM THE FORTNIGHT BEFORE IT: graded from
	// two days back, only the 8m job is graded, and it is predicted from the
	// three before the window.
	env, out = bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"runtimes", "--config", cfg, "--json", "--since", "48h"}); err != nil {
		t.Fatalf("billet jobs runtimes --since 48h: %v", err)
	}
	report = runtimeReportJSON{}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode the report: %v\n%s", err, out)
	}
	if len(report.Tiers) != 1 || report.Tiers[0].Jobs != 1 || report.Tiers[0].Levels["job"] != 1 ||
		report.Tiers[0].MedianAbsErrorSeconds != 120 {
		t.Errorf("graded from two days back, the report is %+v; want the one 8m job, predicted from the "+
			"job's history 120s off\n%s", report.Tiers, out)
	}

	env, out = bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"runtimes", "--config", cfg}); err != nil {
		t.Fatalf("billet jobs runtimes: %v", err)
	}
	for _, want := range []string{
		"billet-2vcpu: 4 jobs, median error 4m0s, 100% within 2x of the prediction",
		"  predicted from the job's own history: 1",
		"  predicted from nothing learned (the 10m0s default): 3",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the text report does not say %q:\n%s", want, out)
		}
	}

	if err := Jobs(t.Context(), env, []string{"runtimes", "--config", cfg, "--tier", "billet-typo"}); err == nil ||
		!strings.Contains(err.Error(), `--tier "billet-typo" is not a tier`) {
		t.Errorf("a tier the config does not name gave %v; want it refused by name", err)
	}
}
