package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// waitUntil polls a condition until it holds or the context ends.
//
// Polling rather than sleeping a fixed amount: a sleep long enough to be
// reliable under -race and a loaded CI box is long enough to make the suite
// slow, and a sleep that is merely usually long enough is a flake. The failure
// message names what was being waited for, because "timed out" alone sends the
// reader to the wrong place.
func waitUntil(ctx context.Context, t *testing.T, what string, cond func() bool) {
	t.Helper()

	for !cond() {
		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// drainLog captures a listener's log so a test can observe that the drain
// actually began.
//
// EVERY DRAIN TEST NEEDS THIS, and the first versions did not have it. The
// observable effects of a drain — the idle escrow released, the job destroyed,
// Run returning — are ALSO what the ordinary teardown does, so a test that waits
// on those alone passes just as happily against a listener that never drained at
// all. The drain announces itself exactly once, and that announcement is the only
// unambiguous evidence that this code path ran.
type drainLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (d *drainLog) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.buf.Write(p)
}

func (d *drainLog) saw(substr string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return strings.Contains(d.buf.String(), substr)
}

func (d *drainLog) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.buf.String()
}

// option returns the logger option, at Info so the drain's own start line is
// captured along with the warnings.
func (d *drainLog) option() Option {
	return WithLogger(slog.New(slog.NewTextHandler(d, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

// beganDraining is the drain's announcement of itself.
const beganDraining = "draining: not taking new work"

// slowPoll makes a fake session behave like a long poll instead of a spin.
//
// A fake that returns ErrNoMessage immediately turns the drain's loop into a hot
// spin: it takes l.mu on every pass through capacity() and drained(), and under
// -race that is enough to starve the heartbeat goroutine. A lease whose renewal
// goes stale is then MOVED OUT of `running` into the cleanup set — the listener
// says so at the point it does it, "THE LEASE GOES; THE OBLIGATION DOES NOT" —
// and drained() only looks at `running`, so the drain reports itself finished
// and its budget never expires.
//
// Short enough not to slow the suite, long enough to let the heartbeat run.
func slowPoll() { time.Sleep(2 * time.Millisecond) }

// outlivesTheDrain is a lease TTL long enough that no lease can expire while a
// drain is being timed out.
//
// A TEST THAT ASSERTS THE DRAIN RAN OUT OF BUDGET MUST OUTLIVE ITS OWN LEASES,
// and that is not obvious until it bites. The drain ends on whichever comes
// first: everything finished, or the budget gone. A lease that expires mid-drain
// empties `running`, so the drain ends by being FINISHED — the other branch,
// which never writes the line those tests assert on. With the 300ms TTL the
// other drain tests use, that happened about one run in four under -race, and it
// is the failure CI caught on #19.
//
// Tests asserting the drain finished its work (gaveUp ABSENT) do not need this:
// a lease expiring early pushes them further towards the outcome they expect.
const outlivesTheDrain = 30 * time.Second

// gaveUp is the drain giving up on its budget, as opposed to finishing. Kept as
// a constant so a reworded message breaks the tests that depend on it here
// rather than making them quietly stop distinguishing the two endings.
const gaveUp = "giving up on the jobs"

// runResult is Run's outcome, observable without being taken.
//
// A plain `chan error` cannot be both watched and waited on: awaitDrainStart has
// to notice that Run returned, and awaitRun has to read what it returned, and a
// receive in the first consumes the value the second needs. That produced a test
// that hung waiting for a result something else had already thrown away.
type runResult struct {
	done chan struct{}

	mu       sync.Mutex
	err      error
	returned bool
}

// startRun runs the listener and records how it ended.
func startRun(ctx context.Context, l *Listener) *runResult {
	r := &runResult{done: make(chan struct{})}

	go func() {
		err := l.Run(ctx)

		r.mu.Lock()
		r.err, r.returned = err, true
		r.mu.Unlock()

		close(r.done)
	}()

	return r
}

func (r *runResult) has() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.returned
}

// awaitDrainStart blocks until the listener says it has begun draining.
//
// IT ALSO WATCHES FOR RUN RETURNING FIRST, because that is the failure this
// otherwise reports as "the drain never began" — thirty seconds later, naming
// the symptom and hiding the cause. A poll or an escrow refill that fails for
// its own reasons ends Run before the cancel ever arrives; under a loaded
// machine running fourteen instrumented test binaries, that is a transient
// database error, and the test should say so rather than describe a drain that
// was never going to happen.
func awaitDrainStart(ctx context.Context, t *testing.T, d *drainLog, run *runResult) {
	t.Helper()

	for !d.saw(beganDraining) {
		// CHECKED IN THIS ORDER, and re-checked after. A drain with nothing to wait
		// for begins and finishes in the same instant, so Run can have returned by
		// the time this looks — and the log is the evidence that it drained on the
		// way out rather than skipping it.
		if run.has() {
			if d.saw(beganDraining) {
				return
			}

			t.Fatalf("Run returned before the drain began: %v\n%s", run.get(), d.String())
		}

		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("timed out waiting for the drain to begin\n%s", d.String())
		}
	}
}

// get is Run's error. Blocks until it has one.
func (r *runResult) get() error {
	<-r.done

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.err
}

// awaitRun receives Run's result within the deadline.
//
// A bare `<-runDone` hangs the whole package when a regression stops Run
// returning, so the failure arrives as a ten-minute timeout with a goroutine
// dump rather than as this test failing.
func awaitRun(ctx context.Context, t *testing.T, run *runResult) {
	t.Helper()

	select {
	case <-run.done:
		if run.get() == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-ctx.Done():
		t.Fatal("Run never returned")
	}
}

// THE FIRST HALF OF A DRAIN: stop offering capacity nobody is using, without
// lying to GitHub about the capacity somebody IS using.
//
// The naive way to stop taking work is to advertise a constant zero. That is
// untrue while a job runs, and the number billet sends is documented as the
// scale set's TOTAL capacity rather than its spare. Releasing the idle escrow
// instead makes the advertisement fall to exactly the work still in flight and
// reach zero by itself when the last job finishes — the drain's own completion
// condition, arrived at without a second source of truth.
func TestADrainReleasesIdleEscrowAndAdvertisesOnlyWhatIsRunning(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	// Room for two runners at four vCPU each, so there is genuinely idle escrow
	// to release while one job runs. With room for exactly one, `held` would be
	// empty anyway and the test would pass against a drain that releases nothing.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		assigned  atomic.Bool
		completed atomic.Bool
		finish    = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// The completion is delivered only once the test asks for it, so the
		// window in which a job is running and billet is draining is controlled by
		// the test rather than by whichever goroutine happens to run first.
		select {
		case <-finish:
			if completed.CompareAndSwap(false, true) {
				return &Message{MessageID: 2, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
			}
		default:
		}

		return nil, ErrNoMessage
	}

	// advertised records what each poll told GitHub, so the assertion is about
	// the number that actually went out rather than about internal state that
	// might never be sent.
	var (
		mu         sync.Mutex
		advertised []int
	)

	session.onPoll = func(capacity int) {
		mu.Lock()
		defer mu.Unlock()

		advertised = append(advertised, capacity)
	}

	lastAdvertised := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()

		if len(advertised) == 0 {
			return 0, false
		}

		return advertised[len(advertised)-1], true
	}

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}),
		WithDrainGrace(20*time.Second),
		dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })
	waitUntil(deadline, t, "the spare capacity to be escrowed", func() bool { return len(l.Held()) > 0 })

	// The drain begins here.
	cancel()

	// ONE condition, deliberately, rather than a separate "held is empty" wait
	// first. `held` also empties during the ordinary teardown, so waiting on that
	// alone would be satisfied by the behaviour this test exists to replace — it
	// would pass against a listener that never drained at all.
	//
	// A POLL that advertises exactly the running job cannot be satisfied that
	// way: it can only happen if the escrow was released, the job was left alone,
	// and the listener was still talking to GitHub, all at once.
	waitUntil(deadline, t, "a poll advertising just the running job", func() bool {
		n, ok := lastAdvertised()

		return ok && n == 1
	})

	if got := len(l.Held()); got != 0 {
		t.Errorf("idle escrow survived the drain: Held() = %d, want 0", got)
	}

	if got := l.Running(); got != 1 {
		t.Fatalf("the drain destroyed a running job: Running() = %d, want 1", got)
	}

	// And it has not stopped polling — which is the point. A listener that
	// stopped here could never learn the job had finished.
	before := session.polls()
	waitUntil(deadline, t, "the drain to keep polling", func() bool { return session.polls() > before })

	close(finish)

	awaitRun(deadline, t, run)

	if got := l.Running(); got != 0 {
		t.Errorf("Running() = %d after the drain, want 0", got)
	}
}

