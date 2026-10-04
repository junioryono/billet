package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// handedOver is the teardown's line for a handoff, written once it has done the
// destroys it owed and left the session and the capacity alone.
const handedOver = "handed over: left the message session open"

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
	f := &handoffFixture{db: openState(t), session: &fakeSession{}, log: &drainLog{}}

	a, err := alloc.New(f.db, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	registerHost(t, a)
	f.a = a

	var assigned atomic.Bool

	f.session.onGet = func() (*Message, error) {
		slowPoll()

		if assigned.CompareAndSwap(false, true) {
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
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
		append([]Option{WithRunner(runner), WithDrainGrace(time.Hour), f.log.option()}, opts...)...)

	return f
}

// owe gives the listener a destroy it owes for a job GitHub already concluded.
func (f *handoffFixture) owe(id int64) {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()

	if f.l.cleanup == nil {
		f.l.cleanup = map[int64]*pendingCleanup{}
	}
	f.l.cleanup[id] = &pendingCleanup{job: Job{RequestID: id}, at: time.Now().Add(time.Hour)}
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

	run := startRun(ctx, f.l)

	waitUntil(deadline, t, "the job to be running", func() bool { return f.l.Running() == 1 })
	f.owe(22)

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

	// AND THE RUNNING JOB'S LEASE IS STILL CHARGED, for the successor to adopt.
	usage, err := f.a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Leases == 0 {
		t.Error("the handoff handed back the running job's capacity")
	}
}

// A SEALED STOP IS THE DEPLOYMENT LEAVING, AND IT STILL DRAINS: an operator who
// sealed asked for exactly that, and the drain closes the session when it ends.
func TestASealedStopStillDrains(t *testing.T) {
	t.Parallel()

	hurry := make(chan struct{})
	f := newHandoffFixture(t, WithRestartHandoff(), WithHurrySignal(hurry))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deadline, endDeadline := context.WithTimeout(t.Context(), 30*time.Second)
	defer endDeadline()

	run := startRun(ctx, f.l)

	waitUntil(deadline, t, "the job to be running", func() bool { return f.l.Running() == 1 })

	if _, err := f.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Reason: "leaving", Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	cancel()
	awaitDrainStart(deadline, t, f.log, run)

	// The job never completes, so the second signal ends the wait.
	close(hurry)
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
