package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

// A CLOCK THE TEST CAN MOVE, FOR EXACTLY ONE DURATION.
//
// alloc's absence grace is five minutes, and it is a real duration rather than a
// tunable — so a test that established a proof by waiting it out would add five
// minutes to every run of this package. Everything else here is real: a real
// control plane, a real node wire, a real node loop and a real container
// runtime. Only the answer to "has this host been empty long enough" is steered.
type offsetClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *offsetClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Now().Add(c.offset)
}

func (c *offsetClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.offset += d
}

// barrierStack is a wire stack whose allocator runs on a steerable clock and
// whose reaper never ticks.
//
// THE REAPER IS OFF BECAUSE CANCELLING THE CONTROL PLANE DOES NOT RECALL A
// COMMAND IT HAS ALREADY GIVEN. Its tick dispatches CommandSweep, the node
// executes commands on its own goroutine, and a sweep delivered just before the
// stop destroys compute placed just after it — so a scenario that stops the
// server and then puts a stray on the host would race a destroy already on its
// way. An hour is "never" for a test and stays a real duration rather than a
// disabling flag, so nothing here depends on how the server reads a zero.
func barrierStack(t *testing.T) (*stack, *offsetClock) {
	t.Helper()

	clock := &offsetClock{}

	return newStackIn(t, t.TempDir(), newPlane(t), overTheWire,
		withClock(clock.now), withReapInterval(time.Hour)), clock
}

// runBarrierStack starts the control plane and returns a stop that is safe to
// call from the test body AND from cleanup.
//
// A DRAIN HAS NO TIMEOUT, so a test that fails an assertion while the node is
// holding compute must still reach the second signal. `run`'s stop is what
// closes it, and a stop reached only on the happy path leaves cleanup waiting
// for work that never completes — a hang instead of the assertion that would
// have named the regression.
func (s *stack) runBarrierStack(t *testing.T) func() {
	t.Helper()

	stop := sync.OnceFunc(s.run(t))

	t.Cleanup(stop)

	return stop
}

