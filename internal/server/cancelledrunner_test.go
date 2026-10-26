package server

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

// cancelPool is a tier that has launched a pool runner for each assigned job,
// against a registry that answers GitHub's per-runner record.
type cancelPool struct {
	a         *alloc.Allocator
	l         *Listener
	reg       *inspectingRegistry
	destroyed []int64
}

func newCancelPool(t *testing.T, reg *inspectingRegistry, assigned ...dispatch.Job) *cancelPool {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	p := &cancelPool{a: newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers), reg: reg}
	if reg.busy == nil {
		reg.busy = map[string]bool{}
	}
	p.l = NewListener(p.a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onDestroy: func(requestID int64) error {
			p.destroyed = append(p.destroyed, requestID)

			return nil
		},
	}), WithRunnerRegistry(reg))
	p.l.writeJobResult = func(context.Context, string, string, int64) error { return nil }

	if err := p.l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refill escrow: %v", err)
	}
	if err := p.l.handle(t.Context(), &Message{MessageID: 1, Assigned: assigned,
		Statistics: &Statistics{TotalAssignedJobs: len(assigned)}}); err != nil {
		t.Fatalf("launch the pool: %v", err)
	}

	return p
}

func (p *cancelPool) launchedFor(t *testing.T, request int64) alloc.PoolRunner {
	t.Helper()

	runner, err := p.a.PoolRunnerLaunchedFor(t.Context(), p.l.tier, request)
	if err != nil {
		t.Fatalf("the runner launched for %d: %v", request, err)
	}

	return runner
}

// cancel delivers a runner-less cancellation of job, with GitHub still counting
// assigned jobs waiting, so the runner is not the pool's surplus and only the
// cancellation can free it. A negative count sends no statistics, so the pool
// does not reconcile in that message at all.
func (p *cancelPool) cancel(t *testing.T, job dispatch.Job, stillAssigned int) {
	t.Helper()

	canceled := job
	canceled.Result = "Canceled"
	var stats *Statistics
	if stillAssigned >= 0 {
		stats = &Statistics{TotalAssignedJobs: stillAssigned}
	}
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{canceled},
		Statistics: stats}); err != nil {
		t.Fatalf("the runner-less cancellation was refused: %v", err)
	}
}

func (p *cancelPool) status(t *testing.T, lease string) string {
	t.Helper()

	member, err := p.a.PoolRunnerByLease(t.Context(), lease)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return "forgotten"
	}
	if err != nil {
		t.Fatalf("PoolRunnerByLease: %v", err)
	}

	return member.Status
}

// THE RUNNER LAUNCHED FOR A CANCELLED JOB IS WITHDRAWN AT ONCE, though jobs
// still wait: it was that job's, and left in the pool it held its lease until
// the offline watch reached it 20 minutes later, while the jobs waiting were
// refused the room it held (#690). GitHub's acknowledged DELETE of its exact id
// is the permission, as it always was.
func TestTheRunnerLaunchedForACancelledJobIsWithdrawnAtOnce(t *testing.T) {
	t.Parallel()

	// WITH STATISTICS AND WITHOUT: a message without them does not reconcile the
	// pool, so only the cancellation itself can retire the runner there.
	for name, stillAssigned := range map[string]int{"with statistics": 5, "without statistics": -1} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			withdrawnAtOnce(t, stillAssigned)
		})
	}
}

func withdrawnAtOnce(t *testing.T, stillAssigned int) {
	t.Helper()

	reg := &inspectingRegistry{githubIDs: map[string]int64{}, byName: map[string][]RunnerState{}}
	p := newCancelPool(t, reg, job11, job12)
	cancelled, other := p.launchedFor(t, 11), p.launchedFor(t, 12)
	reg.githubIDs[cancelled.RunnerName] = 4411
	reg.byName[cancelled.RunnerName] = []RunnerState{{Present: true}}

	p.cancel(t, job11, stillAssigned)

	if !slices.Equal(reg.withdrawn, []int64{4411}) {
		t.Fatalf("withdrawn ids = %v, want exactly the cancelled job's runner 4411", reg.withdrawn)
	}
	if !slices.Equal(reg.askedFor, []string{cancelled.RunnerName}) {
		t.Errorf("GitHub was asked about %v, want only the cancelled job's runner", reg.askedFor)
	}
	if !slices.Contains(p.destroyed, 11) || slices.Contains(p.destroyed, 12) {
		t.Errorf("destroyed %v, want the cancelled job's runner torn down and the other kept", p.destroyed)
	}
	if got := p.status(t, cancelled.LeaseID); got == alloc.PoolRunnerIdle {
		t.Errorf("the cancelled job's runner is still idle in the pool")
	}
	if got := p.status(t, other.LeaseID); got != alloc.PoolRunnerIdle {
		t.Errorf("the other job's runner is %q, want idle and untouched", got)
	}
}

