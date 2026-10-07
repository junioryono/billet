package nodeplane

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

func registerIncarnation(t *testing.T, p *Plane, name, incarnation string) {
	t.Helper()

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: name, Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: incarnation, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

// A DESTROY TAKEN BY A POLL NOBODY READ IS GIVEN AGAIN. A node turning from its
// ordinary loop to a drain abandons a long poll whose handler the server has not
// yet seen go; measured, a destroy handed to that handler waited out the whole
// command timeout while the same process polled beside it. The first Poll here
// is that handler: it takes the command and its answer goes nowhere.
func TestALostDestroyIsGivenAgainToTheProcessThatPolls(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "holder", "holder-1")

	destroyed := make(chan error, 1)

	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 42) }()

	waitForQueued(t, p, "holder")

	lost, ok, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !ok || lost.Kind != nodeapi.CommandDestroy {
		t.Fatalf("the first poll took %+v (ok %v, err %v), want the destroy", lost, ok, err)
	}

	again, ok, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !ok {
		t.Fatalf("the process polled again and was given nothing (ok %v, err %v); "+
			"the destroy it never received waits out the command timeout", ok, err)
	}

	if again.ID != lost.ID || again.Kind != nodeapi.CommandDestroy {
		t.Fatalf("the second poll was given %+v, want the lost destroy %q again", again, lost.ID)
	}

	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: again.ID, OK: true}); err != nil {
		t.Fatalf("report the destroy: %v", err)
	}

	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("the destroy answered by its second delivery failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy was answered and its caller is still waiting")
	}
}

// A LAUNCH IS NEVER GIVEN TWICE, because a second delivery can start a second
// runner for one job; and a destroy that was answered is not given again.
func TestNeitherALaunchNorAnAnsweredDestroyIsGivenAgain(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "holder", "holder-1")

	// Buffered and never read: the launch's outcome is not this test's, it only
	// has to be in flight.
	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	// QUEUED BEFORE IT IS POLLED FOR, because the window is short for the polls
	// that must find nothing, and a slow dispatch would read as a refusal.
	waitForQueued(t, p, "holder")

	launch, ok, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !ok || launch.Kind != nodeapi.CommandLaunch {
		t.Fatalf("the first poll took %+v (ok %v, err %v), want the launch", launch, ok, err)
	}

	if again, ok, err := p.Poll(t.Context(), "holder", "holder-1"); err != nil || ok {
		t.Fatalf("a launch already in flight was given again: %+v (ok %v, err %v)", again, ok, err)
	}

	destroyed := make(chan error, 1)

	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 43) }()

	waitForQueued(t, p, "holder")

	destroy, ok, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !ok || destroy.Kind != nodeapi.CommandDestroy {
		t.Fatalf("the node took %+v (ok %v, err %v), want the destroy", destroy, ok, err)
	}

	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: destroy.ID, OK: true}); err != nil {
		t.Fatalf("report the destroy: %v", err)
	}

	if cmd, ok, err := p.Poll(t.Context(), "holder", "holder-1"); err != nil || ok {
		t.Fatalf("an answered destroy was given again: %+v (ok %v, err %v)", cmd, ok, err)
	}

	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("the answered destroy failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy was answered and its caller is still waiting")
	}
}
