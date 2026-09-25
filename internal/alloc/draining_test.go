package alloc

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// A HOST WHOSE PROCESS IS DRAINING STOPS BEING PLACED ON AND STAYS LIVE, and is
// placed on again once it registers.
//
// Live, because the draining process still answers the destroys and completions
// its running work needs. The assertion is the tier's headroom, which is what
// placement reads, not the column.
func TestADrainingNodeLeavesPlacementButStaysLive(t *testing.T) {
	now := time.Now().UTC()
	a, epoch := withdrawingFleet(t, &now)

	if headroom(t, a, "small") == 0 {
		t.Fatal("the tier advertises nothing before the drain, so this proves nothing")
	}

	if err := a.NodeDraining(t.Context(), "epyc-1", epoch, "p1"); err != nil {
		t.Fatalf("NodeDraining: %v", err)
	}

	if got := headroom(t, a, "small"); got != 0 {
		t.Errorf("headroom while the host drains = %d, want 0: placement is still aiming "+
			"work at a host that refuses it", got)
	}

	if !nodeLive(t, a, "epyc-1") {
		t.Error("marking a host draining took it out of contact; its running work still needs it")
	}

	mustRegister(t, a, NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB,
		Incarnation: "p2",
	})

	if headroom(t, a, "small") == 0 {
		t.Error("a host that registered again after draining is still not placed on")
	}
}

// A LEASE ON A DRAINING HOST STILL SATISFIES ITS FLOOR, unlike one on a host that
// is gone. The job is running and occupies the slot the floor promised; counting
// it unmet would hold the other host for a tier that already has what it was
// promised and cannot buy more.
func TestAFloorIsSatisfiedByALeaseOnADrainingHost(t *testing.T) {
	reserved := tier("reserved", 4, 8*config.GiB)
	reserved.Provider = config.ProviderDocker
	reserved.Reserved = 1

	greedy := tier("greedy", 4, 8*config.GiB)
	greedy.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{reserved, greedy})

	// Two machines, one slot each.
	epochs := map[string]int64{}

	for _, name := range []string{"a", "b"} {
		_, epoch := mustRegister(t, a, NodeRegistration{
			Name: name, Provider: config.ProviderDocker, VCPU: 4, Memory: 64 * config.GiB,
			Incarnation: "p-" + name})
		epochs[name] = epoch
	}

	lease, err := a.Reserve(t.Context(), "reserved")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if lease.TargetNode == "" {
		t.Fatal("escrow did not place the lease on a machine")
	}

	draining := lease.TargetNode
	if err := a.NodeDraining(t.Context(), draining, epochs[draining], "p-"+draining); err != nil {
		t.Fatalf("NodeDraining: %v", err)
	}

	if got := headroom(t, a, "greedy"); got != 1 {
		t.Errorf("the unreserved tier was offered %d slots, want the free machine's one: the "+
			"reserved tier's floor is met by its job on the draining host", got)
	}
}

// FENCED LIKE A WITHDRAWAL: a superseded process, or a registration the plane
// read before the host registered again, cannot take the current one out.
func TestADrainingMarkFromAStaleEpochOrAnotherIncarnationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name        string
		epoch       func(current int64) int64
		incarnation string
	}{
		{"stale epoch", func(current int64) int64 { return current - 1 }, "p1"},
		{"another incarnation", func(current int64) int64 { return current }, "p0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			a, epoch := withdrawingFleet(t, &now)

			err := a.NodeDraining(t.Context(), "epyc-1", tc.epoch(epoch), tc.incarnation)
			if !errors.Is(err, ErrWithdrawalStale) {
				t.Fatalf("NodeDraining = %v, want ErrWithdrawalStale", err)
			}

			if headroom(t, a, "small") == 0 {
				t.Error("a refused draining mark still took the host out of placement")
			}
		})
	}
}
