package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
)

const testHold = time.Hour

// heldPool is a pool whose tier holds failed jobs for testHold, its runner running job 11, and the
// completion that ends that job with result.
func heldPool(t *testing.T, result string) (*runnerlessPool, alloc.PoolRunner, dispatch.Job) {
	t.Helper()

	p := newRunnerlessPool(t, job11)
	p.l.holdOnFailure = testHold
	// THE REAL RECORD OF A COMPLETION, which is what a restart finds it by.
	p.l.completionStore = openState(t)

	runner := p.launchedFor11(t)

	started := job11
	started.RunnerID, started.RunnerName = 77, runner.RunnerName

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11: %v", err)
	}

	finished := started
	finished.Result = result

	return p, runner, finished
}

// complete delivers a job's completion. The handling of a message runs the pool's reconcile too,
// which retires a member whose job ended, so this reaches every way a held VM could be destroyed.
func (p *runnerlessPool) complete(t *testing.T, job dispatch.Job) {
	t.Helper()

	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{job},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("complete the job: %v", err)
	}
}

// restartHeld replaces the listener as a restart does, keeping the durable record of its completions,
// and restores them as Run does before anything else.
func (p *runnerlessPool) restartHeld(t *testing.T, setting time.Duration) {
	t.Helper()

	store := p.l.completionStore

	p.restart()
	p.l.holdOnFailure = setting
	p.l.completionStore = store

	if err := p.l.restoreCompletions(t.Context()); err != nil {
		t.Fatalf("restore completions: %v", err)
	}
}

// holdMessages numbers the ordinary messages a test's passes deliver.
var holdMessages atomic.Int64

// aPass is everything that might destroy a held VM on an ordinary pass: the cleanup clock's retry,
// and another message's reconcile, which retires a member its completion marked.
func (p *runnerlessPool) aPass(t *testing.T) {
	t.Helper()

	p.l.retryCleanup(t.Context())

	if err := p.l.handle(t.Context(), &Message{MessageID: 100 + holdMessages.Add(1),
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("an ordinary message: %v", err)
	}
}

// A FAILED JOB'S VM IS HELD TO ITS BOUND AND NOT PAST IT. Nothing asks the runner to destroy it
// meanwhile, by the cleanup clock or by the pool's retirement, whose journal the completion wrote.
// Its registration is gone at once and only once, since GitHub must not route to a finished runner
// and a pass every few seconds must not spend the App's budget. Its capacity is still charged.
func TestAFailedJobsVMIsHeldToItsBound(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p, runner, failed := heldPool(t, "Failed")

		p.complete(t, failed)

		if len(p.destroyed) != 0 {
			t.Fatalf("destroyed %v at the completion", p.destroyed)
		}

		hold, err := p.a.LeaseHoldOf(t.Context(), runner.LeaseID)
		if err != nil || !hold.Holding() {
			t.Fatalf("the hold after the completion: %+v, %v", hold, err)
		}

		if want := time.Now().Add(testHold); !hold.Until.Equal(want) {
			t.Errorf("the hold runs until %s, want %s", hold.Until, want)
		}

		time.Sleep(testHold / 2)
		p.aPass(t)
		p.aPass(t)

		if len(p.destroyed) != 0 {
			t.Fatalf("destroyed %v inside the hold", p.destroyed)
		}

		removed := 0
		for _, name := range p.registry.names {
			if name == runner.RunnerName {
				removed++
			}
		}
		if removed != 1 {
			t.Errorf("the registration was removed %d times; want once however many passes ran", removed)
		}

		if p.l.Running() == 0 {
			t.Error("the held lease is no longer this listener's, so nothing renews it")
		}

		lease, err := p.a.Lease(t.Context(), runner.LeaseID)
		if err != nil {
			t.Fatalf("read the held lease: %v", err)
		}
		if lease.Phase == alloc.PhaseDone || lease.Phase == alloc.PhaseFailed {
			t.Errorf("a held lease is %s, so its capacity came back with its VM still running", lease.Phase)
		}

		time.Sleep(testHold/2 + time.Second)
		p.aPass(t)

		if !slices.Contains(p.destroyed, 11) {
			t.Errorf("destroyed %v past the bound", p.destroyed)
		}

		hold, err = p.a.LeaseHoldOf(t.Context(), runner.LeaseID)
		if err != nil || hold.Holding() || hold.EndedBy != alloc.HoldEndedByBound {
			t.Errorf("the hold past its bound: %+v, %v", hold, err)
		}
	})
}

