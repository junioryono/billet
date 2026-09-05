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

// drainLog captures a listener's log so a test can observe that the drain began.
//
// EVERY DRAIN TEST NEEDS THIS. The observable effects of a drain — idle escrow
// released, job destroyed, Run returning — are ALSO what the ordinary teardown does,
// so waiting on those alone passes against a listener that never drained. The drain
// announces itself exactly once.
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
// A fake returning ErrNoMessage immediately turns the drain's loop into a hot spin:
// it takes l.mu on every pass through capacity() and drained(), and under -race that
// starves the heartbeat. A lease whose renewal goes stale is then MOVED OUT of
// `running` into the cleanup set, and drained() only looks at `running` — so the
// drain reports itself finished and its budget never expires.
func slowPoll() { time.Sleep(2 * time.Millisecond) }

// outlivesTheDrain is a lease TTL long enough that no lease can expire while a drain
// is being staged or timed out.
//
// A TEST THAT ASSERTS THE DRAIN RAN OUT OF BUDGET MUST OUTLIVE ITS OWN LEASES. The
// drain ends on whichever comes first — everything finished, or the budget gone —
// and a lease expiring mid-drain empties `running`, so it ends by being FINISHED and
// never writes the line those tests assert on. At the 300ms TTL the other drain
// tests use that happened about one run in four under -race.
//
// LONGER THAN THIS FILE'S 30-SECOND SETUP DEADLINES, not merely longer than the
// 200ms drain itself. Making the TTL and the setup deadline both 30 seconds left
// the assertion dependent on which one a delayed CI worker reached first: a lease
// could expire while the test was still waiting for the job to enter `running`,
// then make the drain truthfully report that everything had finished. That is the
// rare ordering that once flaked on CI.
//
// Tests asserting the drain finished its work do not need this.
const outlivesTheDrain = time.Minute

// stoppedWaiting is the drain being told to stop waiting, as opposed to
// finishing. It is what a SECOND SIGNAL produces, and it is now the only way a
// drain ends with work still running: the budget stopped ending anything when
// the deadline was removed, because a timer that destroys somebody's build is
// not a timeout, it is a policy nobody chose.
const stoppedWaiting = "no longer waiting for the jobs still running here"

// finishedEarly is the OTHER way a drain ends: everything it was waiting on left
// `running`, so it stopped because there was nothing left rather than because an
// operator told it to stop waiting.
//
// THERE USED TO BE A THIRD CONSTANT HERE, `gaveUp`, for the drain running out of
// budget. That message is gone with the deadline, and leaving the constant behind
// left `dl.saw(gaveUp)` vacuously false in a test that asserted the drain had NOT
// given up — an assertion that could no longer fail, which is worse than not
// having one. A constant that outlives its message is how that happens.
//
// KEPT SO A FAILURE CAN SAY WHICH HAPPENED. A test that only knows one ending did
// not occur reports something true and useless -- the two exits want opposite
// fixes, and telling them apart is the whole diagnosis. This became a real cost
// once: that flake was reproduced on CI, and the run's logs were replaced by a
// re-run before anyone read which line the drain had written.
const finishedEarly = "everything running here has finished"

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
// IT ALSO WATCHES FOR RUN RETURNING FIRST, because that failure otherwise reports as
// "the drain never began" thirty seconds later, naming the symptom and hiding the
// cause: a poll or an escrow refill that fails for its own reasons ends Run before
// the cancel arrives.
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

