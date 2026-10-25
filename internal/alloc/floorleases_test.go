package alloc

import (
	"maps"
	"slices"
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

	var (
		fromFleet, fromLedger, doctored map[string]int
		heldOn                          map[string]map[string]int
	)

	err := a.db.View(t.Context(), func(tx querier) error {
		free, err := a.fleetResources(t.Context(), tx)
		if err != nil {
			return err
		}

		if free.heldOn != nil {
			heldOn = map[string]map[string]int{}
			for host, byTier := range free.heldOn {
				heldOn[host] = maps.Clone(byTier)
			}
		}

		if fromFleet, err = free.floorLeases(t.Context(), tx, a); err != nil {
			return err
		}

		if fromLedger, err = a.countFloorLeasesPerTier(t.Context(), tx); err != nil {
			return err
		}

		// THE FLEET'S OWN COUNTS ARE WHAT IS READ, not the ledger's again: a
		// count the fleet alone holds shows up in the answer.
		if free.heldOn == nil {
			return nil
		}

		free.heldOn["live"]["reserved"] += 100
		doctored, err = free.floorLeases(t.Context(), tx, a)

		return err
	})
	if err != nil {
		t.Fatalf("read the counts: %v", err)
	}

	// THE PURCHASE'S FLEET CARRIES WHAT EACH TIER HOLDS ON EACH HOST, the
	// host that is gone included, read in the pass that measured the usage.
	if heldOn == nil {
		t.Fatal("the fleet a purchase measures carries no lease counts, so its floors read the ledger again")
	}

	for host, byTier := range landed {
		if !maps.Equal(heldOn[host], byTier) {
			t.Fatalf("the fleet says %s holds %v, the leases landed %v", host, heldOn[host], byTier)
		}
	}

	if doctored["reserved"] != fromFleet["reserved"]+100 {
		t.Fatalf("a count the fleet alone holds moved the floors to %v from %v, so they were not read from it",
			doctored, fromFleet)
	}

	if !maps.Equal(fromLedger, want) {
		t.Fatalf("the ledger counts floors %v, want %v from where the leases landed (%v)", fromLedger, want, landed)
	}

	if !maps.Equal(fromFleet, fromLedger) {
		t.Fatalf("the fleet counts floors %v, the ledger %v", fromFleet, fromLedger)
	}
}

// WITHOUT A FLOOR THE FLEET READS NO TIER ROWS: a host's usage by tier is only
// more rows to read when no floor will count them, and it cost the target's
// purchase more than the pass it saved (2026-10-25).
func TestAFleetWithoutAFloorReadsNoTierRows(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{tier("one", 1, config.GiB), tier("two", 1, config.GiB)})

	for _, label := range []string{"one", "two"} {
		if _, err := a.Reserve(t.Context(), label); err != nil {
			t.Fatalf("Reserve(%s): %v", label, err)
		}
	}

	err := a.db.View(t.Context(), func(tx querier) error {
		free, err := a.fleetResources(t.Context(), tx)
		if err != nil {
			return err
		}

		if free.heldOn != nil {
			t.Errorf("a fleet with no floor read every tier's leases on every host: %v", free.heldOn)
		}

		// AND STILL CHARGED THE TWO LEASES TO THEIR HOST, so the cheaper read
		// is not a read of nothing.
		i := slices.IndexFunc(free.rows, func(n nodeRow) bool { return n.name == "test-host-firecracker" })
		if i < 0 {
			t.Fatalf("the fleet has no row for the host the leases landed on: %v", free.rows)
		}

		if left, total := free.vcpu[free.rows[i].name], free.rows[i].vcpu; left != total-2 {
			t.Errorf("the host has %d vCPU left of %d after two one-vCPU leases", left, total)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("measure the fleet: %v", err)
	}
}
