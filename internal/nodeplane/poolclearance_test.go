package nodeplane

import (
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A POOL RELEASE ASKS ITS NODE WITHOUT THE DEPLOYMENT'S BARRIER (ADR-030): with no drain
// waiting, the barrier loop asks the connected node of each release what it runs, under
// the release's own id, and records the answer against the fence read before the
// question. A node not connected is not asked, and the next pass inside the round asks
// nothing.
func TestAPoolReleaseAsksItsNodeWithoutTheDeploymentsBarrier(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	store.hasBarrier = false
	store.poolTargets = []alloc.PoolClearanceTarget{
		{Order: "o1", Node: barrierHostName}, {Order: "o2", Node: "not-connected"},
	}
	p, reg := barrierPlane(t, store)

	done := make(chan struct{})

	go func() {
		defer close(done)

		p.barrierPass(t.Context())
	}()

	cmd, took, err := p.Poll(t.Context(), barrierHostName, "")
	if err != nil || !took {
		t.Fatalf("the release's node was not asked: took %v, %v", took, err)
	}
	if cmd.Kind != nodeapi.CommandInventory || cmd.BarrierID != "pool-release-o1" {
		t.Fatalf("the node was asked %+v, want an inventory for pool-release-o1", cmd)
	}
	if err := p.Result(barrierHostName, "", nodeapi.CommandResult{
		ID: cmd.ID, OK: true, BarrierID: cmd.BarrierID,
	}); err != nil {
		t.Fatalf("Result: %v", err)
	}
	<-done

	observed := store.poolObservations()
	if len(observed) != 1 {
		t.Fatalf("want one clearance answer recorded, got %+v", observed)
	}
	if obs := observed[0]; obs.Order != "o1" || obs.Node != barrierHostName || !obs.Empty ||
		obs.Fence.Epoch != reg.current() || obs.Fence.Dispatch != 9 {
		t.Fatalf("the answer was recorded as %+v, want o1's empty answer under the fence read before it", obs)
	}
	if got := store.observations(); len(got) != 0 {
		t.Fatalf("a release's answer was recorded against the deployment's barrier: %+v", got)
	}

	p.barrierPass(t.Context())

	if got := store.poolRoundCount(); got != 1 {
		t.Fatalf("a second pass inside the round asked again: %d rounds", got)
	}
}
