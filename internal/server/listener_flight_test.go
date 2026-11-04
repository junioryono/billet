package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
)

// heldLaunches holds every launch open until release closes, as a busy node does.
type heldLaunches struct {
	fakeRunner

	release chan struct{}
	unblock func()
	// entered closes once the first launch has recorded its lease and run beforeHold, so
	// a test's message can name it and find its row.
	entered     chan struct{}
	markEntered func()
	// beforeHold runs inside a launch before it blocks, as the node's side of a launch
	// writes to the ledger before a launch returns.
	beforeHold func(lease *alloc.Lease, job dispatch.Job) error

	mu       sync.Mutex
	contexts []context.Context
	leases   []*alloc.Lease
	started  int
	inflight int
	most     int
}

func newHeldLaunches() *heldLaunches {
	release := make(chan struct{})
	entered := make(chan struct{})

	return &heldLaunches{
		release: release, unblock: sync.OnceFunc(func() { close(release) }),
		entered: entered, markEntered: sync.OnceFunc(func() { close(entered) }),
	}
}

func (r *heldLaunches) Launch(ctx context.Context, lease *alloc.Lease, job dispatch.Job) error {
	r.mu.Lock()
	r.contexts = append(r.contexts, ctx)
	r.started++
	r.inflight++
	r.most = max(r.most, r.inflight)
	r.leases = append(r.leases, lease)
	r.mu.Unlock()

	var err error
	if r.beforeHold != nil {
		err = r.beforeHold(lease, job)
	}

	r.markEntered()

	<-r.release

	r.mu.Lock()
	r.inflight--
	r.mu.Unlock()

	return err
}

func (r *heldLaunches) counts() (started, inflight, most int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.started, r.inflight, r.most
}

func (r *heldLaunches) firstLease() *alloc.Lease {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leases[0]
}

// flightAllocator is the allocator for a test in a bubble, with leases long enough that no
// heartbeat runs. It is what keeps such a test from deadlocking: SQLite backing off on a busy
// WAL sleeps with time.Sleep, which only moves when every goroutine is durably blocked, and
// a heartbeat in that sleep holds the listener's mutex, on which the loop then blocks, which
// is not durable. So time never moves and the sleep never ends. Only in a bubble: a
// deployment's sleep is a real one and ends. None of these tests is about renewal.
func flightAllocator(t *testing.T, tiers []config.Tier) *alloc.Allocator {
	t.Helper()

	return newAllocator(t, alloc.Limits{MaxVCPU: 8 * tierVCPU, MaxMemory: 256 * config.GiB}, tiers,
		alloc.WithLeaseTTL(24*time.Hour))
}

// maxPassesBeforeFirstPoll is how many times a listener reconciles before its first poll
// returns: once at startup and once just before it. Each can start a batch, if the last
// one has already failed, and nothing else may until the poll is answered.
const maxPassesBeforeFirstPoll = 2

// concurrentRegistry is a fakeRunnerRegistry two launches can retire members through at
// once, as the scale-set client it stands in for can.
type concurrentRegistry struct {
	fakeRunnerRegistry

	mu sync.Mutex
}

func (r *concurrentRegistry) RemoveRunner(ctx context.Context, id int64, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.fakeRunnerRegistry.RemoveRunner(ctx, id, name)
}

// runFlightListener runs a background-launching listener in the caller's bubble, and
// returns what stops it and what it returned.
func runFlightListener(t *testing.T, a *alloc.Allocator, label string, session Session,
	runner dispatch.Runner, opts ...Option,
) (stop func(), done <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	// THE SECOND SIGNAL TOO: a drain waits for as long as a job runs, so ending a test
	// whose job never finishes takes both.
	hurry := make(chan struct{})
	opts = append([]Option{
		WithRunner(runner), WithPoolLaunchBatch(2), WithBackgroundLaunches(), WithHurrySignal(hurry),
	}, opts...)
	l := NewListener(a, label, session, opts...)
	finished := make(chan error, 1)
	joined := make(chan struct{})

	go func() {
		defer close(joined)

		finished <- l.Run(ctx)
	}()

	// QUIET BEFORE EACH CANCELLATION, because a bubble can deadlock on one that lands inside a
	// ledger read. SQLite backing off on a busy WAL sleeps with time.Sleep, which only moves
	// when every goroutine is durably blocked, and database/sql's cancellation then waits for
	// that read on an RWMutex, which is not durable: time never moves, so the sleep never
	// ends. Measured about once in 1,500 runs of these tests, and only in a bubble: a
	// deployment's sleep is a real one and ends.
	quiet := func() {
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
	}

	stop = sync.OnceFunc(func() {
		quiet()
		cancel()
		quiet()
		close(hurry)
	})

	// ON EVERY WAY OUT, a failed assertion included: the listener must be gone before the
	// ledger it writes to is closed, and it may be waiting on a launch a test held open.
	t.Cleanup(func() {
		if held, ok := runner.(*heldLaunches); ok {
			held.unblock()
		}
		stop()
		<-joined
	})

	return stop, finished
}