// THE SECOND HALF: a drain that refuses new work but still hears about the old.
//
// Refusing new work is easy and useless on its own — a listener that stopped
// polling would refuse everything, and would also never learn that the job it is
// waiting for had finished. Reporting IS the handover: the completion arrives
// through the same long poll that carries the offers.
func TestADrainRefusesNewWorkAndStillHearsCompletions(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		assigned atomic.Bool
		offered  atomic.Bool
		finished atomic.Bool
		draining atomic.Bool
		offerNow = make(chan struct{})
		finish   = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// An OFFER, delivered only once the drain is under way. This is the work
		// that must be refused.
		select {
		case <-offerNow:
			if offered.CompareAndSwap(false, true) {
				return &Message{MessageID: 2, Available: []Job{{RequestID: 12, RunID: 102}}}, nil
			}
		default:
		}

		select {
		case <-finish:
			if finished.CompareAndSwap(false, true) {
				return &Message{MessageID: 3, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
			}
		default:
		}

		return nil, ErrNoMessage
	}

	// A launch during the drain would mean the refusal did not hold.
	var launches atomic.Int32

	runner := &fakeRunner{onLaunch: func(int64) error {
		if draining.Load() {
			launches.Add(1)
		}

		return nil
	}}

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(runner), WithDrainGrace(20*time.Second), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	draining.Store(true)
	cancel()

	waitUntil(deadline, t, "the drain to be advertising just the running job", func() bool {
		return l.Running() == 1 && len(l.Held()) == 0
	})

	close(offerNow)
	waitUntil(deadline, t, "the offer to be delivered", offered.Load)

	// Give the listener room to do the wrong thing before concluding it did not.
	// Waiting for a further poll is the causal anchor: the offer has been seen and
	// acted on by the time the next one goes out.
	polls := session.polls()
	waitUntil(deadline, t, "a poll after the offer", func() bool { return session.polls() > polls+1 })

	if ids := session.acquiredIDs(); len(ids) != 0 {
		t.Errorf("a draining listener tried to claim work: %v", ids)
	}

	if n := launches.Load(); n != 0 {
		t.Errorf("a draining listener launched %d job(s)", n)
	}

	// And the completion still lands, which is the half that makes the drain
	// terminate at all.
	close(finish)

	awaitRun(deadline, t, run)

	if got := l.Running(); got != 0 {
		t.Errorf("Running() = %d after the drain, want 0", got)
	}
}

