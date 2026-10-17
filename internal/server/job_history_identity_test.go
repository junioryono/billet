package server

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
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

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{started},
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
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{completed},
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
	launchedFor := time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)
	ran := launchedFor.Add(13 * time.Minute)
	assigned11.QueuedAt, assigned12.QueuedAt = launchedFor, ran
	p := newRunnerlessPool(t, assigned11, assigned12)
	swapped := p.launchedFor11(t)

	started := assigned12
	started.RunnerID, started.RunnerName = 77, swapped.RunnerName
	started.Event = "push"
	started.WorkflowRef = "acme/api/.github/workflows/ci.yml@refs/heads/main"
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 2}}); err != nil {
		t.Fatalf("start job 12 on the runner launched for 11: %v", err)
	}

	completed := started
	completed.Result, completed.JobName = "Succeeded", "ran"
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{completed},
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
	if got.GitHubQueuedAt != "2026-10-16T07:13:00.000000000Z" {
		t.Errorf("the swapped runner's queue time is %q, want the job it ran's, 07:13", got.GitHubQueuedAt)
	}
}

// waitsTold is a QueueWaitObserver that keeps what it was told.
type waitsTold struct {
	mu    sync.Mutex
	tiers []string
	waits []time.Duration
}

func (w *waitsTold) JobWaited(tier string, wait time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.tiers = append(w.tiers, tier)
	w.waits = append(w.waits, wait)
}

func (w *waitsTold) told() ([]string, []time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.tiers), slices.Clone(w.waits)
}

// GITHUB'S TIMES REACH THE ROW OF THE JOB THAT RAN, AND ITS WAIT IS TOLD ONCE.
// Driven through handle: a redelivered JobStarted and the completion, whose
// times differ, write nothing over the first and tell nothing again.
func TestAPooledJobsQueueTimesReachItsRowAndItsWaitIsToldOnce(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	runner := p.launchedFor11(t)

	told := &waitsTold{}
	p.a.ObserveQueueWaits(told)

	queued := time.Now().Add(-40 * time.Minute).UTC().Truncate(time.Second)
	started := job11
	started.RunnerID, started.RunnerName = 77, runner.RunnerName
	started.QueuedAt = queued
	started.ScaleSetAssignedAt = queued.Add(30 * time.Second)
	started.RunnerAssignedAt = queued.Add(39 * time.Minute)

	for id := int64(2); id <= 3; id++ {
		if err := p.l.handle(t.Context(), &Message{MessageID: id, Started: []dispatch.Job{started},
			Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
			t.Fatalf("start job 11 (message %d): %v", id, err)
		}
	}

	completed := started
	completed.Result = "Succeeded"
	completed.QueuedAt = queued.Add(time.Hour)
	if err := p.l.handle(t.Context(), &Message{MessageID: 4, Completed: []dispatch.Job{completed},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("complete job 11: %v", err)
	}

	got, err := p.a.Job(t.Context(), runner.LeaseID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}

	stamp := func(at time.Time) string { return at.Format("2006-01-02T15:04:05.000000000Z07:00") }
	if got.GitHubQueuedAt != stamp(queued) ||
		got.GitHubScaleSetAssignedAt != stamp(started.ScaleSetAssignedAt) ||
		got.GitHubRunnerAssignedAt != stamp(started.RunnerAssignedAt) {
		t.Errorf("the row's GitHub times are %q, %q, %q; want JobStarted's, %s, %s, %s",
			got.GitHubQueuedAt, got.GitHubScaleSetAssignedAt, got.GitHubRunnerAssignedAt,
			stamp(queued), stamp(started.ScaleSetAssignedAt), stamp(started.RunnerAssignedAt))
	}
	if got.BoughtAt == "" || got.BoughtAt > got.StartedAt {
		t.Errorf("bought at %q, started at %q: want the lease's purchase, before the start",
			got.BoughtAt, got.StartedAt)
	}

	tiers, waits := told.told()
	if len(waits) != 1 || tiers[0] != p.l.tier {
		t.Fatalf("told %d waits for %v, want one for %s", len(waits), tiers, p.l.tier)
	}
	if waits[0] < 40*time.Minute || waits[0] > 41*time.Minute {
		t.Errorf("the wait told is %s, want the start less GitHub's queue time, about 40m", waits[0])
	}
}
