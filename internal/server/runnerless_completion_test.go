package server

import (
	"context"
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

type recordedResult struct{ lease, result string }

// runnerlessPool is a listener whose tier has launched a pool runner for each
// assigned job, with every teardown, registration removal and result it records.
type runnerlessPool struct {
	a         *alloc.Allocator
	l         *Listener
	session   *fakeSession
	registry  *fakeRunnerRegistry
	destroyed []int64
	results   []recordedResult
}

func newRunnerlessPool(t *testing.T, assigned ...Job) *runnerlessPool {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	p := &runnerlessPool{
		a:        newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers),
		session:  &fakeSession{},
		registry: &fakeRunnerRegistry{},
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
	job11 = Job{RequestID: 11, RunID: 101, JobID: "job-11"}
	job12 = Job{RequestID: 12, RunID: 102, JobID: "job-12"}
	job13 = Job{RequestID: 13, RunID: 103, JobID: "job-13"}
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

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 2}}); err != nil {
		t.Fatalf("start job 12 on the runner launched for 11: %v", err)
	}

	canceled := job11
	canceled.Result = "Canceled"

	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []Job{canceled},
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

	if err := p.l.handle(t.Context(), &Message{MessageID: 4, Completed: []Job{succeeded},
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

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 2}}); err != nil {
		t.Fatalf("start job 12 on the runner launched for 11: %v", err)
	}

	canceled := job11
	canceled.Result = "Canceled"
	succeeded := started
	succeeded.Result = "Succeeded"

	// The runner's own completion in the same message frees the launch identity
	// 11, so nothing but the finished job itself stands between it and the offer.
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []Job{canceled, succeeded},
		Available: []Job{job11, job13}, Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}

	// Job 13 is the control: the offer path had room and was taken.
	if !slices.Contains(p.session.acquired, 13) || slices.Contains(p.session.acquired, 11) {
		t.Errorf("acquired %v, want job 13 and not job 11, which GitHub reported finished", p.session.acquired)
	}
}

// A RUNNER LAUNCHED FOR THE JOB AND STILL IDLE IS WHAT THE CANCELLATION RETIRES,
// because nothing else will: its job is gone and it runs nothing.
func TestARunnerlessCancellationRetiresTheIdleRunnerLaunchedForIt(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	idle := p.launchedFor11(t)

	canceled := job11
	canceled.Result = "Canceled"

	// No statistics, or the pool's reconciliation to them would retire the idle
	// runner whatever the completion did.
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []Job{canceled}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}

	if !slices.Contains(p.destroyed, 11) || !slices.Contains(p.registry.names, idle.RunnerName) {
		t.Errorf("destroyed %v, removed %v: the idle runner launched for a cancelled job was kept",
			p.destroyed, p.registry.names)
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

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11 on its own runner: %v", err)
	}

	succeeded := job11
	succeeded.Result = "Succeeded"

	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []Job{succeeded},
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

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []Job{canceled}}); err != nil {
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
