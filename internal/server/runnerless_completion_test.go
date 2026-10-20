package server

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

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
	registry  *fakeRunnerRegistry
	destroyed []int64
	results   []recordedResult
}

func newRunnerlessPool(t *testing.T, assigned ...dispatch.Job) *runnerlessPool {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	p := &runnerlessPool{
		a:        newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers),
		session:  &fakeSession{},
		registry: &fakeRunnerRegistry{busy: map[string]bool{}},
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

// restart replaces the listener with a fresh one over the same ledger, runner
// and registry, as a controller restart leaves it: nothing held in memory.
func (p *runnerlessPool) restart() {
	runner := p.l.runner
	p.l = NewListener(p.a, p.l.tier, p.session, WithRunner(runner), WithRunnerRegistry(p.registry))
	p.l.writeJobResult = func(_ context.Context, leaseID, result string, _ int64) error {
		p.results = append(p.results, recordedResult{leaseID, result})
		return nil
	}
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

// AN IDLE RUNNER LAUNCHED FOR A CANCELLED JOB IS THE POOL'S TO RETIRE, NOT THE
// CANCELLATION'S. It never ran the job, so nothing of it is recorded on the
// runner's lease; GitHub's count no longer includes the job, so the runner is
// surplus, and the pool retires it by deleting its registration first.
func TestARunnerlessCancellationLeavesTheIdleRunnerLaunchedForItToThePool(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	idle := p.launchedFor11(t)

	canceled := job11
	canceled.Result = "Canceled"
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}

	if !slices.Contains(p.destroyed, 11) || !slices.Contains(p.registry.names, idle.RunnerName) {
		t.Errorf("destroyed %v, removed %v: the idle surplus runner launched for a cancelled job was kept",
			p.destroyed, p.registry.names)
	}
	for _, r := range p.results {
		if r.lease == idle.LeaseID {
			t.Errorf("recorded %q on the idle runner's lease, for a job it never ran", r.result)
		}
	}
}

// AN IDLE RUNNER GITHUB WILL NOT REMOVE IS RUNNING A JOB BILLET HAS NOT HEARD OF.
// Measured on 2026-10-20: the runner launched for a job cancelled before any
// runner took it was refused removal as "currently running a job", and the
// JobStarted naming its real job arrived four minutes later. Settled on the
// cancellation, or left retiring by the pool's scale-down in the same message,
// the runner refused that JobStarted and then its real result. Reinstated busy
// with its job unknown, the JobStarted binds it and the real completion settles
// it, with GitHub's statistics in every message as GitHub sends them.
func TestAnIdleRunnerGitHubWillNotRemoveIsKeptForTheJobItIsRunning(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11, job12)
	swapped := p.launchedFor11(t)
	p.registry.busy[swapped.RunnerName] = true

	// Job 12 runs on the runner launched for 11, whose JobStarted is late; 11 is
	// cancelled, so one runner is surplus and the oldest idle one is chosen.
	canceled := job11
	canceled.Result = "Canceled"
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}
	if !slices.Contains(p.registry.names, swapped.RunnerName) {
		t.Fatalf("removed %v: the pool did not choose the surplus runner launched for 11", p.registry.names)
	}
	if slices.Contains(p.destroyed, 11) {
		t.Fatalf("destroyed %v: a runner GitHub refused to remove was torn down", p.destroyed)
	}
	kept, err := p.a.PoolRunnerByLease(t.Context(), swapped.LeaseID)
	if err != nil || kept.Status != alloc.PoolRunnerBusy || kept.JobID != "" {
		t.Fatalf("the refused runner is %+v (%v), want busy with its job unknown", kept, err)
	}

	started := job12
	started.RunnerID, started.RunnerName = 4402, swapped.RunnerName
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("the delayed JobStarted was refused: %v", err)
	}
	bound, err := p.a.PoolRunnerByLease(t.Context(), swapped.LeaseID)
	if err != nil || bound.Status != alloc.PoolRunnerBusy || bound.JobID != "job-12" || bound.RunID != 102 {
		t.Fatalf("after the delayed JobStarted the runner is %+v (%v), want busy with job 12 of run 102", bound, err)
	}

	delete(p.registry.busy, swapped.RunnerName)
	succeeded := started
	succeeded.Result = "Succeeded"
	if err := p.l.handle(t.Context(), &Message{MessageID: 4, Completed: []dispatch.Job{succeeded},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
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

// A RETIRING LAUNCH RUNNER IS NOT SETTLED BY A CANCELLATION EITHER: the pool
// journaled it for retirement, its job is not this one, and its retirement is
// the pool's to finish.
func TestARunnerlessCancellationDoesNotSettleARetiringLaunchRunner(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	retiring := p.launchedFor11(t)
	if err := p.a.RetirePoolRunner(t.Context(), retiring.LeaseID); err != nil {
		t.Fatalf("journal the runner retiring: %v", err)
	}

	canceled := job11
	canceled.Result = "Canceled"
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled}}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}
	for _, r := range p.results {
		if r.lease == retiring.LeaseID {
			t.Errorf("recorded %q on the retiring runner's lease, for a job it never ran", r.result)
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

// A MEMBER REINSTATED BUSY WHOSE JOB NEVER NAMES ITSELF IS ASKED ABOUT AGAIN, so a
// JobStarted and a completion that were both lost do not hold its capacity for
// ever. Not before the recheck: the JobStarted GitHub promised gets its time.
// Then GitHub's answer decides, as before reinstatement: refused again, it is
// reinstated again; removed, it is torn down and its capacity released.
func TestAReinstatedMemberWhoseJobNeverNamesItselfIsAskedAgain(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	member := p.launchedFor11(t)
	p.registry.busy[member.RunnerName] = true
	now := time.Date(2026, 10, 20, 4, 43, 20, 0, time.UTC)
	p.l.now = func() time.Time { return now }

	reconcile := func(at time.Time) {
		t.Helper()
		now = at
		if err := p.l.reconcilePool(t.Context(), 0); err != nil {
			t.Fatalf("reconcile at %s: %v", at, err)
		}
	}
	asked := func() int {
		n := 0
		for _, name := range p.registry.names {
			if name == member.RunnerName {
				n++
			}
		}
		return n
	}

	start := now
	reconcile(start)
	if got, err := p.a.PoolRunnerByLease(t.Context(), member.LeaseID); err != nil || got.Status != alloc.PoolRunnerBusy {
		t.Fatalf("the refused member is %+v (%v), want reinstated busy", got, err)
	}
	if asked() != 1 {
		t.Fatalf("asked GitHub to remove it %d times, want once", asked())
	}

	reconcile(start.Add(reinstatedRecheck - time.Second))
	if asked() != 1 {
		t.Errorf("asked again %d times before the recheck, want not yet", asked()-1)
	}

	reconcile(start.Add(reinstatedRecheck))
	if asked() != 2 || slices.Contains(p.destroyed, 11) {
		t.Fatalf("at the recheck asked %d times and destroyed %v; want a second request and nothing torn down",
			asked(), p.destroyed)
	}
	if got, err := p.a.PoolRunnerByLease(t.Context(), member.LeaseID); err != nil || got.Status != alloc.PoolRunnerBusy {
		t.Fatalf("refused again, the member is %+v (%v), want reinstated busy again", got, err)
	}

	delete(p.registry.busy, member.RunnerName)
	reconcile(start.Add(2*reinstatedRecheck + time.Second))
	if asked() != 3 || !slices.Contains(p.destroyed, 11) {
		t.Errorf("once GitHub would remove it, asked %d times and destroyed %v; want it removed and torn down",
			asked(), p.destroyed)
	}
	if _, err := p.a.PoolRunnerByLease(t.Context(), member.LeaseID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("the removed member is still journaled (%v)", err)
	}
}

// A CANCELLATION THAT NAMES ITS RUNNER BY ID IS NOT RUNNER-LESS: only a
// completion naming no runner at all is a job none took, so one naming an id
// still settles its launch runner as before.
func TestACancellationNamingOnlyARunnerIDStillSettles(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	own := p.launchedFor11(t)

	canceled := job11
	canceled.Result, canceled.RunnerID = "Canceled", 77
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled}}); err != nil {
		t.Fatalf("the cancellation was refused: %v", err)
	}
	if !slices.Contains(p.results, recordedResult{own.LeaseID, "Canceled"}) || !slices.Contains(p.destroyed, 11) {
		t.Errorf("recorded %v and destroyed %v; want the cancellation settled on its runner", p.results, p.destroyed)
	}
}

