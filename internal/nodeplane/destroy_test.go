package nodeplane

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// A DESTROY IS ADDRESSED, NOT SHOUTED.
//
// It used to go to every live node at once, because the plane did not track
// which one held which request — and the cost is not merely traffic. Every
// dispatch waits up to the command timeout, so the listener serialised teardown
// to one destroy at a time to stop a fleet of twenty turning a shutdown into
// twenty timeouts. Teardown scaled with the size of the fleet rather than with
// the work being torn down.
//
// The plane does know: it recorded the owner when it handed the launch over.
//
// ASSERTED ON WHAT WAS DISPATCHED rather than by racing two pollers for the
// command. A poll can be answered by a redelivered in-flight launch as easily as
// by the destroy, which made the obvious version of this test flaky under load:
// it passed alone and failed in the full run, which is the worst way for a test
// to be wrong.
func TestADestroyGoesOnlyToTheNodeHoldingTheJob(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(2*time.Second))

	// WITH INCARNATIONS, because ownership is only recorded for a node that
	// identified its process — the plane will not attribute a container to a name
	// two hosts could be sharing.
	for _, name := range []string{"holder", "bystander"} {
		if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version:     nodeapi.Version,
			Node:        name,
			Provider:    config.ProviderDocker,
			Deployment:  deployment,
			Incarnation: name + "-1",
			VCPU:        8,
			Memory:      32 * config.GiB,
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	// The holder takes the launch and never answers it, so ownership stands and
	// the container is — as far as the plane knows — running there.
	taken := make(chan struct{})

	go func() {
		if _, _, err := p.Poll(t.Context(), "holder", "holder-1"); err == nil {
			close(taken)
		}
	}()

	// PARKED BEFORE THE LAUNCH IS DISPATCHED, which is what makes this
	// deterministic. A dispatched command is discarded once the command timeout
	// elapses, so starting the poller and the launch together was a race against
	// the scheduler: under the full suite's parallelism the poll goroutine could
	// simply not run inside that window, the launch would expire unclaimed, and
	// the test failed for a reason that had nothing to do with addressing.
	waitFor(t, "the holder to park on a poll",
		func() bool { return p.WaitersForTest("holder") == 1 })

	launched := make(chan error, 1)

	go func() {
		lease := testLease()
		lease.Node = "holder"

		launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	select {
	case <-taken:
	case <-time.After(10 * time.Second):
		t.Fatal("the holder never took the launch")
	}

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 7) }()

	// Nobody is polling now, so a dispatched destroy sits in a queue where it can
	// be counted.
	waitFor(t, "the machine holding the job to be asked to destroy it",
		func() bool { return p.QueuedForTest("holder") > 0 })

	// NOTHING ELSE WAS ASKED. A bystander receiving a destroy for a container it
	// has never heard of answers "not found", which is harmless in itself and is
	// exactly what made teardown wait on every machine in the fleet.
	if got := p.QueuedForTest("bystander"); got != 0 {
		t.Errorf("a machine that never had this job was asked to destroy it (%d queued)", got)
	}

	<-destroyed
	<-launched
}
