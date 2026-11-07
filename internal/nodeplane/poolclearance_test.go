package nodeplane

import (
	"context"
	"fmt"
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

// untilPoolAskDone waits until order's ask is no longer out: its answer is recorded
// before the ask lets go of its claim, so receiving the answer does not say it has.
func untilPoolAskDone(t *testing.T, p *Plane, order string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for {
		p.poolMu.Lock()
		out := p.poolInFlight[order]
		p.poolMu.Unlock()

		if !out {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ask of %s never finished", order)
		}

		time.Sleep(time.Millisecond)
	}
}

// agePoolAsk makes order's last ask a round old, so the next schedule asks it again.
func agePoolAsk(p *Plane, order string) {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()

	p.poolAskedAt[order] = time.Now().Add(-barrierRound)
}

// A POOL RELEASE ASKS ITS NODE WITHOUT THE DEPLOYMENT'S BARRIER (ADR-030): with no drain
// waiting, the clearance loop asks the connected node of each release what it runs,
// under the release's own id, and records the answer against the fence read before the
// question: empty, or compute listed. A node not connected is not asked; a schedule
// inside the round asks nothing, and the first after it asks again.
func TestAPoolReleaseAsksItsNodeWithoutTheDeploymentsBarrier(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	store.hasBarrier = false
	store.poolTargets = []alloc.PoolClearanceTarget{
		{Order: "o1", Node: barrierHostName, Generation: 4}, {Order: "o2", Node: "not-connected"},
	}
	p, reg := barrierPlane(t, store)

	p.schedulePoolAsks(t.Context())
	answerPoolAsk(t, p, barrierHostName, "pool-release-o1", nil)
	p.poolAsks.Wait()

	observed := store.poolObservations()
	if len(observed) != 1 {
		t.Fatalf("want one clearance answer recorded, got %+v", observed)
	}
	if obs := observed[0]; obs.Order != "o1" || obs.Node != barrierHostName || !obs.Empty || obs.Running ||
		obs.Fence.Epoch != reg.current() || obs.Fence.Dispatch != 9 || obs.Generation != 4 || obs.AskedAt.IsZero() {
		t.Fatalf("the answer was recorded as %+v, want o1's empty answer under the fence read before it", obs)
	}
	if got := store.observations(); len(got) != 0 {
		t.Fatalf("a release's answer was recorded against the deployment's barrier: %+v", got)
	}

	p.schedulePoolAsks(t.Context())
	p.poolAsks.Wait()
	if got := p.QueuedForTest(barrierHostName); got != 0 {
		t.Fatalf("a schedule inside the round asked again: %d queued", got)
	}

	agePoolAsk(p, "o1")
	p.schedulePoolAsks(t.Context())
	answerPoolAsk(t, p, barrierHostName, "pool-release-o1", []string{"l1"})
	p.poolAsks.Wait()

	if observed := store.poolObservations(); len(observed) != 2 || observed[1].Empty || !observed[1].Running {
		t.Fatalf("a node listing an instance was recorded as %+v", observed)
	}
}

// A NODE SLOW TO ANSWER HOLDS UP ONLY ITS OWN RELEASE: while its ask is still out,
// another release's node is asked, answers, and is asked again a round later; the slow
// node is not asked a second time beside the first.
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
		p.poolAsks.Wait()
	}()

	quick := func(round int) {
		t.Helper()
		answerPoolAsk(t, p, "fast", "pool-release-quick", nil)
		select {
		case obs := <-store.poolRecorded:
			if obs.Order != "quick" || !obs.Empty {
				t.Fatalf("round %d recorded %+v, want the quick node's empty answer", round, obs)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: the quick node's answer waited on the slow one", round)
		}
	}

	p.schedulePoolAsks(ctx)
	quick(1)
	untilPoolAskDone(t, p, "quick")

	agePoolAsk(p, "slow")
	agePoolAsk(p, "quick")
	p.schedulePoolAsks(ctx)
	quick(2)

	if got := p.QueuedForTest(barrierHostName); got != 1 {
		t.Fatalf("the slow node holds %d queued asks, want its first alone", got)
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

	p.schedulePoolAsks(t.Context())
	p.poolAsks.Wait()

	if got := p.QueuedForTest("old"); got != 0 {
		t.Fatalf("an older node was handed %d command(s)", got)
	}
	if got := store.poolObservations(); len(got) != 0 {
		t.Fatalf("an older node's release recorded %+v", got)
	}
}

// THE POOL'S CLEARANCES RUN ON A LOOP OF THEIR OWN: with the deployment's barrier waiting
// on a node that has taken its question and never answers, a release's node is asked,
// and asked again a round later, while that question is still out.
func TestAPoolReleaseIsAskedWhileTheDeploymentsBarrierWaits(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, reg := barrierPlane(t, store)
	register(t, p, "fast", config.ProviderDocker)
	store.setFence("fast", alloc.NodeFence{
		Epoch: reg.current(), Dispatch: 1, WireVersion: nodeapi.VersionComputeBarrier, Live: true,
	})
	store.poolTargets = []alloc.PoolClearanceTarget{{Order: "quick", Node: "fast"}}
	p.barrierPace = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	looped := make(chan struct{})

	go func() {
		defer close(looped)

		p.BarrierLoop(ctx)
	}()
	defer func() {
		cancel()
		<-looped
	}()

	// THE BARRIER'S QUESTION TO barrierHostName IS TAKEN AND NEVER ANSWERED, so its pass
	// is waiting from here on.
	if cmd, took, err := p.Poll(ctx, barrierHostName, ""); err != nil || !took || cmd.BarrierID != barrierFixtureID {
		t.Fatalf("the deployment's barrier did not ask %s: %+v, %v, %v", barrierHostName, cmd, took, err)
	}

	asked := 0

	for asked < 2 {
		cmd, took, err := p.Poll(ctx, "fast", "")
		if err != nil || !took {
			t.Fatalf("the fast node was not asked: took %v, %v", took, err)
		}
		if err := p.Result("fast", "", nodeapi.CommandResult{ID: cmd.ID, OK: true, BarrierID: cmd.BarrierID}); err != nil {
			t.Fatalf("Result: %v", err)
		}
		if cmd.BarrierID != "pool-release-quick" {
			continue
		}

		asked++
		untilPoolAskDone(t, p, "quick")
		agePoolAsk(p, "quick")
	}

	if got := p.QueuedForTest(barrierHostName); got != 0 {
		t.Fatalf("the barrier's question to %s was answered or re-sent: %d queued", barrierHostName, got)
	}
}

// THE LONGEST UNASKED IS ASKED FIRST: with every slot's worth of orders due again, an
// order never asked takes a slot ahead of them, so orders whose nodes keep failing
// slowly cannot starve another release.
func TestANeverAskedReleaseIsAskedAheadOfDueOnes(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	store.hasBarrier = false
	p, reg := barrierPlane(t, store)

	var targets []alloc.PoolClearanceTarget

	for i := range barrierFanOut + 1 {
		name := fmt.Sprintf("n%d", i)
		register(t, p, name, config.ProviderDocker)
		store.setFence(name, alloc.NodeFence{
			Epoch: reg.current(), Dispatch: 1, WireVersion: nodeapi.VersionComputeBarrier, Live: true,
		})
		targets = append(targets, alloc.PoolClearanceTarget{Order: fmt.Sprintf("o%d", i), Node: name})
	}
	store.poolTargets = targets

	p.poolMu.Lock()
	p.poolAskedAt, p.poolInFlight = map[string]time.Time{}, map[string]bool{}
	for _, target := range targets[:barrierFanOut] {
		p.poolAskedAt[target.Order] = time.Now().Add(-2 * barrierRound)
	}
	p.poolMu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer func() {
		cancel()
		p.poolAsks.Wait()
	}()

	p.schedulePoolAsks(ctx)

	// THE CLAIM, which the schedule takes before it returns; the ask itself is dispatched
	// by its own goroutine after.
	last := targets[barrierFanOut].Order
	p.poolMu.Lock()
	claimed, out := p.poolInFlight[last], len(p.poolInFlight)
	p.poolMu.Unlock()

	if !claimed || out != barrierFanOut {
		t.Fatalf("the order never asked was not asked first: %s claimed %v, %d asks out", last, claimed, out)
	}
}
