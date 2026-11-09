package server

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/state"
)

// handedOver is the teardown's line for a handoff, written once it has done the
// destroys it owed and left the session and the capacity alone.
const handedOver = "handed over: closed no message session"

// handoffFixture is a listener with one running job that never completes and
// one destroy it owes, over a ledger a test can seal.
type handoffFixture struct {
	db      *state.DB
	a       *alloc.Allocator
	l       *Listener
	session *fakeSession
	log     *drainLog

	mu        sync.Mutex
	destroyed []int64

	hurry     chan struct{}
	hurryOnce sync.Once

	// hold makes the next poll block until the run is cancelled, and held says
	// it is blocked, so a test can change what the listener holds while no poll
	// is deciding anything.
	hold atomic.Bool
	held chan struct{}
}

// hurryUp ends a drain's wait; safe to call more than once.
func (f *handoffFixture) hurryUp() { f.hurryOnce.Do(func() { close(f.hurry) }) }

// start runs the listener and registers a cleanup that ends any drain, cancels
// and joins Run before the ledger is closed, so a regression into a drain cannot
// leave the listener running against a closed database.
func (f *handoffFixture) start(ctx context.Context, t *testing.T, cancel context.CancelFunc) *runResult {
	t.Helper()

	run := startRun(ctx, f.l)
	t.Cleanup(func() {
		f.hurryUp()
		cancel()

		select {
		case <-run.done:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return during cleanup")
		}
	})

	return run
}

func (f *handoffFixture) tore(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, got := range f.destroyed {
		if got == id {
			return true
		}
	}

	return false
}

