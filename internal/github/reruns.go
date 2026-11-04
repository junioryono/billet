package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RunReruns reads the attempt a workflow run is on and asks GitHub to run it
// again, which is what `retry.disrupted` is made of.
//
// docs/reference/records/job-reruns.md holds what each of these was measured to
// answer on GitHub.com on 2026-11-04, and ADR-023 what billet decides from it.
//
// The reads need the App's `actions: read` and the requests its `actions: write`.
type RunReruns interface {
	LatestAttempt(ctx context.Context, owner, repository string, runID int64) (RunAttempt, error)
	RerunFailedJobs(ctx context.Context, owner, repository string, runID int64) error
	RerunJob(ctx context.Context, owner, repository string, jobID int64) error
}

// RunStatusCompleted is the status of a run with no job left to finish. A request
// to re-run any other is refused, so nothing is decided about one.
const RunStatusCompleted = "completed"

// RunAttempt is a run as GitHub records it now.
type RunAttempt struct {
	ID      int64
	Status  string
	Attempt int64
	// Jobs are the latest attempt's jobs. They are read only for a completed run:
	// straight after a re-run is accepted GitHub answers `queued`, the attempt it
	// was on and no jobs at all, and a list of nothing must not read as nothing
	// having failed.
	Jobs []AttemptJob
}

// AttemptJob is one job of a run's latest attempt.
type AttemptJob struct {
	WorkflowJob
	RunnerName string
	Status     string
	Conclusion string
}

// RunnerLost reports whether GitHub's record of the job is the positive signature
// of a runner that vanished: the job concluded `failure` with a step that never
// finished, and no step concluded `failure`.
//
// MEASURED, 2026-11-04. A runner killed mid-step left that step `in_progress`
// with no conclusion and no "Complete job" step, and GitHub concluded the job
// about nine minutes later. A job failing on its own leaves its failing step
// concluded `failure` and a finished "Complete job". A person's cancellation
// concludes `cancelled`.
//
// A RUNNER STOPPED GRACEFULLY IS NOT THIS. On SIGTERM it reports the running step
// `failure`, and every surface billet can read is then the same as a job's own
// failure, so that answer is false: what cannot be told from a job's own failure
// is never read as billet's.
func (j AttemptJob) RunnerLost() bool {
	if j.Status != RunStatusCompleted || j.Conclusion != jobConclusionFailure {
		return false
	}

	unfinished := false

	for _, step := range j.Steps {
		if step.Conclusion == jobConclusionFailure {
			return false
		}

		if step.Status != RunStatusCompleted {
			unfinished = true
		}
	}

	return unfinished
}

// Failed reports whether the job is one `rerun-failed-jobs` could run again.
//
// EVERYTHING BUT THE CONCLUSIONS THAT ARE NOT A FAILURE, rather than a list of
// the ones that are: a job counted failed by mistake makes a run's set inexact,
// which leaves it for a person, and one missed would be re-run with the rest.
func (j AttemptJob) Failed() bool {
	switch j.Conclusion {
	case "success", "skipped", "neutral":
		return false
	default:
		return true
	}
}

// jobConclusionFailure is GitHub's word for a failed job or step.
const jobConclusionFailure = "failure"

// rerunsRunning are GitHub's two refusals for a run that is not finished, each
// endpoint's own: 403s, told from a refused credential by their words alone.
// Measured 2026-11-04.
var rerunsRunning = []string{
	"This workflow is already running",
	"The workflow run containing this job is already running",
}

// ErrRunAlreadyRunning says GitHub refused a re-run because the run is going.
var ErrRunAlreadyRunning = errors.New("github: the run is already running")

