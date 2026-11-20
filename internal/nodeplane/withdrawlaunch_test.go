package nodeplane

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A LAUNCH NO NODE HAS TAKEN IS WITHDRAWN, AND ONE A NODE HAS IS NOT. The first never
// reached a host, so nothing started and its caller is told so; the second may be
// starting compute, and only its node can say.
func TestOnlyALaunchNoNodeHasTakenIsWithdrawn(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "keeper", "keeper-1")
	runner := p.NewRunner()

	queued := testLease()
	launched := make(chan error, 1)
	go func() { launched <- runner.Launch(t.Context(), queued, dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "keeper")

	if !runner.WithdrawLaunch(queued.ID) {
		t.Fatal("a launch no node had taken was not withdrawn")
	}
	if err := <-launched; !errors.Is(err, dispatch.ErrWithdrawn) {
		t.Fatalf("a withdrawn launch returned %v, want ErrWithdrawn", err)
	}
	if p.QueuedForTest("keeper") != 0 {
		t.Fatal("a withdrawn launch was left in the queue")
	}

	taken := testLease()
	taken.ID = "l2"
	go func() { launched <- runner.Launch(t.Context(), taken, dispatch.Job{RequestID: 8}) }()
	waitForQueued(t, p, "keeper")
	cmd, ok, err := p.Poll(t.Context(), "keeper", "keeper-1")
	if err != nil || !ok || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll took %+v (ok %v, err %v), want the launch", cmd, ok, err)
	}

	if runner.WithdrawLaunch(taken.ID) {
		t.Fatal("a launch a node had taken was withdrawn")
	}
	if err := p.Result("keeper", "keeper-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("the launch the node took: %v", err)
	}
}

// A LAUNCH WITHDRAWN BEFORE IT REACHED THE PLANE IS REFUSED AS IT ARRIVES: the listener
// publishes a launch before it has a slot to send it, so a cancellation can come first,
// and a withdrawal that found nothing would leave the launch to be queued and waited
// out.
func TestALaunchWithdrawnBeforeItArrivesIsNeverQueued(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	registerIncarnation(t, p, "keeper", "keeper-1")
	runner := p.NewRunner()

	early := testLease()
	if !runner.WithdrawLaunch(early.ID) {
		t.Fatal("a launch withdrawn before it arrived was reported as not withdrawn")
	}
	if err := runner.Launch(t.Context(), early, dispatch.Job{RequestID: 7}); !errors.Is(err, dispatch.ErrWithdrawn) {
		t.Fatalf("a launch withdrawn before it arrived returned %v, want ErrWithdrawn", err)
	}
	if p.QueuedForTest("keeper") != 0 {
		t.Fatal("a launch withdrawn before it arrived was queued")
	}

	// ONCE: the withdrawal is spent on the launch it was for.
	launched := make(chan error, 1)
	go func() { launched <- runner.Launch(t.Context(), early, dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "keeper")
	cmd, ok, err := p.Poll(t.Context(), "keeper", "keeper-1")
	if err != nil || !ok {
		t.Fatalf("the next launch of the lease was not given (ok %v, err %v)", ok, err)
	}
	if err := p.Result("keeper", "keeper-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("the next launch of the lease: %v", err)
	}
}

// A WITHDRAWAL REMEMBERED FOR A LAUNCH NOT YET ARRIVED ENDS WHEN ITS CALLER FORGETS IT: no
// clock ends it, since the launch may wait for a slot behind others, and once its call
// has returned nothing is left for it to refuse.
func TestARememberedWithdrawalEndsWhenItsCallerForgetsIt(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	p := testPlane(t, WithClock(clock.now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(10 * time.Millisecond)
	registerIncarnation(t, p, "keeper", "keeper-1")
	runner := p.NewRunner()

	// DAYS LATER, with the node still live, it is still refused.
	late := testLease()
	runner.WithdrawLaunch(late.ID)
	clock.nanos.Add(int64(48 * time.Hour))
	if _, _, err := p.Poll(t.Context(), "keeper", "keeper-1"); err != nil {
		t.Fatalf("keep the node live: %v", err)
	}
	if err := runner.Launch(t.Context(), late, dispatch.Job{RequestID: 7}); !errors.Is(err, dispatch.ErrWithdrawn) {
		t.Fatalf("a launch whose withdrawal was remembered returned %v, want ErrWithdrawn", err)
	}

	// FORGOTTEN, it is not.
	forgotten := testLease()
	forgotten.ID = "l2"
	runner.WithdrawLaunch(forgotten.ID)
	runner.ForgetWithdrawal(forgotten.ID)
	launched := make(chan error, 1)
	go func() { launched <- runner.Launch(t.Context(), forgotten, dispatch.Job{RequestID: 8}) }()
	waitForQueued(t, p, "keeper")
	cmd, ok, err := p.Poll(t.Context(), "keeper", "keeper-1")
	if err != nil || !ok {
		t.Fatalf("a launch whose withdrawal was forgotten was not given (ok %v, err %v)", ok, err)
	}
	if err := p.Result("keeper", "keeper-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("a launch whose withdrawal was forgotten: %v", err)
	}
}
