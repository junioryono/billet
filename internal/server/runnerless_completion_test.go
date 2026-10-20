package server

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

type recordedResult struct{ lease, result string }

// runnerlessPool is a listener whose tier has launched a pool runner for each
// assigned job, with every teardown, registration removal and result it records.
type runnerlessPool struct {
	a         *alloc.Allocator
	l         *Listener
	session   *fakeSession
	registry  *inspectingRegistry
	destroyed []int64
	results   []recordedResult
}

func newRunnerlessPool(t *testing.T, assigned ...dispatch.Job) *runnerlessPool {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	p := &runnerlessPool{
		a:        newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers),
		session:  &fakeSession{},
		registry: &inspectingRegistry{byName: map[string][]RunnerState{}},
	}
	runner := &fakeRunner{onDestroy: func(requestID int64) error {
		p.destroyed = append(p.destroyed, requestID)
		return nil
	}}
	p.l = NewListener(p.a, tiers[0].Label, p.session, WithRunner(runner),
		WithRunnerRegistry(p.registry))
	p.l.writeJobResult = func(_ context.Context, leaseID, result string, _ int64) error {
		p.results = append(p.results, recordedResult{leaseID, result})
		return nil
	}

	if err := p.l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refill escrow: %v", err)
	}

	if err := p.l.handle(t.Context(), &Message{MessageID: 1, Assigned: assigned,
		Statistics: &Statistics{TotalAssignedJobs: len(assigned)}}); err != nil {
		t.Fatalf("launch the pool: %v", err)
	}

	return p
}

func (p *runnerlessPool) launchedFor11(t *testing.T) alloc.PoolRunner {
	t.Helper()

	runner, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, 11)
	if err != nil {
		t.Fatalf("the runner launched for 11: %v", err)
	}

	return runner
}

var (
	job11 = dispatch.Job{RequestID: 11, RunID: 101, JobID: "job-11"}
	job12 = dispatch.Job{RequestID: 12, RunID: 102, JobID: "job-12"}
	job13 = dispatch.Job{RequestID: 13, RunID: 103, JobID: "job-13"}
)

// A RUNNER-LESS CANCELLATION IS NOT THE END OF A RUNNER BUSY WITH ANOTHER JOB.
//
// A pool member is launched FOR one job and GitHub may give it any job waiting in
// the scale set, so the runner launched for request 11 can be running job 12
// while job 11 is cancelled before any runner takes it. GitHub reports that
// cancellation with no runner, and it names request 11, which is the busy
// runner's LAUNCH identity. Measured on a control plane on 2026-09-22: billet
// tore down the runner running job 12 (GitHub refused, the runner being
// mid-job), recorded "canceled" on its lease so the real "succeeded" was later
// refused as a contradiction, and acknowledged the lease's completion source.
// The cancellation must touch none of that; the runner's own completion settles
// it.
func TestARunnerlessCancellationDoesNotSettleARunnerBusyWithAnotherJob(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11, job12)
	swapped := p.launchedFor11(t)

	started := job12
	started.RunnerID, started.RunnerName = 77, swapped.RunnerName

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 2}}); err != nil {
		t.Fatalf("start job 12 on the runner launched for 11: %v", err)
	}

	canceled := job11
	canceled.Result = "Canceled"

	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{canceled},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}

	if slices.Contains(p.destroyed, 11) {
		t.Errorf("destroyed %v: the compute of the runner launched for 11 was torn down while it runs job 12", p.destroyed)
	}

	if slices.Contains(p.registry.names, swapped.RunnerName) {
		t.Errorf("removed %v: the registration of a runner running job 12 was asked to go", p.registry.names)
	}

	for _, r := range p.results {
		if r.lease == swapped.LeaseID {
			t.Errorf("recorded %q on the lease running job 12, for a job it never ran", r.result)
		}
	}

	after, err := p.a.PoolRunnerByLease(t.Context(), swapped.LeaseID)
	if err != nil {
		t.Fatalf("read the runner launched for 11: %v", err)
	}

	if after.Status != alloc.PoolRunnerBusy || after.SourceAcknowledged {
		t.Errorf("the runner running job 12 is %q with its completion source acknowledged=%v; want busy "+
			"and unacknowledged, since its own completion has not arrived", after.Status, after.SourceAcknowledged)
	}

	// AND ITS OWN COMPLETION STILL SETTLES IT, with the result that is true.
	succeeded := started
	succeeded.Result = "Succeeded"

	if err := p.l.handle(t.Context(), &Message{MessageID: 4, Completed: []dispatch.Job{succeeded},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("job 12's own completion was refused: %v", err)
	}

	if !slices.Contains(p.destroyed, 11) {
		t.Errorf("destroyed %v: job 12's completion did not tear down the runner that ran it", p.destroyed)
	}

	if !slices.Contains(p.results, recordedResult{swapped.LeaseID, "Succeeded"}) {
		t.Errorf("recorded %v: the runner's lease does not carry the result of the job it ran", p.results)
	}
}

