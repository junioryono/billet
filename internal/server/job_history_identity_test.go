package server

import (
	"testing"

	"github.com/junioryono/billet/internal/alloc"
)

// A POOLED RUNNER'S HISTORY ROW NAMES THE JOB GITHUB GAVE IT, and the
// completion fills what JobStarted left out. Driven through handle, so the test
// fails if either call site stops recording rather than only if the allocator
// method breaks.
func TestAPooledJobsHistoryNamesTheJobItRan(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	runner := p.launchedFor11(t)

	started := job11
	started.RunnerID, started.RunnerName = 77, runner.RunnerName
	started.Event = "push"
	started.Owner, started.Repository = "acme", "api"
	started.WorkflowRef = "acme/api/.github/workflows/ci.yml@refs/heads/main"

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11: %v", err)
	}

	got, err := p.a.Job(t.Context(), runner.LeaseID)
	if err != nil {
		t.Fatalf("Job after start: %v", err)
	}
	want := alloc.HistoryJob{JobID: "job-11", WorkflowRef: started.WorkflowRef, Event: "push"}
	if got.Job != want || got.Repo != "acme/api" {
		t.Fatalf("after JobStarted the row says %+v in %q, want %+v in acme/api", got.Job, got.Repo, want)
	}

	completed := started
	completed.Result = "Succeeded"
	completed.JobName = "test (ubuntu-latest)"
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []Job{completed},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("complete job 11: %v", err)
	}

	got, err = p.a.Job(t.Context(), runner.LeaseID)
	if err != nil {
		t.Fatalf("Job after completion: %v", err)
	}
	if got.Job.Name != "test (ubuntu-latest)" {
		t.Errorf("job name = %q, want the completion's", got.Job.Name)
	}
}

// AN ASSIGNMENT THAT NAMES ITS JOB RECORDS IT when the lease is assigned, so a
// job that never reaches JobStarted (a launch that fails) still says whose it
// was.
func TestAnAssignedJobsHistoryNamesTheJob(t *testing.T) {
	t.Parallel()

	assigned := job11
	assigned.Event = "pull_request"
	assigned.Owner, assigned.Repository = "acme", "web"
	assigned.WorkflowRef = "acme/web/.github/workflows/test.yml@refs/pull/9/merge"
	assigned.JobName = "unit"

	p := newRunnerlessPool(t, assigned)

	got, err := p.a.Job(t.Context(), p.launchedFor11(t).LeaseID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	want := alloc.HistoryJob{JobID: "job-11", WorkflowRef: assigned.WorkflowRef,
		Name: "unit", Event: "pull_request"}
	if got.Job != want || got.Repo != "acme/web" {
		t.Errorf("after assignment the row says %+v in %q, want %+v in acme/web", got.Job, got.Repo, want)
	}
}
