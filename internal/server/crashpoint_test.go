package server

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// THE INVARIANT EVERY CRASH POINT IS CHECKED AGAINST.
//
// A control-plane restart can land between any two steps of a job's life —
// offer, acquisition, assignment, launch, start, completion, persistence,
// acknowledgement, session close. After ANY of them, every job must be in exactly
// one of two states:
//
//   - still queued at GitHub, with nothing local claiming to be running it, or
//   - backed by exactly ONE fenced compute obligation.
//
// What must never happen is a job that is neither (lost), or both (double
// launched), or one whose capacity was released while its compute was live.
//
// WHY THIS SUITE EXISTS AT ALL. Rollouts make replacing the controller an ordinary
// operation, and every one of these boundaries is now crossed on purpose rather
// than by accident. A restart that loses a job used to be a rare failure; it is
// about to become a scheduled one.
//
// WHAT IT CANNOT PROVE, said here rather than discovered later: this is billet's
// own fake, so it establishes what BILLET does across a restart. Whether GitHub
// redelivers a given message to a NEW session is a fact about GitHub, and the
// only thing that can settle it is a live conformance test against a real
// organization — see internal/integration. Nothing here may be read as evidence
// about that.

// crashFixture is a listener plus the ledger it shares with its successor.
type crashFixture struct {
	tiers []config.Tier
	alloc *alloc.Allocator

	mu        sync.Mutex
	launched  []int64
	destroyed []int64
	// failLaunch makes a launch fail, which is the ambiguous case custody exists
	// for.
	failLaunch map[int64]error
}

func newCrashFixture(t *testing.T) *crashFixture {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}

	f := &crashFixture{tiers: tiers, failLaunch: map[int64]error{}}
	f.alloc = newAllocator(t, alloc.Limits{MaxVCPU: 64, MaxMemory: 512 * config.GiB}, tiers,
		alloc.WithLeaseTTL(outlivesTheDrain))

	return f
}

func (f *crashFixture) runner() *fakeRunner {
	return &fakeRunner{
		onLaunch: func(requestID int64) error {
			f.mu.Lock()
			defer f.mu.Unlock()

			if err := f.failLaunch[requestID]; err != nil {
				return err
			}

			f.launched = append(f.launched, requestID)

			return nil
		},
		onDestroy: func(id int64) error {
			f.mu.Lock()
			defer f.mu.Unlock()

			f.destroyed = append(f.destroyed, id)

			return nil
		},
	}
}

func (f *crashFixture) launches(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	var n int

	for _, got := range f.launched {
		if got == id {
			n++
		}
	}

	return n
}

// obligations is how many non-terminal leases the ledger holds for one request.
//
// THE WHOLE ASSERTION IN ONE NUMBER. Zero means the job is GitHub's problem and
// billet claims nothing; one means exactly one fenced obligation; more than one
// is a double-book, which is the failure the escrow exists to prevent.
func (f *crashFixture) obligations(t *testing.T, requestID int64) int {
	t.Helper()

	candidates, err := f.alloc.ForceDestroyCandidates(t.Context(), "", "")
	if err != nil {
		t.Fatalf("read what the ledger is holding: %v", err)
	}

	var n int

	for i := range candidates {
		if candidates[i].SchedulerRequest == requestID {
			n++
		}
	}

	return n
}

// escrowed is how many leases are held with no job behind them.
func (f *crashFixture) escrowed(t *testing.T) int {
	t.Helper()

	q, err := f.alloc.Quiescence(t.Context())
	if err != nil {
		t.Fatalf("Quiescence: %v", err)
	}

	return q.Escrowed()
}

