package server

import (
	"context"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/alloc"
)

// RunEvidence reads what GitHub records about a workflow run and a repository.
// internal/scaleset implements it over the App's REST client.
type RunEvidence interface {
	WorkflowRun(ctx context.Context, owner, repository string, runID int64) (WorkflowRun, error)
	DefaultBranch(ctx context.Context, owner, repository string) (string, error)
}

// WorkflowRun is GitHub's record of one run, as a cache authority reads it.
type WorkflowRun struct {
	ID             int64
	Event          string
	HeadBranch     string
	HeadRepository string
	Repository     string
	PullRequests   []RunPullRequest
	// Path is the run's top-level workflow, `.github/workflows/x.yml`.
	Path    string
	HeadSHA string
	// ReferencedWorkflows are the reusable workflows the run called.
	ReferencedWorkflows []ReferencedWorkflow
}

// RunPullRequest is one pull request GitHub associated with a run.
type RunPullRequest struct {
	Number int64
	Base   string
}

// ReferencedWorkflow is one reusable workflow a run called, as
// `owner/repo/.github/workflows/x.yml@ref`, and the commit it resolved to.
type ReferencedWorkflow struct {
	Path string
	SHA  string
}

// CacheAuthority is what a cache may do for one job, decided from evidence
// GitHub produced and billet recorded. The zero value authorises nothing.
type CacheAuthority struct {
	LeaseID    string
	JobID      string
	RunID      int64
	Owner      string
	Repository string
	Event      string
	// Ref is the job's own cache scope: `refs/heads/<branch>`,
	// `refs/tags/<tag>` or `refs/pull/<n>/merge`.
	Ref string
	// BaseRef is a pull request's base branch, `refs/heads/<branch>`, which
	// GitHub also lets a pull request restore from. Empty for everything else.
	BaseRef string
	// DefaultRef is `refs/heads/<default branch>` when the authority was decided.
	DefaultRef string
	// Proven says every piece of evidence agreed. Nothing below it may be true
	// without it.
	Proven bool
	// WriteOwnRef says the job may write under Ref.
	WriteOwnRef bool
	// PublishDefault says the job may publish what the default branch reads.
	PublishDefault bool
}

// defaultBranchWriters are the events GitHub itself lets write the default
// branch's cache: every other event running on the default branch gets a
// read-only cache token there. From GitHub's changelog of 2026-06-26 ("Read-only
// Actions cache for untrusted triggers") and its dependency-caching reference
// as read on 2026-09-24. pull_request_target, issue_comment and workflow_run are
// deliberately absent: an actor without write access can start them.
var defaultBranchWriters = map[string]bool{
	"push": true, "schedule": true, "workflow_dispatch": true, "repository_dispatch": true,
	"delete": true, "registry_package": true, "page_build": true,
}

// branchEvents are the events whose run is for the branch GitHub names as the
// run's head branch.
var branchEvents = defaultBranchWriters

// defaultBranchReaders are the events GitHub runs in the default branch's
// context without letting them write its cache. Their head branch need not be
// the default branch (a pull_request_target's is the pull request's), so their
// ref is proved from the workflow ref alone, and it earns reads and nothing
// else. Every event in neither set is not proven and its job gets nothing.
var defaultBranchReaders = map[string]bool{
	"pull_request_target": true, "issue_comment": true, "workflow_run": true,
}