// A SLOW LAUNCH DOES NOT STOP THE TIER HEARING FROM GITHUB. This is the stall itself:
// on 2026-11-04 one tier was silent for over five minutes behind 30 launches, while
// everything that had finished stayed booted and charged.
func TestALaunchInFlightDoesNotStopTheNextMessage(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		var acknowledged atomic.Int64
		var sent atomic.Bool

		session := &fakeSession{
			stats: &Statistics{TotalAssignedJobs: 2},
			onGet: func() (*Message, error) {
				if sent.CompareAndSwap(false, true) {
					return &Message{MessageID: 7, Statistics: &Statistics{TotalAssignedJobs: 2}}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
			onDelete: func(id int64) error {
				acknowledged.Store(id)

				return nil
			},
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		started, inflight, _ := runner.counts()
		if started != 2 || inflight != 2 {
			t.Fatalf("started %d with %d in flight, want both launches open", started, inflight)
		}
		if got := acknowledged.Load(); got != 7 {
			t.Fatalf("acknowledged message %d with launches held, want 7: the tier stopped hearing GitHub", got)
		}

		runner.unblock()
		stop()
		<-done
	})
}

// AN ENDED LAUNCH BUYS THE NEXT ONE AT ONCE, NOT A LONG POLL LATER. Buying only between
// polls was #346: four runners every one to three minutes with the room sitting free.
// The poll is never cancelled to do it.
func TestAnEndedLaunchBuysTheNextWithoutWaitingForThePoll(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		cancelled := atomic.Bool{}
		session := &fakeSession{
			stats:    &Statistics{TotalAssignedJobs: 5},
			longPoll: time.Hour,
			onPollCtx: func(ctx context.Context) error {
				context.AfterFunc(ctx, func() { cancelled.Store(true) })

				return nil
			},
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		started, _, _ := runner.counts()
		if started != 2 {
			t.Fatalf("started %d before any ended, want the batch of 2", started)
		}

		runner.unblock()
		synctest.Wait()

		started, _, most := runner.counts()
		if started != 5 {
			t.Fatalf("started %d with the one poll still out, want all 5", started)
		}
		if most > 2 {
			t.Fatalf("%d launches overlapped, want at most the batch of 2", most)
		}
		if polls := session.polls(); polls != 1 {
			t.Fatalf("polled %d times, want the one still out", polls)
		}
		if cancelled.Load() {
			t.Fatal("the poll was cancelled to buy the next lease, which leaves unknown what GitHub took from it")
		}

		stop()
		<-done
	})
}

