package nodeplane

import (
	"context"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A LAUNCH GIVEN UP ON AND REPORTED FAILED LATE ENDS ITS OWN LEASE'S OWNERSHIP, through
// the plane as a node meets it: the later launch of the same request keeps its own.
func TestALateFailedLaunchEndsOnlyItsOwnLeaseOnTheWire(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "keeper", "keeper-1")

	take := func(what string) nodeapi.Command {
		t.Helper()

		cmd, ok, err := p.Poll(t.Context(), "keeper", "keeper-1")
		if err != nil || !ok || cmd.Kind != nodeapi.CommandLaunch {
			t.Fatalf("%s: took %+v (ok %v, err %v), want a launch", what, cmd, ok, err)
		}

		return cmd
	}

	// GIVEN UP ON BY ITS CALLER once the node has it, as the command timeout would.
	first := testLease()
	giveUp, abandon := context.WithCancel(t.Context())
	abandoned := make(chan error, 1)
	go func() { abandoned <- p.NewRunner().Launch(giveUp, first, dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "keeper")
	late := take("the first launch")
	abandon()
	if err := <-abandoned; err == nil {
		t.Fatal("a launch nobody answered succeeded")
	}

	later := testLease()
	later.ID = "l2"
	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), later, dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "keeper")
	if err := p.Result("keeper", "keeper-1", nodeapi.CommandResult{ID: take("the later launch").ID, OK: true}); err != nil {
		t.Fatalf("report the later launch: %v", err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("the later launch: %v", err)
	}

	if err := p.Result("keeper", "keeper-1", nodeapi.CommandResult{ID: late.ID, Error: "the guest did not boot"}); err != nil {
		t.Fatalf("report the first launch late: %v", err)
	}

	if p.OwnsForTest(first.ID, "keeper", "keeper-1") {
		t.Error("the launch that failed late still owns its lease")
	}
	if !p.OwnsForTest(later.ID, "keeper", "keeper-1") {
		t.Error("an earlier launch's late failure ended the later launch's ownership")
	}
}

// A DESTROY ASKED OF EVERY NODE ENDS ON EACH WHAT WAS DELIVERED THERE BEFORE THAT NODE
// TOOK IT, through the plane: a launch delivered to one node after another node took its
// leg, and before its own, was torn down by its own leg.
func TestABroadcastDestroyEndsWhatEachNodeTookBeforeIt(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "first", "first-1")
	registerIncarnation(t, p, "second", "second-1")

	take := func(node, incarnation string, kind nodeapi.CommandKind) nodeapi.Command {
		t.Helper()

		cmd, ok, err := p.Poll(t.Context(), node, incarnation)
		if err != nil || !ok || cmd.Kind != kind {
			t.Fatalf("%s took %+v (ok %v, err %v), want a %s", node, cmd, ok, err, kind)
		}

		return cmd
	}
	answer := func(node, incarnation string, cmd nodeapi.Command) {
		t.Helper()

		if err := p.Result(node, incarnation, nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			t.Fatalf("%s report %s: %v", node, cmd.Kind, err)
		}
	}

	// QUEUED, NOT YET DELIVERED, so the destroy below knows no owner and asks every node.
	lease := testLease()
	lease.TargetNode = "second"
	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "second")

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 7) }()
	waitFor(t, "a leg on each node", func() bool {
		return p.QueuedForTest("first") == 1 && p.QueuedForTest("second") == 2
	})

	firstLeg := take("first", "first-1", nodeapi.CommandDestroy)
	answer("second", "second-1", take("second", "second-1", nodeapi.CommandLaunch))
	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
	secondLeg := take("second", "second-1", nodeapi.CommandDestroy)

	answer("first", "first-1", firstLeg)
	answer("second", "second-1", secondLeg)
	if err := <-destroyed; err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if p.OwnsForTest(lease.ID, "second", "second-1") {
		t.Fatal("the node whose own leg tore the launch down still owns it")
	}
}