// A drain is bounded. When the budget runs out the listener stops waiting and
// the ordinary teardown destroys what is left — which is exactly what every
// shutdown did before a drain existed, so an overrun degrades rather than fails.
func TestADrainThatOverrunsItsBudgetDestroysWhatIsLeft(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	// A lease TTL longer than the drain budget — see outlivesTheDrain.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var assigned atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// A REAL LONG POLL BLOCKS, AND SO DOES THIS — see slowPoll. Returning
		// immediately spins the drain loop as fast as the scheduler allows, which
		// starves the heartbeat, and a lease whose renewal goes stale is MOVED OUT
		// OF `running` into the cleanup set (listener.go, "THE LEASE GOES; THE
		// OBLIGATION DOES NOT"). drained() then reports the drain finished and the
		// budget never expires — so the test asserts on a line that was never
		// going to be written.
		//
		// The completion NEVER arrives. This is the job that outlives the drain.
		slowPoll()

		return nil, ErrNoMessage
	}

	var destroyed atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		destroyed.Add(1)

		return nil
	}}

	// Short enough that the test does not wait on it, long enough that the drain
	// genuinely begins first. The assertion is on what happened, not on when.
	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(runner), WithDrainGrace(200*time.Millisecond), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	// It DRAINED first. Without this the test passes against a listener that goes
	// straight to the teardown, because the teardown destroys the job and returns
	// too — the same two observations this asserts on.
	awaitDrainStart(deadline, t, &dl, run)

	awaitRun(deadline, t, run)

	// AND IT STOPPED BECAUSE THE BUDGET RAN OUT, which is the case this test is
	// named for. A drain that ended for any other reason would satisfy everything
	// above.
	if !dl.saw(gaveUp) {
		t.Errorf("the drain did not report giving up on its budget:\n%s", dl.String())
	}

	if n := destroyed.Load(); n == 0 {
		t.Error("the job that outlived the drain was never destroyed")
	}
}