// ONLY A FAILED JOB IS HELD, AND ONLY WHERE THE TIER SAYS. A job that passed or was cancelled goes
// as any other: nobody is waiting to look at it.
func TestOnlyAFailedJobIsHeld(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		result string
		hold   time.Duration
	}{
		"succeeded":       {"Succeeded", testHold},
		"canceled":        {"Canceled", testHold},
		"no hold in tier": {"Failed", 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, runner, job := heldPool(t, tc.result)
			p.l.holdOnFailure = tc.hold

			p.complete(t, job)

			if !slices.Contains(p.destroyed, 11) {
				t.Errorf("destroyed %v; want the VM gone at its completion", p.destroyed)
			}

			if _, err := p.a.LeaseHoldOf(t.Context(), runner.LeaseID); !errors.Is(err, alloc.ErrLeaseNotFound) {
				t.Errorf("a hold exists: %v", err)
			}
		})
	}
}

// A RESTART KEEPS A HOLD AND ITS BOUND. The row is what says when it ends, so neither the process
// starting again nor an edit taking the setting away can move it.
func TestAHoldSurvivesARestart(t *testing.T) {
	t.Parallel()

	for name, setting := range map[string]time.Duration{
		"the same setting": testHold,
		"a longer setting": 24 * time.Hour,
		"no setting":       0,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				p, runner, failed := heldPool(t, "Failed")
				p.complete(t, failed)

				before, err := p.a.LeaseHoldOf(t.Context(), runner.LeaseID)
				if err != nil {
					t.Fatalf("the hold: %v", err)
				}

				time.Sleep(testHold / 2)

				p.restartHeld(t, setting)

				// THE RESTORED RECORD IS WHAT CARRIES IT, with the lease it holds. Without one
				// the pool's retirement would be what destroyed it, later, and this would pass.
				p.l.mu.Lock()
				entry := p.l.cleanup[11]
				p.l.mu.Unlock()

				if entry == nil || entry.lease == nil || entry.lease.ID != runner.LeaseID || !entry.held {
					t.Fatalf("restored %+v for the held job", entry)
				}

				p.aPass(t)

				if len(p.destroyed) != 0 {
					t.Fatalf("destroyed %v inside the hold, after the restart", p.destroyed)
				}

				after, err := p.a.LeaseHoldOf(t.Context(), runner.LeaseID)
				if err != nil || !after.Until.Equal(before.Until) {
					t.Fatalf("the bound moved from %s to %s, %v", before.Until, after.Until, err)
				}

				time.Sleep(testHold/2 + time.Second)
				p.aPass(t)

				if !slices.Contains(p.destroyed, 11) {
					t.Errorf("destroyed %v past the bound, after the restart", p.destroyed)
				}
			})
		})
	}
}

// A RESTARTED LISTENER DESTROYS A HELD VM AT ITS BOUND WHILE ITS SESSION IS STILL REFUSED (#783).
// GitHub keeps a stopped control plane's session for about 90 s, and the completions a restart
// restores need none of it: their cleanup runs before the session opens, through Run itself.
func TestARestartDestroysAHeldVMAtItsBoundBeforeItsSessionOpens(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p, _, failed := heldPool(t, "Failed")
		p.complete(t, failed)

		store, runner := p.l.completionStore, p.l.runner

		time.Sleep(testHold - time.Minute)

		var opens atomic.Int64

		release := make(chan struct{})
		l := NewListener(p.a, p.l.tier, nil, WithRunner(runner), WithRunnerRegistry(p.registry),
			WithSessionReopen(func(ctx context.Context) (Session, error) {
				opens.Add(1)

				// REFUSED UNTIL THE TEST SAYS, as GitHub refuses the successor of a session
				// a stopped control plane left.
				select {
				case <-release:
					return p.session, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}))
		l.holdOnFailure, l.completionStore = testHold, store
		l.writeJobResult = func(context.Context, string, string, int64) error { return nil }

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- l.Run(ctx) }()

		synctest.Wait()

		if len(p.destroyed) != 0 {
			t.Fatalf("destroyed %v inside the hold, while the session waits", p.destroyed)
		}

		// PAST THE BOUND, with the session still refused, and long enough for the cleanup clock.
		time.Sleep(time.Minute + 30*time.Second)
		synctest.Wait()

		if opens.Load() != 1 {
			t.Fatalf("the session was asked for %d times, want one open still waiting", opens.Load())
		}

		if !slices.Contains(p.destroyed, 11) {
			t.Errorf("destroyed %v past the bound while the session was refused; the held VM waited for GitHub",
				p.destroyed)
		}

		close(release)
		cancel()

		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v", err)
		}
	})
}

