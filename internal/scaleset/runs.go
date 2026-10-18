package scaleset

import (
	"context"
	"fmt"
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

// RunQueueTimes reads when GitHub queued each job of a run, by runner name,
// through the target's App client.
func (c *Client) RunQueueTimes(
	ctx context.Context, owner, repository string, runID int64,
) (map[string]time.Time, error) {
	reader, ok := c.policy.(billetgithub.QueueTimes)
	if c.policy == nil || !ok {
		return nil, billetgithub.ErrRunEvidenceUnavailable
	}
	times, err := reader.RunQueueTimes(ctx, owner, repository, runID)
	if err != nil {
		return nil, fmt.Errorf("scaleset: %w", err)
	}

	return times, nil
}

func (c *Client) runEvidence() (billetgithub.RunEvidence, error) {
	evidence, ok := c.policy.(billetgithub.RunEvidence)
	if c.policy == nil || !ok {
		return nil, billetgithub.ErrRunEvidenceUnavailable
	}

	return evidence, nil
}
