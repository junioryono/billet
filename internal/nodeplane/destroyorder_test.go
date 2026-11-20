package nodeplane

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// pollOne takes one command, failing the test if none comes within the poll window.
func pollOne(t *testing.T, p *Plane, holding ...string) nodeapi.Command {
	t.Helper()

	cmd, ok, err := p.Poll(t.Context(), "holder", "holder-1", holding...)
	if err != nil || !ok {
		t.Fatalf("a poll naming %v was given nothing (ok %v, err %v)", holding, ok, err)
	}

	return cmd
}

// A LAUNCH OF A REQUEST WHOSE DESTROY IS OUT IS KEPT UNTIL THE DESTROY IS ANSWERED,
// and the command behind it is given meanwhile. A node may run destroys beside its
// loop, and a destroy names a request, not a machine: one still running when the
// launch was taken could tear down what the launch started.
func TestALaunchIsKeptBehindTheDestroyOfItsRequest(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "holder", "holder-1")

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 7) }()
	waitForQueued(t, p, "holder")
	destroy := pollOne(t, p)

	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "holder")

	other := make(chan error, 1)
	go func() { other <- p.NewRunner().Destroy(t.Context(), 8) }()
	waitFor(t, "the launch to queue", func() bool { return p.QueuedForTest("holder") == 2 })

	if next := pollOne(t, p, destroy.ID); next.Kind != nodeapi.CommandDestroy || next.RequestID != 8 {
		t.Fatalf("a poll with request 7's destroy out was given %+v, want the destroy of request 8", next)
	} else if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: next.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}

	if _, ok, err := p.Poll(t.Context(), "holder", "holder-1", destroy.ID); ok || err != nil {
		t.Fatalf("a launch was given while its request's destroy was still out (ok %v, err %v)", ok, err)
	}

	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: destroy.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}

	launch := pollOne(t, p)
	if launch.Kind != nodeapi.CommandLaunch || launch.RequestIDOf() != 7 {
		t.Fatalf("after the destroy was answered the poll was given %+v, want the launch", launch)
	}
	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: launch.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}

	for _, ch := range []chan error{destroyed, other, launched} {
		if err := <-ch; err != nil {
			t.Fatalf("an answered command failed: %v", err)
		}
	}

	// THE DESTROY'S PROOF ENDS ONLY WHAT IT COULD HAVE TORN DOWN: the launch given
	// after it still belongs to the process that took it.
	if !p.OwnsForTest(testLease().ID, "holder", "holder-1") {
		t.Fatal("the destroy before the launch ended the launch's ownership")
	}
}

// A NODE IS GIVEN NO MORE DESTROYS THAN A POLL CAN NAME (nodeapi.MaxDestroysRunning),
// and a launch still comes. A destroy past it would be left out of every poll, and
// given back as lost on each.
func TestNoMoreDestroysAreGivenThanAPollNames(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "holder", "holder-1")

	results := make(chan error, nodeapi.MaxDestroysRunning+3)
	var held []string

	// THE ONE PAST THE BOUND QUEUED LAST, so which it is is known.
	extra := int64(100 + nodeapi.MaxDestroysRunning)
	for i := range nodeapi.MaxDestroysRunning {
		go func() { results <- p.NewRunner().Destroy(t.Context(), int64(100+i)) }()
	}
	waitFor(t, "the destroys to queue", func() bool { return p.QueuedForTest("holder") == nodeapi.MaxDestroysRunning })
	go func() { results <- p.NewRunner().Destroy(t.Context(), extra) }()
	waitFor(t, "the last destroy to queue", func() bool { return p.QueuedForTest("holder") == nodeapi.MaxDestroysRunning+1 })

	for range nodeapi.MaxDestroysRunning {
		cmd := pollOne(t, p, held...)
		if cmd.Kind != nodeapi.CommandDestroy {
			t.Fatalf("took %+v, want a destroy", cmd)
		}
		held = append(held, cmd.ID)
	}

	go func() { results <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7}) }()
	waitFor(t, "the launch to queue", func() bool { return p.QueuedForTest("holder") == 2 })

	if next := pollOne(t, p, held...); next.Kind != nodeapi.CommandLaunch {
		t.Fatalf("a node holding %d destroys was given %+v, want the launch behind the next destroy",
			len(held), next)
	} else if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: next.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}

	// A LAUNCH OF THE REQUEST WHOSE DESTROY IS KEPT BACK is kept behind it too.
	behind := testLease()
	behind.ID = "l2"
	go func() {
		results <- p.NewRunner().Launch(t.Context(), behind, dispatch.Job{RequestID: extra})
	}()
	waitFor(t, "the second launch to queue", func() bool { return p.QueuedForTest("holder") == 2 })

	if cmd, ok, err := p.Poll(t.Context(), "holder", "holder-1", held...); ok || err != nil {
		t.Fatalf("a poll from a node holding as many destroys as it names was given %+v (ok %v, err %v)",
			cmd, ok, err)
	}

	// ONE ANSWERED, THE NEXT IS GIVEN.
	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: held[0], OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}
	held = held[1:]

	last := pollOne(t, p, held...)
	if last.Kind != nodeapi.CommandDestroy || last.RequestID != extra {
		t.Fatalf("after a destroy was answered the poll was given %+v, want the destroy of request %d", last, extra)
	}

	for _, id := range append(held, last.ID) {
		if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: id, OK: true}); err != nil {
			t.Fatalf("report: %v", err)
		}
	}

	// ITS DESTROY ANSWERED, the launch behind it is given.
	if launch := pollOne(t, p); launch.Kind != nodeapi.CommandLaunch || launch.RequestIDOf() != extra {
		t.Fatalf("after every destroy was answered the poll was given %+v, want the launch of request %d",
			launch, extra)
	} else if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: launch.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}

	for range nodeapi.MaxDestroysRunning + 3 {
		if err := <-results; err != nil {
			t.Fatalf("an answered command failed: %v", err)
		}
	}
}

// A DESTROY ANSWERED WAKES A POLL PARKED BEHIND IT: what the plane kept back for it
// is given at once, not when the poll's window ends.
func TestADestroyAnsweredWakesThePollItHeldBack(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(time.Hour)
	registerIncarnation(t, p, "keeper", "keeper-1")

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 7) }()
	waitForQueued(t, p, "keeper")

	destroy, ok, err := p.Poll(t.Context(), "keeper", "keeper-1")
	if err != nil || !ok {
		t.Fatalf("the destroy was not given (ok %v, err %v)", ok, err)
	}

	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "keeper")

	type poll struct {
		cmd nodeapi.Command
		err error
	}
	polled := make(chan poll, 1)
	go func() {
		cmd, _, err := p.Poll(t.Context(), "keeper", "keeper-1", destroy.ID)
		polled <- poll{cmd, err}
	}()
	waitFor(t, "the poll to park", func() bool { return p.WaitersForTest("keeper") == 1 })

	if err := p.Result("keeper", "keeper-1", nodeapi.CommandResult{ID: destroy.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}

	select {
	case got := <-polled:
		if got.err != nil || got.cmd.Kind != nodeapi.CommandLaunch {
			t.Fatalf("the woken poll took %+v (err %v), want the launch", got.cmd, got.err)
		}
		if err := p.Result("keeper", "keeper-1", nodeapi.CommandResult{ID: got.cmd.ID, OK: true}); err != nil {
			t.Fatalf("report: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a poll parked behind a destroy was not woken when it was answered")
	}

	for _, ch := range []chan error{destroyed, launched} {
		if err := <-ch; err != nil {
			t.Fatalf("an answered command failed: %v", err)
		}
	}
}