// DecideCacheAuthority is the one place the publication rule lives.
//
// binding is the pool member's durable JobStarted record, completion what a
// JobCompleted said about the same job (nil at request time), run GitHub's
// record of the run and defaultBranch the repository's default branch, both
// read fresh by the caller. Anything missing, unequal or unreadable leaves the
// authority unproven, and an unproven authority writes nothing: could not tell
// is never publish.
func DecideCacheAuthority(
	leaseID string, binding alloc.PoolRunner, completion *Job, run WorkflowRun,
	defaultBranch string,
) CacheAuthority {
	identity := binding.Identity
	authority := CacheAuthority{
		LeaseID: leaseID, JobID: binding.JobID, RunID: binding.RunID,
		Owner: identity.Owner, Repository: identity.Repository, Event: identity.Event,
	}
	full := identity.Owner + "/" + identity.Repository

	switch {
	case leaseID == "" || binding.LeaseID != leaseID || binding.JobID == "" || binding.RunID <= 0,
		identity.Owner == "" || identity.Repository == "" || identity.Event == "",
		identity.WorkflowRef == "", defaultBranch == "":
		return authority
	case completion != nil && (completion.JobID != binding.JobID ||
		completion.RunID != binding.RunID || !strings.EqualFold(completion.Owner, identity.Owner) ||
		!strings.EqualFold(completion.Repository, identity.Repository) ||
		completion.Event != identity.Event || completion.WorkflowRef != identity.WorkflowRef):
		return authority
	case run.ID != binding.RunID || run.Event != identity.Event,
		!strings.EqualFold(run.Repository, full), !strings.EqualFold(run.HeadRepository, full):
		// A fork's pull request has a head repository of its own, and is never
		// proven.
		return authority
	}

	ref, ok := jobRef(full, identity, run, defaultBranch)
	if !ok {
		return authority
	}

	authority.Ref = ref
	authority.DefaultRef = "refs/heads/" + defaultBranch
	authority.Proven = true
	if identity.Event == "pull_request" && len(run.PullRequests) == 1 {
		authority.BaseRef = "refs/heads/" + run.PullRequests[0].Base
	}
	if ref == authority.DefaultRef {
		authority.PublishDefault = defaultBranchWriters[identity.Event]
		authority.WriteOwnRef = authority.PublishDefault
	} else {
		authority.WriteOwnRef = true
	}

	return authority
}

// jobRef is the ref the job runs for, or false when the evidence cannot say.
//
// THE WORKFLOW REF IS TRUSTED ONLY WHERE IT IS THE RUN'S OWN. A job defined in
// the run's top-level workflow carries that workflow's ref; so does a reusable
// workflow of the same repository that GitHub records at the run's own commit,
// since a local `./` call runs at that commit. A reusable workflow pinned to
// another ref carries the callee's ref, and a push to a feature branch calling
// `owner/repo/...@main` would otherwise read as a push to main. For a branch
// event the ref must also name the run's head branch, which is what tells a
// branch from a tag GitHub reports under the same head-branch name.
func jobRef(full string, identity alloc.JobIdentity, run WorkflowRun,
	defaultBranch string,
) (string, bool) {
	workflow, ref, ok := strings.Cut(identity.WorkflowRef, "@")
	if !ok || ref == "" || strings.Contains(ref, "@") {
		return "", false
	}
	owner, rest, ok := strings.Cut(workflow, "/")
	if !ok {
		return "", false
	}
	repository, path, ok := strings.Cut(rest, "/")
	if !ok || !strings.EqualFold(owner+"/"+repository, full) || path == "" {
		return "", false
	}

	if path != run.Path && !calledAtTheRunsCommit(identity.WorkflowRef, run) {
		return "", false
	}

	switch {
	case identity.Event == "pull_request":
		if len(run.PullRequests) != 1 {
			return "", false
		}
		want := "refs/pull/" + strconv.FormatInt(run.PullRequests[0].Number, 10) + "/merge"
		if ref != want {
			return "", false
		}

		return ref, true
	case branchEvents[identity.Event]:
		if run.HeadBranch == "" || ref != "refs/heads/"+run.HeadBranch {
			return "", false
		}

		return ref, true
	case defaultBranchReaders[identity.Event]:
		if ref != "refs/heads/"+defaultBranch {
			return "", false
		}

		return ref, true
	default:
		return "", false
	}
}

func calledAtTheRunsCommit(workflowRef string, run WorkflowRun) bool {
	if run.HeadSHA == "" {
		return false
	}
	for _, called := range run.ReferencedWorkflows {
		if called.Path == workflowRef && called.SHA == run.HeadSHA {
			return true
		}
	}

	return false
}