// THE FIRST HALF OF A DRAIN: stop offering capacity nobody is using, without lying
// to GitHub about the capacity somebody IS using.
//
// Advertising a constant zero is untrue while a job runs, and the number billet
// sends is the scale set's TOTAL capacity rather than its spare. Releasing the idle
// escrow makes the advertisement fall to the work still in flight and reach zero by
// itself — the drain's completion condition, without a second source of truth.
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
	// DECLARED BEFORE onPoll IS INSTALLED so the hook can read what the listener
	// is holding. It is assigned before Run starts, which happens-before the
	// goroutine that calls the hook.
	var l *Listener

	var (
		mu         sync.Mutex
		advertised []int
		// heldWhenSent pairs each advertisement with the escrow still held when it
		// went out. The pairing is the assertion: either number alone cannot show
		// which happened first, and the order is the whole invariant — GitHub's
		// live number must never exceed what this listener can back.
		heldWhenSent []int
	)

	session.onPoll = func(capacity int) {
		mu.Lock()
		defer mu.Unlock()

		advertised = append(advertised, capacity)
		heldWhenSent = append(heldWhenSent, len(l.Held()))
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

	l = NewListener(a, "billet-4vcpu-a", session,
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
	// way: it can only happen if the job was left alone and the listener was
	// still talking to GitHub, both at once.
	waitUntil(deadline, t, "a poll advertising just the running job", func() bool {
		n, ok := lastAdvertised()

		return ok && n == 1
	})

	// WAITED FOR, NOT ASSUMED, and the reason is the ordering this test now also
	// asserts below. The escrow goes back AFTER the poll that carried the smaller
	// number, so between that advertisement going out and the poll returning
	// there is a window where the number is already low and the escrow is still
	// held. That window is the point — checking immediately here asserted the old
	// order, where the release came first.
	waitUntil(deadline, t, "the idle escrow to go back", func() bool { return len(l.Held()) == 0 })

	// AND IT WENT BACK IN THE RIGHT ORDER. The teardown's own rule is that the
	// last maxCapacity GitHub saw stays live until the session ends, so releasing
	// the escrow FIRST leaves a positive advertisement standing with nothing
	// behind it — GitHub assigns against it, and the assignment arrives to find
	// `held` empty and is declined. The poll that first lowers the number must
	// still be holding.
	mu.Lock()
	sent := append([]int(nil), advertised...)
	holding := append([]int(nil), heldWhenSent...)
	mu.Unlock()

	// AGAINST THE PEAK, not against the first poll: the advertisement climbs as
	// the job is taken (a discovery slot, then the assigned job), so the
	// withdrawal is the first drop below the highest number GitHub was ever told.
	var (
		peak    int
		lowered bool
	)

	for i, n := range sent {
		if n > peak {
			peak = n

			continue
		}

		if i > 0 && n < peak {
			lowered = true

			if holding[i] == 0 {
				t.Errorf("the escrow went back before, or with, the poll that lowered the "+
					"advertisement from %d to %d: advertised=%v held=%v",
					peak, n, sent, holding)
			}

			break
		}
	}

	if !lowered {
		t.Errorf("no poll advertised less than the peak of %d, so nothing was withdrawn: %v",
			peak, sent)
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
	//
	// EVERY launch is counted, not only the ones that see the flag. Running()
	// reports 1 as soon as the lease enters the listener's map, which happens
	// BEFORE the runner is called — so waiting on it and then setting the flag
	// can set it while the first job's launch is still in flight, and that launch
	// is then counted as one the drain permitted. The test failed for a reason
	// with nothing to do with draining.
	var launches, total atomic.Int32

	runner := &fakeRunner{onLaunch: func(int64) error {
		total.Add(1)

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

	// AND FOR ITS LAUNCH TO HAVE FINISHED, which Running() does not imply. The
	// flag must not be set while the pre-drain launch is still inside the runner.
	waitUntil(deadline, t, "the first launch to complete", func() bool { return total.Load() == 1 })

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

// A DRAIN THAT OVERRUNS DESTROYS NOTHING, AND DOES NOT END.
//
// This was TestADrainThatOverrunsItsBudgetDestroysWhatIsLeft, and it was correct
// about the behaviour it asserted: the budget expired, the listener stopped
// waiting, and the ordinary teardown destroyed whatever was still running. That
// is the defect. A job may run for days, elapsed time is not evidence that one
// stopped making progress, and GitHub does not requeue a job whose runner
// vanished after starting — so a timer was deciding to fail a build.
//
// Inverted rather than deleted, because the property worth pinning is the exact
// one that changed: passing the old threshold must now be a NON-event.
func TestADrainThatOverrunsDestroysNothingAndKeepsWaiting(t *testing.T) {
	const (
		setupBudget = 30 * time.Second
		// The threshold this drain sails past. It decides only when billet starts
		// SAYING the drain is long.
		reportAfter = 100 * time.Millisecond
	)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), setupBudget)
	defer cancelDeadline()

	var assigned atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// A REAL LONG POLL BLOCKS, AND SO DOES THIS — see slowPoll. The completion
		// never arrives: this is the job that outlives the drain.
		slowPoll()

		return nil, ErrNoMessage
	}

	var destroyed atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		destroyed.Add(1)

		return nil
	}}

	// HELD BY THE TEST, so the drain ends when this says so and not before.
	// stopsWithoutWaiting is pre-closed and would end it instantly, which is the
	// one thing that must not happen while the assertions below are being made.
	hurry := make(chan struct{})

	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session, WithRunner(runner),
		WithDrainGrace(reportAfter), WithHurrySignal(hurry), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	// PAST THE THRESHOLD. The old budget would have expired here.
	waitUntil(deadline, t, "the drain to report that it is running long",
		func() bool { return dl.saw("still draining") })

	// AND IT IS STILL GOING, which is what the whole change is about: the
	// threshold passed, billet said so, and nothing ended.
	select {
	case <-run.done:
		// WHICH ENDING, NAMED, because the two want opposite fixes: believing
		// everything had finished means a lease left `running` while its job was
		// still going, while any other exit means something still ends a drain on
		// a timer.
		if dl.saw(finishedEarly) {
			t.Fatalf("the drain ended because it believed everything had finished, so a "+
				"lease left `running` while its job was still going:\n%s", dl.String())
		}

		t.Fatalf("the drain ended on its own after %s; nothing may end it but a "+
			"completion or a second signal:\n%s", reportAfter, dl.String())
	case <-time.After(10 * reportAfter):
	}

	if n := destroyed.Load(); n != 0 {
		t.Errorf("an overrunning drain destroyed %d job(s); passing a reporting "+
			"threshold must not fail a build:\n%s", n, dl.String())
	}

	if got := l.Running(); got != 1 {
		t.Errorf("Running() = %d while the drain waits, want the job still there", got)
	}

	// NOW END IT, which with work in flight is the only way left.
	close(hurry)

	awaitRun(deadline, t, run)

	if !dl.saw(stoppedWaiting) {
		t.Errorf("the drain did not report that it stopped waiting:\n%s", dl.String())
	}

	// AND STILL NOTHING WAS DESTROYED. A second signal ends the WAIT; the guests
	// keep running, the node keeps holding them, and the next control plane
	// re-adopts their leases.
	if n := destroyed.Load(); n != 0 {
		t.Errorf("stopping the wait destroyed %d job(s); that is the emergency path, "+
			"not what a second signal does:\n%s", n, dl.String())
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

	// IT FINISHED, rather than needing to be told to stop. A promise sits in
	// `acquiring` and nothing will ever assign it, so a drain that waited for one
	// would still be waiting — and with no deadline left, waiting is forever.
	if !dl.saw(finishedEarly) {
		t.Errorf("the drain did not end by finding everything finished; it waited for a "+
			"promise that will never be assigned:\n%s", dl.String())
	}

	if dl.saw(stoppedWaiting) {
		t.Errorf("the drain had to be told to stop waiting, which means it was waiting "+
			"for a promise nothing will ever assign:\n%s", dl.String())
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

	if d.onPollCtx != nil {
		if err := d.onPollCtx(context.Background()); err != nil {
			return nil, err
		}
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
// merely ignores its context, the second signal is the only thing left that ends
// the drain, and it must not be swallowed.
//
// THIS TEST USED TO ASSERT A DEADLINE. It was TestADrainIsBounded…, and the
// drain was bounded — by a timer that then destroyed the jobs still running,
// and that timer is gone. What survives is the more important half: whatever
// the session is doing, an operator's second signal has to work.
//
// WHAT IT DOES NOT PROVE, and the limitation is real: a session that BLOCKS
// forever inside GetMessage is not interrupted by anything here, because the
// signal is only observed between calls. That is the same hole the teardown
// documents for Destroy — "NOTHING can bound a Destroy that ignores its context
// outright, and neither interface forbids one" — and the same answer applies.
// So the fake below returns promptly and this covers the signal being noticed,
// not a wedged transport being interrupted.
func TestASecondSignalEndsADrainEvenWhenTheSessionIgnoresItsContext(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
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

	var (
		dl        drainLog
		destroyed atomic.Int32
	)

	runner := &fakeRunner{onDestroy: func(int64) error {
		destroyed.Add(1)

		return nil
	}}

	hurry := make(chan struct{})

	l := NewListener(a, "billet-4vcpu-a", deafSession{fakeSession: inner},
		WithRunner(runner), WithDrainGrace(200*time.Millisecond),
		WithHurrySignal(hurry), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	cancel()

	awaitDrainStart(deadline, t, &dl, run)

	// THE DRAIN GRACE PASSES AND NOTHING HAPPENS, which is the half that changed.
	waitUntil(deadline, t, "the drain to report that it is running long",
		func() bool { return dl.saw("still draining") })

	if run.has() {
		t.Fatalf("the drain ended without a second signal:\n%s", dl.String())
	}

	close(hurry)

	select {
	case <-run.done:
		if err := run.get(); err == nil {
			t.Error("Run returned nil after its context was cancelled")
		}
	case <-deadline.Done():
		t.Fatal("a session that ignores its context swallowed the second signal, so " +
			"nothing an operator can do ends this drain")
	}

	if !dl.saw(stoppedWaiting) {
		t.Errorf("the drain stopped for some reason other than the second signal:\n%s",
			dl.String())
	}

	// AND THE JOB SURVIVED IT. Ending the wait is not authority to end the work.
	if n := destroyed.Load(); n != 0 {
		t.Errorf("the second signal destroyed %d job(s):\n%s", n, dl.String())
	}
}

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

	// IT NOTICED, rather than being told. This is the ordinary happy shutdown: the
	// work finished and the drain ended by itself.
	if !dl.saw(finishedEarly) {
		t.Errorf("the drain did not end by noticing its work had finished:\n%s", dl.String())
	}

	if dl.saw(stoppedWaiting) {
		t.Errorf("the drain had to be told to stop waiting for work that had already "+
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
		WithRunner(&fakeRunner{}), stopsWithoutWaiting(), dl.option())

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

// A SECOND SIGNAL STOPS THE WAITING. IT DOES NOT STOP THE JOBS.
//
// A drain waits for as long as the work takes. An operator who cannot wait needs
// a lever that ends the WAIT and still finishes the teardown properly —
// otherwise the only thing left is killing the process, which strands exactly
// the containers the drain existed to protect.
//
// WHAT DISTINGUISHES HURRYING FROM KILLING CHANGED, and so did this test. It used
// to assert the compute was DESTROYED, which was the only observable difference
// available while a hurry destroyed things. Now the difference is that the
// teardown billet OWES still runs — the destroy a completion asked for, the
// session close, the idle escrow — while a job that is still executing is left
// alone. Both kinds of work are staged here, because a test with only one cannot
// tell "tore down what it owed" from "tore down everything".
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

	var (
		mu        sync.Mutex
		destroyed []int64
	)

	runner := &fakeRunner{onDestroy: func(id int64) error {
		mu.Lock()
		destroyed = append(destroyed, id)
		mu.Unlock()

		return nil
	}}

	tore := func(id int64) bool {
		mu.Lock()
		defer mu.Unlock()

		for _, got := range destroyed {
			if got == id {
				return true
			}
		}

		return false
	}

	hurry := make(chan struct{})

	// An hour, so waiting it out is not something this test could do by accident.
	var dl drainLog

	l := NewListener(a, "billet-4vcpu-a", session,
		WithRunner(runner), WithDrainGrace(time.Hour), WithHurrySignal(hurry), dl.option())

	run := startRun(ctx, l)

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	// A DESTROY THIS LISTENER OWES, for a job GitHub already concluded. Nothing
	// about the drain excuses it: the completion asked for this teardown before
	// any of this began.
	l.mu.Lock()
	if l.cleanup == nil {
		l.cleanup = map[int64]*pendingCleanup{}
	}
	l.cleanup[22] = &pendingCleanup{job: Job{RequestID: 22}, at: time.Now().Add(time.Hour)}
	l.mu.Unlock()

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

	// THE TEARDOWN STILL RAN. This is the half that distinguishes hurrying from
	// killing the process: the destroy request 22's completion asked for is owed
	// whatever else is happening, and skipping it strands that container.
	if !tore(22) {
		t.Errorf("the hurried drain skipped the teardown it owed; request 22's compute "+
			"is still on its host:\n%s", dl.String())
	}

	// AND THE RUNNING JOB SURVIVED IT. Ending the wait is not authority to end the
	// work: the guest keeps going, the node keeps holding it, and the next control
	// plane re-adopts its lease.
	if tore(11) {
		t.Errorf("the hurried drain destroyed the job that was still running, which "+
			"fails a build GitHub does not requeue:\n%s", dl.String())
	}
}

// The hurry channel has to survive the trip from the control plane to every
// listener, or the second signal is a no-op the operator cannot see.
func TestTheHurryChannelReachesEveryListener(t *testing.T) {
	hurry := make(chan struct{})

	s := New(nil, nil, nil, "owner", nil, WithHurry(hurry))

	l := NewListener(nil, "tier", nil, s.listenerOpts(s.prov)...)
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

func TestTheCompletionLedgerReachesEveryListener(t *testing.T) {
	db := openState(t)
	s := New(nil, nil, nil, "owner", nil, WithCompletionLedger(db))
	l := NewListener(nil, "tier", nil, s.listenerOpts(s.prov)...)
	if l.completionStore != db {
		t.Fatal("the control plane's completion ledger never reached the listener")
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
		WithRunner(&fakeRunner{}), stopsWithoutWaiting(), dl.option())

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

// A DRAIN THAT OVERRUNS ALWAYS SAYS SO, however it was busy when the budget ran
// out.
//
// The branch that decides is at the TOP of the loop, so a call that fails BELOW
// it with the expired drain context has to send the loop back round. It did not:
// `cancelledWhileServing` answers false once draining, so every such failure
// took `stopping`, which returns ctx.Err() without a word. The listener returned
// in silence and the operator was told nothing.
//
// What is lost is not a log line for its own sake. It is the ONLY record of which
// jobs were still running when billet stopped waiting for them — work that is
// left alive on its hosts, holding capacity, for the next control plane to
// re-adopt. An operator who is told nothing has no idea any of it is out there.
//
// THE ORDERING IS STAGED, NOT SLEPT FOR, and the first version of this test got
// that wrong in a way worth recording: it cancelled and then slept inside
// onPoll, on the theory that the sleep consumed the drain's budget. It does not.
// `beginDrain` has not run at that point — the budget does not exist yet, and is
// created fresh when the loop next reaches the top. The test passed and killed
// its mutant for a reason its own comment misdescribed, which is the same defect
// as a test that asserts the wrong thing, one level up.
//
// So: let the cancellation reach `beginDrain` first, then block a LATER
// GetMessage on the drain context itself until it expires, and return its error.
// The failure then lands below the branch with the budget genuinely gone, every
// time and with no wall clock involved.
func TestADrainThatOverrunsWhileInsideACallStillReportsGivingUp(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	var dl drainLog

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		assigned atomic.Bool
		polls    atomic.Int64
		ready    = make(chan struct{})
	)

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// SLOW, because returning immediately spins the drain loop as fast as the
		// scheduler allows and starves the heartbeat — this file documents that
		// twice, and the consequence is a drain that reports itself FINISHED and
		// never writes the line this test asserts on.
		slowPoll()

		return nil, ErrNoMessage
	}

	// THE FAKE HOLDS THE CALL ON THE DRAIN'S OWN CONTEXT. onPollCtx receives the
	// context the loop is polling with, so once that context IS the drain's, this
	// blocks inside the call until the budget expires — which is precisely the
	// state the defect mishandled.
	session.onPollCtx = func(pollCtx context.Context) error {
		// UNTIL THE JOB IS RUNNING, this hook does nothing: the assignment has to
		// be delivered and launched by an ordinary poll first, and blocking here
		// would deadlock against the wait for it.
		select {
		case <-ready:
		default:
			return nil
		}

		if polls.Add(1) == 1 {
			// Cancelling sends the loop into beginDrain on its next pass. The
			// drain's budget does not exist until then, which is why a sleep here
			// could never consume it.
			cancel()

			return nil
		}

		// Draining now, so pollCtx IS the drain context. Block until the second
		// signal cancels it and hand back its error: the failure then lands BELOW
		// the loop's exit branch with the drain genuinely over, every time.
		//
		// IT USED TO WAIT FOR A BUDGET. That deadline is gone — a drain waits for
		// as long as the work takes — so waiting on it here hung forever, which is
		// exactly what the change means and a fair way to be told about it.
		<-pollCtx.Done()

		return pollCtx.Err()
	}

	hurry := make(chan struct{})

	l := NewListener(a, "billet-4vcpu-a", session, WithRunner(&fakeRunner{}),
		WithDrainGrace(200*time.Millisecond), WithHurrySignal(hurry), dl.option())

	run := startRun(ctx, l)

	// THE JOB MUST ACTUALLY BE RUNNING before the cancellation, or a fixture
	// regression degrades this into a test of an empty drain that reports itself
	// finished — which the assertion below would then have to accept.
	deadline, cancelDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelDeadline()

	waitUntil(deadline, t, "the job to be running", func() bool { return l.Running() == 1 })

	close(ready)

	// THE SIGNAL ARRIVES WHILE THE FAKE IS INSIDE THE CALL, which is the state
	// this test exists for: the drain ends underneath a poll rather than between
	// two of them.
	awaitDrainStart(deadline, t, &dl, run)
	close(hurry)

	if err := run.get(); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	// THAT EXIT SPECIFICALLY. This fixture holds a running job, so a drain that
	// reported everything had finished would be reporting something false, and
	// accepting either exit would let that pass.
	if !dl.saw(stoppedWaiting) {
		t.Errorf("the drain returned without reporting that it had stopped waiting, so "+
			"nothing told the operator which jobs were left running on their "+
			"hosts:\n%s", dl.String())
	}
}
