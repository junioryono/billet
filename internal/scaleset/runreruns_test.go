package scaleset

import (
	"context"
	"errors"
	"testing"

	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/server"
)

// rerunsPolicy is a policy client whose re-run calls answer from fields.
type rerunsPolicy struct {
	billetgithub.RunnerGroupPolicyClient

	attempt billetgithub.RunAttempt
	err     error
}

func (r rerunsPolicy) LatestAttempt(context.Context, string, string, int64) (billetgithub.RunAttempt, error) {
	return r.attempt, r.err
}

func (r rerunsPolicy) RerunFailedJobs(context.Context, string, string, int64) error { return r.err }

func (r rerunsPolicy) RerunJob(context.Context, string, string, int64) error { return r.err }

// GITHUB'S ANSWER REACHES THE LOOP AS THE SERVER'S OWN SENTINEL, which is what
// decides whether a whole App installation pauses, one run waits, or the request
// counts as a failed try. Every other failure keeps GitHub's answer in its chain.
func TestARerunsAnswerIsTheServersSentinel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"primary rate limit", &billetgithub.APIError{Status: 403, RateLimited: true}, server.ErrRerunThrottled},
		{"secondary rate limit", &billetgithub.APIError{Status: 429, RateLimited: true}, server.ErrRerunThrottled},
		{"no actions: write", &billetgithub.APIError{Status: 403}, server.ErrRerunRefused},
		{"credential", &billetgithub.APIError{Status: 401}, server.ErrRerunRefused},
		{"already running", billetgithub.ErrRunAlreadyRunning, server.ErrRerunRunning},
		{"run gone", &billetgithub.APIError{Status: 404}, nil},
		{"transport", errors.New("connection reset"), nil},
	} {
		c := &Client{policy: rerunsPolicy{err: tc.err}}

		for call, err := range map[string]error{
			"failed jobs": c.RerunFailedJobs(t.Context(), "acme", "api", 31),
			"one job":     c.RerunJob(t.Context(), "acme", "api", 77),
			"the attempt": func() error {
				_, err := c.LatestAttempt(t.Context(), "acme", "api", 31)
				return err
			}(),
		} {
			sentinel := errors.Is(err, server.ErrRerunThrottled) || errors.Is(err, server.ErrRerunRefused) ||
				errors.Is(err, server.ErrRerunRunning)
			switch {
			case err == nil:
				t.Errorf("%s, %s: no error", tc.name, call)
			case tc.want == nil && sentinel:
				t.Errorf("%s, %s: %v reads as more than a failed try", tc.name, call, err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("%s, %s: %v, want %v", tc.name, call, err, tc.want)
			case !errors.Is(err, tc.err):
				t.Errorf("%s, %s: %v no longer carries GitHub's answer", tc.name, call, err)
			}
		}
	}
}