// A START WAITS FOR THE LAUNCH OF THE RUNNER IT NAMES. Its pool row may not exist until
// that launch returns, and everything handling a start assumes it does.
func TestAStartWaitsForTheLaunchItNames(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		var acknowledged atomic.Bool
		var sent atomic.Bool

		session := &fakeSession{
			stats: &Statistics{TotalAssignedJobs: 1},
			onGet: func() (*Message, error) {
				if sent.CompareAndSwap(false, true) {
					<-runner.entered

					name := provider.InstanceName(runner.firstLease().ID)

					return &Message{
						MessageID:  9,
						Started:    []dispatch.Job{{RunnerName: name, RunnerID: 41, RunID: 3, JobID: "job-3"}},
						Statistics: &Statistics{TotalAssignedJobs: 1},
					}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
			onDelete: func(int64) error {
				acknowledged.Store(true)

				return nil
			},
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		if acknowledged.Load() {
			t.Fatal("a start was handled while the launch it names had not returned")
		}

		runner.unblock()
		synctest.Wait()

		if !acknowledged.Load() {
			t.Fatal("the start was never handled after its launch returned")
		}

		members, err := a.PoolRunners(t.Context(), tiers[0].Label)
		if err != nil {
			t.Fatalf("PoolRunners: %v", err)
		}
		if len(members) != 1 || members[0].Status != alloc.PoolRunnerBusy {
			t.Fatalf("pool %+v, want one member bound busy to the job that started on it", members)
		}

		stop()
		<-done
	})
}

// A COMPLETION THAT NAMES NO RUNNER WAITS TOO, for the launch only the resolver can find
// behind it: on GitHub.com its request id on the wire is zero. What the resolver decides
// about it reads the launch's pool row and lease, which are not settled until it returns.
func TestARunnerlessCompletionWaitsForTheLaunchBehindIt(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		var (
			mu           sync.Mutex
			acknowledged []int64
			delivered    int
		)

		job := dispatch.Job{RunID: 6, JobID: "job-61"}
		cancelled := job
		cancelled.Result = "canceled"

		session := &fakeSession{
			onGet: func() (*Message, error) {
				mu.Lock()
				delivered++
				n := delivered
				mu.Unlock()

				switch n {
				case 1:
					return &Message{MessageID: 1, Assigned: []dispatch.Job{job},
						Statistics: &Statistics{TotalAssignedJobs: 1}}, nil
				case 2:
					return &Message{MessageID: 2, Completed: []dispatch.Job{cancelled},
						Statistics: &Statistics{TotalAssignedJobs: 0}}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
			onDelete: func(id int64) error {
				mu.Lock()
				defer mu.Unlock()

				acknowledged = append(acknowledged, id)

				return nil
			},
		}
		acked := func(id int64) bool {
			mu.Lock()
			defer mu.Unlock()

			return slices.Contains(acknowledged, id)
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		if !acked(1) {
			t.Fatal("the assignment was not handled, so this proves nothing about its completion")
		}
		if acked(2) {
			t.Fatal("a completion was handled while the launch behind it had not returned")
		}

		runner.unblock()
		synctest.Wait()

		if !acked(2) {
			t.Fatal("the completion was never handled after its launch returned")
		}

		stop()
		<-done
	})
}

// NOR IS IT AN OFFLINE MEMBER. A runner whose launch is still out has not had the chance to
// come online, however long that takes, and withdrawing it would tear down what the launch
// is bringing up.
func TestALaunchInFlightIsNotInspectedAsOffline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		runner.beforeHold = func(lease *alloc.Lease, job dispatch.Job) error {
			return a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: lease.ID, Tier: tiers[0].Label,
				LaunchRequestID: job.RequestID, RunnerName: provider.InstanceName(lease.ID)})
		}

		session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 1}, longPoll: time.Minute}
		registry := &inspectingRegistry{}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner, WithRunnerRegistry(registry))

		// PAST THE IDLE TIME THAT WOULD HAVE IT INSPECTED, a poll at a time.
		time.Sleep(offlineIdleAfter + 5*time.Minute)
		synctest.Wait()

		if registry.asked != 0 {
			t.Fatalf("asked GitHub about %v with their launches still out, want none", registry.askedFor)
		}

		runner.unblock()
		stop()
		<-done
	})
}

// A MEMBER WHOSE LAUNCH HAS NOT RETURNED IS NOT SURPLUS. Its row can exist first, and
// retiring it would tear down compute the launch is still bringing up.
func TestALaunchInFlightIsNotRetiredAsSurplus(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		runner.beforeHold = func(lease *alloc.Lease, job dispatch.Job) error {
			return a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: lease.ID, Tier: tiers[0].Label,
				LaunchRequestID: job.RequestID, RunnerName: provider.InstanceName(lease.ID)})
		}

		destroyed := atomic.Int64{}
		runner.onDestroy = func(int64) error {
			destroyed.Add(1)

			return nil
		}

		var sent atomic.Bool

		session := &fakeSession{
			stats: &Statistics{TotalAssignedJobs: 1},
			onGet: func() (*Message, error) {
				if sent.CompareAndSwap(false, true) {
					// ITS ROW EXISTS FIRST, or this would not reach the guard it is here for.
					<-runner.entered

					return &Message{MessageID: 2, Statistics: &Statistics{TotalAssignedJobs: 0}}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
		}

		registry := &concurrentRegistry{}
		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner, WithRunnerRegistry(registry))
		synctest.Wait()

		if len(registry.names) != 0 || destroyed.Load() != 0 {
			t.Fatalf("removed %v and destroyed %d with the launch still open, want neither",
				registry.names, destroyed.Load())
		}

		runner.unblock()
		stop()
		<-done
	})
}