// A PROMISE IS NOT A RUNNING JOB, and waiting for one is waiting for something
// only the teardown can resolve.
//
// `acquiring` holds escrow for work billet claimed from GitHub and has not been
// assigned. GitHub may never assign it — another scale set can win the same
// offer — and the listener deliberately KEEPS such a promise: renewLoop reports
// it as stale and holds the lease, because "the thing that resolves it is the
// session ending, which releases every promise with it".
//
// So a drain that waits for `acquiring` to empty waits for the session close it
// is standing in front of. It spends its entire budget and then destroys
// nothing, having achieved a six-hour pause. Found by the end-to-end suite,
// which stopped with running=0 and would not exit.
func TestADrainDoesNotWaitForAPromiseThatWasNeverAssigned(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		offered atomic.Bool
		settled = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		// An OFFER that is acquired and then never assigned, which is the state
		// GitHub leaves behind when another scale set wins the same job.
		if offered.CompareAndSwap(false, true) {
			close(settled)

			return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// A REAL LONG POLL BLOCKS, AND SO DOES THIS. Returning ErrNoMessage
		// immediately spins the loop through refillEscrow — a database write — as
		// fast as the scheduler allows, and on a machine running the whole suite
		// under -race that is enough to turn a transient error into a certainty.
		// The listener then ends for its own reasons before the cancel arrives,
		// and the drain this test is about never happens.
		<-settled

		return nil, ErrNoMessage
	}

	// The drain's own WARN is the assertion, not a stopwatch. Run returns either
	// way — the question is whether it returned because there was nothing to wait
	// for, or because it waited out its whole budget first — and only the log
	// distinguishes those. A time threshold would have to be both well under the
	// grace and well over a scheduling hiccup, which is the shape of a flake.
	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}),
		WithDrainGrace(20*time.Second),
		dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the offer to be acquired", func() bool { return l.Acquiring() == 1 })

	if got := l.Running(); got != 0 {
		t.Fatalf("nothing should be running: Running() = %d", got)
	}

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	awaitRun(deadline, t, run)

	if dl.saw(gaveUp) {
		t.Errorf("the drain waited out its budget for a promise that will never be "+
			"assigned:\n%s", dl.String())
	}
}

// deafSession ignores the context it is handed.
//
// Neither Session nor Runner promises to honour one, and the teardown budgets
// exist precisely because a bad implementation can wedge the listener. The drain
// needs the same defence: without its own deadline check it would inherit its
// bound from whether the session happens to notice a cancelled context, which is
// not a bound at all.
type deafSession struct {
	*fakeSession
}

func (d deafSession) GetMessage(_ context.Context, _ int64, capacity int) (*Message, error) {
	d.acquiredMu.Lock()
	d.polled++
	d.acquiredMu.Unlock()

	if d.onPoll != nil {
		d.onPoll(capacity)
	}

	if d.onGet != nil {
		return d.onGet()
	}

	return nil, ErrNoMessage
}

// A DRAIN IS BOUNDED BY ITS OWN DEADLINE, not by the session noticing one.
//
// The fake used elsewhere returns ctx.Err() when its context ends, so a drain
// with no deadline check of its own still stops — the poll fails and the loop
// exits. That makes the check look redundant. It is not: against a session that
// merely ignores its context, the deadline is what ends the drain.
//
// WHAT THIS DOES NOT PROVE, and the limitation is real: a session that BLOCKS
// forever inside GetMessage is not bounded by anything here, because the
// deadline is only observed between calls. That is the same hole the teardown
// documents for Destroy — "NOTHING can bound a Destroy that ignores its context
// outright, and neither interface forbids one" — and the same answer applies.
// Pretending otherwise would be the more dangerous comment to leave. So the fake
// below returns promptly and this test covers the deadline being noticed, not a
// wedged transport being interrupted.
func TestADrainIsBoundedEvenWhenTheSessionIgnoresItsContext(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	// A lease TTL longer than the drain budget — see outlivesTheDrain.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var assigned atomic.Bool

	inner := &fakeSession{}
	inner.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// Blocks briefly rather than spinning — see slowPoll.
		slowPoll()

		// No completion, ever, and no notice taken of any deadline.
		return nil, ErrNoMessage
	}

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", deafSession{fakeSession: inner},
		WithRunner(&fakeRunner{}), WithDrainGrace(200*time.Millisecond), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	select {
	case <-run.done:
		err := run.get()
		if err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("a session that ignores its context made the drain unbounded")
	}

	if !dl.saw(gaveUp) {
		t.Errorf("the drain stopped for some reason other than its own deadline:\n%s",
			dl.String())
	}
}