func newHandoffFixture(t *testing.T, opts ...Option) *handoffFixture {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-handoff")}
	f := &handoffFixture{db: openState(t), session: &fakeSession{}, log: &drainLog{},
		hurry: make(chan struct{}), held: make(chan struct{}, 1)}

	a, err := alloc.New(f.db, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	registerHost(t, a)
	f.a = a

	var assigned atomic.Bool

	f.session.onPollCtx = func(ctx context.Context) error {
		if !f.hold.Load() {
			return nil
		}

		select {
		case f.held <- struct{}{}:
		default:
		}
		<-ctx.Done()

		return ctx.Err()
	}

	f.session.onGet = func() (*Message, error) {
		slowPoll()

		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []dispatch.Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// No completion ever: a drain would wait for this job for as long as it ran.
		return nil, ErrNoMessage
	}

	runner := &fakeRunner{onDestroy: func(id int64) error {
		f.mu.Lock()
		f.destroyed = append(f.destroyed, id)
		f.mu.Unlock()

		return nil
	}}

	f.l = NewListener(a, tiers[0].Label, f.session,
		append([]Option{WithRunner(runner), WithDrainGrace(time.Hour), WithHurrySignal(f.hurry),
			f.log.option()}, opts...)...)

	return f
}

// owe gives the listener a destroy it owes for a job GitHub already concluded.
func (f *handoffFixture) owe(id int64) {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()

	if f.l.cleanup == nil {
		f.l.cleanup = map[int64]*pendingCleanup{}
	}
	f.l.cleanup[id] = &pendingCleanup{job: dispatch.Job{RequestID: id}, at: time.Now().Add(time.Hour)}
}

// A STOP WHILE THE DEPLOYMENT ADMITS WORK IS A RESTART, AND IT DOES NOT WAIT FOR
// THE JOBS (#365). The job below never completes, so a drain would never end; the
// handoff returns at once, leaves the job running for the next control plane to
// re-adopt, still performs the destroy it owes, and does not close the message
// session, because closing it is what lets GitHub lower the advertisement and
// orphan the jobs it re-offers meanwhile (#368).
func TestAStopWhileAdmittingHandsOverWithoutWaiting(t *testing.T) {
	t.Parallel()

	f := newHandoffFixture(t, WithRestartHandoff())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, endDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer endDeadline()

	run := f.start(ctx, t, cancel)

	waitUntil(deadline, t, "the job to be running", func() bool { return f.l.Running() == 1 })
	f.owe(22)

	// A POLL HELD OPEN while the escrow is staged, so the loop cannot hand that
	// escrow back as surplus before the stop: what this test sees released is the
	// stop's doing.
	f.hold.Store(true)
	select {
	case <-f.held:
	case <-deadline.Done():
		t.Fatal("the listener never polled again")
	}

	f.l.mu.Lock()
	runningLease := f.l.running[11]
	f.l.mu.Unlock()
	if runningLease == nil {
		t.Fatal("request 11 has no running lease")
	}

	// HELD ESCROW, which releaseAll would hand back and a handoff must not.
	if err := f.l.refillEscrowUngated(deadline, f.l.capacity()+1, 1); err != nil {
		t.Fatalf("staging held escrow: %v", err)
	}
	held := f.l.Held()
	if len(held) == 0 {
		t.Fatal("the fixture staged no held escrow, so the release could not be observed")
	}

	cancel()
	awaitRun(deadline, t, run)

	if f.log.saw(beganDraining) {
		t.Errorf("an unsealed stop drained instead of handing over:\n%s", f.log.String())
	}
	if !f.log.saw(handedOver) {
		t.Errorf("the handoff never reached its teardown:\n%s", f.log.String())
	}
	if n := f.session.closes(); n != 0 {
		t.Errorf("the handoff closed its message session %d time(s); it must stay open for the "+
			"successor", n)
	}
	if f.tore(11) {
		t.Errorf("the handoff destroyed the job still running:\n%s", f.log.String())
	}
	if !f.tore(22) {
		t.Errorf("the handoff skipped the destroy it owed for a concluded job:\n%s", f.log.String())
	}

	// AND THE RUNNING JOB'S OWN LEASE IS STILL CHARGED, for the successor to adopt,
	// and so is the held escrow.
	for _, h := range append([]*alloc.Lease{runningLease}, held...) {
		got, err := f.a.Lease(t.Context(), h.ID)
		if err != nil {
			t.Fatalf("Lease %s: %v", h.ID, err)
		}
		if got.Phase == alloc.PhaseDone || got.Phase == alloc.PhaseFailed {
			t.Errorf("the handoff released held escrow %s (%s); it is reclaimed when its "+
				"renewal stops, not handed back under a session left open", h.ID, got.Phase)
		}
	}
}

// oweRecorded gives the listener a destroy it owes for a completion the ledger
// holds: one carrying GitHub's result, on a listener whose completion store was
// set before it started.
func (f *handoffFixture) oweRecorded(id int64) {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()

	if f.l.cleanup == nil {
		f.l.cleanup = map[int64]*pendingCleanup{}
	}
	f.l.cleanup[id] = &pendingCleanup{job: dispatch.Job{RequestID: id, Result: "succeeded", CompletionID: 7},
		at: time.Now().Add(time.Hour)}
}

// A HANDOFF LEAVES A RECORDED COMPLETION'S DESTROY TO THE NEXT PROCESS, which
// restores it from the ledger, rather than wait on a node for it; a destroy the
// ledger does not hold is still done before it stops.
func TestAHandoffLeavesARecordedCompletionsDestroyToTheNextProcess(t *testing.T) {
	t.Parallel()

	f := newHandoffFixture(t, WithRestartHandoff())
	f.l.completionStore = f.db

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, endDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer endDeadline()

	run := f.start(ctx, t, cancel)

	waitUntil(deadline, t, "the job to be running", func() bool { return f.l.Running() == 1 })
	f.oweRecorded(33)
	f.owe(22)

	cancel()
	awaitRun(deadline, t, run)

	if !f.log.saw(handedOver) {
		t.Fatalf("the stop did not hand over:\n%s", f.log.String())
	}
	if f.tore(33) {
		t.Errorf("the handoff waited on the destroy of a completion the ledger holds:\n%s", f.log.String())
	}
	if !f.log.saw("leaving a finished job's destroy to the next control plane") {
		t.Errorf("the destroy left to the next process was not said:\n%s", f.log.String())
	}
	if !f.tore(22) {
		t.Errorf("the handoff skipped a destroy the ledger does not hold:\n%s", f.log.String())
	}
}

// WHAT A HANDOFF LEAVES, THE NEXT PROCESS DOES. A completion recorded the way a
// message records it, whose destroy failed once, is left by the handoff's
// teardown with its pending row and lease in the ledger; a fresh listener over
// that ledger restores it, destroys the compute and releases the lease.
func TestTheNextProcessDestroysWhatAHandoffLeft(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	p.l.completionStore = openState(t)
	runner := p.launchedFor11(t)

	started := job11
	started.RunnerID, started.RunnerName = 77, runner.RunnerName
	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []dispatch.Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11: %v", err)
	}

	fake, ok := p.l.runner.(*fakeRunner)
	if !ok {
		t.Fatalf("the pool's runner is a %T", p.l.runner)
	}
	destroy := fake.onDestroy
	nodeBusy := true
	fake.onDestroy = func(id int64) error {
		if nodeBusy {
			return errors.New("the node is still settling this job's cache")
		}

		return destroy(id)
	}

	finished := started
	finished.Result = "Succeeded"
	if err := p.l.handle(t.Context(), &Message{MessageID: 3, Completed: []dispatch.Job{finished},
		Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
		t.Fatalf("complete job 11: %v", err)
	}
	p.l.mu.Lock()
	owed := p.l.cleanup[11]
	p.l.mu.Unlock()
	if owed == nil || owed.job.Result == "" {
		t.Fatalf("the failed destroy left no obligation carrying the result: %+v", owed)
	}

	nodeBusy = false
	p.l.handingOff = true
	p.l.destroyAll(t.Context(), false, nil)
	if slices.Contains(p.destroyed, 11) {
		t.Fatal("the handoff destroyed job 11's compute itself")
	}

	rows, err := p.l.completionStore.PendingCompletions(t.Context(), p.l.tier)
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	if !slices.ContainsFunc(rows, func(row state.PendingCompletion) bool {
		return row.RequestID == 11 && !row.Retired && row.LeaseID == runner.LeaseID
	}) {
		t.Fatalf("the ledger does not hold job 11's completion with its lease for the next process: %+v", rows)
	}

	p.restartHeld(t, 0)
	p.l.retryCleanup(t.Context())

	if !slices.Contains(p.destroyed, 11) {
		t.Fatalf("the next process did not destroy what the handoff left: destroyed %v", p.destroyed)
	}
	// A RELEASED LEASE IS NO LONGER A LIVE ONE, which is how the ledger answers.
	if lease, err := p.a.Lease(t.Context(), runner.LeaseID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("job 11's lease after the next process destroyed its compute: %+v, %v; want it released",
			lease, err)
	}
}

// A SEALED STOP STILL DESTROYS A RECORDED COMPLETION: the deployment is leaving,
// and no next process will restore it.
func TestASealedStopStillDestroysARecordedCompletion(t *testing.T) {
	t.Parallel()

	f := newHandoffFixture(t, WithRestartHandoff())
	f.l.completionStore = f.db

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, endDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer endDeadline()

	run := f.start(ctx, t, cancel)

	waitUntil(deadline, t, "the job to be running", func() bool { return f.l.Running() == 1 })
	f.oweRecorded(33)
	if _, err := f.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Reason: "leaving", Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	cancel()
	awaitDrainStart(deadline, t, f.log, run)
	f.hurryUp()
	awaitRun(deadline, t, run)

	if !f.tore(33) {
		t.Errorf("a sealed stop left a recorded completion's destroy undone:\n%s", f.log.String())
	}
}

// A SEALED STOP IS THE DEPLOYMENT LEAVING, AND IT STILL DRAINS: an operator who
// sealed asked for exactly that, and the drain closes the session when it ends.
func TestASealedStopStillDrains(t *testing.T) {
	t.Parallel()

	f := newHandoffFixture(t, WithRestartHandoff())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, endDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer endDeadline()

	run := f.start(ctx, t, cancel)

	waitUntil(deadline, t, "the job to be running", func() bool { return f.l.Running() == 1 })

	if _, err := f.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Reason: "leaving", Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	cancel()
	awaitDrainStart(deadline, t, f.log, run)

	// The job never completes, so the second signal ends the wait.
	f.hurryUp()
	awaitRun(deadline, t, run)

	if f.log.saw(handedOver) {
		t.Errorf("a sealed stop handed over:\n%s", f.log.String())
	}
	if n := f.session.closes(); n != 1 {
		t.Errorf("a sealed stop closed its session %d time(s), want 1", n)
	}
	if f.tore(11) {
		t.Errorf("the drain destroyed the job still running:\n%s", f.log.String())
	}
}

// A STOP THAT CANNOT READ ADMISSION IS NOT ENTITLED TO CALL ITSELF A RESTART.
func TestAStopThatCannotReadAdmissionDrains(t *testing.T) {
	t.Parallel()

	f := newHandoffFixture(t, WithRestartHandoff())

	if err := f.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if f.l.handsOff(ctx) {
		t.Error("a stop that could not read admission chose to hand over")
	}
	if !f.log.saw("drains rather than handing over") {
		t.Errorf("the unreadable admission was not reported:\n%s", f.log.String())
	}
}

// WITHOUT THE OPTION NOTHING CHANGES: every other caller of the listener keeps
// the drain it was written against.
func TestAListenerWithoutTheOptionNeverHandsOver(t *testing.T) {
	t.Parallel()

	f := newHandoffFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if f.l.handsOff(ctx) {
		t.Error("a listener without WithRestartHandoff chose to hand over")
	}
}

// The option has to survive the trip from the control plane to every listener.
func TestTheStopHandoffReachesEveryListener(t *testing.T) {
	t.Parallel()

	s := New(nil, nil, nil, "owner", nil, WithStopHandoff())

	l := NewListener(nil, "tier", nil, s.listenerOpts(s.prov)...)
	if !l.restartHandoff {
		t.Fatal("the stop handoff never reached the listener")
	}
}

// A CANCELLATION THAT ENDS RUN BEFORE ITS LOOP IS DECIDED THE SAME WAY, so a stop
// arriving during startup is not closed and released as if the deployment were
// leaving.
func TestAStopDuringStartupIsDecidedLikeAnyOther(t *testing.T) {
	t.Parallel()

	live := newHandoffFixture(t, WithRestartHandoff())
	live.l.noteStop(t.Context())
	if live.l.handingOff {
		t.Error("a startup failure with no stop asked was taken for a handoff")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	f := newHandoffFixture(t, WithRestartHandoff())
	f.l.noteStop(ctx)
	if !f.l.handingOff {
		t.Error("a stop during startup while admitting was not a handoff")
	}
}

// BOTH WAYS RUN CAN END BEFORE ITS LOOP ASK THE SAME QUESTION. The test above calls
// noteStop directly, so this is what notices a call site removed from Run.
func TestRunDecidesAStopAtEveryEarlyReturn(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("listener.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)

	for _, call := range []string{
		"if err := l.refreshAdoptedCapacity(ctx); err != nil {\n\t\tl.noteStop(ctx)",
		"if err := l.reconcilePool(ctx, l.observed.TotalAssignedJobs); err != nil {\n\t\t\tl.noteStop(ctx)",
	} {
		if !strings.Contains(source, call) {
			t.Errorf("Run no longer decides a stop before returning here:\n%s", call)
		}
	}
}
