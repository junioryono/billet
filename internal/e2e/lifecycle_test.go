package e2e

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/provider"
)

// A job arrives, a container runs it, the job finishes, the container goes.
//
// THE FIRST TEST IN THIS PROJECT THAT EXERCISES THE WHOLE CHAIN. Everything
// before it proved one seam at a time: that the listener escrows correctly, that
// the allocator's ledger balances, that the docker provider builds the right
// argv. None of them could have caught a mistake in how those pieces are wired
// to each other, and that is where this project's worst defect so far lived —
// acquiring jobs by the wrong id was invisible to every unit test because
// billet's types agreed with billet's mistake.
func TestAJobRunsAndIsCleanedUp(t *testing.T) {
	s := newStack(t)

	// Available first, then assigned, exactly as the real service sequences them:
	// billet bids on what is AVAILABLE, and GitHub answers by ASSIGNING. Reversing
	// them here would let billet acquire by the wrong id and still pass.
	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 4001, "push", testTier))

	stop := s.run(t)
	defer stop()

	// Acquired by the id from the AVAILABLE message.
	deadline := time.Now().Add(30 * time.Second)

	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	if got := s.plane.acquiredIDs(); got[0] != 4001 {
		t.Fatalf("acquired request %d, want the id from the JobAvailable message (4001)", got[0])
	}

	// GitHub assigns it, and a container appears.
	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 4001, "push", testTier))

	names := s.awaitContainer(t, 1)

	// Named after the LEASE, which is what makes it reconcilable after a crash.
	leaseID, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("container %q does not carry billet's name shape", names[0])
	}

	if leaseID == "" {
		t.Fatalf("container %q carries no lease id", names[0])
	}

	// The job finishes, and the container goes with it.
	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		fakeactions.JobJSON("JobCompleted", 4001, "push", testTier))

	s.awaitContainer(t, 0)

	// AND THE JOB'S OWN LEASE IS TERMINAL. A container that is gone while its
	// lease still holds vCPU is the leak this subsystem exists to prevent.
	//
	// Asserted against THAT lease rather than against total usage, which never
	// falls to zero while a listener is running: the listener escrows capacity
	// BEFORE advertising it, so free escrow it is holding to offer GitHub looks
	// identical to a leak in an aggregate. My first version asserted the
	// aggregate and failed against correct behaviour.
	deadline = time.Now().Add(30 * time.Second)

	for {
		_, err := s.alloc.Lease(t.Context(), leaseID)
		if errors.Is(err, alloc.ErrLeaseNotFound) {
			break
		}

		if err != nil {
			t.Fatalf("read the job's lease: %v", err)
		}

		if time.Now().After(deadline) {
			t.Fatalf("lease %s still holds capacity after its container was destroyed", leaseID)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// A pull request gets no container, and no runner registration either.
//
// The docker backend shares the host kernel, so it refuses work billet cannot
// vouch for. What this adds over the unit test is the ORDER: the refusal has to
// happen before the registration is minted, because a registration with nothing
// to consume it is an orphan on GitHub that billet will never clean up — one per
// pull request, accumulating quietly.
func TestAPullRequestIsRefusedBeforeAnythingIsMinted(t *testing.T) {
	s := newStack(t)

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 4002, "pull_request", testTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)

	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 4002, "pull_request", testTier))

	// Give it long enough that a container would have appeared if it were going
	// to. There is no positive signal for "nothing happened", so this is a wait
	// rather than a poll.
	time.Sleep(3 * time.Second)

	instances, err := s.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(instances) != 0 {
		t.Fatalf("ran pull-request code in a container: %v", instances)
	}

	// AND NOTHING WAS MINTED. This is the half a provider-level test cannot see.
	if calls := s.plane.Calls("generatejitconfig"); len(calls) != 0 {
		t.Errorf("minted %d runner registration(s) for a job that was refused; "+
			"each one is an orphan on GitHub", len(calls))
	}
}

// Capacity comes back when a job is refused, not just when one finishes.
//
// A refusal is a launch failure as far as the listener is concerned, so a
// listener that kept the escrow would advertise less after every pull request
// until it advertised none at all — and the symptom would be a control plane
// that silently stops accepting work, not an error anybody sees.
//
// Asserted by REFUSING MORE JOBS THAN THE BUDGET HOLDS and checking billet is
// still bidding at the end. The obvious assertion — total usage returns to zero
// — is wrong: the listener escrows capacity before advertising it, so free
// escrow it is holding to offer GitHub is indistinguishable from a leak in an
// aggregate. Draining the budget is what tells the two apart.
func TestRefusedWorkReturnsItsCapacity(t *testing.T) {
	s := newStack(t)

	stop := s.run(t)
	defer stop()

	// The budget is 8 vCPU and the tier costs 2, so four leases exhaust it.
	// Six refusals is comfortably more than that.
	const refusals = 6

	for i := range refusals {
		id := int64(4100 + i)

		s.plane.queue(fakeactions.StatisticsJSON(1, 0),
			fakeactions.JobJSON("JobAvailable", id, "pull_request", testTier))

		deadline := time.Now().Add(30 * time.Second)

		for !slices.Contains(s.plane.acquiredIDs(), id) {
			if time.Now().After(deadline) {
				t.Fatalf("billet stopped bidding after %d refusals; refused work is "+
					"consuming capacity permanently", i)
			}

			time.Sleep(50 * time.Millisecond)
		}

		s.plane.queue(fakeactions.StatisticsJSON(0, 1),
			fakeactions.JobJSON("JobAssigned", id, "pull_request", testTier))
	}

	// And nothing ran, which is the other half: bidding freely would be no
	// comfort if the refusal had stopped working.
	instances, err := s.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(instances) != 0 {
		t.Fatalf("ran pull-request code in a container: %v", instances)
	}
}

// Every message is acknowledged, and only after its work is done.
//
// An unacknowledged message is redelivered, so a missing ack is a job billet
// processes twice. Acking too early is worse: the cursor advances past work that
// was never done, and the message is never seen again.
func TestMessagesAreAcknowledged(t *testing.T) {
	s := newStack(t)

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 4004, "push", testTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)

	for {
		s.plane.mu.Lock()
		acked := len(s.plane.deleted)
		s.plane.mu.Unlock()

		if acked > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("billet never acknowledged the message; it would be redelivered forever")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// The scale set is created with the tier's label, on the wire.
//
// Asserted against the request BODY rather than the returned struct, because the
// struct is billet's own type and would agree with billet's own mistake.
func TestTheScaleSetCarriesTheTierLabel(t *testing.T) {
	s := newStack(t)

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)

	for {
		if calls := s.plane.Calls("runnerscalesets"); len(calls) > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("billet never asked about the scale set")
		}

		time.Sleep(50 * time.Millisecond)
	}

	// ONLY the collection POST. Every other scale-set path is a sub-resource —
	// /runnerscalesets/{id}/sessions, /acquirejobs, /generatejitconfig — and a
	// substring filter matched all of them, so the first version of this
	// assertion failed against a session-create body that was never going to
	// carry a label.
	var created int

	for _, c := range s.plane.Calls("runnerscalesets") {
		if c.Method != http.MethodPost || !strings.HasSuffix(c.Path, "runnerscalesets") {
			continue
		}

		created++

		if !strings.Contains(c.Body, testTier) {
			t.Errorf("created a scale set that does not carry the tier label: %s", c.Body)
		}
	}

	if created == 0 {
		t.Error("billet never created a scale set, so this asserted nothing")
	}
}