// A RESTART BETWEEN EVERY PAIR OF STEPS LEAVES ONE OBLIGATION OR NONE.
//
// The table walks the boundaries a controller can stop at. Each case builds a
// listener, drives it to that boundary, stops it the way a crash would, and then
// asks the LEDGER — not the dead process's memory — what the deployment is
// holding.
func TestARestartAtEveryBoundaryLeavesOneObligationOrNone(t *testing.T) {
	const requestID = 7

	cases := []struct {
		name string
		// drive takes the listener up to the boundary.
		drive func(t *testing.T, f *crashFixture, l *Listener, session *fakeSession)
		// wantLaunches is how many times the compute may have been created.
		wantLaunches int
		// wantObligations is what the ledger must hold afterwards.
		wantObligations int
	}{{
		name: "before the offer is acquired",
		drive: func(_ *testing.T, _ *crashFixture, l *Listener, _ *fakeSession) {
			// Escrow exists; nothing has been promised to GitHub.
			_ = l
		},
		wantLaunches:    0,
		wantObligations: 0,
	}, {
		name: "after acquiring and before the assignment",
		drive: func(t *testing.T, _ *crashFixture, l *Listener, _ *fakeSession) {
			t.Helper()

			if err := l.acquire(t.Context(), []Job{{RequestID: requestID}}); err != nil {
				t.Fatalf("acquire: %v", err)
			}
		},
		// AN ACQUISITION IS A PROMISE TO GITHUB WITH NO COMPUTE BEHIND IT. Nothing
		// has launched, and the lease is still escrow — so the ledger holds no
		// running obligation for this request, and the promise itself is what the
		// next controller inherits.
		wantLaunches:    0,
		wantObligations: 0,
	}, {
		name: "after the assignment and before the launch",
		drive: func(t *testing.T, _ *crashFixture, l *Listener, _ *fakeSession) {
			t.Helper()

			if err := l.acquire(t.Context(), []Job{{RequestID: requestID}}); err != nil {
				t.Fatalf("acquire: %v", err)
			}

			if _, _, err := l.assign(t.Context(), Job{RequestID: requestID}); err != nil {
				t.Fatalf("assign: %v", err)
			}
		},
		wantLaunches: 0,
		// ONE OBLIGATION, AND IT IS THE RIGHT NUMBER. The lease is assigned to this
		// request; nothing has started, but the capacity is committed to it and the
		// next controller must not hand the same slot to a second job.
		wantObligations: 1,
	}, {
		name: "after the launch",
		drive: func(t *testing.T, _ *crashFixture, l *Listener, _ *fakeSession) {
			t.Helper()

			if err := l.acquire(t.Context(), []Job{{RequestID: requestID}}); err != nil {
				t.Fatalf("acquire: %v", err)
			}

			lease, _, err := l.assign(t.Context(), Job{RequestID: requestID})
			if err != nil {
				t.Fatalf("assign: %v", err)
			}

			if err := l.launch(t.Context(), lease, Job{RequestID: requestID}); err != nil {
				t.Fatalf("launch: %v", err)
			}
		},
		wantLaunches:    1,
		wantObligations: 1,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrashFixture(t)
			session := &fakeSession{}

			l := NewListener(f.alloc, f.tiers[0].Label, session, WithRunner(f.runner()),
				WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

			if err := l.prepareEscrow(t.Context()); err != nil {
				t.Fatalf("prepare escrow: %v", err)
			}

			tc.drive(t, f, l, session)

			// THE CRASH. Nothing graceful runs: no teardown, no session close, no
			// release. What survives is exactly what is in the ledger, which is the
			// only thing a successor can read.
			crash(l)

			if got := f.launches(requestID); got != tc.wantLaunches {
				t.Errorf("the compute for request %d was created %d times, want %d",
					requestID, got, tc.wantLaunches)
			}

			if got := f.obligations(t, requestID); got != tc.wantObligations {
				t.Errorf("the ledger holds %d obligation(s) for request %d, want %d",
					got, requestID, tc.wantObligations)
			}
		})
	}
}