// LatestAttempt reads a run's status and attempt and, once it is completed, every
// job of that attempt. The whole list or an error, as listRunJobs is: a re-run is
// decided from the set of jobs that failed, and a prefix of it is a different set.
func (c *runnerGroupPolicyClient) LatestAttempt(
	ctx context.Context, owner, repository string, runID int64,
) (RunAttempt, error) {
	if !c.configured() {
		return RunAttempt{}, ErrRunEvidenceUnavailable
	}
	if runID <= 0 {
		return RunAttempt{}, fmt.Errorf("github: a workflow run needs a positive id, not %d", runID)
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return RunAttempt{}, err
	}

	var body struct {
		ID         *int64  `json:"id"`
		Status     *string `json:"status"`
		RunAttempt *int64  `json:"run_attempt"`
	}
	if err := c.getJSON(ctx, endpoint+"/actions/runs/"+strconv.FormatInt(runID, 10),
		"read workflow run", &body); err != nil {
		return RunAttempt{}, err
	}
	if body.ID == nil || body.Status == nil || body.RunAttempt == nil || *body.RunAttempt <= 0 {
		return RunAttempt{}, errors.New("github: the workflow run response was incomplete")
	}
	if *body.ID != runID {
		return RunAttempt{}, fmt.Errorf("github: asked for workflow run %d and was answered with %d",
			runID, *body.ID)
	}

	attempt := RunAttempt{ID: runID, Status: *body.Status, Attempt: *body.RunAttempt}
	if attempt.Status != RunStatusCompleted {
		return attempt, nil
	}

	err = c.listRunJobs(ctx, owner, repository, runID, "latest", MaxRunJobs, func(record jobRecord,
		name string,
	) error {
		job, err := workflowJobOf(record)
		if err != nil {
			return err
		}
		if record.Status == nil {
			return fmt.Errorf("github: job %d does not say its status", job.ID)
		}
		listed := AttemptJob{WorkflowJob: job, RunnerName: name, Status: *record.Status}
		if record.Conclusion != nil {
			listed.Conclusion = *record.Conclusion
		}
		attempt.Jobs = append(attempt.Jobs, listed)

		return nil
	})
	if err != nil {
		return RunAttempt{}, err
	}

	return attempt, nil
}

// RerunFailedJobs asks GitHub to run every failed job of a completed run again,
// with the jobs that need them.
func (c *runnerGroupPolicyClient) RerunFailedJobs(
	ctx context.Context, owner, repository string, runID int64,
) error {
	if !c.configured() {
		return ErrRunEvidenceUnavailable
	}
	if runID <= 0 {
		return fmt.Errorf("github: a workflow run needs a positive id, not %d", runID)
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return err
	}

	return c.rerun(ctx, endpoint+"/actions/runs/"+strconv.FormatInt(runID, 10)+"/rerun-failed-jobs",
		"re-run a run's failed jobs")
}

// RerunJob asks GitHub to run one job of a completed run again.
func (c *runnerGroupPolicyClient) RerunJob(
	ctx context.Context, owner, repository string, jobID int64,
) error {
	if !c.configured() {
		return ErrRunEvidenceUnavailable
	}
	if jobID <= 0 {
		return fmt.Errorf("github: a job needs a positive id, not %d", jobID)
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return err
	}

	return c.rerun(ctx, endpoint+"/actions/jobs/"+strconv.FormatInt(jobID, 10)+"/rerun",
		"re-run a job")
}

// rerun makes one request and says how GitHub answered. An answer other than
// GitHub's 201 is GitHub's own error, or ErrRunAlreadyRunning beside it.
//
// A 201'S BODY IS NOT LOOKED AT. get refuses an answer that echoes the credential
// because a reader may render what it says, and only the status of this one is
// used; a refusal's message goes through apiErrorWithout, which removes it.
func (c *runnerGroupPolicyClient) rerun(ctx context.Context, endpoint, operation string) error {
	token, err := c.installationToken(ctx)
	if err != nil {
		return err
	}

	status, header, body, err := c.exchange(withRESTOperation(ctx, operation), http.MethodPost, endpoint, token)
	if !errors.Is(err, errNotSent) {
		c.rest.answered(operation, status, header, time.Now())
	}
	if err != nil {
		return fmt.Errorf("github: %s: %w", operation, err)
	}
	if status == http.StatusCreated {
		return nil
	}

	refused := apiErrorWithout(status, body, token)
	if api, ok := errors.AsType[*APIError](refused); ok && status == http.StatusForbidden && runningRefusal(api.Message) {
		return fmt.Errorf("github: %s: %w: %w", operation, ErrRunAlreadyRunning, refused)
	}

	return fmt.Errorf("github: %s: %w", operation, refused)
}

// runningRefusal says whether GitHub's message is one of its two for a run that
// is not finished.
func runningRefusal(message string) bool {
	for _, words := range rerunsRunning {
		if strings.Contains(message, words) {
			return true
		}
	}

	return false
}