// A STOP FINISHES THE LAUNCHES IT FOUND BEFORE RUN RETURNS (#393). Each writes to a
// ledger the caller may close once Run has returned.
func TestRunDoesNotReturnBeforeItsLaunchesDo(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 1}, longPoll: time.Minute}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		stop()
		synctest.Wait()

		select {
		case err := <-done:
			t.Fatalf("Run returned (%v) with a launch still open", err)
		default:
		}

		runner.unblock()

		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}

		if _, inflight, _ := runner.counts(); inflight != 0 {
			t.Fatalf("%d launches outlived Run", inflight)
		}
	})
}

// A STOP CANCELS NO LAUNCH IT FOUND, BEGUN OR WAITING ITS TURN (#393). Cancelled partway a
// launch cannot verify what it started, and a drain then waits on a lease nothing accounts
// for; one still waiting is bound to a job, and an assignment is a promise a drain keeps.
func TestAStopCancelsNoLaunchItFound(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		var sent atomic.Bool

		// TWO DIRECT ASSIGNMENTS AGAINST ONE SLOT, so the second is queued behind the first.
		session := &fakeSession{
			onGet: func() (*Message, error) {
				if sent.CompareAndSwap(false, true) {
					return &Message{
						MessageID: 3,
						Assigned: []dispatch.Job{
							{RequestID: 21, RunID: 1, JobID: "job-21"},
							{RequestID: 22, RunID: 2, JobID: "job-22"},
						},
						Statistics: &Statistics{TotalAssignedJobs: 2},
					}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner, WithPoolLaunchBatch(1))
		synctest.Wait()

		if started, _, _ := runner.counts(); started != 1 {
			t.Fatalf("started %d with one slot, want 1", started)
		}

		stop()
		synctest.Wait()

		runner.mu.Lock()
		begun := runner.contexts[0]
		runner.mu.Unlock()

		if err := begun.Err(); err != nil {
			t.Fatalf("the stop cancelled a launch that had begun: %v", err)
		}

		runner.unblock()
		<-done

		runner.mu.Lock()
		defer runner.mu.Unlock()

		if len(runner.contexts) != 2 {
			t.Fatalf("ran %d launches, want both to reach the runner", len(runner.contexts))
		}
		if err := runner.contexts[1].Err(); err != nil {
			t.Fatalf("the stop cancelled a launch waiting its turn, whose job was promised: %v", err)
		}
	})
}

// A FENCED CONTROLLER STARTS NOTHING, not even a launch it had already handed off and which
// is still waiting its turn. A stop does not cancel that launch, so nothing else would stop
// it reaching a node on behalf of a process that no longer owns its lease.
func TestAFencedControllerStartsNoLaunchStillWaiting(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		var (
			fenced atomic.Bool
			sent   atomic.Bool
		)

		session := &fakeSession{
			onGet: func() (*Message, error) {
				if sent.CompareAndSwap(false, true) {
					return &Message{
						MessageID: 5,
						Assigned: []dispatch.Job{
							{RequestID: 51, RunID: 1, JobID: "job-51"},
							{RequestID: 52, RunID: 2, JobID: "job-52"},
						},
						Statistics: &Statistics{TotalAssignedJobs: 2},
					}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner,
			WithPoolLaunchBatch(1), WithLeadershipLostCheck(fenced.Load))
		synctest.Wait()

		if started, _, _ := runner.counts(); started != 1 {
			t.Fatalf("started %d with one slot, want the first with the second waiting", started)
		}

		fenced.Store(true)
		runner.unblock()
		synctest.Wait()

		if started, _, _ := runner.counts(); started != 1 {
			t.Fatalf("started %d launches, want the one begun before the fence and no other", started)
		}

		stop()
		<-done
	})
}

