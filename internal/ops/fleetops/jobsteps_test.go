package fleetops

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/usage"
)

// writeJobsConfig is writeCAConfig with an App key real enough to sign a JWT.
func writeJobsConfig(t *testing.T, stateDir string) string {
	t.Helper()

	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}
	path := filepath.Join(dir, "billet.yaml")
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + keyPath + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// fakeJobsGitHub stands in for GitHub's REST API: the installation token, and
// run 4242's job list answered by jobs. It counts the job-list requests.
//
// NOT PARALLEL-SAFE: app.GitHubAPIBase is the process's, so a test using this
// runs alone.
func fakeJobsGitHub(t *testing.T, jobs http.HandlerFunc) *atomic.Int32 {
	t.Helper()

	var asked atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/2/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/4242/jobs", func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer installation-secret" {
			t.Errorf("Authorization = %q", got)
		}
		jobs(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	prev := app.GitHubAPIBase
	app.GitHubAPIBase = srv.URL
	t.Cleanup(func() { app.GitHubAPIBase = prev })

	return &asked
}

// noJobs answers a run with no job on any of billet's runners.
func noJobs(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, `{"total_count":1,"jobs":[{"id":9,"name":"other","runner_name":"elsewhere","steps":[]}]}`)
}

// stepsOn answers the run's job list with one job on lease's runner.
func stepsOn(lease, steps string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":7001,"name":"test","run_attempt":2,
			"runner_name":"billet-%s","steps":[%s]}]}`, lease, steps)
	}
}

// measuredUsage is a usage report with every group read but the thread split.
var measuredUsage = alloc.JobUsage{
	Source: alloc.UsageSourceHost, Unmeasured: []string{alloc.UsageThreads},
	Samples: 11, IntervalMillis: 1000, WindowMillis: 10_000,
	CPUUserMicros: 1_000_000, MemoryPeakBytes: 1 << 30,
	EnergyActiveMicrojoules: 50_000_000, EnergySource: alloc.EnergyRAPLUnsplit,
}

// clocklessSeries is an encoded series of the codec that records no start.
func clocklessSeries(t *testing.T) *alloc.UsageSeries {
	t.Helper()

	var points []usage.Point
	for s := range int64(11) {
		points = append(points, usage.Point{OffsetMillis: s * 1000, CPUUsage: s * 100_000})
	}
	data, _, err := usage.EncodeSeries(points, alloc.MaxUsageSeriesBytes)
	if err != nil {
		t.Fatalf("EncodeSeries: %v", err)
	}

	return &alloc.UsageSeries{Codec: usage.SeriesCodec, Data: data}
}

const forgingStep = `{"number":1,"name":"Run make\nstep 2     forged","status":"completed",
	"conclusion":"success","started_at":"2026-10-09T12:00:01Z","completed_at":"2026-10-09T12:00:04Z"}`

// THE COMMAND READS THE STEPS FROM GITHUB, assembled as the CLI assembles it:
// the lease's own runner is asked for in run 4242, every step is printed with
// its name quoted, and a series with no start on the host's clock gives no
// step any usage and says why.
func TestJobsShowListsTheStepsGitHubRecorded(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	usageReport := measuredUsage
	lease := seedJob(t, stateDir, jobSeed{requestID: 77, job: &forgedJob, usage: &usageReport,
		series: clocklessSeries(t)})
	asked := fakeJobsGitHub(t, stepsOn(lease, forgingStep))

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})

	for _, want := range []string{
		`github job 7001 "test", attempt 2, on runner billet-` + lease + ", steps listed: 1",
		`step 1     "Run make\nstep 2     forged" "success", started 2026-10-09T12:00:01Z, took 3s`,
		"usage per step: could not tell, because the series was recorded without its start on the host's clock",
		"measured by the host epyc-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nstep 2 ") {
		t.Errorf("a step name forged a line of the report:\n%s", out)
	}
	if asked.Load() != 1 {
		t.Errorf("GitHub was asked %d times, want once", asked.Load())
	}
}

// A FAILED GITHUB READ IS A LINE OF THE REPORT, NOT A FAILED COMMAND: the
// record and the usage print as before, the error is quoted, the token is not
// in it, and a refusal that reads as a missing permission names it.
func TestJobsShowSaysWhyTheStepsCouldNotBeRead(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		want   []string
		hint   bool
	}{
		"github down":      {http.StatusBadGateway, []string{`could not be read from GitHub: "github: list the run's jobs: HTTP 502: down\nstep 1     forged"`}, false},
		"no actions: read": {http.StatusForbidden, []string{"HTTP 403", "needs the App's `actions: read`"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			cfg := writeJobsConfig(t, stateDir)
			usageReport := measuredUsage
			lease := seedJob(t, stateDir, jobSeed{requestID: 77, job: &forgedJob, usage: &usageReport})
			fakeJobsGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"message":"down\nstep 1     forged"}`)
			})

			out := capture(t, func() {
				if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
					t.Errorf("a failed GitHub read failed the command: %v", err)
				}
			})
			for _, want := range append(tc.want, "measured by the host epyc-1", `"acme/api"`) {
				if !strings.Contains(out, want) {
					t.Errorf("the report does not say %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "needs the App's") != tc.hint {
				t.Errorf("permission hint shown = %v, want %v:\n%s", !tc.hint, tc.hint, out)
			}
			if strings.Contains(out, "installation-secret") || strings.Contains(out, "\nstep 1 ") {
				t.Errorf("the report carries the token or a forged line:\n%s", out)
			}
		})
	}
}