// THE RECORD REACHES THE LOOP IN ITS TERMS, the judgements made by the package that
// knows what GitHub's words mean.
func TestAnAttemptReachesTheServerJudged(t *testing.T) {
	t.Parallel()

	lost := billetgithub.AttemptJob{
		WorkflowJob: billetgithub.WorkflowJob{ID: 77, Steps: []billetgithub.JobStep{{Status: "in_progress"}}},
		RunnerName:  "billet-a", Status: billetgithub.RunStatusCompleted, Conclusion: "failure",
	}
	own := billetgithub.AttemptJob{
		WorkflowJob: billetgithub.WorkflowJob{ID: 78, Steps: []billetgithub.JobStep{
			{Status: billetgithub.RunStatusCompleted, Conclusion: "failure"},
		}},
		RunnerName: "billet-b", Status: billetgithub.RunStatusCompleted, Conclusion: "failure",
	}
	fine := billetgithub.AttemptJob{
		WorkflowJob: billetgithub.WorkflowJob{ID: 79}, RunnerName: "billet-c",
		Status: billetgithub.RunStatusCompleted, Conclusion: "success",
	}

	c := &Client{policy: rerunsPolicy{attempt: billetgithub.RunAttempt{
		ID: 31, Status: billetgithub.RunStatusCompleted, Attempt: 3, Jobs: []billetgithub.AttemptJob{lost, own, fine},
	}}}

	got, err := c.LatestAttempt(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("LatestAttempt: %v", err)
	}
	if !got.Completed || got.Attempt != 3 || len(got.Jobs) != 3 {
		t.Fatalf("attempt = %+v", got)
	}
	want := []server.AttemptJob{
		{ID: 77, RunnerName: "billet-a", RunnerLost: true, Failed: true},
		{ID: 78, RunnerName: "billet-b", RunnerLost: false, Failed: true},
		{ID: 79, RunnerName: "billet-c", RunnerLost: false, Failed: false},
	}
	for i := range want {
		if got.Jobs[i] != want[i] {
			t.Errorf("job %d = %+v, want %+v", i, got.Jobs[i], want[i])
		}
	}

	queued := &Client{policy: rerunsPolicy{attempt: billetgithub.RunAttempt{ID: 31, Status: "queued", Attempt: 3}}}
	if got, err := queued.LatestAttempt(t.Context(), "acme", "api", 31); err != nil || got.Completed {
		t.Errorf("a queued run = %+v, %v, want not completed", got, err)
	}

	if _, err := (&Client{}).LatestAttempt(t.Context(), "acme", "api", 31); !errors.Is(err,
		billetgithub.ErrRunEvidenceUnavailable) {
		t.Errorf("a client with no App = %v, want ErrRunEvidenceUnavailable", err)
	}
}

// stepsPolicy is a policy client whose run's job list answers from fields, and says
// which run it was asked about.
type stepsPolicy struct {
	billetgithub.RunnerGroupPolicyClient

	jobs  map[string]billetgithub.WorkflowJob
	err   error
	asked *int64
}

func (s stepsPolicy) RunnerJobs(_ context.Context, _, _ string, runID int64,
) (map[string]billetgithub.WorkflowJob, error) {
	*s.asked = runID

	return s.jobs, s.err
}

// AN ALERT RULE NAMING A STEP READS EVERY JOB OF THE RUN ONCE, each step's name and
// conclusion and whether GitHub has concluded the job, and GitHub's refusal keeps its
// chain.
func TestARunsJobStepsAreReadOnceByRunner(t *testing.T) {
	t.Parallel()

	var asked int64
	c := &Client{policy: stepsPolicy{asked: &asked, jobs: map[string]billetgithub.WorkflowJob{
		"billet-abc": {Status: "completed", Steps: []billetgithub.JobStep{
			{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success"},
			{Number: 2, Name: "Run migrations", Status: "completed", Conclusion: "failure"},
		}},
		"billet-def": {Status: "in_progress", Steps: []billetgithub.JobStep{{Number: 1, Name: "Compile"}}},
	}}}

	jobs, err := c.RunJobSteps(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatal(err)
	}
	if asked != 31 {
		t.Errorf("asked about run %d", asked)
	}
	abc := jobs["billet-abc"]
	if !abc.Completed || len(abc.Steps) != 2 || abc.Steps[1].Name != "Run migrations" ||
		abc.Steps[1].Conclusion != "failure" {
		t.Errorf("billet-abc = %+v", abc)
	}
	if def := jobs["billet-def"]; def.Completed || len(def.Steps) != 1 {
		t.Errorf("an unfinished job = %+v, want it said so", def)
	}

	refused := &billetgithub.APIError{Status: 403}
	c = &Client{policy: stepsPolicy{asked: &asked, err: refused}}
	if _, err := c.RunJobSteps(t.Context(), "acme", "api", 31); !errors.Is(err, refused) {
		t.Errorf("a refused read = %v, want GitHub's answer in its chain", err)
	}
}