// A RUNNER RECOVERED BUSY IS LEFT TO RECOVERY, never asked about by the pool's
// recheck: recovery inspects it at GitHub on the node's adoption, and the
// retirement the recheck uses destroys by request, which after a restart cannot
// tell an unreachable holder from no compute at all.
func TestARunnerRecoveredBusyIsLeftToRecovery(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	recovered := p.launchedFor11(t)
	recovered.RunnerID = 77
	if err := p.a.PreserveRecoveredBusyPoolRunner(t.Context(), recovered); err != nil {
		t.Fatalf("recover the runner as busy: %v", err)
	}
	// FROM THE WALL CLOCK, which the fixture's own first pass ran on, so no time
	// it may have kept lies in this clock's future.
	now := time.Now()
	p.l.now = func() time.Time { return now }

	for _, at := range []time.Time{now, now.Add(reinstatedRecheck), now.Add(3 * reinstatedRecheck)} {
		now = at
		if err := p.l.reconcilePool(t.Context(), 1); err != nil {
			t.Fatalf("reconcile at %s: %v", at, err)
		}
	}
	if slices.Contains(p.registry.names, recovered.RunnerName) || slices.Contains(p.destroyed, 11) {
		t.Errorf("removed %v and destroyed %v: the pool's recheck took a runner recovery holds",
			p.registry.names, p.destroyed)
	}
}

