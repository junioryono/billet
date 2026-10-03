package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
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

	ref, own, ok := jobRef(full, identity, run, defaultBranch)
	if !ok {
		return authority
	}

	authority.Ref = ref
	authority.DefaultRef = "refs/heads/" + defaultBranch
	authority.Proven = true
	if identity.Event == "pull_request" && len(run.PullRequests) == 1 {
		authority.BaseRef = "refs/heads/" + run.PullRequests[0].Base
	}

	// A REUSABLE WORKFLOW READS AND NEVER WRITES. Its ref is the callee's, and the
	// only thing relating it to the run's own ref is the commit both resolved to:
	// a tag named main at commit S calling `@refs/heads/main` while the branch is
	// also at S presents exactly what a push to main calling a local workflow does,
	// and GitHub records nothing else that tells them apart (#318). A job defined in
	// the run's top-level workflow carries the run's own triggering ref, which does.
	if !own {
		return authority
	}

	if ref == authority.DefaultRef {
		authority.PublishDefault = defaultBranchWriters[identity.Event]
		authority.WriteOwnRef = authority.PublishDefault
	} else {
		authority.WriteOwnRef = true
	}

	return authority
}

// jobRef is the ref the job runs for, and whether that ref is the run's own
// triggering ref rather than a reusable workflow's; false when the evidence
// cannot say.
//
// THE WORKFLOW REF IS TRUSTED ONLY WHERE IT IS THE RUN'S OWN. A job defined in
// the run's top-level workflow carries that workflow's ref. A reusable workflow
// of the same repository that GitHub records at the run's own commit ran that
// commit's code, so its ref places the job for reading, but it is the callee's
// ref and is never the run's own (#318). A reusable workflow pinned to another
// commit carries a ref unrelated to the run, and a push to a feature branch
// calling `owner/repo/...@main` would otherwise read as a push to main. For a
// branch event the ref must also name the run's head branch.
func jobRef(full string, identity alloc.JobIdentity, run WorkflowRun,
	defaultBranch string,
) (string, bool, bool) {
	workflow, ref, ok := strings.Cut(identity.WorkflowRef, "@")
	if !ok || ref == "" || strings.Contains(ref, "@") {
		return "", false, false
	}
	owner, rest, ok := strings.Cut(workflow, "/")
	if !ok {
		return "", false, false
	}
	repository, path, ok := strings.Cut(rest, "/")
	if !ok || !strings.EqualFold(owner+"/"+repository, full) || path == "" {
		return "", false, false
	}

	// A FILE NAMED LIKE THE TOP-LEVEL WORKFLOW IS THE TOP-LEVEL WORKFLOW ONLY IF
	// THE RUN DID NOT CALL IT: a workflow can call itself pinned to another ref,
	// and the job then carries the callee's ref under the run's own file name.
	called := calledByTheRun(identity.WorkflowRef, run)
	topLevel := path == run.Path && !called
	if !topLevel && !calledAtTheRunsCommit(identity.WorkflowRef, run) {
		return "", false, false
	}

	// AND A FILE THE RUN ALSO CALLED, UNDER ANY SPELLING OF ANY REF, IS NEVER THE
	// RUN'S OWN. calledByTheRun compares whole strings, and GitHub can name the
	// callee's ref differently there (`@main` beside `refs/heads/main`), which
	// would read a self-call as the top-level workflow. Withholding writes from
	// every file the run references costs only a self-calling workflow's
	// publication.
	if topLevel && callsTheFile(full, path, run) {
		topLevel = false
	}

	switch {
	case identity.Event == "pull_request":
		if len(run.PullRequests) != 1 {
			return "", false, false
		}
		want := "refs/pull/" + strconv.FormatInt(run.PullRequests[0].Number, 10) + "/merge"
		if ref != want {
			return "", false, false
		}

		return ref, topLevel, true
	case branchEvents[identity.Event]:
		if run.HeadBranch == "" || ref != "refs/heads/"+run.HeadBranch {
			return "", false, false
		}

		return ref, topLevel, true
	case defaultBranchReaders[identity.Event]:
		if ref != "refs/heads/"+defaultBranch {
			return "", false, false
		}

		return ref, topLevel, true
	default:
		return "", false, false
	}
}

// cacheAuthorityLimit bounds reading GitHub's evidence for one authority. A
// completion waits on it, so a slow GitHub costs the job's cache and never its
// teardown.
const cacheAuthorityLimit = 10 * time.Second

