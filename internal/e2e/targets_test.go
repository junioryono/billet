package e2e

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/provider/simulated"
)

const secondTier = "billet-2vcpu-beta"

// A job queued on the SECOND target is acquired on that target's session,
// registered through that target's credential, and never reaches the first
// target's service: the deployment serves two owners at once, each through its
// own App, on one fleet.
//
// Over the wire on the simulated backend, because that is the shape in which
// the plane's per-target routing exists, and it runs wherever the tests do.
// The second target is an organization rather than a repository because the
// harness backends refuse untrusted work, which is all a repository target can
// carry; withSecondTarget says why, and the repository path has its own proofs.
func TestAJobOnASecondTargetIsServedByThatTargetsCredential(t *testing.T) {
	second := newPlaneFor(t, secondTier, 8)

	s := newWireStack(t,
		withBackend(config.ProviderSimulated, simulatedBackend(simulated.NewHost())),
		withSecondTarget(second, secondTier))

	// The job is offered on the second target's service only.
	second.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 5001, "push", secondTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)

	for len(second.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the second target's job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	if got := second.acquiredIDs(); got[0] != 5001 {
		t.Fatalf("acquired request %d on the second target, want 5001", got[0])
	}

	second.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 5001, "push", secondTier))

	s.awaitOneRunning(t)

	// THE REGISTRATION WAS MINTED THROUGH THE SECOND TARGET'S SERVICE, and the
	// first target's service minted nothing and acquired nothing: a mint on the
	// organization's App for the repository's job would register the runner on
	// the wrong owner.
	second.mu.Lock()
	mintedOnSecond := second.registeredRunner
	second.mu.Unlock()

	s.plane.mu.Lock()
	mintedOnFirst := s.plane.registeredRunner
	s.plane.mu.Unlock()

	if mintedOnSecond == "" {
		t.Error("no runner registration was minted through the second target's service")
	}

	if mintedOnFirst != "" {
		t.Errorf("a runner registration %q was minted through the FIRST target's service for the second target's job",
			mintedOnFirst)
	}

	if got := s.plane.acquiredIDs(); len(got) != 0 {
		t.Errorf("the first target's service saw acquisitions %v for a job it never offered", got)
	}

	// AND BOTH SCALE SETS EXIST, each created through its own service.
	s.plane.mu.Lock()
	firstExists := s.plane.exists
	s.plane.mu.Unlock()

	second.mu.Lock()
	secondExists := second.exists
	second.mu.Unlock()

	if !firstExists || !secondExists {
		t.Errorf("scale sets created: first=%v second=%v, want both", firstExists, secondExists)
	}

	// The registration-token calls prove the OWNER each client registered
	// against: acme on the first service, beta on the second, and neither on
	// the other's.
	if calls := s.plane.Calls("/orgs/acme/actions/runners/registration-token"); len(calls) == 0 {
		t.Error("the first target's client never registered against acme")
	}

	if calls := second.Calls("/orgs/beta/actions/runners/registration-token"); len(calls) == 0 {
		t.Error("the second target's client never registered against beta")
	}

	if calls := second.Calls("/orgs/acme/"); len(calls) != 0 {
		t.Errorf("the second target's service was asked about acme: %v", calls)
	}

	if calls := s.plane.Calls("/orgs/beta/"); len(calls) != 0 {
		t.Errorf("the first target's service was asked about beta: %v", calls)
	}
}
