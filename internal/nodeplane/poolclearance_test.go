package nodeplane

import (
	"context"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// answerPoolAsk plays a node answering the inventory it was handed for a pool release:
// it must be an inventory under wantID.
func answerPoolAsk(t *testing.T, p *Plane, node, wantID string, instances []string) {
	t.Helper()

	cmd, took, err := p.Poll(t.Context(), node, "")
	if err != nil || !took {
		t.Fatalf("%s was not asked: took %v, %v", node, took, err)
	}
	if cmd.Kind != nodeapi.CommandInventory || cmd.BarrierID != wantID {
		t.Fatalf("%s was asked %+v, want an inventory for %s", node, cmd, wantID)
	}
	if err := p.Result(node, "", nodeapi.CommandResult{
		ID: cmd.ID, OK: true, BarrierID: cmd.BarrierID, Instances: instances,
	}); err != nil {
		t.Fatalf("Result: %v", err)
	}
}

// A POOL RELEASE ASKS ITS NODE WITHOUT THE DEPLOYMENT'S BARRIER (ADR-030): with no drain
// waiting, the barrier loop asks the connected node of each release what it runs, under
// the release's own id, and records the answer against the fence read before the
// question, empty or not. A node not connected is not asked; a pass inside the round
// asks nothing, and the first pass after it asks again.
func TestAPoolReleaseAsksItsNodeWithoutTheDeploymentsBarrier(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	store.hasBarrier = false
	store.poolTargets = []alloc.PoolClearanceTarget{
		{Order: "o1", Node: barrierHostName}, {Order: "o2", Node: "not-connected"},
	}
	p, reg := barrierPlane(t, store)

	p.barrierPass(t.Context())
	answerPoolAsk(t, p, barrierHostName, "pool-release-o1", nil)
	p.poolRound.Wait()

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

	// A CANCELLED PASS, so a round wrongly started inside the pace returns at once rather
	// than waiting on a node nobody plays.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	p.barrierPass(cancelled)
	p.poolRound.Wait()
	if got := store.poolRoundCount(); got != 1 {
		t.Fatalf("a pass inside the round started another: %d rounds", got)
	}

	p.poolAskedAt = time.Now().Add(-barrierRound)
	p.barrierPass(t.Context())
	answerPoolAsk(t, p, barrierHostName, "pool-release-o1", []string{"l1"})
	p.poolRound.Wait()

	if got := store.poolRoundCount(); got != 2 {
		t.Fatalf("the first pass after the round did not ask again: %d rounds", got)
	}
	if observed := store.poolObservations(); len(observed) != 2 || observed[1].Empty {
		t.Fatalf("a node listing an instance was recorded as %+v", observed)
	}
}

// A NODE SLOW TO ANSWER HOLDS UP ONLY ITS OWN RELEASE: the pass returns while it is
// still asked, so the deployment's barrier is not delayed, and another release's node
// is asked and recorded beside it.
func TestASlowPoolNodeHoldsUpOnlyItsOwnRelease(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	store.hasBarrier = false
	store.poolRecorded = make(chan alloc.PoolClearanceObservation, 4)
	p, reg := barrierPlane(t, store)
	register(t, p, "fast", config.ProviderDocker)
	store.setFence("fast", alloc.NodeFence{
		Epoch: reg.current(), Dispatch: 1, WireVersion: nodeapi.VersionComputeBarrier, Live: true,
	})
	store.poolTargets = []alloc.PoolClearanceTarget{
		{Order: "slow", Node: barrierHostName}, {Order: "quick", Node: "fast"},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer func() {
		cancel()
		p.poolRound.Wait()
	}()

	passed := make(chan struct{})

	go func() {
		defer close(passed)

		p.barrierPass(ctx)
	}()

	select {
	case <-passed:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass waited on the pool's nodes")
	}

	answerPoolAsk(t, p, "fast", "pool-release-quick", nil)

	select {
	case obs := <-store.poolRecorded:
		if obs.Order != "quick" || !obs.Empty {
			t.Fatalf("recorded %+v, want the quick node's empty answer", obs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the quick node's answer waited on the slow one")
	}
}

// A NODE WHOSE WIRE HAS NO INVENTORY IS NOT ASKED FOR ITS RELEASE, as it is not for the
// deployment's barrier: the command would hold its one slot until it timed out.
func TestAPoolNodeBelowTheBarrierProtocolIsNotAsked(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	store.hasBarrier = false
	store.poolTargets = []alloc.PoolClearanceTarget{{Order: "o1", Node: "old"}}
	p := testPlane(t, WithRegistrar(&countingRegistrar{}), WithBarrierStore(store))

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		MinVersion: nodeapi.MinVersion,
		Version:    nodeapi.VersionComputeBarrier - 1,
		Node:       "old", Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register an older node: %v", err)
	}
	store.setFence("old", alloc.NodeFence{Epoch: 1, Dispatch: 1, WireVersion: nodeapi.VersionComputeBarrier - 1})

	p.barrierPass(t.Context())
	p.poolRound.Wait()

	if got := p.QueuedForTest("old"); got != 0 {
		t.Fatalf("an older node was handed %d command(s)", got)
	}
	if got := store.poolObservations(); len(got) != 0 {
		t.Fatalf("an older node's release recorded %+v", got)
	}
}
