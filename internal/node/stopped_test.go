package node

import (
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

type stoppedFixture struct {
	r    *Runner
	p    *fakeProvider
	name string
	now  time.Time
}

// newStoppedFixture launches one job's guest the way a pool member is launched,
// with the runner's clock under the test's control.
func newStoppedFixture(t *testing.T) *stoppedFixture {
	t.Helper()

	f := &stoppedFixture{p: &fakeProvider{kind: config.ProviderDocker},
		now: time.Date(2026, 9, 30, 16, 40, 0, 0, time.UTC)}

	a, host := newAllocatorWithHost(t)
	f.r = New(a, host, &fakeJIT{setID: 7}, f.p, nil)
	f.r.now = func() time.Time { return f.now }

	if err := f.r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(f.p.launched) != 1 {
		t.Fatalf("launched %d guests, want 1", len(f.p.launched))
	}

	f.name = f.p.launched[0].Name

	return f
}

func (f *stoppedFixture) sweepAt(t *testing.T, after time.Duration) {
	t.Helper()

	f.now = time.Date(2026, 9, 30, 16, 40, 0, 0, time.UTC).Add(after)
	if err := f.r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep at +%s: %v", after, err)
	}
}

func TestADrainStopsWaitingOnAGuestTwoSweepsSawStopped(t *testing.T) {
	t.Parallel()

	f := newStoppedFixture(t)
	f.p.stop(f.name)

	f.sweepAt(t, 0)

	if !f.r.Holding() {
		t.Fatal("let go after one observation of a stopped guest")
	}

	f.sweepAt(t, strayGrace)

	if f.r.Holding() {
		t.Fatalf("still holding a guest two sweeps %s apart saw stopped; a drain would wait on it forever",
			strayGrace)
	}

	// THE ENTRY IS KEPT FOR ITS DESTROY: nothing was destroyed by noticing, and
	// the destroy the plane sends when the job's completion arrives still finds
	// the guest and removes it.
	if len(f.p.destroyed) != 0 {
		t.Fatalf("noticing a stopped guest destroyed %v", f.p.destroyed)
	}

	if err := f.r.Destroy(t.Context(), 11); err != nil {
		t.Fatalf("Destroy after the drain let go: %v", err)
	}

	if !slices.Contains(f.p.destroyed, "instance-"+f.name) {
		t.Fatalf("the later destroy did not reach the guest; destroyed %v", f.p.destroyed)
	}
}

// Asked long after one sweep, with no second observation, Holding still holds:
// the grace is proved by a sweep, never by the clock at the moment it is asked.
func TestOneObservationIsNotAProofHoweverLongAgo(t *testing.T) {
	t.Parallel()

	f := newStoppedFixture(t)
	f.p.stop(f.name)

	f.sweepAt(t, 0)
	f.now = f.now.Add(10 * strayGrace)

	if !f.r.Holding() {
		t.Fatal("let go on one observation and the passage of time")
	}
}

func TestTwoStoppedObservationsInsideTheGraceAreNotAProof(t *testing.T) {
	t.Parallel()

	f := newStoppedFixture(t)
	f.p.stop(f.name)

	f.sweepAt(t, 0)
	f.sweepAt(t, strayGrace-time.Second)

	if !f.r.Holding() {
		t.Fatalf("let go on two observations %s apart", strayGrace-time.Second)
	}
}

// A guest seen running between two stopped observations starts the grace again.
func TestAGuestSeenRunningAgainStartsTheGraceAgain(t *testing.T) {
	t.Parallel()

	f := newStoppedFixture(t)
	f.p.stop(f.name)
	f.sweepAt(t, 0)

	f.p.live[f.name].Running = true
	f.sweepAt(t, time.Minute)

	f.p.stop(f.name)
	f.sweepAt(t, strayGrace)

	if !f.r.Holding() {
		t.Fatal("let go on observations an intervening running one should have separated")
	}

	f.sweepAt(t, time.Minute+2*strayGrace)

	if f.r.Holding() {
		t.Fatal("still holding after two fresh stopped observations a grace apart")
	}
}

func TestARunningGuestIsHeldWhateverTheSweeps(t *testing.T) {
	t.Parallel()

	f := newStoppedFixture(t)

	f.sweepAt(t, 0)
	f.sweepAt(t, 10*strayGrace)

	if !f.r.Holding() {
		t.Fatal("stopped holding a guest that is running")
	}
}