// A DRAIN THAT FINISHES ITS WORK STOPS BECAUSE IT FINISHED, not because it ran
// out of budget.
//
// Both endings return the same error from Run, so a test that only waits for Run
// is satisfied either way — it would pass against a drain whose completion check
// never fires and which simply waits out its whole grace. The distinguishing
// evidence is which of the two log lines was written.
func TestADrainStopsWhenTheWorkFinishesRatherThanWhenItsBudgetDoes(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		assigned  atomic.Bool
		completed atomic.Bool
		finish    = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		select {
		case <-finish:
			if completed.CompareAndSwap(false, true) {
				return &Message{MessageID: 2, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
			}
		default:
		}

		return nil, ErrNoMessage
	}

	var dl drainLog

	// A grace far longer than the test, so waiting it out is not something the
	// test can do by accident.
	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}),
		WithDrainGrace(20*time.Second),
		dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	close(finish)

	select {
	case <-run.done:
		err := run.get()
		if err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("Run never returned after the job finished")
	}

	if dl.saw(gaveUp) {
		t.Errorf("the drain ran out of budget rather than noticing its work had "+
			"finished:\n%s", dl.String())
	}
}

// THE CANCELLATION ALMOST ALWAYS LANDS INSIDE THE LONG POLL, not between two of
// them, and that is where the first implementation of this got it wrong.
//
// A listener spends nearly all its life blocked in GetMessage. Checking for
// cancellation only at the top of the loop meant the poll returned
// context.Canceled first and Run treated it as a failed poll — so the drain
// never started at all. The other tests here cancel between polls and pass
// either way, which is why this one blocks the session until the cancel has
// definitely happened.
func TestACancellationInsideTheLongPollStillDrains(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		assigned  atomic.Bool
		completed atomic.Bool
		inPoll    = make(chan struct{}, 1)
		release   = make(chan struct{})
		finish    = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		select {
		case <-finish:
			if completed.CompareAndSwap(false, true) {
				return &Message{MessageID: 2, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
			}
		default:
		}

		// BLOCK INSIDE THE POLL until the test says otherwise, so the cancellation
		// is guaranteed to arrive while the listener is in here rather than
		// between two calls. Without this the placement of the cancel is up to the
		// scheduler and the test proves nothing in particular.
		select {
		case inPoll <- struct{}{}:
			<-release
		default:
		}

		return nil, ErrNoMessage
	}

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}), WithDrainGrace(20*time.Second), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	// Wait until the listener is definitely inside a poll, then cancel, then let
	// the poll return.
	select {
	case <-inPoll:
	case <-deadline.Done():
		t.Fatal("the listener never blocked inside a poll")
	}

	cancel()
	close(release)

	// It drained rather than returning: idle escrow handed back, job untouched.
	awaitDrainStart(deadline, t, &dl, run)

	if got := l.Running(); got != 1 {
		t.Fatalf("the job was abandoned rather than drained: Running() = %d, want 1", got)
	}

	close(finish)

	awaitRun(deadline, t, run)

	if got := l.Running(); got != 0 {
		t.Errorf("Running() = %d after the drain, want 0", got)
	}
}

// AN IDLE LEASE THE DRAIN COULD NOT RELEASE MUST STILL COME BACK.
//
// releaseIdleEscrow takes the leases out of `held` before releasing them, so one
// whose release fails and is not put back is in neither place: the teardown's
// own release pass walks `held` and would never see it, and the capacity would
// sit out until the reaper expired the lease. That is a silent, slow leak of the
// exact resource the drain is trying to hand back.
//
// Driven by a drain grace of one nanosecond, which makes the release context
// expire before the first release can commit. That is not a realistic setting —
// it is the cheapest deterministic way to make a local database call fail.
func TestAnIdleLeaseTheDrainCouldNotReleaseIsReleasedAtShutdown(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", &fakeSession{},
		WithRunner(&fakeRunner{}), WithDrainGrace(time.Nanosecond), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the escrow to be taken", func() bool { return len(l.Held()) > 0 })

	cancel()

	select {
	case <-run.done:
	case <-deadline.Done():
		t.Fatal("Run never returned")
	}

	// The capacity is back. Whether the drain released it or the teardown did is
	// not the point — that NEITHER did is.
	room, err := a.Headroom(t.Context(), "billet-4vcpu-a")
	if err != nil {
		t.Fatalf("Headroom: %v", err)
	}

	if want := 8 / tierVCPU; room != want {
		t.Errorf("headroom = %d, want %d: an idle lease was dropped by the drain and "+
			"never released", room, want)
	}
}