// ResolveCacheAuthority reads GitHub's record of the binding's run and the
// repository's default branch, fresh, and decides. A binding that names no run
// is not asked about; a failed read is returned beside an unproven authority,
// which writes nothing.
func ResolveCacheAuthority(
	ctx context.Context, evidence RunEvidence, leaseID string, binding alloc.PoolRunner,
	completion *Job,
) (CacheAuthority, error) {
	unproven := DecideCacheAuthority(leaseID, binding, completion, WorkflowRun{}, "")
	identity := binding.Identity
	if evidence == nil || binding.RunID <= 0 || identity.Owner == "" || identity.Repository == "" {
		return unproven, nil
	}

	ctx, cancel := context.WithTimeout(ctx, cacheAuthorityLimit)
	defer cancel()

	run, err := evidence.WorkflowRun(ctx, identity.Owner, identity.Repository, binding.RunID)
	if err != nil {
		return unproven, fmt.Errorf("server: read workflow run %d of %s/%s: %w",
			binding.RunID, identity.Owner, identity.Repository, err)
	}
	defaultBranch, err := evidence.DefaultBranch(ctx, identity.Owner, identity.Repository)
	if err != nil {
		return unproven, fmt.Errorf("server: read the default branch of %s/%s: %w",
			identity.Owner, identity.Repository, err)
	}

	return DecideCacheAuthority(leaseID, binding, completion, run, defaultBranch), nil
}

// ScopedCacheAuthority is the authority a tier with the given cache
// configuration may act on: a job of another repository than the tier's
// namespace is not proven for it, whatever GitHub says about the job.
func ScopedCacheAuthority(spec config.CacheSpec, authority CacheAuthority) CacheAuthority {
	if spec.Publish != config.CachePublishDefaultBranch ||
		!strings.EqualFold(authority.Owner, spec.Owner) ||
		!strings.EqualFold(authority.Repository, spec.Repository) {
		authority.Proven, authority.WriteOwnRef, authority.PublishDefault = false, false, false
	}

	return authority
}

// completionCacheAuthority is the authority a completed job's destroy carries.
func (l *Listener) completionCacheAuthority(ctx context.Context, job Job, leaseID string) CacheAuthority {
	if l.cacheSpec.Publish != config.CachePublishDefaultBranch || l.runEvidence == nil ||
		l.alloc == nil || leaseID == "" {
		return CacheAuthority{}
	}

	binding, err := l.alloc.PoolRunnerByLease(ctx, leaseID)
	if err != nil {
		l.log.Warn("could not read the job a completed runner ran; its caches will not publish",
			"tier", l.tier, "lease", leaseID, "error", err)

		return CacheAuthority{}
	}
	authority, err := ResolveCacheAuthority(ctx, l.runEvidence, leaseID, binding, &job)
	if err != nil {
		l.log.Warn("could not read GitHub's record of a completed job; its caches will not publish",
			"tier", l.tier, "lease", leaseID, "error", err)
	}
	authority = ScopedCacheAuthority(l.cacheSpec, authority)
	l.log.Info("decided what a completed job's caches may publish", "tier", l.tier,
		"lease", leaseID, "job", authority.JobID, "event", authority.Event, "ref", authority.Ref,
		"proven", authority.Proven, "publish_default", authority.PublishDefault)

	return authority
}

// calledByTheRun reports whether GitHub records the workflow as one the run
// called, at any commit.
func calledByTheRun(workflowRef string, run WorkflowRun) bool {
	for _, called := range run.ReferencedWorkflows {
		if called.Path == workflowRef {
			return true
		}
	}

	return false
}

// callsTheFile reports whether the run called repository full's workflow file at
// path at any ref, however GitHub spelled it. Repository names fold case; paths
// do not.
//
// SPLIT AT THE FIRST `@`, deliberately: a workflow file whose own name holds an
// `@` is then read as the file before it, which can only withhold a write from
// a job that might have had one. Splitting at the last would misread a ref that
// holds an `@` and could let a self-call through.
func callsTheFile(full, path string, run WorkflowRun) bool {
	for _, called := range run.ReferencedWorkflows {
		workflow, _, _ := strings.Cut(called.Path, "@")
		owner, rest, ok := strings.Cut(workflow, "/")
		if !ok {
			continue
		}
		repository, file, ok := strings.Cut(rest, "/")
		if ok && strings.EqualFold(owner+"/"+repository, full) && file == path {
			return true
		}
	}

	return false
}

// calledAtTheRunsCommit reports whether the run called the workflow at the
// run's own head commit, as a local `./` call does. A reusable workflow GitHub
// resolved to that commit ran exactly the code of the commit the run is for, so
// its ref may stand for the run's; a pinned one resolved elsewhere may not.
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
