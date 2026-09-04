package e2e

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/fakeactions"
)

// A JOB RUNS ON A NODE REACHED OVER THE ACTUAL WIRE.
//
// The same lifecycle the in-process suite proves, with a real HTTP listener, a
// real node loop and a real Docker daemon between the control plane and the
// compute. Every other test of the protocol necessarily supplies one of its two
// sides; this one supplies neither.
//
// It exists because the wire is the ONLY way a job reaches a machine. It used to
// have a rival: `server --dev` ran a node inside the control-plane process and
// deliberately bypassed the wire, so this test covered a path a single-machine
// deployment never took. That is inverted now — --dev is gone, a single machine
// runs `billet server` and `billet node` side by side, and every deployment
// shape billet has goes through exactly what this test exercises.
func TestAJobRunsOnANodeAcrossTheWire(t *testing.T) {
	forEachBackend(t, aJobRunsOnANodeAcrossTheWire)
}

func aJobRunsOnANodeAcrossTheWire(t *testing.T, opts ...stackOpt) {
	s := newWireStack(t, opts...)

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 4001, "push", testTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)

	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job across the wire")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 4001, "push", testTier))

	// THE CONTAINER IS THE PROOF. It means a command crossed the wire, the node
	// executed it, the JIT registration was minted BY THE CONTROL PLANE and
	// carried back, and the lease operations behind all of that went over HTTP
	// rather than through a function call.
	names := s.awaitOneRunning(t)

	if len(names) != 1 {
		t.Fatalf("expected one container started across the wire, got %v", names)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		fakeactions.JobJSON("JobCompleted", 4001, "push", testTier))

	// And the teardown crosses it too: a destroy is broadcast to the fleet, the
	// node removes the container, and the capacity comes back.
	s.awaitGone(t)
}
