package server

import (
	"bytes"
	"context"
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

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}),
		WithDrainGrace(20*time.Second))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

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

	if err := <-runDone; err == nil {
		t.Error("Run returned nil after its context was cancelled")
	}

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

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(runner), WithDrainGrace(20*time.Second))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

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

	if err := <-runDone; err == nil {
		t.Error("Run returned nil after its context was cancelled")
	}

	if got := l.Running(); got != 0 {
		t.Errorf("Running() = %d after the drain, want 0", got)
	}
}

// A drain is bounded. When the budget runs out the listener stops waiting and
// the ordinary teardown destroys what is left — which is exactly what every
// shutdown did before a drain existed, so an overrun degrades rather than fails.
func TestADrainThatOverrunsItsBudgetDestroysWhatIsLeft(t *testing.T) {
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

		// The completion NEVER arrives. This is the job that outlives the drain.
		return nil, ErrNoMessage
	}

	var destroyed atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		destroyed.Add(1)

		return nil
	}}

	// Short enough that the test does not wait on it, long enough that the drain
	// genuinely begins first. The assertion is on what happened, not on when.
	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(runner), WithDrainGrace(200*time.Millisecond))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	select {
	case err := <-runDone:
		if err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("Run never returned after the drain budget expired")
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

	var offered atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		// An OFFER that is acquired and then never assigned, which is the state
		// GitHub leaves behind when another scale set wins the same job.
		if offered.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		return nil, ErrNoMessage
	}

	// The drain's own WARN is the assertion, not a stopwatch. Run returns either
	// way — the question is whether it returned because there was nothing to wait
	// for, or because it waited out its whole budget first — and only the log
	// distinguishes those. A time threshold would have to be both well under the
	// grace and well over a scheduling hiccup, which is the shape of a flake.
	var (
		logMu  sync.Mutex
		logged bytes.Buffer
	)

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}),
		WithDrainGrace(20*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(&syncWriter{mu: &logMu, w: &logged},
			&slog.HandlerOptions{Level: slog.LevelWarn}))))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

	waitUntil(deadline, t, "the offer to be acquired", func() bool { return l.Acquiring() == 1 })

	if got := l.Running(); got != 0 {
		t.Fatalf("nothing should be running: Running() = %d", got)
	}

	cancel()

	select {
	case err := <-runDone:
		if err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("Run never returned")
	}

	logMu.Lock()
	defer logMu.Unlock()

	if strings.Contains(logged.String(), "stopped waiting") {
		t.Errorf("the drain waited out its budget for a promise that will never be "+
			"assigned:\n%s", logged.String())
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
// exits. That made the check look redundant, and it is not: against a session
// that ignores its context the drain would never end, Run would never return,
// and the process could not be stopped at all.
func TestADrainIsBoundedEvenWhenTheSessionIgnoresItsContext(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

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

		// No completion, ever, and no notice taken of any deadline.
		return nil, ErrNoMessage
	}

	l := NewListener(a, "billet-4vcpu-a", deafSession{fakeSession: inner},
		WithRunner(&fakeRunner{}), WithDrainGrace(200*time.Millisecond))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	select {
	case err := <-runDone:
		if err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("a session that ignores its context made the drain unbounded")
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

	var (
		logMu  sync.Mutex
		logged bytes.Buffer
	)

	// A grace far longer than the test, so waiting it out is not something the
	// test can do by accident.
	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}),
		WithDrainGrace(20*time.Second),
		WithLogger(slog.New(slog.NewTextHandler(&syncWriter{mu: &logMu, w: &logged},
			&slog.HandlerOptions{Level: slog.LevelWarn}))))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	waitUntil(deadline, t, "the drain to begin", func() bool { return len(l.Held()) == 0 })

	close(finish)

	select {
	case err := <-runDone:
		if err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("Run never returned after the job finished")
	}

	logMu.Lock()
	defer logMu.Unlock()

	if strings.Contains(logged.String(), "stopped waiting") {
		t.Errorf("the drain ran out of budget rather than noticing its work had "+
			"finished:\n%s", logged.String())
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

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(&fakeRunner{}), WithDrainGrace(20*time.Second))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

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
	waitUntil(deadline, t, "the drain to begin", func() bool { return len(l.Held()) == 0 })

	if got := l.Running(); got != 1 {
		t.Fatalf("the job was abandoned rather than drained: Running() = %d, want 1", got)
	}

	close(finish)

	if err := <-runDone; err == nil {
		t.Error("Run returned nil after its context was cancelled")
	}

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

	l := NewListener(a, "billet-4vcpu-a", &fakeSession{},
		WithRunner(&fakeRunner{}), WithDrainGrace(time.Nanosecond))

	runDone := make(chan error, 1)

	go func() { runDone <- l.Run(ctx) }()

	waitUntil(deadline, t, "the escrow to be taken", func() bool { return len(l.Held()) > 0 })

	cancel()

	select {
	case <-runDone:
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