// A RUNNER GITHUB REPORTS BUSY, OR WILL NOT DELETE, TOOK ANOTHER JOB whose
// JobStarted has not arrived, and is kept for it: the cancellation frees only a
// runner that has no work.
func TestACancelledJobsRunnerThatTookAnotherJobIsKept(t *testing.T) {
	t.Parallel()

	for name, script := range map[string]struct {
		state       RunnerState
		withdrawErr error
		asked       bool
	}{
		"reported busy":    {state: RunnerState{Present: true, Online: true, Busy: true}},
		"deletion refused": {state: RunnerState{Present: true, Online: true}, withdrawErr: ErrRunnerBusy, asked: true},
		"absent":           {state: RunnerState{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reg := &inspectingRegistry{githubIDs: map[string]int64{}, byName: map[string][]RunnerState{},
				withdrawErr: script.withdrawErr}
			p := newCancelPool(t, reg, job11, job12)
			cancelled := p.launchedFor(t, 11)
			if script.state.Present {
				reg.githubIDs[cancelled.RunnerName] = 4411
			}
			reg.byName[cancelled.RunnerName] = []RunnerState{script.state}

			p.cancel(t, job11, 5)

			if len(reg.askedFor) != 1 {
				t.Fatalf("GitHub was asked about %v, want the cancelled job's runner once", reg.askedFor)
			}
			if asked := len(reg.withdrawn) > 0; asked != script.asked {
				t.Errorf("withdrawn ids = %v, want a deletion asked %v", reg.withdrawn, script.asked)
			}
			if slices.Contains(p.destroyed, 11) {
				t.Errorf("destroyed %v: a runner that may be running a job was torn down", p.destroyed)
			}
			if got := p.status(t, cancelled.LeaseID); got != alloc.PoolRunnerIdle {
				t.Errorf("the kept runner is %q, want idle, so a late JobStarted still binds it", got)
			}
		})
	}
}

// A WITHDRAWAL BEFORE THE OFFLINE WATCH FIRST RAN SURVIVES A FAILED CLAIM: GitHub
// has deleted the runner, its acknowledgement is the only evidence, and the
// watch's first run must not forget it, or the member stays idle with no
// registration behind it.
func TestAWithdrawalBeforeTheWatchRanSurvivesAFailedClaim(t *testing.T) {
	t.Parallel()

	reg := &inspectingRegistry{githubIDs: map[string]int64{}, byName: map[string][]RunnerState{}}
	p := newCancelPool(t, reg, job11, job12)
	cancelled := p.launchedFor(t, 11)
	reg.githubIDs[cancelled.RunnerName] = 4411
	reg.byName[cancelled.RunnerName] = []RunnerState{{Present: true}}

	// A fresh listener over the same ledger, as a restart leaves it: its offline
	// watch has never run.
	p.l = NewListener(p.a, p.l.tier, &fakeSession{}, WithRunner(p.l.runner), WithRunnerRegistry(reg))
	p.l.writeJobResult = func(context.Context, string, string, int64) error { return nil }
	failures := 1
	p.l.claimRetirement = func(ctx context.Context, lease string) error {
		if failures > 0 {
			failures--

			return errors.New("the ledger did not answer")
		}

		return p.a.RetirePoolRunner(ctx, lease)
	}

	p.cancel(t, job11, -1)
	if got := p.status(t, cancelled.LeaseID); got != alloc.PoolRunnerIdle {
		t.Fatalf("after a failed claim the member is %q, want idle until the claim is retried", got)
	}

	if err := p.l.reconcilePool(t.Context(), 5); err != nil {
		t.Fatalf("reconcilePool: %v", err)
	}
	if !slices.Equal(reg.withdrawn, []int64{4411}) {
		t.Errorf("withdrawn ids = %v, want the one deletion, not asked again", reg.withdrawn)
	}
	if got := p.status(t, cancelled.LeaseID); got == alloc.PoolRunnerIdle {
		t.Errorf("the withdrawn member is still idle after the watch's first run")
	}
}

// A BURST PAST ONE MESSAGE'S BUDGET LEAVES THE REST TO THE OFFLINE WATCH, and no
// withdrawal begun near the end runs past it: the withdrawals run on the message
// path, and a slow GitHub must not hold every other completion behind them.
//
// IN A BUBBLE, because the deadlines are the thing under test. At 14 seconds a
// lookup, two answer by 28 seconds, a third begun then is cut at the batch's 30
// (fresh per-withdrawal deadlines with only a start check ran it to 42), and a
// fourth is never begun. At 25 seconds a lookup, the first is cut at its own 20
// and the second at the batch's 30, so none is withdrawn.
func TestABurstOfCancellationsStopsAtItsBudget(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		lookup           time.Duration
		asked, withdrawn int
	}{
		"lookups inside their own bound": {lookup: 14 * time.Second, asked: 3, withdrawn: 2},
		"lookups past their own bound":   {lookup: 25 * time.Second, asked: 2, withdrawn: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				burstWithin(t, tc.lookup, tc.asked, tc.withdrawn)
			})
		})
	}
}

func burstWithin(t *testing.T, lookup time.Duration, asked, withdrawn int) {
	t.Helper()

	jobs := []dispatch.Job{job11, job12, job13, {RequestID: 14, RunID: 104, JobID: "job-14"}}
	reg := &inspectingRegistry{githubIDs: map[string]int64{}, byName: map[string][]RunnerState{},
		lookup: lookup}
	p := newCancelPool(t, reg, jobs...)
	for i := range jobs {
		runner := p.launchedFor(t, jobs[i].RequestID)
		reg.githubIDs[runner.RunnerName] = 4400 + jobs[i].RequestID
		reg.byName[runner.RunnerName] = []RunnerState{{Present: true}}
	}

	cancelled := make([]dispatch.Job, 0, len(jobs))
	for i := range jobs {
		job := jobs[i]
		job.Result = "Canceled"
		cancelled = append(cancelled, job)
	}

	began := time.Now()
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: cancelled}); err != nil {
		t.Fatalf("the cancellations were refused: %v", err)
	}

	if took := time.Since(began); took > cancelledWithdrawBudget {
		t.Errorf("the message took %s, past its %s withdrawal budget", took, cancelledWithdrawBudget)
	}
	if len(reg.askedFor) != asked || len(reg.withdrawn) != withdrawn {
		t.Errorf("asked about %d runners and withdrew %v, want %d asked and %d withdrawn",
			len(reg.askedFor), reg.withdrawn, asked, withdrawn)
	}
}