// A WATCHED MEMBER WHOSE ROW SETTLEMENT REMOVED, not the pool's own retirement,
// leaves the watch on the next pass rather than staying in it for the life of
// the listener.
func TestAWatchedMemberWhoseRowIsGoneLeavesTheWatch(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	member := p.launchedFor11(t)
	p.registry.busy[member.RunnerName] = true
	if err := p.l.reconcilePool(t.Context(), 0); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	watched := func() bool {
		p.l.mu.Lock()
		defer p.l.mu.Unlock()
		_, ok := p.l.reinstated[member.LeaseID]
		return ok
	}
	if !watched() {
		t.Fatal("a member reinstated busy is not watched")
	}

	if err := p.a.ForgetPoolRunner(t.Context(), member.LeaseID); err != nil {
		t.Fatalf("forget the settled member's row: %v", err)
	}
	if err := p.l.reconcilePool(t.Context(), 0); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if watched() {
		t.Error("a member whose row is gone is still watched")
	}
}

// A COMPLETION NAMING THE ID OF A RUNNER BOUND TO ANOTHER JOB IS NOT ITS: GitHub
// contradicting its own binding settles nothing on the runner running that job,
// which stays to report its own result.
func TestACompletionNamingTheIDOfARunnerBoundToAnotherJobSettlesNothing(t *testing.T) {
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
	canceled.Result, canceled.RunnerID = "Canceled", 77
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{canceled}}); err != nil {
		t.Fatalf("the completion was refused: %v", err)
	}
	for _, r := range p.results {
		if r.lease == swapped.LeaseID {
			t.Errorf("recorded %q on the lease of the runner running job 12", r.result)
		}
	}
	if slices.Contains(p.destroyed, 11) || slices.Contains(p.registry.names, swapped.RunnerName) {
		t.Errorf("destroyed %v and removed %v: the runner running job 12 was torn down", p.destroyed,
			p.registry.names)
	}
}

// A COMPLETION NAMING THE RUNNER'S OWN ID IS THAT RUNNER'S, though it names no
// runner by name and the runner is busy with its job unknown: it settles it, its
// lease restored from the runner's binding as a named completion's is, rather
// than being detached and its result dropped, after a restart too.
func TestACompletionNamingAPlaceholdersOwnRunnerIDSettlesIt(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	recovered := p.launchedFor11(t)
	recovered.RunnerID = 77
	if err := p.a.PreserveRecoveredBusyPoolRunner(t.Context(), recovered); err != nil {
		t.Fatalf("recover the runner as busy: %v", err)
	}
	p.restart()

	succeeded := job11
	succeeded.Result, succeeded.RunnerID = "Succeeded", 77
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{succeeded}}); err != nil {
		t.Fatalf("the completion was refused: %v", err)
	}
	if !slices.Contains(p.results, recordedResult{recovered.LeaseID, "Succeeded"}) || !slices.Contains(p.destroyed, 11) {
		t.Errorf("recorded %v and destroyed %v; want the runner's own completion settled", p.results, p.destroyed)
	}
}