// A TIER DOES NOT BUY PAST WHAT MAY START. The rest would sit bought behind the launches
// with its capacity charged, which another tier could have used.
func TestAPoolPassBuysOnlyWhatMayStart(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 5}, longPoll: time.Minute}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)

		// THROUGH SEVERAL POLLS, each of which reconciles again.
		time.Sleep(5 * time.Minute)
		synctest.Wait()

		active, err := a.ActiveRunnerLeases(t.Context(), tiers[0].Label)
		if err != nil {
			t.Fatalf("ActiveRunnerLeases: %v", err)
		}
		if active != 2 {
			t.Fatalf("%d leases bought with 2 launches open and a batch of 2, want 2", active)
		}

		runner.unblock()
		stop()
		<-done
	})
}

// DIRECT ASSIGNMENTS TAKE TURNS AS THE POOL DOES. They are bound to their jobs and have
// to run, but a node handed all of them at once times them out in its queue.
func TestDirectAssignmentsDoNotOverlapPastTheBatch(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		var sent atomic.Bool

		session := &fakeSession{
			onGet: func() (*Message, error) {
				if sent.CompareAndSwap(false, true) {
					return &Message{
						MessageID: 4,
						Assigned: []dispatch.Job{
							{RequestID: 31, RunID: 1, JobID: "job-31"},
							{RequestID: 32, RunID: 2, JobID: "job-32"},
							{RequestID: 33, RunID: 3, JobID: "job-33"},
						},
						Statistics: &Statistics{TotalAssignedJobs: 3},
					}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		if started, inflight, _ := runner.counts(); started != 2 || inflight != 2 {
			t.Fatalf("started %d with %d open, want the batch of 2 and the third waiting its turn", started, inflight)
		}

		runner.unblock()
		synctest.Wait()

		if started, _, most := runner.counts(); started != 3 || most > 2 {
			t.Fatalf("started %d, at most %d at once, want all 3 at no more than 2", started, most)
		}

		stop()
		<-done
	})
}

// A FAILED LAUNCH DOES NOT BUY THE NEXT ONE AT ONCE. launch settles an ordinary failure
// and returns no error, and waking on that would buy and fail again without pause for as
// long as a node refuses. The retry waits for the next poll.
func TestAFailedLaunchDoesNotBuyTheNextUntilThePoll(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)

		var attempts atomic.Int64

		runner := &fakeRunner{onLaunch: func(int64) error {
			attempts.Add(1)

			return errors.New("the node refused")
		}}
		// FAR MORE DEMAND THAN THOSE PASSES CAN REACH, so one that does not wait is unmistakable.
		session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 50}, longPoll: time.Hour}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		if got := attempts.Load(); got > maxPassesBeforeFirstPoll*2 {
			t.Fatalf("%d launch attempts with one poll out and a batch of 2, want at most %d",
				got, maxPassesBeforeFirstPoll*2)
		}

		stop()
		<-done
	})
}

// NOR ONE WHOSE CLEANUP COULD NOT FINISH. Such a launch is still in the listener's books,
// so being there says nothing about whether it started anything.
func TestAFailedLaunchWhoseCleanupFailedDoesNotBuyTheNext(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		runner.unblock()

		runner.beforeHold = func(lease *alloc.Lease, job dispatch.Job) error {
			if err := a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: lease.ID, Tier: tiers[0].Label,
				LaunchRequestID: job.RequestID, RunnerName: provider.InstanceName(lease.ID)}); err != nil {
				t.Errorf("RegisterPoolRunner: %v", err)
			}

			return errors.New("the node refused")
		}

		session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 50}, longPoll: time.Hour}
		registry := &concurrentRegistry{fakeRunnerRegistry: fakeRunnerRegistry{err: errors.New("GitHub would not remove it")}}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner, WithRunnerRegistry(registry))
		synctest.Wait()

		if started, _, _ := runner.counts(); started > maxPassesBeforeFirstPoll*2 {
			t.Fatalf("%d launch attempts with one poll out and a batch of 2, want at most %d",
				started, maxPassesBeforeFirstPoll*2)
		}

		stop()
		<-done
	})
}