// A JOB GITHUB NO LONGER LISTS IS SAID TO BE MISSING, never shown as a job with
// no steps.
func TestJobsShowSaysWhenGitHubListsNoJobOnTheRunner(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	lease := seedMeasuredJob(t, stateDir, 77, nil)
	fakeJobsGitHub(t, noJobs)

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})
	if !strings.Contains(out, "lists no job on runner billet-"+lease+", so there are no steps to show") {
		t.Errorf("a job GitHub does not list was not said to be missing:\n%s", out)
	}
}

// WITHOUT A RECORDED GITHUB JOB THERE IS NOTHING TO ASK FOR, and GitHub is not
// asked.
func TestJobsShowWithNoRecordedJobAsksNothing(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	lease := seedJob(t, stateDir, jobSeed{requestID: 77})
	asked := fakeJobsGitHub(t, noJobs)

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})
	if !strings.Contains(out, "steps      no GitHub job is recorded for this lease") {
		t.Errorf("a lease with no job did not say so:\n%s", out)
	}
	if asked.Load() != 0 {
		t.Errorf("GitHub was asked %d times about a lease that names no job", asked.Load())
	}
}

// A SILENT GITHUB IS CUT OFF AT THE COMMAND'S OWN DEADLINE, and the report
// still prints.
func TestJobsShowStopsWaitingForGitHub(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	lease := seedMeasuredJob(t, stateDir, 77, nil)
	fakeJobsGitHub(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	prev := stepsReadLimit
	stepsReadLimit = 300 * time.Millisecond
	t.Cleanup(func() { stepsReadLimit = prev })

	started := time.Now()
	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the command waited %s on a 300ms limit", took)
	}
	if !strings.Contains(out, "could not be read from GitHub") || !strings.Contains(out, "deadline exceeded") {
		t.Errorf("a silent GitHub was not reported as one:\n%s", out)
	}
}

// timedSteps is a jobSteps on a steady series: 0.1s of CPU, 1 KiB of each
// byte count and 1 J a second from 12:00:00, sampled every second to 12:00:10,
// with a memory level of 100 MiB plus the second.
func timedSteps(steps ...github.JobStep) jobSteps {
	first := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tl := usage.Timeline{First: first}
	for s := range int64(11) {
		tl.Points = append(tl.Points, usage.Point{OffsetMillis: s * 1000, CPUUsage: s * 100_000,
			DiskRead: s * 1024, DiskWrite: s * 1024, NetRx: s * 1024, NetTx: s * 1024,
			EnergyActive: s * 1_000_000, MemoryCurrent: 100<<20 + s})
	}
	measured := &alloc.RecordedUsage{JobUsage: alloc.JobUsage{EnergySource: alloc.EnergyRAPL}}

	return jobSteps{runner: "billet-l1", timeline: &tl, measured: measured,
		job: github.WorkflowJob{ID: 1, Name: "j", RunAttempt: 1, Steps: steps}}
}

