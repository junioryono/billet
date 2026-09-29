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

// A SWAPPED POOL RUNNER'S HISTORY NAMES THE JOB IT RAN, not the one it was
// launched for. GitHub gives a pooled runner any waiting job, so the assignment
// that caused the launch says nothing about what ran; recorded at assignment,
// the launch's job would win the write-once column and the job that ran could
// never be written.
func TestASwappedPoolRunnersHistoryNamesTheJobItRan(t *testing.T) {
	t.Parallel()

	assigned11, assigned12 := job11, job12
	assigned11.Owner, assigned11.Repository, assigned11.JobName = "acme", "web", "launched-for"
	assigned12.Owner, assigned12.Repository = "acme", "api"
	p := newRunnerlessPool(t, assigned11, assigned12)
	swapped := p.launchedFor11(t)

	started := assigned12
	started.RunnerID, started.RunnerName = 77, swapped.RunnerName
	started.Event = "push"
	started.WorkflowRef = "acme/api/.github/workflows/ci.yml@refs/heads/main"
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 2}}); err != nil {
		t.Fatalf("start job 12 on the runner launched for 11: %v", err)
	}

	completed := started
	completed.Result, completed.JobName = "Succeeded", "ran"
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []Job{completed},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("complete job 12: %v", err)
	}

	got, err := p.a.Job(t.Context(), swapped.LeaseID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	want := alloc.HistoryJob{JobID: "job-12", WorkflowRef: started.WorkflowRef, Name: "ran", Event: "push"}
	if got.Job != want || got.Repo != "acme/api" {
		t.Errorf("the swapped runner's row says %+v in %q, want the job it ran, %+v in acme/api",
			got.Job, got.Repo, want)
	}
}
