package scaleset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/server"
)

var _ server.RunEvidence = (*Client)(nil)

// WorkflowRun reads GitHub's record of one run through the target's App client.
func (c *Client) WorkflowRun(
	ctx context.Context, owner, repository string, runID int64,
) (server.WorkflowRun, error) {
	evidence, err := c.runEvidence()
	if err != nil {
		return server.WorkflowRun{}, err
	}
	run, err := evidence.WorkflowRun(ctx, owner, repository, runID)
	if err != nil {
		return server.WorkflowRun{}, fmt.Errorf("scaleset: %w", err)
	}

	out := server.WorkflowRun{
		ID: run.ID, Event: run.Event, HeadBranch: run.HeadBranch,
		HeadRepository: run.HeadRepository, Repository: run.Repository,
		Path: run.Path, HeadSHA: run.HeadSHA,
	}
	for _, pr := range run.PullRequests {
		out.PullRequests = append(out.PullRequests, server.RunPullRequest{Number: pr.Number, Base: pr.Base})
	}
	for _, called := range run.ReferencedWorkflows {
		out.ReferencedWorkflows = append(out.ReferencedWorkflows,
			server.ReferencedWorkflow{Path: called.Path, SHA: called.SHA})
	}

	return out, nil
}

// DefaultBranch reads a repository's current default branch.
func (c *Client) DefaultBranch(ctx context.Context, owner, repository string) (string, error) {
	evidence, err := c.runEvidence()
	if err != nil {
		return "", err
	}
	branch, err := evidence.DefaultBranch(ctx, owner, repository)
	if err != nil {
		return "", fmt.Errorf("scaleset: %w", err)
	}

	return branch, nil
}

var _ server.QueueTimes = (*Client)(nil)

// RunJobs reads when GitHub queued each job of a run and the commit it ran on, by
// runner name, through the target's App client. A throttle is
// server.ErrQueueTimesThrottled and a refused credential or permission
// server.ErrQueueTimesRefused, which the sweep pauses the App installation for rather
// than the one run.
func (c *Client) RunJobs(
	ctx context.Context, owner, repository string, runID int64,
) (map[string]server.RunJob, error) {
	reader, ok := c.policy.(billetgithub.QueueTimes)
	if c.policy == nil || !ok {
		return nil, billetgithub.ErrRunEvidenceUnavailable
	}
	records, err := reader.RunJobs(ctx, owner, repository, runID)
	if err == nil {
		jobs := make(map[string]server.RunJob, len(records))
		for name, record := range records {
			jobs[name] = server.RunJob{QueuedAt: record.QueuedAt, HeadSHA: record.HeadSHA,
				HeadBranch: record.HeadBranch, RunAttempt: record.RunAttempt}
		}

		return jobs, nil
	}

	return nil, installationError(err, server.ErrQueueTimesThrottled, server.ErrQueueTimesRefused)
}

// installationError says whether GitHub's answer stops the App installation rather
// than the one request: a throttle wraps throttled, and a refused credential or
// permission refused. The one reading of those two answers, for every sweep that
// pauses an installation on them.
func installationError(err, throttled, refused error) error {
	answer, ok := errors.AsType[*billetgithub.APIError](err)
	switch {
	case ok && answer.RateLimited:
		return fmt.Errorf("scaleset: %w: %w", throttled, err)
	case ok && (answer.Status == http.StatusUnauthorized || answer.Status == http.StatusForbidden):
		return fmt.Errorf("scaleset: %w: %w", refused, err)
	default:
		return fmt.Errorf("scaleset: %w", err)
	}
}

var _ server.RunReruns = (*Client)(nil)

// LatestAttempt reads the attempt a run is on through the target's App client and,
// once the run is completed, its jobs in the server's terms.
func (c *Client) LatestAttempt(ctx context.Context, owner, repository string, runID int64,
) (server.RunAttempt, error) {
	reruns, err := c.runReruns()
	if err != nil {
		return server.RunAttempt{}, err
	}
	attempt, err := reruns.LatestAttempt(ctx, owner, repository, runID)
	if err != nil {
		return server.RunAttempt{}, rerunError(err)
	}

	out := server.RunAttempt{Completed: attempt.Status == billetgithub.RunStatusCompleted, Attempt: attempt.Attempt}
	for i := range attempt.Jobs {
		job := &attempt.Jobs[i]
		out.Jobs = append(out.Jobs, server.AttemptJob{ID: job.ID, RunnerName: job.RunnerName, Attempt: job.RunAttempt,
			RunnerLost: job.RunnerLost(), Failed: job.Failed()})
	}

	return out, nil
}

// RerunFailedJobs asks GitHub to run a run's failed jobs again.
func (c *Client) RerunFailedJobs(ctx context.Context, owner, repository string, runID int64) error {
	reruns, err := c.runReruns()
	if err != nil {
		return err
	}

	return rerunError(reruns.RerunFailedJobs(ctx, owner, repository, runID))
}

// RerunJob asks GitHub to run one job again.
func (c *Client) RerunJob(ctx context.Context, owner, repository string, jobID int64) error {
	reruns, err := c.runReruns()
	if err != nil {
		return err
	}

	return rerunError(reruns.RerunJob(ctx, owner, repository, jobID))
}

func (c *Client) runReruns() (billetgithub.RunReruns, error) {
	reruns, ok := c.policy.(billetgithub.RunReruns)
	if c.policy == nil || !ok {
		return nil, billetgithub.ErrRunEvidenceUnavailable
	}

	return reruns, nil
}

// rerunError says in the server's terms what stops a run's turn differently: a
// throttle and a refused credential pause the App installation rather than the one
// run, and a run already going is not a failure.
func rerunError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, billetgithub.ErrRunAlreadyRunning) {
		return fmt.Errorf("scaleset: %w: %w", server.ErrRerunRunning, err)
	}

	return installationError(err, server.ErrRerunThrottled, server.ErrRerunRefused)
}

// RESTBudget is the installation's REST budget as GitHub last stated it to the
// target's App client, false before any answer stated one or for a client
// built without the App's credentials.
func (c *Client) RESTBudget() (billetgithub.RESTBudget, bool) {
	account, ok := c.policy.(billetgithub.RESTAccount)
	if c.policy == nil || !ok {
		return billetgithub.RESTBudget{}, false
	}

	return account.RESTBudget()
}

// ObserveREST tells o of every request the target's App client makes with the
// installation's token.
func (c *Client) ObserveREST(o billetgithub.RESTObserver) {
	if account, ok := c.policy.(billetgithub.RESTAccount); c.policy != nil && ok {
		account.ObserveREST(o)
	}
}

var _ server.RESTHeadroom = (*Client)(nil)

// RESTHeadroom is the budget in the server's terms.
func (c *Client) RESTHeadroom() (int64, int64, time.Time, bool) {
	b, ok := c.RESTBudget()

	return b.Remaining, b.Limit, b.Reset, ok
}

func (c *Client) runEvidence() (billetgithub.RunEvidence, error) {
	evidence, ok := c.policy.(billetgithub.RunEvidence)
	if c.policy == nil || !ok {
		return nil, billetgithub.ErrRunEvidenceUnavailable
	}

	return evidence, nil
}