// A REFUSAL ENDS WITH A PURCHASE, NOT WITH FINDING EVERY SLOT TAKEN. A pass that bought
// answered it. One that only found its slots taken bought nothing and says nothing of
// room, so a refusal this message's assignments just met stands, and one a pass met itself
// is reported even though it also bought.
func TestARefusalDoesNotOutliveThePurchaseThatAnsweredIt(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("a-work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8 * tierVCPU, MaxMemory: 256 * config.GiB}, tiers)
	runner := newHeldLaunches()

	t.Cleanup(runner.unblock)

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner), WithPoolLaunchBatch(2),
		WithBackgroundLaunches())

	small := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 256 * config.GiB}, tiers)
	full := NewListener(small, tiers[0].Label, &fakeSession{}, WithRunner(runner), WithPoolLaunchBatch(2),
		WithBackgroundLaunches())

	// RELEASED FIRST: each listener's launches are held open until this.
	defer func() {
		runner.unblock()
		l.launches.Wait()
		full.launches.Wait()
	}()

	stale := refusal{unread: errors.New("an earlier pass could not read why")}
	refused := func() error {
		l.mu.Lock()
		defer l.mu.Unlock()

		return l.refused.unread
	}

	l.refuse(stale, false)

	if err := l.reconcilePool(t.Context(), 5); err != nil {
		t.Fatalf("reconcilePool: %v", err)
	}
	if err := refused(); err != nil {
		t.Fatalf("still reporting %q after a pass that bought a batch", err)
	}

	l.refuse(stale, false)

	if err := l.reconcilePool(t.Context(), 5); err != nil {
		t.Fatalf("reconcilePool: %v", err)
	}
	if err := refused(); err == nil {
		t.Fatal("a pass that found every slot taken and bought nothing cleared a refusal it has no evidence against")
	}

	if err := full.reconcilePool(t.Context(), 5); err != nil {
		t.Fatalf("reconcilePool: %v", err)
	}

	full.mu.Lock()
	room := full.refused.room
	full.mu.Unlock()

	if room == nil {
		t.Fatal("a pass that bought one lease and was refused the next reports no refusal")
	}
}

// LAUNCHES STUCK ON A NODE ARE NOT PROGRESS. A listener with its launches beside its loop
// reconciles every poll, and dating the waiter there would let them hold every tier
// sharing that node for as long as they stay stuck, where a waiter silent for
// WaiterAllowance lets others past.
func TestLaunchesStuckOnANodeDoNotDateAWaiter(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		t.Cleanup(runner.unblock)

		order := newAdmissionQueue(config.AdmissionFair)
		session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 5}, longPoll: time.Minute}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner, WithAdmissionQueue(order))
		synctest.Wait()

		start := time.Now()

		// THROUGH MORE THAN AN ALLOWANCE OF POLLS, each of which reconciles.
		time.Sleep(WaiterAllowance + 2*time.Minute)
		synctest.Wait()

		_, progress, waiting := order.waitingSince(tiers[0].Label)
		if !waiting {
			t.Fatal("the tier is not waiting, so this proves nothing")
		}
		if progress.After(start) {
			t.Fatalf("waiter dated %s after its launches stuck, so it never yields its place",
				progress.Sub(start))
		}
		if polls := session.polls(); polls < 4 {
			t.Fatalf("polled %d times, want the several that would have dated it", polls)
		}

		runner.unblock()
		stop()
		<-done
	})
}

// A LAUNCH'S FATAL ERROR STILL STOPS THE LISTENER. It cannot return to the loop any
// more, so it has to reach it some other way: a verification failure says the ledger
// disagrees about whose runner this is.
func TestABackgroundLaunchFailureReachesTheLoop(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := newHeldLaunches()
		runner.unblock()

		runner.beforeHold = func(lease *alloc.Lease, job dispatch.Job) error {
			// REGISTERED UNDER ANOTHER REQUEST, which launch's verification refuses.
			return a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: lease.ID, Tier: tiers[0].Label,
				LaunchRequestID: job.RequestID + 1000, RunnerName: provider.InstanceName(lease.ID)})
		}

		session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 1}, longPoll: time.Minute}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		defer stop()

		err := <-done
		if err == nil || !strings.Contains(err.Error(), "verify pool runner") {
			t.Fatalf("Run returned %v, want the launch's verification failure", err)
		}
	})
}
