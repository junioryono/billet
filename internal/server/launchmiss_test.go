package server

import (
	"context"
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

// A LAUNCH THAT STARTED NOTHING KEEPS THE JOB IT WAS BOUGHT FOR ON ITS ROW: the
// repository, the run, the job and its name. No job will ever claim the row, since its
// lease ends with the failure, and these are what a job GitHub stopped offering after it
// is read by (#911). Driven through handle, so the test fails if the launch path stops
// recording rather than only if the allocator does.
func TestALaunchThatStartedNothingNamesTheJobItWasBoughtFor(t *testing.T) {
	t.Parallel()

	tiers := runnerlessTiers()
	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(int64) error { return errors.New("no node took the command") },
	}), WithRunnerRegistry(&fakeRunnerRegistry{busy: map[string]bool{}}))
	l.writeJobResult = func(context.Context, string, string, int64) error { return nil }

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refill escrow: %v", err)
	}

	job := dispatch.Job{RunID: 44695474509, JobID: "lint-app", JobName: "Lint App",
		Owner: "acme", Repository: "app", WorkflowRef: "acme/app/.github/workflows/build.yml@refs/heads/main",
		Event: "push"}
	if err := l.handle(t.Context(), &Message{MessageID: 1, Assigned: []dispatch.Job{job},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("handle the assignment: %v", err)
	}

	leases, _, err := a.JobsOfRun(t.Context(), job.RunID)
	if err != nil || len(leases) != 1 {
		t.Fatalf("the run's rows are %v (%v), want the one lease bought for its job", leases, err)
	}
	got, err := a.Job(t.Context(), leases[0])
	if err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if got.Repo != "acme/app" || got.RunID != job.RunID || got.Job.JobID != "lint-app" || got.Job.Name != "Lint App" {
		t.Fatalf("the row of a launch that started nothing says job %+v in %q, run %d; want lint-app "+
			"(Lint App) in acme/app, run %d", got.Job, got.Repo, got.RunID, job.RunID)
	}
}