// A LISTENER STOPPED WHILE ITS FIRST SESSION IS REFUSED RETURNS, closes nothing it never opened, and
// leaves its cleanup loop joined (#783).
func TestAListenerStoppedBeforeItsSessionOpensReturns(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p := newRunnerlessPool(t, job11)

		l := NewListener(p.a, p.l.tier, nil, WithRunner(p.l.runner), WithRunnerRegistry(p.registry),
			WithSessionReopen(func(ctx context.Context) (Session, error) {
				<-ctx.Done()

				return nil, ctx.Err()
			}))

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- l.Run(ctx) }()

		synctest.Wait()
		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want the cancellation", err)
		}
	})
}

// AN ENDED HOLD ENDS IT: whatever ended it, the next pass destroys, and what ended it stands.
func TestAnEndedHoldIsDestroyedOnTheNextPass(t *testing.T) {
	t.Parallel()

	for _, by := range []string{alloc.HoldEndedByOperator, alloc.HoldEndedBySession, alloc.HoldEndedByDrain} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()

			p, runner, failed := heldPool(t, "Failed")
			p.complete(t, failed)

			if len(p.destroyed) != 0 {
				t.Fatalf("destroyed %v at the completion", p.destroyed)
			}

			ended, err := p.a.EndLeaseHold(t.Context(), runner.LeaseID, by)
			if err != nil || !ended {
				t.Fatalf("end the hold: %t, %v", ended, err)
			}

			p.aPass(t)

			if !slices.Contains(p.destroyed, 11) {
				t.Errorf("destroyed %v after the hold ended", p.destroyed)
			}

			hold, err := p.a.LeaseHoldOf(t.Context(), runner.LeaseID)
			if err != nil || hold.EndedBy != by {
				t.Errorf("the ended hold: %+v, %v; want ended by %s", hold, err, by)
			}
		})
	}
}

// A HOLD DOES NOT OUTLIVE A WITHDRAWAL THAT FAILED: it is tried again on the next pass, and the VM
// stays held meanwhile.
func TestAHoldTriesItsWithdrawalAgain(t *testing.T) {
	t.Parallel()

	p, runner, failed := heldPool(t, "Failed")
	p.registry.err = errors.New("GitHub did not answer")

	p.complete(t, failed)

	if len(p.destroyed) != 0 {
		t.Fatalf("destroyed %v with its withdrawal failing", p.destroyed)
	}

	p.registry.err = nil
	p.registry.names = nil

	p.l.retryCleanup(t.Context())

	if !slices.Contains(p.registry.names, runner.RunnerName) {
		t.Errorf("removed %v; want the runner's withdrawal tried again", p.registry.names)
	}

	p.registry.names = nil
	p.l.retryCleanup(t.Context())

	if slices.Contains(p.registry.names, runner.RunnerName) {
		t.Errorf("removed %v again, after a withdrawal that landed", p.registry.names)
	}
}

// A SHUTDOWN LEAVES A HOLD ALONE, as it leaves a running job: it says nothing about whether anyone has
// finished looking, and a converge or an upgrade restarts the control plane all the time. Through the
// real teardown, which owes and destroys every other completion it finds.
func TestAShutdownLeavesAHeldVMAlone(t *testing.T) {
	t.Parallel()

	p, runner, failed := heldPool(t, "Failed")
	p.complete(t, failed)

	destroyed := p.l.destroyAll(t.Context(), false, nil)
	if destroyed[11] || slices.Contains(p.destroyed, 11) {
		t.Errorf("a shutdown destroyed %v, a held VM among them", p.destroyed)
	}

	hold, err := p.a.LeaseHoldOf(t.Context(), runner.LeaseID)
	if err != nil || !hold.Holding() {
		t.Errorf("after the shutdown the hold reads %+v, %v", hold, err)
	}

	// A FORCE IS AN OPERATOR ENDING WORK, which a hold gives way to.
	if destroyed := p.l.destroyAll(t.Context(), true, nil); !destroyed[11] {
		t.Errorf("a force destroyed %v, without the held VM", destroyed)
	}
}

