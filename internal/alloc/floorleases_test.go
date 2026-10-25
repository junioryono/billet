package alloc

import (
	"maps"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A PURCHASE COUNTS A FLOOR'S LEASES FROM WHAT IT ALREADY READ, and the count
// is the ledger's own: a lease on a draining host counts toward its tier's
// floor, a lease on a host the plane has lost contact with does not, and every
// tier's count agrees with CountOpenPerTierOnLiveHosts, which the potential
// view still asks.
func TestAFleetCountsAFloorsLeasesAsTheLedgerDoes(t *testing.T) {
	reserved := tier("reserved", 1, config.GiB)
	reserved.Reserved = 3
	other := tier("other", 1, config.GiB)

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{reserved, other}, WithPlacement(config.PlacementSpread))

	epochs := map[string]int64{}

	for _, name := range []string{"draining", "gone", "live"} {
		_, epoch := mustRegister(t, a, NodeRegistration{
			Name: name, Provider: config.ProviderFirecracker, VCPU: 8, Memory: 32 * config.GiB,
			Incarnation: "p-" + name})
		epochs[name] = epoch
	}

	// WHERE EACH LEASE LANDED, by tier, so what the floors must count is known
	// from the placement rather than assumed.
	landed := map[string]map[string]int{}

	for _, label := range []string{"reserved", "other", "reserved", "other", "reserved", "other"} {
		lease, err := a.Reserve(t.Context(), label)
		if err != nil {
			t.Fatalf("Reserve(%s): %v", label, err)
		}

		if landed[lease.TargetNode] == nil {
			landed[lease.TargetNode] = map[string]int{}
		}

		landed[lease.TargetNode][label]++
	}

	// THE CASES WERE MADE: leases on the host that goes, which the floors leave
	// out, and on the one that drains, which they keep.
	if len(landed["gone"]) == 0 || len(landed["draining"]) == 0 {
		t.Fatalf("leases landed %v, so one of the hosts this is about holds none", landed)
	}

	if err := a.NodeDraining(t.Context(), "draining", epochs["draining"], "p-draining"); err != nil {
		t.Fatalf("NodeDraining: %v", err)
	}

	if err := a.NodeGone(t.Context(), "gone", epochs["gone"]); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	want := map[string]int{}
	for _, host := range []string{"draining", "live"} {
		for label, n := range landed[host] {
			want[label] += n
		}
	}

	var fromFleet, fromLedger map[string]int

	err := a.db.View(t.Context(), func(tx querier) error {
		free, err := a.fleetResources(t.Context(), tx)
		if err != nil {
			return err
		}

		if fromFleet, err = free.floorLeases(t.Context(), tx, a); err != nil {
			return err
		}

		fromLedger, err = a.countFloorLeasesPerTier(t.Context(), tx)

		return err
	})
	if err != nil {
		t.Fatalf("read the counts: %v", err)
	}

	if !maps.Equal(fromLedger, want) {
		t.Fatalf("the ledger counts floors %v, want %v from where the leases landed (%v)", fromLedger, want, landed)
	}

	if !maps.Equal(fromFleet, fromLedger) {
		t.Fatalf("the fleet counts floors %v, the ledger %v", fromFleet, fromLedger)
	}
}