// A SUCCESSOR NEVER LAUNCHES A SECOND CONTAINER FOR A JOB THAT IS ALREADY
// RUNNING.
//
// This is the double-launch half of the invariant, and it is the expensive one:
// two runners for one job means GitHub hands the work to one of them and the
// other holds a slot forever, while the ledger believes both are real.
func TestASuccessorDoesNotRelaunchAJobAlreadyRunning(t *testing.T) {
	const requestID = 7

	f := newCrashFixture(t)

	first := NewListener(f.alloc, f.tiers[0].Label, &fakeSession{}, WithRunner(f.runner()),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatalf("prepare escrow: %v", err)
	}

	if err := first.acquire(t.Context(), []Job{{RequestID: requestID}}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	lease, _, err := first.assign(t.Context(), Job{RequestID: requestID})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	if err := first.launch(t.Context(), lease, Job{RequestID: requestID}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	crash(first)

	// The successor inherits the ledger and nothing else.
	second := NewListener(f.alloc, f.tiers[0].Label, &fakeSession{}, WithRunner(f.runner()),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	if err := second.refreshAdoptedCapacity(t.Context()); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// GITHUB REDELIVERS THE ASSIGNMENT, which is the case that matters: an
	// unacknowledged message comes back, and the successor must recognise the
	// request rather than treating it as new work.
	if _, _, err := second.assign(t.Context(), Job{RequestID: requestID}); err != nil &&
		!errors.Is(err, alloc.ErrConflict) {
		// A refusal is a correct answer here. What is not correct is a second
		// launch, which the assertion below catches either way.
		t.Logf("the successor declined the redelivered assignment: %v", err)
	}

	if got := f.launches(requestID); got != 1 {
		t.Errorf("the compute for request %d was created %d times across a restart; a job "+
			"whose runner already exists must never get a second one", requestID, got)
	}

	if got := f.obligations(t, requestID); got != 1 {
		t.Errorf("the ledger holds %d obligation(s) for request %d after a restart, want "+
			"exactly 1", got, requestID)
	}
}

// A CRASH NEVER RELEASES CAPACITY WHOSE COMPUTE IS LIVE.
//
// The other direction, and the quieter failure: a lease terminalised while its
// container runs lets another tier escrow that slot, and the host is overcommitted
// with nothing to notice.
func TestACrashLeavesRunningCapacityCharged(t *testing.T) {
	const requestID = 7

	f := newCrashFixture(t)

	l := NewListener(f.alloc, f.tiers[0].Label, &fakeSession{}, WithRunner(f.runner()),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	if err := l.prepareEscrow(t.Context()); err != nil {
		t.Fatalf("prepare escrow: %v", err)
	}

	if err := l.acquire(t.Context(), []Job{{RequestID: requestID}}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	lease, _, err := l.assign(t.Context(), Job{RequestID: requestID})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	if err := l.launch(t.Context(), lease, Job{RequestID: requestID}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	crash(l)

	held, err := f.alloc.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the lease after a crash: %v", err)
	}

	if held.Phase.Terminal() {
		t.Errorf("a crash terminalised lease %s as %s while its container was running; "+
			"another tier can now escrow that slot", held.ID, held.Phase)
	}

	if len(f.destroyed) != 0 {
		t.Errorf("a crash destroyed %v; nothing about a process dying is evidence that the "+
			"work on its hosts should end", f.destroyed)
	}
}

// AN AMBIGUOUS LAUNCH LEAVES EXACTLY ONE OBLIGATION, NOT ZERO.
//
// A launch that failed without proof its compute is gone is the case custody
// exists for: the container may be running, so the capacity stays charged. A
// crash at that instant must not turn "may exist" into "does not".
func TestACrashDuringAnAmbiguousLaunchKeepsTheObligation(t *testing.T) {
	const requestID = 7

	f := newCrashFixture(t)
	f.failLaunch[requestID] = fmt.Errorf("%w: the node did not answer", ErrCustody)

	l := NewListener(f.alloc, f.tiers[0].Label, &fakeSession{}, WithRunner(f.runner()),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	if err := l.prepareEscrow(t.Context()); err != nil {
		t.Fatalf("prepare escrow: %v", err)
	}

	if err := l.acquire(t.Context(), []Job{{RequestID: requestID}}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	lease, _, err := l.assign(t.Context(), Job{RequestID: requestID})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	// CUSTODY IS A HANDLED OUTCOME, SO launch RETURNS NIL. That is deliberate and
	// worth stating here, because the obvious assertion is the wrong one: an error
	// would mean the capacity goes back and GitHub reassigns, which is exactly what
	// must NOT happen when compute may exist. The listener keeps the lease and the
	// node holds it.
	if err := l.launch(t.Context(), lease, Job{RequestID: requestID}); err != nil {
		t.Fatalf("an ambiguous launch reported an error, which hands the capacity back "+
			"while a container may be running: %v", err)
	}

	// AND THE AMBIGUOUS PATH WAS ACTUALLY TAKEN. Without this the test passes
	// identically against a fixture that stopped injecting the failure — at which
	// point it would be measuring an ordinary launch and saying nothing about the
	// case it is named for.
	if got := f.launches(requestID); got != 0 {
		t.Fatalf("the runner completed %d launch(es) for request %d; this test needs the "+
			"launch to fail ambiguously", got, requestID)
	}

	crash(l)

	if got := f.obligations(t, requestID); got != 1 {
		t.Errorf("the ledger holds %d obligation(s) after an ambiguous launch, want 1: "+
			"compute that MAY exist keeps its capacity charged until something proves "+
			"otherwise", got)
	}
}

// ESCROW A CRASH LEAVES BEHIND IS RECOVERABLE, NOT LOST.
//
// A controller that dies holding escrow leaves leases nothing is renewing. They
// are not released — releasing on a crash is how a slot gets sold twice — so the
// reaper is what recovers them, and the assertion is that they are still THERE to
// be recovered.
func TestACrashLeavesEscrowForTheReaperRatherThanReleasingIt(t *testing.T) {
	f := newCrashFixture(t)

	l := NewListener(f.alloc, f.tiers[0].Label, &fakeSession{}, WithRunner(f.runner()),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	if err := l.prepareEscrow(t.Context()); err != nil {
		t.Fatalf("prepare escrow: %v", err)
	}

	before := f.escrowed(t)
	if before == 0 {
		t.Fatal("the fixture escrowed nothing, so this test would pass vacuously")
	}

	crash(l)

	if after := f.escrowed(t); after != before {
		t.Errorf("a crash changed the escrow from %d to %d; it is neither released nor "+
			"lost — the reaper reclaims it once nothing renews it", before, after)
	}
}

// crash stops a listener the way a killed process does: nothing graceful runs.
//
// NOT A SHUTDOWN. A shutdown closes the session, performs the destroys it owes
// and releases what it can; a crash does none of that, and the whole point of
// this suite is what the LEDGER says afterwards rather than what an orderly
// teardown would have tidied up.
func crash(l *Listener) {
	l.seal()

	l.mu.Lock()
	l.held = nil
	l.running = map[int64]*alloc.Lease{}
	l.acquiring = map[int64]*promise{}
	l.cleanup = map[int64]*pendingCleanup{}
	l.mu.Unlock()
}
