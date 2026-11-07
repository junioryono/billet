package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// What GitHub.com answered on 2026-11-04 (docs/reference/records/job-reruns.md).
const (
	lostSteps = `{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},
		{"number":2,"name":"Hold the runner","status":"in_progress","conclusion":null}`
	ownFailureSteps = `{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},
		{"number":2,"name":"Hold the runner","status":"completed","conclusion":"failure"},
		{"number":3,"name":"Complete job","status":"completed","conclusion":"success"}`
	cancelledSteps = `{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},
		{"number":2,"name":"Hold the runner","status":"completed","conclusion":"cancelled"},
		{"number":3,"name":"Complete job","status":"completed","conclusion":"success"}`
)

// rerunServer is GitHub's run, job list and two re-run routes, counting each.
type rerunServer struct {
	*runnerGroupPolicyClient

	run, jobs   string
	failed, one atomic.Int32
	jobReads    atomic.Int32
	answer      func(w http.ResponseWriter)
	seenFilter  atomic.Value
}

func newRerunServer(t *testing.T) *rerunServer {
	t.Helper()

	key, _ := testKeyPKCS1(t)
	s := &rerunServer{answer: func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated) }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, s.run)
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31/jobs", func(w http.ResponseWriter, r *http.Request) {
		s.jobReads.Add(1)
		s.seenFilter.Store(r.URL.Query().Get("filter"))
		fmt.Fprint(w, s.jobs)
	})
	mux.HandleFunc("POST /repos/acme/api/actions/runs/31/rerun-failed-jobs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer installation-secret" {
			t.Errorf("Authorization = %q", got)
		}
		s.failed.Add(1)
		s.answer(w)
	})
	mux.HandleFunc("POST /repos/acme/api/actions/jobs/77/rerun", func(w http.ResponseWriter, _ *http.Request) {
		s.one.Add(1)
		s.answer(w)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.runnerGroupPolicyClient = newRunnerGroupPolicyClient(defaultPolicyBounds, srv.URL,
		OrganizationTarget("acme"), 11, 22, key)

	return s
}

// attemptJobJSON is one job of a run's latest attempt.
func attemptJobJSON(id int64, conclusion, steps string) string {
	return fmt.Sprintf(`{"id":%d,"name":"hold","run_attempt":1,"runner_name":"billet-lease-%d","status":"completed",
		"conclusion":%q,"steps":[%s]}`, id, id, conclusion, steps)
}

// THE RECORD IS READ AS GITHUB KEEPS IT, and the signature of a lost runner is told
// from everything else GitHub was measured to record: the job's own failure, a
// runner stopped gracefully (which looks the same, so it is not the signature), and
// a person's cancellation.
func TestALostRunnerIsToldFromEverythingElseGitHubRecords(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, conclusion, steps string
		lost, failed            bool
	}{
		{"killed mid-step", "failure", lostSteps, true, true},
		{"failed on its own, or stopped gracefully", "failure", ownFailureSteps, false, true},
		{"cancelled by a person", "cancelled", cancelledSteps, false, true},
		{"succeeded", "success", `{"number":1,"name":"x","status":"completed","conclusion":"success"}`, false, false},
		{"skipped", "skipped", "", false, false},
		{"neutral", "neutral", "", false, false},
		// NO STEP AT ALL PROVES NOTHING: what ended the job is unknown.
		{"failed with no steps", "failure", "", false, true},
		// A FAILED STEP WINS over one left unfinished.
		{"a failed step beside an unfinished one", "failure",
			`{"number":1,"name":"x","status":"completed","conclusion":"failure"},
			 {"number":2,"name":"y","status":"in_progress","conclusion":null}`, false, true},
		// A SPELLING GITHUB ADDS TOMORROW is failed, and never lost.
		{"an unknown conclusion", "startup_failure", lostSteps, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRerunServer(t)
			s.run = `{"id":31,"status":"completed","conclusion":"failure","run_attempt":2}`
			s.jobs = fmt.Sprintf(`{"total_count":1,"jobs":[%s]}`, attemptJobJSON(77, tc.conclusion, tc.steps))

			attempt, err := s.LatestAttempt(t.Context(), "acme", "api", 31)
			if err != nil {
				t.Fatalf("LatestAttempt: %v", err)
			}
			if attempt.Attempt != 2 || attempt.Status != RunStatusCompleted || len(attempt.Jobs) != 1 {
				t.Fatalf("attempt = %+v", attempt)
			}
			job := attempt.Jobs[0]
			if job.ID != 77 || job.RunnerName != "billet-lease-77" {
				t.Errorf("job = %+v", job)
			}
			if got := job.RunnerLost(); got != tc.lost {
				t.Errorf("RunnerLost = %v, want %v", got, tc.lost)
			}
			if got := job.Failed(); got != tc.failed {
				t.Errorf("Failed = %v, want %v", got, tc.failed)
			}
			if got := s.seenFilter.Load(); got != "latest" {
				t.Errorf("filter = %v, want only the latest attempt", got)
			}
		})
	}
}

