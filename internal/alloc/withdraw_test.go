package alloc

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// withdrawingFleet is one docker host that registered as a named process, on a
// deployment whose only tier can run nowhere else.
func withdrawingFleet(t *testing.T, now *time.Time) (*Allocator, int64) {
	t.Helper()

	small := tier("small", 4, 16*config.GiB)
	small.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{small},
		WithClock(func() time.Time { return *now }),
		WithLeaseTTL(30*time.Second))

	// Room for two of the tier, so a test can hold one running and one escrowed.
	_, epoch := mustRegister(t, a, NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB,
		Incarnation: "p1",
	})

	return a, epoch
}

// A HOST THAT WITHDRAWS STOPS BEING PLACED ON AT ONCE, and comes back when it
// registers again.
//
// THE ASSERTION IS THE TIER'S HEADROOM, not the liveness column. Placement
// escrows against ListPlaceableNodes, so what the issue's five-minute delay
// actually was is a tier that went on advertising a stopped host — and a
// withdrawal that wrote a column nothing reads would leave that untouched.
func TestAWithdrawnNodeIsNotPlacedOnUntilItRegistersAgain(t *testing.T) {
	now := time.Now().UTC()
	a, epoch := withdrawingFleet(t, &now)

	if headroom(t, a, "small") == 0 {
		t.Fatal("the tier advertises nothing before the withdrawal, so this proves nothing")
	}

	if err := a.NodeWithdrawn(t.Context(), "epyc-1", epoch, "p1"); err != nil {
		t.Fatalf("NodeWithdrawn: %v", err)
	}

	if got := headroom(t, a, "small"); got != 0 {
		t.Errorf("headroom after the host withdrew = %d, want 0: placement is still "+
			"aiming work at a host that said it is leaving", got)
	}

	nodes, err := a.RegisteredNodes(t.Context())
	if err != nil {
		t.Fatalf("RegisteredNodes: %v", err)
	}

	if len(nodes) != 1 || nodes[0].Name != "epyc-1" {
		t.Fatalf("registered nodes = %+v, want the one host still recorded", nodes)
	}

	if nodes[0].Live {
		t.Error("a withdrawn host is still recorded as live")
	}

	// NOT A DECOMMISSION. The host is still expected back, and a drain still
	// counts it; only placement stopped.
	if nodes[0].Decommissioned != "" {
		t.Errorf("a withdrawal recorded a decommission at %q", nodes[0].Decommissioned)
	}

	// AND THE ROW COMES BACK WITH THE HOST. A registration is the one thing that
	// makes a host placeable, and a withdrawal must leave it able to do that.
	mustRegister(t, a, NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB,
		Incarnation: "p2",
	})

	if headroom(t, a, "small") == 0 {
		t.Error("a host that registered again after withdrawing is still not placed on")
	}
}

// A WITHDRAWAL TOUCHES THE ONE HOST THAT WITHDREW. The statement is keyed on
// the name as well as the fences, and a second host at the same site must go
// on being placed on — that is the whole point of the message.
func TestAWithdrawalLeavesTheOtherHostsPlaceable(t *testing.T) {
	now := time.Now().UTC()
	a, epoch := withdrawingFleet(t, &now)

	mustRegister(t, a, NodeRegistration{
		Name: "epyc-2", Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB,
		Incarnation: "q1",
	})

	if err := a.NodeWithdrawn(t.Context(), "epyc-1", epoch, "p1"); err != nil {
		t.Fatalf("NodeWithdrawn: %v", err)
	}

	if nodeLive(t, a, "epyc-1") {
		t.Error("the host that withdrew is still recorded as live")
	}

	if !nodeLive(t, a, "epyc-2") {
		t.Error("a withdrawal took a host that did not withdraw out of the fleet")
	}

	if headroom(t, a, "small") == 0 {
		t.Error("the tier stopped advertising although another host can still serve it")
	}
}

// A WITHDRAWAL IS FENCED ON BOTH THE EPOCH AND THE INCARNATION, and each
// sub-test is what fails if one of the two predicates is dropped from the
// statement. A stale epoch is a host that registered again since the plane
// read it; a foreign incarnation is a superseded process still holding the
// certificate. Neither may take the current registration out of the fleet.
func TestAWithdrawalFromAStaleEpochOrAnotherIncarnationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name        string
		epoch       func(current int64) int64
		incarnation string
	}{
		{"a stale epoch", func(current int64) int64 { return current - 1 }, "p1"},
		{"an epoch from the future", func(current int64) int64 { return current + 1 }, "p1"},
		{"another incarnation", func(current int64) int64 { return current }, "p0"},
		{"no incarnation at all", func(current int64) int64 { return current }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			a, epoch := withdrawingFleet(t, &now)

			err := a.NodeWithdrawn(t.Context(), "epyc-1", tc.epoch(epoch), tc.incarnation)
			if !errors.Is(err, ErrWithdrawalStale) {
				t.Fatalf("NodeWithdrawn = %v, want ErrWithdrawalStale", err)
			}

			if !nodeLive(t, a, "epyc-1") {
				t.Error("a refused withdrawal still took the host out of the fleet")
			}

			if headroom(t, a, "small") == 0 {
				t.Error("a refused withdrawal still stopped placement on the host")
			}
		})
	}

	t.Run("a host that was never registered", func(t *testing.T) {
		now := time.Now().UTC()
		a, _ := withdrawingFleet(t, &now)

		err := a.NodeWithdrawn(t.Context(), "nobody", 1, "p1")
		if !errors.Is(err, ErrWithdrawalStale) {
			t.Fatalf("NodeWithdrawn for an unknown host = %v, want ErrWithdrawalStale", err)
		}
	})
}

// A WITHDRAWAL OBSERVES NOTHING ABOUT ANYBODY'S JOB, so it records no
// disruption and releases nothing — which is what separates it from NodeGone
// one function over, and the mutation that adds NodeGone's disruption write is
// what this fails against.
//
// The node only withdraws once it holds nothing, so a running lease here is not
// the ordinary case; it is the case the LEDGER operation has to be correct for
// regardless, because a lease escrowed against the host in the last instant is
// reachable and the plane answers that one "nothing started" rather than the
// ledger blaming the host.
func TestAWithdrawalMarksNoDisruptionAndReleasesNothing(t *testing.T) {
	now := time.Now().UTC()
	a, epoch := withdrawingFleet(t, &now)

	running := busyLease(t, a)
	escrowed := reserve(t, a, "small")

	if err := a.NodeWithdrawn(t.Context(), "epyc-1", epoch, "p1"); err != nil {
		t.Fatalf("NodeWithdrawn: %v", err)
	}

	for _, id := range []string{running.ID, escrowed.ID} {
		if token, _ := leaseDisruption(t, a, id); token != "" {
			t.Errorf("a withdrawal recorded %q against lease %s", token, id)
		}

		fresh, err := a.Lease(t.Context(), id)
		if err != nil {
			t.Fatalf("Lease(%s): %v", id, err)
		}

		if fresh.Phase == PhaseDone || fresh.Phase == PhaseFailed {
			t.Errorf("a withdrawal released lease %s (phase %s); it may only stop new placement",
				id, fresh.Phase)
		}
	}
}