// The job left behind is still finished: an offer for it in the same message is
// not acquired, although no lease and no commitment names it any more.
func TestARunnerlessCancellationStillFinishesItsJob(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11, job12)
	swapped := p.launchedFor11(t)

	started := job12
	started.RunnerID, started.RunnerName = 77, swapped.RunnerName

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 2}}); err != nil {
		t.Fatalf("start job 12 on the runner launched for 11: %v", err)
	}

	canceled := job11
	canceled.Result = "Canceled"
	succeeded := started
	succeeded.Result = "Succeeded"

	// The runner's own completion in the same message frees the launch identity
	// 11, so nothing but the finished job itself stands between it and the offer.
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{canceled, succeeded},
		Available: []dispatch.Job{job11, job13}, Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}

	// Job 13 is the control: the offer path had room and was taken.
	if !slices.Contains(p.session.acquired, 13) || slices.Contains(p.session.acquired, 11) {
		t.Errorf("acquired %v, want job 13 and not job 11, which GitHub reported finished", p.session.acquired)
	}
}

// A RUNNER LAUNCHED FOR THE JOB AND STILL IDLE IS WHAT THE CANCELLATION RETIRES,
// because nothing else will: its job is gone and it runs nothing. GitHub deleting
// the id it holds for the runner is the proof, asked for before anything is
// journaled, and the cancellation is not recorded on a lease that never ran it.
func TestARunnerlessCancellationRetiresTheIdleRunnerLaunchedForIt(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	idle := p.launchedFor11(t)
	p.registry.byName[idle.RunnerName] = []RunnerState{{Present: true, Online: true, ID: 4401}}

	canceled := job11
	canceled.Result = "Canceled"

	// No statistics, or the pool's reconciliation to them would retire the idle
	// runner whatever the completion did.
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}

	if !slices.Equal(p.registry.withdrawn, []int64{4401}) || !slices.Contains(p.destroyed, 11) {
		t.Errorf("withdrew %v, destroyed %v: the idle runner launched for a cancelled job was not retired "+
			"by deleting the id GitHub holds", p.registry.withdrawn, p.destroyed)
	}
	for _, r := range p.results {
		if r.lease == idle.LeaseID {
			t.Errorf("recorded %q on the idle runner's lease, for a job it never ran", r.result)
		}
	}
	if _, err := p.a.PoolRunnerByLease(t.Context(), idle.LeaseID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("the retired runner's journal is still read (%v), want it forgotten", err)
	}
}

// AN IDLE RUNNER GITHUB WILL NOT DELETE IS RUNNING A JOB BILLET HAS NOT HEARD OF.
// Measured on 2026-10-20: a runner launched for a job cancelled before any runner
// took it was refused deletion as "currently running a job", and the JobStarted
// naming its real job arrived four minutes later. Settled on the cancellation,
// the runner's lease took the cancelled job's result, that JobStarted was refused
// and the real result was refused as a contradiction. Kept idle and unjournaled,
// the JobStarted binds it and the real completion settles it.
func TestARunnerlessCancellationKeepsAnIdleRunnerGitHubWillNotDelete(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11, job12)
	swapped := p.launchedFor11(t)
	p.registry.byName[swapped.RunnerName] = []RunnerState{{Present: true, Online: true, ID: 4402}}
	p.registry.withdrawErr = errors.New("job still running: Runner is currently running a job and cannot be deleted")

	canceled := job11
	canceled.Result = "Canceled"
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}
	if !slices.Equal(p.registry.withdrawn, []int64{4402}) {
		t.Fatalf("withdrew %v, want the id GitHub holds asked for once", p.registry.withdrawn)
	}
	if slices.Contains(p.destroyed, 11) {
		t.Fatalf("destroyed %v: a runner GitHub refused to delete was torn down", p.destroyed)
	}
	kept, err := p.a.PoolRunnerByLease(t.Context(), swapped.LeaseID)
	if err != nil || kept.Status != alloc.PoolRunnerIdle {
		t.Fatalf("the refused runner is %+v (%v), want it idle and unjournaled", kept, err)
	}

	started := job12
	started.RunnerID, started.RunnerName = 4402, swapped.RunnerName
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 2}}); err != nil {
		t.Fatalf("the delayed JobStarted was refused: %v", err)
	}
	bound, err := p.a.PoolRunnerByLease(t.Context(), swapped.LeaseID)
	if err != nil || bound.Status != alloc.PoolRunnerBusy || bound.JobID != "job-12" || bound.RunID != 102 {
		t.Fatalf("after the delayed JobStarted the runner is %+v (%v), want busy with job 12 of run 102", bound, err)
	}

	succeeded := started
	succeeded.Result = "Succeeded"
	if err := p.l.handle(t.Context(), &Message{MessageID: 4, Completed: []dispatch.Job{succeeded},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("the real completion was refused: %v", err)
	}
	var recorded []string
	for _, r := range p.results {
		if r.lease == swapped.LeaseID {
			recorded = append(recorded, r.result)
		}
	}
	if !slices.Equal(recorded, []string{"Succeeded"}) || !slices.Contains(p.destroyed, 11) {
		t.Errorf("recorded %v on the runner's lease and destroyed %v; want only the real job's Succeeded, "+
			"then its teardown", recorded, p.destroyed)
	}
}