// A HOLD THAT HAS ENDED IS DESTROYED BY ITS COMPLETION AND NOT BY A RETIREMENT THAT GOT THERE FIRST: the
// completion's destroy is bound to the node holding the VM and carries what a release needs, and a
// retirement's goes by request id. A message arriving before the cleanup clock is what would do it.
func TestAnEndedHoldIsDestroyedByItsCompletion(t *testing.T) {
	t.Parallel()

	p, runner, failed := heldPool(t, "Failed")
	p.complete(t, failed)

	if _, err := p.a.EndLeaseHold(t.Context(), runner.LeaseID, alloc.HoldEndedByOperator); err != nil {
		t.Fatal(err)
	}

	if err := p.l.handle(t.Context(), &Message{MessageID: 4,
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("an ordinary message: %v", err)
	}

	if slices.Contains(p.destroyed, 11) {
		t.Fatalf("a retirement destroyed %v ahead of the completion", p.destroyed)
	}

	fake, ok := p.l.runner.(*fakeRunner)
	if !ok {
		t.Fatalf("the runner is %T", p.l.runner)
	}

	var result string
	fake.onDestroyCompleted = func(requestID int64, got string) error {
		result = got
		p.destroyed = append(p.destroyed, requestID)

		return nil
	}

	p.l.retryCleanup(t.Context())

	if !strings.EqualFold(result, "failed") {
		t.Errorf("the destroy was told the result %q, so it did not come from the completion", result)
	}
}

// A JOB THAT FAILS DURING A DRAIN IS NOT HELD: someone wants this tier empty, and it is not what they
// are waiting for. A hold already open stands.
func TestNoHoldOpensDuringADrain(t *testing.T) {
	t.Parallel()

	p, runner, failed := heldPool(t, "Failed")

	p.l.mu.Lock()
	p.l.draining = true
	p.l.mu.Unlock()

	p.complete(t, failed)

	if !slices.Contains(p.destroyed, 11) {
		t.Errorf("destroyed %v; a job failing during a drain was kept", p.destroyed)
	}

	if _, err := p.a.LeaseHoldOf(t.Context(), runner.LeaseID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("a hold opened during a drain: %v", err)
	}
}

// A PROCESS STOPPED RIGHT AFTER IT STARTED LEAVES A HOLD ALONE, before any pass has looked at it: what
// keeps a shutdown off a held VM has to be there from the restore, or a quick restart costs the VM.
func TestAShutdownRightAfterARestartLeavesAHeldVMAlone(t *testing.T) {
	t.Parallel()

	p, _, failed := heldPool(t, "Failed")
	p.complete(t, failed)

	p.restartHeld(t, testHold)

	if destroyed := p.l.destroyAll(t.Context(), false, nil); destroyed[11] || slices.Contains(p.destroyed, 11) {
		t.Errorf("a shutdown before the first pass destroyed %v", p.destroyed)
	}
}

// EXACTLY ONE PARTY RENEWS A LEASE, and after a restart that is the node that adopted what survived. A
// restored hold's lease stays on its entry, out of what this listener's heartbeat renews.
func TestARestartDoesNotTakeAHoldsLeaseBack(t *testing.T) {
	t.Parallel()

	p, runner, failed := heldPool(t, "Failed")
	p.complete(t, failed)

	p.restartHeld(t, testHold)

	p.l.mu.Lock()
	defer p.l.mu.Unlock()

	if lease := p.l.running[11]; lease != nil {
		t.Errorf("the restored listener renews lease %s beside the node that adopted it", lease.ID)
	}

	if entry := p.l.cleanup[11]; entry == nil || entry.lease == nil || entry.lease.ID != runner.LeaseID {
		t.Errorf("restored %+v for the held job", entry)
	}
}