// at is a step time this many seconds after the timed series' first sample.
func at(seconds float64) time.Time {
	return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).Add(time.Duration(seconds * float64(time.Second)))
}

// EACH STEP SAYS WHAT ITS WINDOW USED, AND COULD-NOT-TELL IS NEVER A NUMBER: a
// covered step gets its usage, a partial one says how much is covered, a step
// inside one interval says it was split, and a step without both times, with
// no length or with its times backwards gets none.
func TestRenderStepsSaysWhatEachWindowUsed(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	renderSteps(&out, timedSteps(
		github.JobStep{Number: 1, Name: "covered", Conclusion: "success", StartedAt: at(2), CompletedAt: at(5)},
		github.JobStep{Number: 2, Name: "partial", StartedAt: at(8), CompletedAt: at(14)},
		github.JobStep{Number: 3, Name: "brief", StartedAt: at(3.2), CompletedAt: at(3.8)},
		github.JobStep{Number: 4, Name: "pending", Status: "queued"},
		github.JobStep{Number: 5, Name: "started", StartedAt: at(9)},
		github.JobStep{Number: 6, Name: "instant", StartedAt: at(4), CompletedAt: at(4)},
		github.JobStep{Number: 7, Name: "backwards", StartedAt: at(6), CompletedAt: at(4)},
		github.JobStep{Number: 8, Name: "outside", StartedAt: at(20), CompletedAt: at(30)},
	))
	report := out.String()

	for _, want := range []string{
		"aligned to the second on two clocks",
		`step 1     "covered" "success", started 2026-10-09T12:00:02Z, took 3s` + "\n" +
			"           cpu 0.3s, peak memory 100.0 MiB, disk read 3.0 KiB, written 3.0 KiB, " +
			"network received 3.0 KiB, sent 3.0 KiB, energy 3.0 J active\n",
		"partial: the series covers 2s of the step's 6s, and over that part: cpu 0.2s",
		"cpu 0.1s, peak memory not sampled inside the step",
		"(no sample inside the step: its share of the interval around it, split by time)",
		`step 4     "pending" "queued", no start or end time from GitHub, so no usage`,
		`step 5     "started", started 2026-10-09T12:00:09Z, no end time from GitHub, so no duration or usage`,
		"took 0s (start and end in the same second), so no usage can be placed in it",
		"GitHub's end is 2s before its start, so no usage",
		"the series covers none of this step, so no usage",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the steps do not say %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "cpu 0.6s") {
		t.Errorf("the partial step was given its whole window's usage:\n%s", report)
	}
}

// A GROUP THE HOST NEVER READ IS "not measured" IN EVERY STEP, never a zero.
func TestRenderStepsSaysNotMeasuredForAGroupNeverRead(t *testing.T) {
	t.Parallel()

	steps := timedSteps(github.JobStep{Number: 1, Name: "s", StartedAt: at(1), CompletedAt: at(3)})
	steps.measured = &alloc.RecordedUsage{JobUsage: alloc.JobUsage{Unmeasured: []string{
		alloc.UsageCPU, alloc.UsageMemory, alloc.UsageIO, alloc.UsageNet, alloc.UsageEnergy}}}
	var out bytes.Buffer
	renderSteps(&out, steps)

	want := "cpu not measured, peak memory not measured, disk not measured, network not measured, " +
		"energy not measured"
	if !strings.Contains(out.String(), want) {
		t.Errorf("unmeasured groups were not said to be:\n%s", out.String())
	}
}

// A STEP NAME OR A GITHUB ERROR CANNOT FORGE A LINE.
func TestRenderStepsQuotesWhatGitHubChose(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	renderSteps(&out, timedSteps(github.JobStep{Number: 1, Name: "a\nstep 9     forged",
		Conclusion: "ok\nstep 8     forged", StartedAt: at(1), CompletedAt: at(2)}))
	renderSteps(&out, jobSteps{err: errors.New("github: HTTP 500: x\nstep 7     forged")})
	for _, forged := range []string{"\nstep 9 ", "\nstep 8 ", "\nstep 7 "} {
		if strings.Contains(out.String(), forged) {
			t.Errorf("%q forged a line:\n%s", forged, out.String())
		}
	}
}