// A SECOND SIGNAL STOPS THE WAITING, NOT THE TEARDOWN.
//
// The drain is measured in hours because a job is. An operator who does not want
// to wait needs a lever that ends the WAIT and still destroys and releases
// properly — otherwise the only thing left is killing the process, which strands
// exactly the containers the drain existed to protect.
//
// The assertion is that the compute was destroyed. A test that only checked Run
// returned would pass against a hurry that killed the process outright, which is
// the behaviour this replaces.
func TestAHurriedDrainStopsWaitingButStillTearsDown(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var assigned atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// No completion ever: without the hurry this drain runs for its full hour.
		return nil, ErrNoMessage
	}

	var destroyed atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		destroyed.Add(1)

		return nil
	}}

	hurry := make(chan struct{})

	// An hour, so waiting it out is not something this test could do by accident.
	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(runner), WithDrainGrace(time.Hour), WithHurrySignal(hurry), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	close(hurry)

	select {
	case <-run.done:
		err := run.get()
		if err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("the second signal did not end the drain's wait")
	}

	// AND THE TEARDOWN STILL RAN. This is the half that distinguishes hurrying
	// from giving up.
	if n := destroyed.Load(); n == 0 {
		t.Error("the hurried drain skipped the teardown; its compute was never destroyed")
	}
}

// The hurry channel has to survive the trip from the control plane to every
// listener, or the second signal is a no-op the operator cannot see.
func TestTheHurryChannelReachesEveryListener(t *testing.T) {
	hurry := make(chan struct{})

	s := New(nil, nil, nil, "owner", nil, WithHurry(hurry))

	l := NewListener(nil, "tier", nil, s.listenerOpts()...)
	if l.hurry == nil {
		t.Fatal("the hurry channel never reached the listener")
	}

	close(hurry)

	select {
	case <-l.hurry:
	default:
		t.Error("the listener holds a different channel from the one that was closed")
	}
}

// A FATAL ERROR IS STILL FATAL WHEN IT ARRIVES DURING A SHUTDOWN.
//
// The first version of the drain treated any failure coinciding with
// cancellation as the cancellation itself: it looked at the clock and not at the
// error. So a contract violation — an acquisition response containing an id
// nobody offered for, which stops the control plane on purpose because billet
// can no longer tell which of its commitments are real — would be erased if the
// operator happened to press Ctrl-C at the same moment, and the drain would go
// on running that session for hours.
//
// The rule is asked the other way round: once the caller has asked billet to
// stop, an error IS the shutdown arriving unless it is one billet must stop for
// regardless. Looking only for context.Canceled instead was worse in a quieter
// way — a cancellation landing inside handle() surfaces as whatever domain error
// that path produces, none of which wrap the context error, so the drain was
// skipped intermittently for the most ordinary reason there is.
func TestAFatalErrorDuringCancellationIsNotSwallowedByTheDrain(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	// THE NAMED ONE. Under the rule this test guards, a cancellation makes billet
	// drain unless the error is one it must stop for whatever else is happening —
	// and there is exactly one of those, because "which of my commitments are
	// real" is the only question a drain cannot proceed without an answer to.
	fatal := fmt.Errorf("%w: the ids do not match", ErrUntrustworthySession)

	var (
		ready   atomic.Bool
		release = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		// Block until the test has cancelled, so the error and the cancellation are
		// genuinely simultaneous rather than merely close together. Without this
		// the test proves nothing about which of the two the listener acted on.
		if ready.CompareAndSwap(false, true) {
			<-release
		}

		return nil, fatal
	}

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}), WithDrainGrace(20*time.Second), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the listener to be inside a poll", ready.Load)

	cancel()
	close(release)

	select {
	case <-run.done:
		err := run.get()
		// stopping() reports the cancellation once ctx is done, which is the
		// listener's existing contract and not what this test is about. What must
		// NOT happen is the loop carrying on into a drain on a session it has just
		// been told it cannot trust.
		if err == nil {
			t.Error("Run returned nil")
		}
	case <-deadline.Done():
		t.Fatal("Run never returned; the fatal error was swallowed and the drain " +
			"carried on against a session billet cannot trust")
	}

	if dl.saw(beganDraining) {
		t.Errorf("a fatal error was treated as a cancellation and the listener "+
			"drained instead of stopping:\n%s", dl.String())
	}
}