// seal puts the deployment into the state a drain waits from.
func (s *stack) seal(t *testing.T) state.Admission {
	t.Helper()

	current, err := s.db.Admission(t.Context())
	if err != nil {
		t.Fatalf("read admission: %v", err)
	}

	sealed, err := s.db.Seal(t.Context(), state.SealRequest{
		Expect:     current.Generation,
		Provenance: state.ProvenanceOperator,
		Reason:     "e2e compute barrier",
		Actor:      "e2e",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	return sealed
}

// clearanceFor is one host's line out of the fleet's answer.
func (s *stack) clearanceFor(
	t *testing.T, node string,
) (alloc.ComputeClearance, alloc.NodeClearance) {
	t.Helper()

	c, err := s.alloc.ComputeClear(t.Context())
	if err != nil {
		t.Fatalf("ComputeClear: %v", err)
	}

	for _, n := range c.Nodes {
		if n.Node == node {
			return c, n
		}
	}

	t.Fatalf("node %q is not in the fleet's clearance (%d hosts)", node, len(c.Nodes))

	return c, alloc.NodeClearance{}
}

// ask puts one real inventory command on the real wire and waits for the answer.
func (s *stack) ask(t *testing.T, barrierID string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	if err := s.wire.AskNodeForTest(ctx, s.node, barrierID); err != nil {
		t.Fatalf("ask the fleet what it is running: %v", err)
	}
}

// THE WHOLE OF THE COMPUTE BARRIER, ON REAL COMPUTE: the ledger reports quiet
// while compute billet started is still running, and the barrier is what sees it.
//
// The scenario is built in two halves, because A LIVE BILLET DESTROYS A STRAY
// ALMOST AT ONCE and that is correct behaviour rather than something to work
// around — measured here: 45ms after the lease went terminal, the host logged
// "destroying an instance whose lease is gone". So first a real job runs and is
// cleaned up, which exercises the chain this test's subject travels: a real
// dispatch, a real container, a real teardown. Then, with the control plane
// stopped and nothing left sweeping, a container is put on the host carrying a
// lease name the ledger has never heard of.
//
// That is the second of the two shapes the ledger cannot see — a launch whose
// lease was reclaimed creates compute it then fails to destroy. Nothing in the
// ledger records it, and the host's own inventory is the only instrument that can
// find it. It is also exactly the window the barrier exists for: the host that
// would sweep the stray away is the host being stopped.
//
// Nothing here is simulated. The stray is a real container under billet's own
// name shape, the inventory travels the real node wire as a real command, and
// the list comes back out of a real container runtime.
func TestTheBarrierSeesRealComputeWhoseLeaseIsGone(t *testing.T) {
	s, clock := barrierStack(t)

	// A REAL JOB, THROUGH THE REAL DISPATCH PATH, run to completion so the
	// control plane can be stopped with nothing outstanding.
	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 4101, "push", testTier))

	stop := s.runBarrierStack(t)

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 4101, "push", testTier))

	names := s.awaitOneRunning(t)

	if _, ours := provider.LeaseOf(names[0]); !ours {
		t.Fatalf("container %q does not carry a billet lease name", names[0])
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		fakeactions.JobJSON("JobCompleted", 4101, "push", testTier))

	s.awaitGone(t)

	// THE CONTROL PLANE STOPS. Nothing sweeps after this: the reaper rode the
	// server's own tick, and this node's sweep is not on a timer.
	stop()

	// A CONTAINER WITH NO LEASE, put there the way a lost launch leaves one —
	// billet's own name shape, on the host's real container runtime, with nothing
	// in the ledger to attribute it to.
	const strayLease = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaff"

	if _, err := s.provider.Launch(t.Context(), provider.Spec{
		Name:      provider.InstanceName(strayLease),
		Image:     testImage,
		VCPU:      1,
		Memory:    config.GiB,
		Command:   []string{"sleep", "300"},
		Trust:     provider.TrustTrusted,
		JITConfig: "acceptance-stray",
	}); err != nil {
		t.Fatalf("put a stray on the host: %v", err)
	}

	sealed := s.seal(t)

	// THE LEDGER BARRIER IS SATISFIED. This assertion is the defect stated
	// positively: if it ever fails, the ledger has grown the ability to see this
	// class and the compute barrier's premise is gone.
	q, err := s.alloc.Quiescence(t.Context())
	if err != nil {
		t.Fatalf("Quiescence: %v", err)
	}

	if !q.Quiet() {
		t.Fatalf("the ledger did not report quiet while only a stray remained: %+v", q)
	}

	// ...and the compute is there, on the host, right now.
	instances, err := s.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// RUNNING, NOT MERELY PRESENT. `List` reports exited containers too, and the
	// node's inventory reports their lease ids just the same — so without this a
	// stray whose command returned immediately would satisfy every assertion
	// below while proving nothing about compute that is still executing.
	if len(instances) != 1 || !instances[0].Running {
		t.Fatalf("expected exactly one RUNNING stray, found %s", describe(instances))
	}

	barrier, err := s.alloc.RequestComputeBarrier(t.Context(), sealed.Generation, "e2e")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	s.ask(t, barrier.ID)

	c, host := s.clearanceFor(t, s.node)

	if c.Clear() {
		t.Fatal("the fleet was reported CLEAR while a container billet named was running")
	}

	if host.State != alloc.ClearanceRunning {
		t.Fatalf("host state is %v, want %v — the host answered the inventory, so its own "+
			"answer must be what the report shows", host.State, alloc.ClearanceRunning)
	}

	// The stray goes, for real.
	if _, err := s.provider.Destroy(t.Context(), instances[0].ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	s.ask(t, barrier.ID)

	c, host = s.clearanceFor(t, s.node)

	if c.Clear() {
		t.Fatal("one empty answer cleared the fleet; the grace is what makes a run evidence")
	}

	if host.State != alloc.ClearanceSettling {
		t.Fatalf("host state is %v, want %v", host.State, alloc.ClearanceSettling)
	}

	// A SECOND EMPTY ANSWER, TAKEN AFTER THE GRACE HAS ELAPSED. Both ends of the
	// run come from the allocator's clock, so moving it forward between the two
	// samples is exactly the span the predicate measures.
	clock.advance(6 * time.Minute)
	s.ask(t, barrier.ID)

	c, host = s.clearanceFor(t, s.node)

	if host.State != alloc.ClearanceProved {
		t.Fatalf("host state is %v, want %v (empty since %s, clears at %s)",
			host.State, alloc.ClearanceProved, host.EmptySince, host.ClearAt)
	}

	if !c.Clear() {
		t.Fatalf("the fleet is not clear though its only host is proved: %+v", c)
	}
}

