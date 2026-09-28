package server

import (
	"context"
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

type fakeRunEvidence struct {
	run    WorkflowRun
	branch string
	err    error
	asked  int
}

func (f *fakeRunEvidence) WorkflowRun(context.Context, string, string, int64) (WorkflowRun, error) {
	f.asked++
	return f.run, f.err
}

func (f *fakeRunEvidence) DefaultBranch(context.Context, string, string) (string, error) {
	return f.branch, f.err
}

// completedPool starts job 11 on the runner launched for it as a push to main
// and completes it, and returns what the runner was handed.
func completedPool(t *testing.T, spec config.CacheSpec, evidence RunEvidence) []CacheAuthority {
	t.Helper()

	started := Job{RequestID: 11, RunID: 101, JobID: "job-11", Owner: "acme", Repository: "api",
		Event: "push", WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"}
	assigned := started
	assigned.Owner, assigned.Repository, assigned.Event, assigned.WorkflowRef = "", "", "", ""

	p := newRunnerlessPool(t, assigned)
	runner := &fakeRunner{}
	p.l.runner = runner
	WithCachePublication(spec, evidence)(p.l)

	member := p.launchedFor11(t)
	started.RunnerID, started.RunnerName = 77, member.RunnerName
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completed := started
	completed.Result = "succeeded"
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []Job{completed},
		Statistics: &Statistics{}}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	return runner.authorities
}

func defaultBranchSpec() config.CacheSpec {
	return config.CacheSpec{Publish: config.CachePublishDefaultBranch, Owner: "acme", Repository: "api"}
}

// A COMPLETION CARRIES THE AUTHORITY THE LISTENER DECIDED, through handle, from
// the binding JobStarted recorded and GitHub's record of the run. A test of
// DecideCacheAuthority alone stays green if the listener stops calling it.
func TestACompletedDefaultBranchJobCarriesItsPublication(t *testing.T) {
	t.Parallel()

	evidence := &fakeRunEvidence{branch: "main", run: WorkflowRun{ID: 101, Event: "push",
		HeadBranch: "main", HeadRepository: "acme/api", Repository: "acme/api",
		Path: ".github/workflows/ci.yml", HeadSHA: "abc"}}
	got := completedPool(t, defaultBranchSpec(), evidence)

	if len(got) != 1 || !got[0].PublishDefault || got[0].JobID != "job-11" || got[0].LeaseID == "" {
		t.Fatalf("authorities = %+v, want one that publishes the default branch for job-11", got)
	}
}

// NOTHING PUBLISHES WHEN THE EVIDENCE CANNOT BE READ, and the teardown still
// happens: a slow or failing GitHub costs the cache, never the job.
func TestAnUnreadableRunPublishesNothing(t *testing.T) {
	t.Parallel()

	evidence := &fakeRunEvidence{err: errors.New("github is down")}
	got := completedPool(t, defaultBranchSpec(), evidence)

	if len(got) != 1 || got[0].Proven || got[0].PublishDefault || got[0].WriteOwnRef {
		t.Fatalf("authorities = %+v, want one unproven completion", got)
	}
}

// A JOB OF ANOTHER REPOSITORY THAN THE TIER'S NAMESPACE IS NOT PROVEN FOR IT,
// however GitHub describes the job: an organization pool may run any of its
// repositories, and only the configured one owns this namespace.
func TestAJobOfAnotherRepositoryDoesNotPublishIntoTheNamespace(t *testing.T) {
	t.Parallel()

	evidence := &fakeRunEvidence{branch: "main", run: WorkflowRun{ID: 101, Event: "push",
		HeadBranch: "main", HeadRepository: "acme/api", Repository: "acme/api",
		Path: ".github/workflows/ci.yml", HeadSHA: "abc"}}
	spec := defaultBranchSpec()
	spec.Repository = "web"
	got := completedPool(t, spec, evidence)

	if len(got) != 1 || got[0].Proven || got[0].PublishDefault {
		t.Fatalf("authorities = %+v, want an unproven authority for another repository", got)
	}
}

// A TRUSTED-ONLY TIER NEVER ASKS GITHUB, so a deployment that did not opt in
// needs no new permission and spends no API budget.
func TestATrustedOnlyTierNeverAsksForRunEvidence(t *testing.T) {
	t.Parallel()

	evidence := &fakeRunEvidence{branch: "main"}
	got := completedPool(t, config.CacheSpec{Publish: config.CachePublishTrustedOnly}, evidence)

	if evidence.asked != 0 {
		t.Errorf("GitHub was asked %d times for a trusted-only tier", evidence.asked)
	}
	if len(got) != 1 || got[0] != (CacheAuthority{}) {
		t.Fatalf("authorities = %+v, want one zero authority", got)
	}
}

var _ = alloc.PoolRunner{}
