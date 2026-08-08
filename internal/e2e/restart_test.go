package e2e

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider"
)

// A REAL container survives a controller crash, against a REAL docker daemon.
//
// The unit tests assert this against a fake backend, which proves the logic but
// not the identity. Adoption works only if the restarted process can still SEE
// the container, and that depends on the deployment id being read back from disk
// and on docker's label filter matching it — neither of which the unit tests
// touch. Getting either wrong makes every surviving container invisible: leases
// reaped, capacity resold, containers running forever.
//
// THE CONTROL PLANE IS NOT RUN HERE, and that is the point rather than a
// shortcut. Stopping the server is a GRACEFUL shutdown, which tears its
// containers down on the way out — correct behaviour, and the opposite of the
// situation being tested. A crash is a process that stops without doing any of
// that, so the test drives the runner directly and then abandons it.
func TestARealContainerIsAdoptedAfterACrash(t *testing.T) {
	s := newStack(t)

	lease := s.launchDirectly(t)

	// The controller dies: no shutdown, no teardown, just the state lock going
	// away. The container knows nothing about it and keeps running.
	s.closeDB()

	restarted := newStackIn(t, s.dir, s.plane)

	if err := restarted.runner.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	live, err := restarted.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(live) != 1 {
		t.Fatalf("the restarted controller destroyed a running job, or could not see it: %v", live)
	}

	if want := provider.InstanceName(lease.ID); live[0].Name != want {
		t.Fatalf("a different container survived: %q, want %q", live[0].Name, want)
	}

	// Adopted, so the capacity stays held rather than being resold underneath a
	// job that is still running.
	if _, err := restarted.alloc.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("the adopted job's lease was released, so its capacity can be handed out: %v", err)
	}

	// And when GitHub reports the job finished, the container goes. That is the
	// ordinary end of an adoption: the lease is released by whoever handles the
	// completion, and the next Tend finds the heartbeat refused.
	if err := restarted.alloc.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := restarted.runner.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	live, err = restarted.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(live) != 0 {
		t.Fatalf("the adopted container outlived its job: %v", live)
	}
}

// A container whose lease did not survive is destroyed on restart.
//
// The other half, and the one that keeps a host from being over-committed:
// nothing is waiting for this container's result and its capacity has already
// gone back to the budget, so every second it keeps running is a second the
// ledger is wrong about the machine.
func TestARealOrphanIsDestroyedAfterACrash(t *testing.T) {
	s := newStack(t)

	lease := s.launchDirectly(t)

	// The job finished and its lease was released, but the controller died before
	// the container was destroyed.
	if err := s.alloc.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	s.closeDB()

	restarted := newStackIn(t, s.dir, s.plane)

	if err := restarted.runner.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	live, err := restarted.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(live) != 0 {
		t.Fatalf("an orphan survived the restart: %v", live)
	}
}

// Another deployment's containers are invisible, against a real daemon.
//
// This is what makes recovery safe to run at all. Two billets on one machine
// share a hostname and therefore a default node name, and they keep separate
// state directories so nothing stops both from running. If they could see each
// other's containers, the first to recover would find the other's lease ids
// absent from its own database and act on live jobs it has no relationship with.
func TestADifferentDeploymentIsInvisible(t *testing.T) {
	first := newStack(t)
	first.launchDirectly(t)

	// A second installation: its own state directory, therefore its own identity.
	second := newStack(t)

	live, err := second.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(live) != 0 {
		t.Fatalf("one deployment can see another's containers: %v", live)
	}

	// And recovery leaves them alone, which is the consequence that matters.
	if err := second.runner.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	stillThere, err := first.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(stillThere) != 1 {
		t.Fatalf("a second deployment's recovery destroyed the first's job: %v", stillThere)
	}
}

// A launch whose cleanup cannot be confirmed keeps its capacity, against a real
// daemon.
func TestARealUnconfirmedCleanupHoldsCapacity(t *testing.T) {
	s := newStack(t)

	lease := s.launchDirectly(t)

	// Simulate the ambiguous outcome the other way round: the compute exists and
	// the lease is open, and nothing is managing it because the process that did
	// has gone. Recovery takes custody either way.
	s.closeDB()

	restarted := newStackIn(t, s.dir, s.plane)

	if err := restarted.runner.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Tend keeps the lease alive rather than letting the reaper take it.
	for range 3 {
		if err := restarted.runner.Tend(t.Context()); err != nil {
			t.Fatalf("Tend: %v", err)
		}
	}

	if _, err := restarted.alloc.Lease(t.Context(), lease.ID); err != nil {
		if errors.Is(err, alloc.ErrLeaseNotFound) {
			t.Fatal("the held lease was released while its container was still running")
		}

		t.Fatalf("read the held lease: %v", err)
	}
}

// launchDirectly reserves, assigns and launches one job without running the
// control plane, and returns its lease.
//
// The listener is deliberately not involved: these tests are about what survives
// a process that STOPS WITHOUT SHUTTING DOWN, and a running server tears its
// containers down when its context is cancelled.
func (s *stack) launchDirectly(t *testing.T) *alloc.Lease {
	t.Helper()

	// The scale set exists, as it would anywhere the control plane has run once.
	// These tests never start it, and a runner cannot mint a registration against
	// a set that is not there.
	s.plane.mu.Lock()
	s.plane.exists = true
	s.plane.mu.Unlock()

	lease, err := s.alloc.Reserve(t.Context(), testTier)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	const requestID = 9001

	if err := s.alloc.Assign(t.Context(), lease.ID, lease.Epoch, requestID, requestID); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := s.runner.Launch(t.Context(), lease, // A push, so the container backend accepts it: these tests are about
		// recovery, not about the trust boundary.
		node.Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	return lease
}
