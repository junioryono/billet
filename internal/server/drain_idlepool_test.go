package server

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// idleLeft is the drain ending with pool slots that never started a job still
// registered, as opposed to everything having finished.
const idleLeft = "only pool runners that never started a job remain"

// joinOnCleanup ends a listener's drain and waits for Run before the allocator's
// database closes, on every path out of the test. The channel goes to
// WithHurrySignal.
func joinOnCleanup(t *testing.T) (chan struct{}, func(*runResult)) {
	t.Helper()

	hurry := make(chan struct{})

	var once sync.Once

	return hurry, func(run *runResult) {
		t.Cleanup(func() {
			once.Do(func() { close(hurry) })

			select {
			case <-run.done:
			case <-time.After(30 * time.Second):
				t.Error("Run did not return after the hurry signal")
			}
		})
	}
}

// poolStatus is the ledger's status for the slot on lease, or "" when it cannot
// be read.
func poolStatus(ctx context.Context, a *alloc.Allocator, lease string) string {
	member, err := a.PoolRunnerByLease(ctx, lease)
	if err != nil {
		return ""
	}

	return member.Status
}

// A DRAIN DOES NOT WAIT FOR A POOL SLOT THAT NEVER STARTED A JOB.
//
// GitHub's assigned count stays above such a slot while the jobs it counts are
// declined rather than withdrawn, so the pool's scale-down never retires it, and
// GitHub hands it no job while its tier drains. The drain waits for the busy slot,
// then ends with the idle one left registered and charged for the next control
// plane to adopt.
func TestADrainEndsWhenOnlyIdlePoolSlotsRemain(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		step        atomic.Int32
		busy        atomic.Pointer[alloc.PoolRunner]
		finish      = make(chan struct{})
		completedAt atomic.Int64
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		slowPoll()

		switch step.Load() {
		case 0:
			step.Store(1)

			// TWO ANONYMOUS POOL SLOTS, grown from GitHub's count alone.
			return &Message{MessageID: 1, Statistics: &Statistics{TotalAssignedJobs: 2}}, nil
		case 1:
			members, err := a.PoolRunners(t.Context(), "billet-4vcpu-a")
			if err != nil || len(members) != 2 {
				return nil, ErrNoMessage
			}

			busy.Store(&members[0])
			step.Store(2)

			started := job12
			started.RunnerID, started.RunnerName = 77, members[0].RunnerName

			// THE ASSIGNMENT FINDS NO ESCROW AND IS DECLINED, and GitHub starts
			// the job on an idle slot.
			return &Message{MessageID: 2, Assigned: []Job{job12}, Started: []Job{started},
				Statistics: &Statistics{TotalAssignedJobs: 2}}, nil
		case 2:
			select {
			case <-finish:
			default:
				return nil, ErrNoMessage
			}

			step.Store(3)
			completedAt.Store(int64(session.polls()))

			done := job12
			done.RunnerID, done.RunnerName, done.Result = 77, busy.Load().RunnerName, "Succeeded"

			// STILL ONE ASSIGNED, so the idle slot is never surplus.
			return &Message{MessageID: 3, Completed: []Job{done},
				Statistics: &Statistics{TotalAssignedJobs: 1}}, nil
		default:
			return nil, ErrNoMessage
		}
	}

	var (
		mu        sync.Mutex
		destroyed []int64
	)

	runner := &fakeRunner{onDestroy: func(requestID int64) error {
		mu.Lock()
		defer mu.Unlock()

		destroyed = append(destroyed, requestID)

		return nil
	}}
	registry := &fakeRunnerRegistry{}

	var dl drainLog

	hurry, join := joinOnCleanup(t)

	l := NewListener(a, "billet-4vcpu-a", session, WithRunner(runner),
		WithRunnerRegistry(registry), WithDrainGrace(20*time.Second),
		WithHurrySignal(hurry), dl.option())

	run := startRun(ctx, l)
	join(run)

	// COMMITTED, not merely delivered: the Started is bound in the ledger.
	waitUntil(deadline, t, "job 12 to be bound to its pool slot", func() bool {
		b := busy.Load()

		return b != nil && poolStatus(t.Context(), a, b.LeaseID) == alloc.PoolRunnerBusy
	})

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	began := session.polls()
	waitUntil(deadline, t, "the drain to poll while job 12 runs", func() bool {
		return session.polls() >= began+5 || run.has()
	})

	if run.has() {
		t.Fatalf("the drain ended while a pool slot was still running job 12:\n%s", dl.String())
	}

	close(finish)

	select {
	case <-run.done:
	case <-deadline.Done():
		t.Fatalf("the drain never ended once only an idle pool slot remained:\n%s", dl.String())
	}

	if !dl.saw(idleLeft) {
		t.Errorf("the drain did not say it left an idle pool slot behind:\n%s", dl.String())
	}

	if dl.saw(stoppedWaiting) {
		t.Errorf("the drain had to be told to stop waiting:\n%s", dl.String())
	}

	// A Started for the idle slot could be in the message after the completion.
	if polls := session.polls(); int64(polls) <= completedAt.Load() {
		t.Errorf("the drain ended at poll %d without polling again after the completion "+
			"delivered at poll %d", polls, completedAt.Load())
	}

	members, err := a.PoolRunners(t.Context(), "billet-4vcpu-a")
	if err != nil {
		t.Fatalf("read the pool: %v", err)
	}

	var idle alloc.PoolRunner
	for _, m := range members {
		if m.LeaseID != busy.Load().LeaseID {
			idle = m
		}
	}

	if idle.LeaseID == "" || idle.Status != alloc.PoolRunnerIdle {
		t.Fatalf("pool after the drain = %+v; want the idle slot still journalled", members)
	}

	mu.Lock()
	defer mu.Unlock()

	if slices.Contains(destroyed, idle.LaunchRequestID) {
		t.Errorf("destroyed %v: the idle slot was torn down instead of left for the next control plane",
			destroyed)
	}

	if slices.Contains(registry.names, idle.RunnerName) {
		t.Errorf("removed %v: the idle slot's registration was removed", registry.names)
	}

	lease, err := a.Lease(t.Context(), idle.LeaseID)
	if err != nil {
		t.Fatalf("the idle slot's lease: %v", err)
	}

	if lease.Phase.Terminal() {
		t.Errorf("the idle slot's lease is %s; its compute is still up, so it must stay charged",
			lease.Phase)
	}
}

