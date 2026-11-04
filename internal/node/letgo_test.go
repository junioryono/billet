package node

import (
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// WHAT A SUPERSEDED PROCESS SAYS IT HOLDS IS EVERYTHING IT HOLDS COMPUTE FOR, and what
// it lets go of is gone from its wait without being touched: the current process
// reported the compute, so destroying it, or releasing its lease, would take a job
// from the process now responsible for it.
func TestAProcessLetsGoOfWhatTheCurrentOneAdoptedWithoutTouchingIt(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	r.Superseded()

	if got := r.CustodyLeases(); !slices.Equal(got, []string{lease.ID}) {
		t.Fatalf("the process says it holds %v, want the lease in custody", got)
	}

	r.LetGo([]string{lease.ID})

	if r.Holding() {
		t.Fatal("the process is still waiting on what it let go of")
	}

	if got := r.CustodyLeases(); len(got) != 0 {
		t.Fatalf("the process says it still holds %v", got)
	}

	if n := len(p.destroyed); n != 0 {
		t.Fatalf("letting go destroyed %d instances", n)
	}

	got, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("letting go ended the lease: %v", err)
	}

	if got.Phase == alloc.PhaseDone || got.Phase == alloc.PhaseFailed {
		t.Fatalf("letting go settled the lease as %s", got.Phase)
	}

	// AND THE TICK THAT WOULD HAVE TENDED IT DOES NOT.
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if n := len(p.destroyed); n != 0 {
		t.Fatalf("a tend after letting go destroyed %d instances", n)
	}
}

// A JOB A LAUNCH LEFT RUNNING AFTER THE SUPERSESSION IS HELD TOO: the process says so,
// and letting go of it ends the wait on it.
func TestARunningJobIsClaimedAndLetGo(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := r.CustodyLeases(); !slices.Equal(got, []string{lease.ID}) {
		t.Fatalf("the process says it holds %v, want the running job", got)
	}

	r.LetGo([]string{"another-lease"})

	if !r.Holding() {
		t.Fatal("letting go of another lease ended the wait on this one")
	}

	r.LetGo([]string{lease.ID})

	if r.Holding() {
		t.Fatal("the process is still waiting on a running job it let go of")
	}
}

// A LAUNCH UNDER WAY IS NAMED AND KEPT: this process is what is starting it, and it
// renews the lease until the launch ends.
func TestALaunchUnderWayIsClaimedAndNotLetGo(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	done := r.holdWhileLaunching(lease, "billet-launching")
	defer done()

	if got := r.CustodyLeases(); !slices.Equal(got, []string{lease.ID}) {
		t.Fatalf("the process says it holds %v, want the launch", got)
	}

	r.LetGo([]string{lease.ID})

	if !r.Holding() {
		t.Fatal("letting go ended the wait on a launch still under way")
	}
}