// A CANCELLATION THAT SURFACES AS SOMETHING ELSE IS STILL A CANCELLATION.
//
// This is the case that made the drain unreliable rather than broken, which is
// worse. When the caller cancels, whatever call is in flight fails — and it does
// not necessarily fail with context.Canceled. A cancellation landing inside
// handle() comes back as whatever domain error that path produces, and those do
// not wrap the context error. A rule that looked for context.Canceled therefore
// skipped the drain intermittently, depending on where the cancel landed, which
// is the shape of a feature that passes its tests and does not work on a real
// machine.
//
// The session here fails with a plain error the moment the context is cancelled,
// so the placement is not left to the scheduler.
func TestACancellationThatSurfacesAsAnotherErrorStillDrains(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		assigned atomic.Bool
		inPoll   = make(chan struct{}, 1)
		release  = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// BLOCKED INSIDE THE POLL until the test has cancelled, because that is
		// the only placement that tests anything. If the cancel lands between two
		// polls, the top of the loop notices it directly and the drain begins
		// whatever any call returned — so the error classification is never
		// consulted and a rule that gets it wrong still passes.
		select {
		case inPoll <- struct{}{}:
			<-release
		default:
		}

		// NOT a context error, and deliberately not wrapping one: this stands in
		// for every domain error a cancelled call can produce on its way out.
		if ctx.Err() != nil {
			return nil, errors.New("the transport gave up")
		}

		return nil, ErrNoMessage
	}

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}), WithDrainGrace(2*time.Second), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	select {
	case <-inPoll:
	case <-deadline.Done():
		t.Fatal("the listener never blocked inside a poll")
	}

	cancel()
	close(release)

	awaitDrainStart(deadline, t, &dl, run)
}

// THE ONE ERROR THAT IS FATAL WHATEVER ELSE IS HAPPENING, driven through the
// real acquisition path rather than constructed by the test.
//
// GitHub returning an id nobody offered for means billet cannot tell which of
// its commitments are real. Draining against that session would be operating on
// exactly the state that is not safe to operate on — so this must stop, and a
// cancellation arriving at the same moment must not turn it into a drain.
func TestAnUntrustworthyScaleSetResponseStopsTheListenerEvenWhileStopping(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var offered atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if offered.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		return nil, ErrNoMessage
	}

	// THE SHUTDOWN ARRIVES EXACTLY AT THE VIOLATION.
	//
	// Cancelling earlier does not test this: the listener would notice the
	// cancellation on its way through handle, begin draining, and decline the
	// offer — so the violation never happens and there is nothing to be fatal
	// about. That is correct behaviour and a different case. The two have to
	// coincide, so the cancel is fired from inside the acquisition that returns
	// an id nobody offered for.
	session.onAcquire = func([]int64) ([]int64, error) {
		cancel()

		return []int64{11, 999}, nil
	}

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}), WithDrainGrace(20*time.Second), dl.option())

	run := startRun(ctx, l)

	select {
	case <-run.done:
	case <-deadline.Done():
		t.Fatal("the listener drained against a session it cannot trust")
	}

	if dl.saw(beganDraining) {
		t.Errorf("the listener drained against a scale set it cannot trust:\n%s", dl.String())
	}
}

// AN ERROR DURING THE DRAIN ENDS THE DRAIN.
//
// Suppression is for the cancellation arriving mid-call, before the drain has
// taken over. Once draining, a failure is a failure again: a listener that
// swallowed them would loop on a broken session until its whole grace expired —
// hours — and only then tear down, having achieved nothing but a delay.
func TestAnErrorDuringTheDrainEndsIt(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	var (
		assigned atomic.Bool
		draining atomic.Bool
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		if draining.Load() {
			return nil, errors.New("the session is gone")
		}

		return nil, ErrNoMessage
	}

	var dl drainLog

	// An hour, so a drain that swallowed the error would hang rather than finish
	// slowly — and this test would fail on its deadline rather than pass late.
	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}), WithDrainGrace(time.Hour), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	draining.Store(true)

	select {
	case <-run.done:
	case <-deadline.Done():
		t.Fatal("a failing session during the drain did not end it")
	}
}