// A RUNNER LAUNCHED FOR AN ASSIGNED JOB IS WAITED FOR before its Started arrives:
// the idle-slot rule is for anonymous slots alone.
func TestADrainStillWaitsForARunnerLaunchedForAnAssignedJob(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var assigned atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		slowPoll()

		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{job11},
				Statistics: &Statistics{TotalAssignedJobs: 1}}, nil
		}

		return nil, ErrNoMessage
	}

	var dl drainLog

	hurry, join := joinOnCleanup(t)

	l := NewListener(a, "billet-4vcpu-a", session, WithRunner(&fakeRunner{}),
		WithDrainGrace(20*time.Second), WithHurrySignal(hurry), dl.option())

	run := startRun(ctx, l)
	join(run)

	// THE SLOT IS JOURNALLED IDLE, so only its assignment identity keeps the
	// drain waiting.
	waitUntil(deadline, t, "the runner for job 11 to be journalled idle", func() bool {
		member, err := a.PoolRunnerLaunchedFor(t.Context(), "billet-4vcpu-a", 11)

		return err == nil && member.Status == alloc.PoolRunnerIdle
	})

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	began := session.polls()
	waitUntil(deadline, t, "the drain to poll several times", func() bool {
		return session.polls() >= began+5 || run.has()
	})

	if run.has() || dl.saw(idleLeft) {
		t.Fatalf("the drain ended on a runner launched for assigned job 11 before it finished:\n%s",
			dl.String())
	}
}
