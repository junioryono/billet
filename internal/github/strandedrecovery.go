package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// StrandedRecoverer reads a run's whole latest attempt and cancels a run, which with
// RerunFailedJobs is what recovering a job GitHub stopped offering is made of (#911,
// retry.stranded). The read needs `actions: read` and the cancel `actions: write`.
//
// MEASURED 2026-11-22, on a run whose one job was queued with no runner: a cancel
// answered 202, the run read `completed` with its job `cancelled` 3.5 s later, a
// second cancel answered 409, and a request to re-run the run's failed jobs then
// answered 201 and ran the cancelled job again as the next attempt. Measured with
// the organization owner's token: the shared test App holds `actions: read` only.
type StrandedRecoverer interface {
	QueuedAttempt(ctx context.Context, owner, repository string, runID int64) (QueuedAttempt, error)
	CancelRun(ctx context.Context, owner, repository string, runID int64) error
}

// QueuedAttempt is a run as GitHub records it now: whether it is over, the attempt
// it is on and, unless it is over, every job of that attempt.
type QueuedAttempt struct {
	Completed bool
	Attempt   int64
	Jobs      []QueuedJob
}

// QueuedJob is one job of a run's latest attempt: its name, GitHub's status for it,
// the attempt it belongs to, and the runner it ran on, empty for one no runner took.
type QueuedJob struct {
	Name       string
	Status     string
	Attempt    int64
	RunnerName string
}

// ErrRunAlreadyCompleted says GitHub refused a cancel because the run is over.
var ErrRunAlreadyCompleted = errors.New("github: the run is already completed")

var _ StrandedRecoverer = (*runnerGroupPolicyClient)(nil)

// QueuedAttempt reads a run's status and attempt and, unless the run is completed,
// every job of its latest attempt, within the bound JobsNamed reads to. The whole
// list or an error: a job missing from a prefix, or one whose record does not say
// its name, status or attempt, could be the one that decides.
func (c *runnerGroupPolicyClient) QueuedAttempt(ctx context.Context, owner, repository string, runID int64,
) (QueuedAttempt, error) {
	if !c.configured() {
		return QueuedAttempt{}, ErrRunEvidenceUnavailable
	}
	if runID <= 0 {
		return QueuedAttempt{}, fmt.Errorf("github: a workflow run needs a positive id, not %d", runID)
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return QueuedAttempt{}, err
	}

	var body struct {
		ID         *int64  `json:"id"`
		Status     *string `json:"status"`
		RunAttempt *int64  `json:"run_attempt"`
	}
	if err := c.getJSON(ctx, endpoint+"/actions/runs/"+strconv.FormatInt(runID, 10),
		"read workflow run", &body); err != nil {
		return QueuedAttempt{}, err
	}
	if body.ID == nil || body.Status == nil || body.RunAttempt == nil || *body.RunAttempt <= 0 {
		return QueuedAttempt{}, errors.New("github: the workflow run response was incomplete")
	}
	if *body.ID != runID {
		return QueuedAttempt{}, fmt.Errorf("github: asked for workflow run %d and was answered with %d",
			runID, *body.ID)
	}

	out := QueuedAttempt{Completed: *body.Status == RunStatusCompleted, Attempt: *body.RunAttempt}
	if out.Completed {
		return out, nil
	}

	err = c.listRunJobs(ctx, owner, repository, runID, "latest", strandedMaxJobs, func(record jobRecord,
		runner string,
	) error {
		if record.Name == nil || record.Status == nil || record.RunAttempt == nil {
			return errors.New("github: a job of the run does not say its name, status or attempt")
		}
		out.Jobs = append(out.Jobs, QueuedJob{Name: *record.Name, Status: *record.Status,
			Attempt: *record.RunAttempt, RunnerName: runner})

		return nil
	})
	if err != nil {
		return QueuedAttempt{}, err
	}

	return out, nil
}

// CancelRun asks GitHub to cancel a run. GitHub answers 202 and completes the run
// shortly after; a run already completed is ErrRunAlreadyCompleted.
func (c *runnerGroupPolicyClient) CancelRun(ctx context.Context, owner, repository string, runID int64) error {
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

	return c.post(ctx, endpoint+"/actions/runs/"+strconv.FormatInt(runID, 10)+"/cancel", "cancel a run",
		http.StatusAccepted)
}
