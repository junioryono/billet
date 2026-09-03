package nodeplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
)

// barrierRound is how often an outstanding barrier asks the fleet again.
//
// WELL UNDER alloc's absence grace, because what is being established is a
// CONTINUOUS run rather than a single answer. At thirty seconds a five-minute
// window is ten samples, so a host that goes non-empty at any point in it is
// noticed rather than sampled around.
//
// The cost of asking is one command on a node that is by definition idle — the
// barrier only runs once the ledger holds nothing — and the cost of asking too
// rarely is a proof that says less than it appears to.
const barrierRound = 30 * time.Second

// barrierIdle is how long the loop waits when there is nothing to prove.
const barrierIdle = 5 * time.Second

// barrierFanOut bounds how many hosts are asked at once.
//
// AGAINST THE SERVER'S CONCURRENCY, NOT THE CLIENT'S PATIENCE. A node executes
// one command at a time and each command's timeout starts when it is QUEUED, so
// a fleet-wide fan-out starts one clock per host against queues that serve them
// in turn. This is the same lesson the listener's teardown learned; the number
// is small because a barrier is never urgent.
const barrierFanOut = 8

// BarrierStore is the durable half of a compute barrier.
//
// SEPARATE FROM Registrar because it is asked on a timer rather than on the
// command path, and because a plane without one simply never proves anything —
// which is the correct behaviour for the in-process and test wirings, not a
// degraded one.
type BarrierStore interface {
	// ComputeBarrierInForce reports the durable request, if a waiter made one.
	ComputeBarrierInForce(ctx context.Context) (alloc.ComputeBarrier, bool, error)
	// DropComputeBarrier removes a request that can no longer mean anything.
	DropComputeBarrier(ctx context.Context, id string) error
	// AdmissionGeneration is what the barrier's own generation is compared
	// against, so a resume voids it, and whether the deployment is sealed at it.
	AdmissionGeneration(ctx context.Context) (int64, bool, error)
	// Quiescence is the LEDGER barrier. The compute barrier only means something
	// once this holds nothing: until then a legitimate in-flight launch moves a
	// host's dispatch fence on every round and no run can ever complete.
	Quiescence(ctx context.Context) (alloc.Quiescence, error)
	// NodeFenceOf reads the epoch and dispatch generation an observation must be
	// taken against, captured BEFORE the host is asked.
	NodeFenceOf(ctx context.Context, node string) (alloc.NodeFence, bool, error)
	// RecordBarrierObservation stores one fenced answer, or ends that host's run.
	RecordBarrierObservation(ctx context.Context, obs alloc.BarrierObservation) error
	// InvalidateBarrierRun discards whatever a host had proved, because a new
	// incarnation is arriving and the ledger cannot see that yet.
	InvalidateBarrierRun(ctx context.Context, node string) error
	// InvalidateEveryBarrierRun does the same for an arrival that names no host
	// billet can identify — a loopback registration whose body would not decode.
	InvalidateEveryBarrierRun(ctx context.Context) error
	// ResolveQuarantineFor is reused rather than reimplemented: a barrier's
	// inventory is a real inventory, and feeding it through the one path that
	// already fences one frees quarantined capacity while the drain waits.
	ResolveQuarantineFor(ctx context.Context, node string, running []string, epoch int64) (int, error)
}

// WithBarrierStore lets this plane answer a compute barrier.
func WithBarrierStore(s BarrierStore) Option { return func(p *Plane) { p.barriers = s } }