// A JOB STILL RUNNING IS NOT LOST, whatever its steps say: its conclusion is not
// written yet.
func TestAJobWithNoConclusionYetIsNotLost(t *testing.T) {
	t.Parallel()

	job := AttemptJob{Status: "in_progress", Conclusion: "failure",
		WorkflowJob: WorkflowJob{Steps: []JobStep{{Status: "in_progress"}}}}
	if job.RunnerLost() {
		t.Error("a running job read as a lost runner")
	}

	// AND A STEP WHOSE STATUS IS NOT SAID IS NOT AN UNFINISHED ONE.
	unsaid := AttemptJob{Status: RunStatusCompleted, Conclusion: "failure",
		WorkflowJob: WorkflowJob{Steps: []JobStep{{Status: ""}}}}
	if unsaid.RunnerLost() {
		t.Error("a step with no status read as one that never finished")
	}
}

// A RUN NOT FINISHED LISTS NO JOBS, AND ASKS FOR NONE. Measured: straight after a
// re-run is accepted GitHub answers `queued`, the old attempt and zero jobs, and a
// list of nothing must not read as nothing having failed.
func TestARunNotFinishedIsNotAskedForItsJobs(t *testing.T) {
	t.Parallel()

	s := newRerunServer(t)
	s.run = `{"id":31,"status":"queued","conclusion":null,"run_attempt":1}`
	s.jobs = `{"total_count":0,"jobs":[]}`

	attempt, err := s.LatestAttempt(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("LatestAttempt: %v", err)
	}
	if attempt.Status != "queued" || attempt.Attempt != 1 || attempt.Jobs != nil {
		t.Errorf("attempt = %+v", attempt)
	}
	if got := s.jobReads.Load(); got != 0 {
		t.Errorf("the job list was read %d times for a run with jobs left", got)
	}
}