// A HOST THAT CANNOT BE REACHED BLOCKS THE DRAIN, and a person is what moves it.
//
// The second half of the safety claim: a proof is about every expected host, and
// a host billet cannot ask is not evidence of anything. It is the case an
// operator meets when a machine is off, and the only way through is an explicit
// decommission that records what it did and did not establish.
//
// WHAT IS UNDER TEST STARTS AT THE UNREACHABLE ROW, NOT BEFORE IT. The node loop
// really is stopped, so nothing is heartbeating under that name, but the row is
// then marked through `NodeGone` rather than by waiting out the plane's own
// expiry — that is a lease TTL of wall clock for a fact `internal/nodeplane`
// already tests. So this proves what a drain and a decommission do ABOUT such a
// host, and claims nothing about what put it in that state.
func TestAnUnreachableHostBlocksUntilSomebodyDecommissionsIt(t *testing.T) {
	s, _ := barrierStack(t)

	sealed := s.seal(t)

	if _, err := s.alloc.RequestComputeBarrier(t.Context(), sealed.Generation, "e2e"); err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	// THE MACHINE GOES AWAY FOR REAL: the node loop stops, so nothing is
	// heartbeating under this name any more, and the control plane records what
	// it records when a host stops answering.
	s.stopNode()

	fence, found, err := s.alloc.NodeFenceOf(t.Context(), s.node)
	if err != nil || !found {
		t.Fatalf("NodeFenceOf(%s): %v (found=%v)", s.node, err, found)
	}

	if err := s.alloc.NodeGone(t.Context(), s.node, fence.Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	before, host := s.clearanceFor(t, s.node)

	if before.Clear() {
		t.Fatal("a fleet with an unreachable host was reported clear")
	}

	if host.State != alloc.ClearanceUnreachable {
		t.Fatalf("host state is %v, want %v", host.State, alloc.ClearanceUnreachable)
	}

	// An unforced decommission must refuse: nothing has been proved about it.
	proven, err := s.alloc.Decommission(t.Context(), alloc.DecommissionRequest{
		Node: s.node, Actor: "e2e",
	})
	if err == nil {
		t.Fatalf("decommission succeeded (proven=%v) with no proof and no --force", proven)
	}

	// Forced, it goes through — and stays marked as something billet never
	// established.
	proven, err = s.alloc.Decommission(t.Context(), alloc.DecommissionRequest{
		Node: s.node, Actor: "e2e", Force: true,
	})
	if err != nil {
		t.Fatalf("forced Decommission: %v", err)
	}

	if proven {
		t.Fatal("a forced decommission of an unreachable host reported itself PROVEN")
	}

	after, err := s.alloc.ComputeClear(t.Context())
	if err != nil {
		t.Fatalf("ComputeClear: %v", err)
	}

	if !after.Clear() {
		t.Fatalf("the fleet is not clear after its only host was decommissioned: %+v", after)
	}

	unproven := after.Unproven()
	if len(unproven) != 1 || unproven[0] != s.node {
		t.Fatalf("Unproven() is %v, want [%s] — a forced exclusion that stops being "+
			"reported is the laundering this exists to prevent", unproven, s.node)
	}
}

// A HOST ON THE OLD WIRE IS NEVER ASKED, AND BLOCKS FOREVER RATHER THAN BEING
// ASSUMED CLEAR.
//
// The bridge window is a real deployment state: the server is upgraded first and
// nodes roll at leisure, so a fleet mid-upgrade holds hosts that cannot answer an
// inventory at all. Two separate mechanisms have to agree about them, and each
// has its own unit test — the plane must not SEND the command (a refusal is not
// an inventory, and asking burns the host's single command slot for the full
// command timeout), and the ledger must report it as unprovable. This is the
// only place they are exercised together, against a registration that really did
// negotiate 13 on the real wire rather than a row written to say it had.
func TestAHostOnTheOldWireIsNeverAskedAndNeverProved(t *testing.T) {
	s, clock := barrierStack(t)

	deployment, err := state.DeploymentID(s.dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	registerAtWire(t, s.wireAddr, deployment, "legacy-1", nodeapi.VersionComputeBarrier-1)

	sealed := s.seal(t)

	barrier, err := s.alloc.RequestComputeBarrier(t.Context(), sealed.Generation, "e2e")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	// THE HALF THAT LIVES IN THE PLANE: a barrier round would not put the command
	// on that host at all. Asserted here rather than only in internal/nodeplane
	// because the two halves are one rule — a host that cannot answer must be
	// skipped AND reported — and a test that saw only the ledger's half would
	// survive deleting the skip, which costs that host's single command slot for
	// the full command timeout on every round.
	if targets := s.wire.BarrierTargetsForTest(); len(targets) != 1 || targets[0] != s.node {
		t.Fatalf("a barrier round would ask %v; it must ask only %s, because a refusal "+
			"is not an inventory", targets, s.node)
	}

	// THE CURRENT HOST IS PROVED, so nothing but the old one can be what blocks.
	s.ask(t, barrier.ID)
	clock.advance(6 * time.Minute)
	s.ask(t, barrier.ID)

	if _, host := s.clearanceFor(t, s.node); host.State != alloc.ClearanceProved {
		t.Fatalf("the current host is %v, want %v", host.State, alloc.ClearanceProved)
	}

	c, legacy := s.clearanceFor(t, "legacy-1")

	if legacy.State != alloc.ClearanceBelowProtocol {
		t.Fatalf("the old host is %v, want %v", legacy.State, alloc.ClearanceBelowProtocol)
	}

	if legacy.WireVersion != nodeapi.VersionComputeBarrier-1 {
		t.Fatalf("the ledger recorded wire %d for a host that negotiated %d",
			legacy.WireVersion, nodeapi.VersionComputeBarrier-1)
	}

	if c.Clear() {
		t.Fatal("a fleet holding a host that cannot answer an inventory was reported CLEAR")
	}

	blocking := c.Blocking()
	if len(blocking) != 1 || blocking[0].Node != "legacy-1" {
		t.Fatalf("Blocking() is %v, want just legacy-1 — the report has to name the host "+
			"an operator would have to upgrade or decommission", blocking)
	}
}

// describe renders an inventory readably. The slice holds POINTERS, so the
// obvious %+v prints addresses — which is what a failure here would otherwise
// say about the one thing worth seeing.
func describe(instances []*provider.Instance) string {
	out := make([]string, 0, len(instances))

	for _, inst := range instances {
		out = append(out, fmt.Sprintf("%s(running=%v)", inst.Name, inst.Running))
	}

	return "[" + strings.Join(out, " ") + "]"
}

// registerAtWire joins the fleet over the real wire, at a version this build's
// nodeclient cannot send.
//
// A RAW REQUEST BECAUSE THE POINT IS THE VERSION. nodeclient always announces
// this build's range, so the only way to be a host mid-upgrade is to write the
// body an older one would have written.
func registerAtWire(t *testing.T, addr, deployment, name string, version int) {
	t.Helper()

	body, err := json.Marshal(nodeapi.RegisterRequest{
		Version: version, MinVersion: nodeapi.MinVersion,
		Node: name, Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
		InventoryKnown: true,
	})
	if err != nil {
		t.Fatalf("marshal a registration: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+addr+"/v1/register", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build a registration: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register %s at wire %d: %v", name, version, err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		out, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("register %s at wire %d: %s, and its body would not read: %v",
				name, version, res.Status, err)
		}

		t.Fatalf("register %s at wire %d: %s: %s", name, version, res.Status, out)
	}
}