// BarrierLoop asks the fleet what it is running, for as long as somebody is
// waiting for an answer.
//
// A TIMER FOR THE SAME REASON Watch IS ONE. `billet drain` runs in a separate
// process with no handle to this plane, so its request is a durable row and this
// is what observes it. Nothing else would: an idle sealed deployment dispatches
// no commands at all, which is exactly the state a drain is waiting out.
//
// IT PROVES NOTHING BY ITSELF. Every answer is recorded against a fence captured
// before the question was asked, and the predicate that reads those records is
// alloc's. This loop only makes sure the question keeps being asked.
func (p *Plane) BarrierLoop(ctx context.Context) {
	if p.barriers == nil {
		return
	}

	for {
		wait := p.barrierPass(ctx)

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// barrierPass runs one round and reports how long to wait before the next.
func (p *Plane) barrierPass(ctx context.Context) time.Duration {
	barrier, found, err := p.barriers.ComputeBarrierInForce(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return barrierIdle
		}

		p.log.Warn("could not read whether anybody is waiting for the fleet to be proved idle",
			"error", err)

		return barrierIdle
	}

	if !found {
		return barrierIdle
	}

	// SELF-CLEANING ON A RESUME. The barrier is scoped to the admission
	// generation it was requested under; once somebody reopens admission the
	// fleet may take work again, so every run under it describes a fleet that no
	// longer exists. Dropping the request is what stops this loop asking forever
	// after a drain was abandoned.
	generation, sealed, err := p.barriers.AdmissionGeneration(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("could not read admission while proving the fleet idle", "error", err)
		}

		return barrierIdle
	}

	if generation != barrier.Generation || !sealed {
		if err := p.barriers.DropComputeBarrier(ctx, barrier.ID); err != nil && ctx.Err() == nil {
			p.log.Warn("could not drop a compute barrier whose admission generation moved",
				"barrier", barrier.ID, "error", err)
		}

		return barrierIdle
	}

	// THE LEDGER FIRST. While a lease is still open a launch may legitimately be
	// dispatched, which moves a host's fence and discards its run — so asking
	// before the ledger is quiet burns commands to produce answers that can never
	// add up. Waiting here is also what makes the two barriers ORDERED rather
	// than merely both true at some point.
	q, err := p.barriers.Quiescence(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("could not read what the ledger still holds while proving the fleet idle",
				"error", err)
		}

		return barrierIdle
	}

	if !q.Quiet() {
		return barrierIdle
	}

	p.askFleet(ctx, barrier.ID)

	return barrierRound
}

// askFleet puts one inventory command to every host that can answer.
func (p *Plane) askFleet(ctx context.Context, barrierID string) {
	targets := p.barrierTargets()
	if len(targets) == 0 {
		return
	}

	var (
		wg   sync.WaitGroup
		slot = make(chan struct{}, barrierFanOut)
	)

	for _, n := range targets {
		wg.Add(1)

		go func() {
			defer wg.Done()

			select {
			case slot <- struct{}{}:
			case <-ctx.Done():
				return
			}

			defer func() { <-slot }()

			p.askNode(ctx, n, barrierID)
		}()
	}

	wg.Wait()
}

// barrierTargets lists the live hosts whose wire can answer an inventory.
//
// A HOST BELOW THE BARRIER VERSION IS SKIPPED HERE AND BLOCKS IN THE LEDGER.
// Sending it the command would burn its single command slot for the full
// command timeout on every round, and the answer would be a refusal — which is
// not an inventory. alloc reports it as too old to be asked, which is the fact
// an operator can act on.
func (p *Plane) barrierTargets() []*node {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.expireStaleLocked()

	out := make([]*node, 0, len(p.nodes))

	for _, n := range p.nodes {
		// A wire of zero is a registration this build did not negotiate — an
		// in-process or test wiring — and is asked, because it is this build.
		if n.wireVersion > 0 && n.wireVersion < nodeapi.VersionComputeBarrier {
			continue
		}

		out = append(out, n)
	}

	return out
}