// AN ANSWER THAT DOES NOT SAY IS NOT A RUN WITH NOTHING TO SAY: every field the
// decision reads is required, and a job list cut short is an error.
func TestAnIncompleteAnswerAboutAnAttemptIsRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ run, jobs, want string }{
		"no status":    {run: `{"id":31,"run_attempt":1}`, want: "incomplete"},
		"no attempt":   {run: `{"id":31,"status":"completed"}`, want: "incomplete"},
		"attempt zero": {run: `{"id":31,"status":"completed","run_attempt":0}`, want: "incomplete"},
		"another run":  {run: `{"id":32,"status":"completed","run_attempt":1}`, want: "answered with 32"},
		"no id":        {run: `{"status":"completed","run_attempt":1}`, want: "incomplete"},
		"a job with no status": {
			run:  `{"id":31,"status":"completed","run_attempt":1}`,
			jobs: `{"total_count":1,"jobs":[{"id":77,"name":"hold","run_attempt":1,"runner_name":"r","steps":[]}]}`,
			want: "does not say the job's status",
		},
		"a job with no attempt": {
			run:  `{"id":31,"status":"completed","run_attempt":1}`,
			jobs: `{"total_count":1,"jobs":[{"id":77,"name":"hold","status":"completed","conclusion":"failure","runner_name":"r","steps":[]}]}`,
			want: "does not say the job's attempt",
		},
		"a completed job that does not say how it ended": {
			run:  `{"id":31,"status":"completed","run_attempt":1}`,
			jobs: `{"total_count":1,"jobs":[{"id":77,"name":"hold","run_attempt":1,"status":"completed","runner_name":"r","steps":[]}]}`,
			want: "does not say how it ended",
		},
		"a step with no status": {
			run: `{"id":31,"status":"completed","run_attempt":1}`,
			jobs: `{"total_count":1,"jobs":[{"id":77,"name":"hold","run_attempt":1,"status":"completed","conclusion":"failure",
				"runner_name":"r","steps":[{"number":1,"name":"x","conclusion":null}]}]}`,
			want: "a step does not say its status",
		},
		"a completed step that does not say how it ended": {
			run: `{"id":31,"status":"completed","run_attempt":1}`,
			jobs: `{"total_count":1,"jobs":[{"id":77,"name":"hold","run_attempt":1,"status":"completed","conclusion":"failure",
				"runner_name":"r","steps":[{"number":1,"name":"x","status":"completed"}]}]}`,
			want: "a completed step does not say how it ended",
		},
		"a list shorter than its count": {
			run:  `{"id":31,"status":"completed","run_attempt":1}`,
			jobs: `{"total_count":2,"jobs":[]}`,
			want: "ended after 0 of the 2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newRerunServer(t)
			s.run, s.jobs = tc.run, tc.jobs

			_, err := s.LatestAttempt(t.Context(), "acme", "api", 31)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("LatestAttempt = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

// EACH REQUEST REACHES ITS OWN ROUTE, as a POST with the installation's token, and
// GitHub's 201 is the only success.
func TestEachRerunRequestReachesItsRoute(t *testing.T) {
	t.Parallel()

	s := newRerunServer(t)
	if err := s.RerunFailedJobs(t.Context(), "acme", "api", 31); err != nil {
		t.Errorf("RerunFailedJobs: %v", err)
	}
	if err := s.RerunJob(t.Context(), "acme", "api", 77); err != nil {
		t.Errorf("RerunJob: %v", err)
	}
	if s.failed.Load() != 1 || s.one.Load() != 1 {
		t.Errorf("failed-jobs requests %d, job requests %d, want one of each", s.failed.Load(), s.one.Load())
	}

	s.answer = func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }
	if err := s.RerunJob(t.Context(), "acme", "api", 77); err == nil {
		t.Error("a 200 read as accepted; only GitHub's 201 is")
	}
}

// GITHUB'S TWO WORDS FOR A RUN NOT FINISHED ARE TOLD FROM A REFUSED CREDENTIAL, by
// the words, because both are 403s. A throttle stays a throttle.
func TestARerunsAnswerIsClassified(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, body string
		status     int
		running    bool
		rateLimit  bool
	}{
		{name: "the failed-jobs route's word", status: 403, running: true,
			body: `{"message":"This workflow is already running","status":"403"}`},
		{name: "the job route's word", status: 403, running: true,
			body: `{"message":"The workflow run containing this job is already running","status":"403"}`},
		{name: "no actions: write", status: 403,
			body: `{"message":"Resource not accessible by integration"}`},
		{name: "a throttle", status: 403, rateLimit: true,
			body: `{"message":"API rate limit exceeded for installation"}`},
		// THE WORDS ONLY COUNT ON A 403.
		{name: "the words on a 500", status: 500,
			body: `{"message":"This workflow is already running"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRerunServer(t)
			s.answer = func(w http.ResponseWriter) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}

			err := s.RerunJob(t.Context(), "acme", "api", 77)
			if err == nil {
				t.Fatal("a refusal read as accepted")
			}
			if got := errors.Is(err, ErrRunAlreadyRunning); got != tc.running {
				t.Errorf("running = %v, want %v: %v", got, tc.running, err)
			}
			api, ok := errors.AsType[*APIError](err)
			if !ok || api.Status != tc.status || api.RateLimited != tc.rateLimit {
				t.Errorf("the API error = %+v, want status %d, throttled %v", api, tc.status, tc.rateLimit)
			}
		})
	}
}

// AN ANSWER THAT ECHOES THE TOKEN DOES NOT CARRY IT INTO AN ERROR, which is what a
// log line and the ledger's detail column are written from.
func TestARerunsRefusalNeverCarriesTheToken(t *testing.T) {
	t.Parallel()

	s := newRerunServer(t)
	s.answer = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"bad token installation-secret"}`)
	}

	err := s.RerunJob(t.Context(), "acme", "api", 77)
	if err == nil || strings.Contains(err.Error(), "installation-secret") {
		t.Errorf("err = %v", err)
	}
}

// A CLIENT WITH NO APP, A JOB OR RUN WITH NO ID AND A PATH SEGMENT THAT IS NOT ONE
// SEND NOTHING.
func TestARerunWithNothingToAskAboutSendsNothing(t *testing.T) {
	t.Parallel()

	s := newRerunServer(t)
	if err := s.RerunJob(t.Context(), "acme", "api", 0); err == nil || !strings.Contains(err.Error(), "positive id") {
		t.Errorf("job 0 = %v", err)
	}
	if err := s.RerunFailedJobs(t.Context(), "acme", "api", -1); err == nil ||
		!strings.Contains(err.Error(), "positive id") {
		t.Errorf("run -1 = %v", err)
	}
	if _, err := s.LatestAttempt(t.Context(), "acme", "api", 0); err == nil || !strings.Contains(err.Error(), "positive id") {
		t.Errorf("run 0 = %v", err)
	}
	if err := s.RerunFailedJobs(t.Context(), "acme", "../other", 31); err == nil {
		t.Error("a repository segment that is not one was asked about")
	}
	if s.failed.Load() != 0 || s.one.Load() != 0 {
		t.Errorf("requests reached GitHub: %d, %d", s.failed.Load(), s.one.Load())
	}

	var none *runnerGroupPolicyClient
	if err := none.RerunJob(t.Context(), "acme", "api", 77); !errors.Is(err, ErrRunEvidenceUnavailable) {
		t.Errorf("a client with no App = %v", err)
	}
	if err := none.RerunFailedJobs(t.Context(), "acme", "api", 31); !errors.Is(err, ErrRunEvidenceUnavailable) {
		t.Errorf("a client with no App = %v", err)
	}
	if _, err := none.LatestAttempt(t.Context(), "acme", "api", 31); !errors.Is(err, ErrRunEvidenceUnavailable) {
		t.Errorf("a client with no App = %v", err)
	}
}

// ACTIONS: WRITE IS ASKED FOR AND ACCEPTED ONLY WHERE IT IS NEEDED. Unlike read,
// which billet requests by default, an App holding write where no tier re-runs a
// job holds more than billet claims, and is refused by name.
func TestActionsWriteIsExactInBothDirections(t *testing.T) {
	t.Parallel()

	for _, scope := range []Scope{ScopeOrganization, ScopeRepository} {
		plain, read, write := Permissions(scope, ActionsNone), Permissions(scope, ActionsRead), Permissions(scope, ActionsWrite)
		if write["actions"] != "write" || len(write) != len(plain)+1 {
			t.Errorf("%s: the write set = %v, want %v plus actions: write", scope, write, plain)
		}

		for _, tc := range []struct {
			name    string
			granted map[string]string
			asked   ActionsAccess
			want    string
		}{
			{"write where write is needed", write, ActionsWrite, ""},
			{"read where write is needed", read, ActionsWrite, "actions: want write, granted read"},
			{"none where write is needed", plain, ActionsWrite, "actions: want write, not granted"},
			{"write where read is needed", write, ActionsRead, "actions: want read, granted write"},
			{"write where none is needed", write, ActionsNone, "actions: granted write, but billet never requested it"},
			{"read where none is needed", read, ActionsNone, ""},
		} {
			got := (&Installation{Permissions: tc.granted}).PermissionMismatches(scope, tc.asked)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("%s, %s: %v, want it accepted", scope, tc.name, got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("%s, %s: %v, want one saying %q", scope, tc.name, got, tc.want)
			}
		}
	}
}

// THE MANIFEST ASKS FOR WHAT THE LEVEL SAYS, so an App created for re-runs holds
// them from its first install.
func TestTheManifestCarriesTheActionsLevel(t *testing.T) {
	t.Parallel()

	for level, want := range map[ActionsAccess]string{ActionsNone: "", ActionsRead: "read", ActionsWrite: "write"} {
		m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed", ScopeOrganization, level)
		if got := m.Permissions["actions"]; got != want {
			t.Errorf("level %d asked for actions %q, want %q", level, got, want)
		}
	}
}

// A 201 WHOSE BODY BROKE OFF WAS STILL ACCEPTED: reading it as a failed try would ask
// again for a run already going.
func TestA201WhoseBodyBrokeOffIsAccepted(t *testing.T) {
	t.Parallel()

	s := newRerunServer(t)
	s.answer = func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "{")
	}

	if err := s.RerunFailedJobs(t.Context(), "acme", "api", 31); err != nil {
		t.Errorf("a 201 cut short = %v, want it accepted", err)
	}
}

// A MISMATCH ON ACTIONS SAYS WHAT ASKS FOR EACH LEVEL, and no other permission's does.
func TestAnActionsMismatchSaysWhatAsksForIt(t *testing.T) {
	t.Parallel()

	got := (&Installation{Permissions: Permissions(ScopeOrganization, ActionsWrite)}).
		PermissionMismatches(ScopeOrganization, ActionsNone)
	if len(got) != 1 || !strings.Contains(got[0], "retry.disrupted") {
		t.Errorf("mismatches = %v", got)
	}

	other := (&Installation{Permissions: map[string]string{"metadata": "read"}}).
		PermissionMismatches(ScopeOrganization, ActionsNone)
	for _, problem := range other {
		if strings.Contains(problem, "retry.disrupted") {
			t.Errorf("a mismatch of another permission names actions' hint: %q", problem)
		}
	}
}