// AN IDLE RUNNER GITHUB REPORTS BUSY, OR CANNOT ANSWER FOR, IS KEPT WITHOUT A
// DELETE BEING ASKED: could not tell is not idle.
func TestARunnerlessCancellationAsksNoDeleteOfARunnerGitHubReportsBusy(t *testing.T) {
	t.Parallel()

	for name, answer := range map[string][]RunnerState{
		"busy":       {{Present: true, Online: true, Busy: true, ID: 4403}},
		"absent":     {{}},
		"unanswered": nil,
	} {
		p := newRunnerlessPool(t, job11)
		runner := p.launchedFor11(t)
		if answer != nil {
			p.registry.byName[runner.RunnerName] = answer
		}

		canceled := job11
		canceled.Result = "Canceled"
		if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled}}); err != nil {
			t.Fatalf("%s: the runner-less cancellation was refused: %v", name, err)
		}
		if len(p.registry.withdrawn) != 0 || slices.Contains(p.destroyed, 11) || len(p.registry.askedFor) != 1 {
			t.Errorf("%s: asked about %v, withdrew %v, destroyed %v; want one inspection and nothing removed",
				name, p.registry.askedFor, p.registry.withdrawn, p.destroyed)
		}
		if kept, err := p.a.PoolRunnerByLease(t.Context(), runner.LeaseID); err != nil ||
			kept.Status != alloc.PoolRunnerIdle {
			t.Errorf("%s: the runner is %+v (%v), want it idle and unjournaled", name, kept, err)
		}
	}
}

// A completion naming the job the launch runner is actually running settles that
// runner even without a runner name: it is that runner's completion.
func TestARunnerlessCompletionOfTheRunnersOwnJobSettlesIt(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	own := p.launchedFor11(t)

	started := job11
	started.RunnerID, started.RunnerName = 77, own.RunnerName

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11 on its own runner: %v", err)
	}

	succeeded := job11
	succeeded.Result = "Succeeded"

	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{succeeded},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("the completion was refused: %v", err)
	}

	if !slices.Contains(p.destroyed, 11) ||
		!slices.Contains(p.results, recordedResult{own.LeaseID, "Succeeded"}) {
		t.Errorf("destroyed %v, recorded %v: the runner's own job completed and it was not settled",
			p.destroyed, p.results)
	}
}

// A RUNNER RECOVERED AS BUSY HAS NO JOB YET, AND THAT IS NOT PERMISSION. After a
// restart a registration GitHub reports busy is journaled busy with its job
// unknown until a delayed JobStarted names it. A runner-less completion names a
// job no runner took, so it is not that runner's, and not knowing which job the
// runner is running must hold the runner rather than settle it.
func TestARunnerlessCancellationDoesNotSettleARecoveredBusyRunner(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	recovered := p.launchedFor11(t)
	recovered.RunnerID = 77

	if err := p.a.PreserveRecoveredBusyPoolRunner(t.Context(), recovered); err != nil {
		t.Fatalf("recover the runner as busy: %v", err)
	}

	canceled := job11
	canceled.Result = "Canceled"

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}

	if slices.Contains(p.destroyed, 11) || slices.Contains(p.registry.names, recovered.RunnerName) {
		t.Errorf("destroyed %v, removed %v: a runner running an unknown job was settled by a job it never ran",
			p.destroyed, p.registry.names)
	}

	for _, r := range p.results {
		if r.lease == recovered.LeaseID {
			t.Errorf("recorded %q on the recovered runner's lease, for a job it never ran", r.result)
		}
	}

	after, err := p.a.PoolRunnerByLease(t.Context(), recovered.LeaseID)
	if err != nil {
		t.Fatalf("read the recovered runner: %v", err)
	}

	if after.Status != alloc.PoolRunnerBusy || after.SourceAcknowledged {
		t.Errorf("the recovered runner is %q with its completion source acknowledged=%v; want busy and unacknowledged",
			after.Status, after.SourceAcknowledged)
	}
}