// askNode asks one host what it is running and records the answer against the
// fence it had before the question was sent.
//
// THE FENCE IS CAPTURED FIRST, AND THAT ORDER IS THE PROOF. A launch dispatched
// after this read has already advanced the host's dispatch generation, so the
// recording transaction finds a different number and discards the run — the
// answer is refused rather than an invalidation racing it.
//
// Over-invalidating is possible and harmless: a launch that lands between the
// capture and the queueing costs this round.
func (p *Plane) askNode(ctx context.Context, n *node, barrierID string) {
	fence, found, err := p.barriers.NodeFenceOf(ctx, n.name)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("could not read a host's barrier fence", "node", n.name, "error", err)
		}

		return
	}

	if !found {
		// In the plane's map and not in the ledger. Nothing can be proved about a
		// host the clearance query will never walk, so there is nothing to record.
		return
	}

	id, err := commandID()
	if err != nil {
		p.log.Warn("could not mint an inventory command id", "node", n.name, "error", err)

		return
	}

	pend := &pending{
		cmd: nodeapi.Command{
			ID: id, Kind: nodeapi.CommandInventory, BarrierID: barrierID,
		},
		done: make(chan nodeapi.CommandResult, 1),
		// FENCED TO THE REGISTRATION THE FENCE ABOVE WAS READ UNDER. dispatch
		// refuses if a registration is in flight or the installed epoch has moved,
		// which is what stops a superseded process answering under its
		// replacement's epoch.
		expectedLedgerEpoch: fence.Epoch,
	}

	res, err := p.dispatch(ctx, n, pend)

	obs := alloc.BarrierObservation{Node: n.name, BarrierID: barrierID, Fence: fence}

	switch {
	case err != nil:
		// A host that could not be asked has not said it is empty. The run ends.
		if ctx.Err() != nil {
			return
		}

		p.log.Info("a host did not answer what it is running; its idle run starts again",
			"node", n.name, "error", err)
	case !res.OK:
		p.log.Info("a host could not read its provider; its idle run starts again",
			"node", n.name, "error", res.Error)
	case res.BarrierID != barrierID:
		// AN ECHO THAT DOES NOT MATCH IS NOT AN ANSWER TO THIS QUESTION. Only
		// reachable from a node that is confused or hostile, and the safe reading
		// of both is that this host has said nothing.
		p.log.Warn("a host answered an inventory for a different barrier",
			"node", n.name, "asked", barrierID, "answered", res.BarrierID)
	default:
		obs.Empty = len(res.Instances) == 0

		// A REAL INVENTORY, SO IT SETTLES REAL CAPACITY. The quarantine resolver
		// is the one path that already fences an inventory against the node epoch,
		// and reusing it means a drain actively converges the quarantined leases it
		// is otherwise waiting on rather than merely watching them.
		if freed, err := p.barriers.ResolveQuarantineFor(
			ctx, n.name, res.Instances, fence.Epoch); err != nil {
			if ctx.Err() == nil {
				p.log.Warn("could not reconcile quarantined capacity from a barrier inventory",
					"node", n.name, "error", err)
			}
		} else if freed > 0 {
			p.log.Info("freed capacity held for compute a host says it is not running",
				"node", n.name, "leases", freed)
		}
	}

	if err := p.barriers.RecordBarrierObservation(ctx, obs); err != nil && ctx.Err() == nil {
		p.log.Warn("could not record what a host said it is running",
			"node", n.name, "error", err)
	}
}

// BarrierTargetsForTest names the hosts a barrier round would ask, sorted.
//
// SORTED RATHER THAN IN askFleet's ORDER, which is a map iteration and therefore
// no order at all — an assertion against it would pass or fail by chance.
//
// Exported for tests only, and it exists because the skip it reveals is half of
// one rule: a host below the barrier version must not be SENT the command, and
// must be reported as unprovable. The two halves live in different packages, so
// without this an end-to-end test can only see the second and a mutant that
// deletes the first survives it.
func (p *Plane) BarrierTargetsForTest() []string {
	// barrierTargets takes p.mu itself, so nothing here may read p.nodes: sizing
	// the slice from it would be a read outside the lock.
	targets := p.barrierTargets()

	out := make([]string, 0, len(targets))

	for _, n := range targets {
		out = append(out, n.name)
	}

	slices.Sort(out)

	return out
}

// AskNodeForTest runs one barrier round against one host.
//
// Exported for tests only. The loop's own cadence is minutes wide by design, and
// a test that waited it out would be testing time.NewTimer.
func (p *Plane) AskNodeForTest(ctx context.Context, name, barrierID string) error {
	p.mu.Lock()
	n, ok := p.nodes[name]
	p.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrUnregistered, name)
	}

	if p.barriers == nil {
		return errors.New("nodeplane: this plane has no barrier store")
	}

	p.askNode(ctx, n, barrierID)

	return nil
}
