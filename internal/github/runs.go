package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// RunEvidence reads what GitHub's REST API records about a workflow run and a
// repository, which is what a cache authority proves a job's branch from.
//
// THE RUN IS THE EVIDENCE, NOT THE SCALE-SET MESSAGE'S WORKFLOW REF. A job
// inside a reusable workflow can carry the called workflow's ref, so a push to
// a feature branch calling `owner/repo/.github/workflows/x.yml@main` would read
// as a default-branch push while running the feature branch's code. The run's
// head branch and event are the run's own.
//
// Needs the App's `actions: read` permission, which only a deployment that
// publishes caches from a default branch asks for.
type RunEvidence interface {
	WorkflowRun(ctx context.Context, owner, repository string, runID int64) (WorkflowRun, error)
	DefaultBranch(ctx context.Context, owner, repository string) (string, error)
}

// WorkflowRun is the part of GitHub's workflow-run record a cache authority
// reads. Every field is required; a response missing one is refused rather
// than read as empty.
type WorkflowRun struct {
	ID    int64
	Event string
	// HeadBranch is the branch the run is for, without refs/heads/.
	HeadBranch string
	// HeadRepository is the full name of the repository the head commit lives
	// in, which differs from the run's repository for a fork's pull request.
	HeadRepository string
	// Repository is the full name of the repository the run belongs to.
	Repository string
	// PullRequests are the same-repository pull requests GitHub associated
	// with the run, each with its base branch. GitHub leaves this empty for a
	// fork's pull request.
	PullRequests []RunPullRequest
	// Path is the run's top-level workflow file, `.github/workflows/x.yml`.
	Path string
	// HeadSHA is the commit the run is for.
	HeadSHA string
	// ReferencedWorkflows are the reusable workflows the run called, each as
	// `owner/repo/.github/workflows/x.yml@ref` with the commit it resolved to.
	ReferencedWorkflows []ReferencedWorkflow
}

// ReferencedWorkflow is one reusable workflow a run called.
type ReferencedWorkflow struct {
	Path string
	SHA  string
}

// RunPullRequest is one pull request associated with a workflow run.
type RunPullRequest struct {
	Number int64
	Base   string
}

// ErrRunEvidenceUnavailable says the client was built without the credentials
// to read run evidence at all.
var ErrRunEvidenceUnavailable = errors.New("github: run evidence is not configured")

// WorkflowRun reads one run of a repository the App is installed on.
func (c *runnerGroupPolicyClient) WorkflowRun(
	ctx context.Context, owner, repository string, runID int64,
) (WorkflowRun, error) {
	if !c.configured() {
		return WorkflowRun{}, ErrRunEvidenceUnavailable
	}
	if runID <= 0 {
		return WorkflowRun{}, fmt.Errorf("github: a workflow run needs a positive id, not %d", runID)
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return WorkflowRun{}, err
	}

	var body struct {
		ID             *int64  `json:"id"`
		Event          *string `json:"event"`
		HeadBranch     *string `json:"head_branch"`
		HeadRepository *struct {
			FullName *string `json:"full_name"`
		} `json:"head_repository"`
		Repository *struct {
			FullName *string `json:"full_name"`
		} `json:"repository"`
		PullRequests *[]struct {
			Number *int64 `json:"number"`
			Base   *struct {
				Ref *string `json:"ref"`
			} `json:"base"`
		} `json:"pull_requests"`
		Path                *string `json:"path"`
		HeadSHA             *string `json:"head_sha"`
		ReferencedWorkflows []struct {
			Path *string `json:"path"`
			SHA  *string `json:"sha"`
		} `json:"referenced_workflows"`
	}
	if err := c.getJSON(ctx, endpoint+"/actions/runs/"+strconv.FormatInt(runID, 10),
		"read workflow run", &body); err != nil {
		return WorkflowRun{}, err
	}

	if body.ID == nil || body.Event == nil || body.HeadBranch == nil ||
		body.HeadRepository == nil || body.HeadRepository.FullName == nil ||
		body.Repository == nil || body.Repository.FullName == nil || body.PullRequests == nil ||
		body.Path == nil || body.HeadSHA == nil {
		return WorkflowRun{}, errors.New("github: the workflow run response was incomplete")
	}
	if *body.ID != runID {
		return WorkflowRun{}, fmt.Errorf("github: asked for workflow run %d and was answered with %d",
			runID, *body.ID)
	}

	run := WorkflowRun{
		ID: *body.ID, Event: *body.Event, HeadBranch: *body.HeadBranch,
		HeadRepository: *body.HeadRepository.FullName, Repository: *body.Repository.FullName,
		Path: *body.Path, HeadSHA: *body.HeadSHA,
	}
	for _, called := range body.ReferencedWorkflows {
		if called.Path == nil || called.SHA == nil {
			return WorkflowRun{}, errors.New("github: the workflow run named an incomplete reusable workflow")
		}
		run.ReferencedWorkflows = append(run.ReferencedWorkflows,
			ReferencedWorkflow{Path: *called.Path, SHA: *called.SHA})
	}
	for _, pr := range *body.PullRequests {
		if pr.Number == nil || *pr.Number <= 0 || pr.Base == nil || pr.Base.Ref == nil {
			return WorkflowRun{}, errors.New("github: the workflow run named an incomplete pull request")
		}
		run.PullRequests = append(run.PullRequests, RunPullRequest{Number: *pr.Number, Base: *pr.Base.Ref})
	}

	return run, nil
}

// DefaultBranch reads a repository's current default branch.
//
// UNCACHED ON PURPOSE: a default branch can be renamed, and a publication
// decided against the old name would write into the baseline the new default
// reads.
func (c *runnerGroupPolicyClient) DefaultBranch(
	ctx context.Context, owner, repository string,
) (string, error) {
	if !c.configured() {
		return "", ErrRunEvidenceUnavailable
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return "", err
	}

	var body struct {
		DefaultBranch *string `json:"default_branch"`
	}
	if err := c.getJSON(ctx, endpoint, "read repository", &body); err != nil {
		return "", err
	}
	if body.DefaultBranch == nil || strings.TrimSpace(*body.DefaultBranch) == "" {
		return "", errors.New("github: the repository response named no default branch")
	}

	return *body.DefaultBranch, nil
}

// getJSON reads one bounded JSON document with the installation token.
func (c *runnerGroupPolicyClient) getJSON(ctx context.Context, endpoint, operation string,
	into any,
) error {
	token, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("github: build %s request: %w", operation, err)
	}
	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doWithTimeout(c.client, req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", operation, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("github: %s: %w", operation, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: %s: %w", operation, apiError(resp.StatusCode, body))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("github: decode %s: %w", operation, err)
	}

	return nil
}

// repositoryEndpoint is `/repos/{owner}/{repository}` with both segments
// checked and escaped, because they come from a scale-set message.
func repositoryEndpoint(base, owner, repository string) (string, error) {
	for _, segment := range []string{owner, repository} {
		if segment == "" || segment == "." || segment == ".." ||
			strings.ContainsAny(segment, "/\\?#%\x00\r\n") {
			return "", fmt.Errorf("github: %q is not a repository path segment", segment)
		}
	}

	return base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository), nil
}
