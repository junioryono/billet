package nodeplane

import (
	"strconv"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// NOTHING OF A REQUEST PASSES ITS OWN COMMAND HELD BACK, AND NOTHING UNORDERED PASSES
// ANY: a second destroy of the request stays behind the launch it must follow, a sweep
// stays behind both, and only a command of another request goes ahead.
func TestNothingPassesALaunchHeldBackButAnotherRequest(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "holder", "holder-1")

	results := make(chan error, 6)
	queue := func(what string, f func() error) {
		t.Helper()

		before := p.QueuedForTest("holder")
		go func() { results <- f() }()
		waitFor(t, what, func() bool { return p.QueuedForTest("holder") == before+1 })
	}

	queue("the first destroy", func() error { return p.NewRunner().Destroy(t.Context(), 7) })
	first := pollOne(t, p)

	queue("the launch", func() error {
		return p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
	})
	queue("the second destroy", func() error { return p.NewRunner().Destroy(t.Context(), 7) })
	queue("the sweep", func() error { return p.NewRunner().Sweep(t.Context()) })
	queue("another request's destroy", func() error { return p.NewRunner().Destroy(t.Context(), 8) })
	queue("a log read", func() error {
		_, err := p.ReadJobLog(t.Context(), "holder", nodeapi.LogReadSpec{})

		return err
	})

	answer := func(cmd nodeapi.Command) {
		t.Helper()

		if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			t.Fatalf("report %s: %v", cmd.Kind, err)
		}
	}

	// A LOG READ TOUCHES NO LEASE, and is given past everything held back.
	if read := pollOne(t, p, first.ID); read.Kind != nodeapi.CommandReadLog {
		t.Fatalf("a poll with request 7's launch held back was given %+v, want the log read", read)
	} else if err := p.Result("holder", "holder-1", nodeapi.CommandResult{
		ID: read.ID, OK: true, Log: &nodeapi.LogReadResult{},
	}); err != nil {
		t.Fatalf("report the log read: %v", err)
	}

	// THE SWEEP IS BETWEEN, so the other request's destroy waits behind it too.
	if cmd, ok, err := p.Poll(t.Context(), "holder", "holder-1", first.ID); ok || err != nil {
		t.Fatalf("a poll with request 7's destroy out was given %+v (ok %v, err %v)", cmd, ok, err)
	}

	answer(first)

	launch := pollOne(t, p)
	second := pollOne(t, p)
	sweep := pollOne(t, p, second.ID)
	other := pollOne(t, p, second.ID)

	for i, c := range []struct {
		cmd     nodeapi.Command
		kind    nodeapi.CommandKind
		request int64
	}{
		{launch, nodeapi.CommandLaunch, 7}, {second, nodeapi.CommandDestroy, 7},
		{sweep, nodeapi.CommandSweep, 0}, {other, nodeapi.CommandDestroy, 8},
	} {
		if c.cmd.Kind != c.kind || c.cmd.RequestIDOf() != c.request {
			t.Fatalf("command %d after the first destroy was answered was %s of request %d, want %s of %d",
				i+1, c.cmd.Kind, c.cmd.RequestIDOf(), c.kind, c.request)
		}
		answer(c.cmd)
	}

	for range 6 {
		if err := <-results; err != nil {
			t.Fatalf("an answered command failed: %v", err)
		}
	}
}

// THE BOUND COUNTS WHAT THE NODE SAYS IT HOLDS as well as what this plane gave: a
// destroy answered, or given up on, can still be in the node's hands until its report's
// reply reaches it, and the poll names it.
func TestTheBoundCountsTheDestroysThePollNames(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "holder", "holder-1")

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 9) }()
	waitForQueued(t, p, "holder")

	named := make([]string, nodeapi.MaxDestroysRunning)
	for i := range named {
		named[i] = "answered-" + strconv.Itoa(i)
	}

	if cmd, ok, err := p.Poll(t.Context(), "holder", "holder-1", named...); ok || err != nil {
		t.Fatalf("a node naming %d destroys it holds was given %+v (ok %v, err %v)", len(named), cmd, ok, err)
	}

	cmd := pollOne(t, p, named[1:]...)
	if cmd.Kind != nodeapi.CommandDestroy || cmd.RequestID != 9 {
		t.Fatalf("a node naming one fewer was given %+v, want the destroy", cmd)
	}
	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := <-destroyed; err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}
