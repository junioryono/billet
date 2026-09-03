package alloc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// testClock is a clock a test moves by hand.
//
// THE GRACE IS FIVE MINUTES, and a test that waited it out would be testing
// time.Sleep. Every assertion here about "long enough" is arithmetic on a clock
// the test owns, so it holds on any machine under any load.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// barrierAllocator is one host, one tier, and a clock the test drives.
func barrierAllocator(t *testing.T) (*Allocator, *testClock) {
	t.Helper()

	clock := newTestClock()

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{tier("barrier-tier", 2, 4*config.GiB)}, WithClock(clock.Now))

	registerBarrierHost(t, a, "host-a")

	return a, clock
}

func registerBarrierHost(t *testing.T, a *Allocator, name string) int64 {
	t.Helper()

	reg := testRegistration(name, config.ProviderFirecracker)
	reg.WireMin = 12
	reg.WireMax = BarrierWireVersion
	reg.WireVersion = BarrierWireVersion

	epoch, err := a.RegisterNode(t.Context(), reg)
	if err != nil {
		t.Fatalf("RegisterNode(%s): %v", name, err)
	}

	return epoch
}

func sealBarrier(t *testing.T, a *Allocator) int64 {
	t.Helper()

	sealed, err := a.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	return sealed.Generation
}

// rerequestBarrier reopens the deployment, closes it again, and asks for a
// barrier under the generation that leaves — the way a second operator's drain
// arrives after somebody resumed.
func rerequestBarrier(t *testing.T, a *Allocator, current int64) ComputeBarrier {
	t.Helper()

	resumed, err := a.db.Resume(t.Context(), state.ResumeRequest{
		Expect: current, Clears: state.ProvenanceOperator, Actor: "someone else",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	sealed, err := a.db.Seal(t.Context(), state.SealRequest{
		Expect: resumed.Generation, Provenance: state.ProvenanceOperator, Actor: "ops",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	barrier, err := a.RequestComputeBarrier(t.Context(), sealed.Generation, "ops")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	return barrier
}

func requestBarrier(t *testing.T, a *Allocator) ComputeBarrier {
	t.Helper()

	generation := sealBarrier(t, a)

	barrier, err := a.RequestComputeBarrier(t.Context(), generation, "ops")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	return barrier
}

func fenceOf(t *testing.T, a *Allocator, node string) NodeFence {
	t.Helper()

	fence, found, err := a.NodeFenceOf(t.Context(), node)
	if err != nil {
		t.Fatalf("NodeFenceOf(%s): %v", node, err)
	}
	if !found {
		t.Fatalf("NodeFenceOf(%s): the host is not in the ledger", node)
	}

	return fence
}

// observe answers the barrier for the fixture's first host.
//
// host-a BY DEFAULT: the multi-host tests are about what a SECOND host's silence
// does to the fleet's verdict, so only the first ever answers. observeNode is
// for the one test that needs runs on more than one.
func observe(t *testing.T, a *Allocator, barrier ComputeBarrier, empty bool) {
	t.Helper()

	observeNode(t, a, barrier, "host-a", empty)
}

func observeNode(t *testing.T, a *Allocator, barrier ComputeBarrier, node string, empty bool) {
	t.Helper()

	if err := a.RecordBarrierObservation(t.Context(), BarrierObservation{
		Node: node, BarrierID: barrier.ID, Fence: fenceOf(t, a, node), Empty: empty,
	}); err != nil {
		t.Fatalf("RecordBarrierObservation(%s): %v", node, err)
	}
}

func clearance(t *testing.T, a *Allocator) ComputeClearance {
	t.Helper()

	c, err := a.ComputeClear(t.Context())
	if err != nil {
		t.Fatalf("ComputeClear: %v", err)
	}

	return c
}

func stateOf(t *testing.T, c ComputeClearance, node string) ClearanceState {
	t.Helper()

	return lineOf(t, c, node).State
}

// lineOf is a host's whole clearance line, for an assertion about what the run
// carries rather than only what it is called.
func lineOf(t *testing.T, c ComputeClearance, node string) NodeClearance {
	t.Helper()

	for _, n := range c.Nodes {
		if n.Node == node {
			return n
		}
	}

	t.Fatalf("no clearance for %s in %+v", node, c.Nodes)

	return NodeClearance{}
}

// establishProof takes a host all the way to ClearanceProved.
//
// TWO OBSERVATIONS SPANNING THE GRACE, because that is what a proof IS: the run
// claims the host was continuously empty across the window, and the evidence is
// an empty answer taken at or after the far end of it. One answer plus a clock
// is elapsed time authorising a claim about a machine, which is the P0 an
// adversarial review found in the first version of this.
func establishProof(t *testing.T, a *Allocator, clock *testClock, barrier ComputeBarrier) {
	t.Helper()

	observe(t, a, barrier, true)
	clock.advance(computeAbsenceGrace)
	observe(t, a, barrier, true)
}

// storedRuns counts the rows in compute_barrier_nodes, whatever barrier they
// name.
//
// ComputeClear CANNOT SEE THEM. It filters by the barrier in force, so a row
// left behind by a superseded barrier is invisible to it whether or not anything
// deleted it — which makes every assertion phrased through ComputeClear blind to
// the deletion it is named for. Adversarial review found two of exactly that.
func storedRuns(t *testing.T, a *Allocator) int {
	t.Helper()

	var n int

	if err := a.db.View(t.Context(), func(q querier) error {
		return q.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM compute_barrier_nodes`).Scan(&n)
	}); err != nil {
		t.Fatalf("count stored runs: %v", err)
	}

	return n
}

// ONE EMPTY ANSWER IS NOT CLEARANCE, AND NEITHER IS ONE PLUS A CLOCK.
//
// A create the daemon has already accepted can be invisible to a listing issued
// straight afterwards, so a host that answers "nothing" once has said something
// about a moment rather than about a machine. What the grace requires is a
// SECOND look at the end of it — and the host going quiet in between (a node
// that disconnected a second after answering) leaves the claim unmade, however
// much time passes.
func TestOneEmptyAnswerIsNotClearance(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceSettling {
		t.Fatalf("one empty answer reported %v, want %v", got, ClearanceSettling)
	}

	if c := clearance(t, a); c.Clear() {
		t.Fatal("the fleet was reported clear on a single empty answer")
	}

	// AND NO AMOUNT OF SILENCE TURNS IT INTO ONE. The host said nothing further;
	// the epoch and dispatch fences have not moved, so neither of those catches
	// it, and only the missing second observation does.
	clock.advance(10 * computeAbsenceGrace)

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved {
		t.Fatalf("a single empty answer aged into a proof: %v", got)
	}

	if c := clearance(t, a); c.Clear() {
		t.Fatal("the fleet was reported clear on one answer and a clock; that is elapsed " +
			"time authorising a claim about a machine")
	}
}

// A RUN IS PROVED BY A SAMPLE AT THE END OF THE GRACE.
//
// The companion to the test above, and not optional: a predicate that proved
// nothing would pass that one and make the whole barrier unusable.
func TestARunSampledAtTheEndOfTheGraceIsProved(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	observe(t, a, barrier, true)

	// One instant short of the grace is still not enough, even with a sample.
	clock.advance(computeAbsenceGrace - time.Nanosecond)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceSettling {
		t.Fatalf("a sample just inside the grace reported %v, want %v", got, ClearanceSettling)
	}

	clock.advance(time.Nanosecond)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("a run sampled at the end of the grace reported %v, want %v",
			got, ClearanceProved)
	}

	if c := clearance(t, a); !c.Clear() {
		t.Fatalf("a fleet whose only host is proved is not clear: %+v", c)
	}
}

// A RUN IS CONTINUOUS OR IT IS NOTHING. Repeated empty answers must keep the
// clock the first one started — otherwise no host could ever cross the grace —
// and a single non-empty answer must throw all of it away.
func TestARunIsPreservedByEmptyAnswersAndEndedByAnyOtherKind(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	observe(t, a, barrier, true)

	clock.advance(computeAbsenceGrace / 2)
	observe(t, a, barrier, true)

	// THE SECOND EMPTY ANSWER DID NOT RESTART THE CLOCK. Half the grace has
	// passed, so a sample half a grace later is one taken at the far end of the
	// run the FIRST answer started — and if empty_since had been reset by that
	// second answer, this would still be half a grace short.
	clock.advance(computeAbsenceGrace / 2)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("a continuous run was restarted by its own samples: %v", got)
	}

	// And a host that says it IS running something ends the run outright.
	observe(t, a, barrier, false)

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved {
		t.Fatal("a host that reported compute stayed proved clear")
	}

	clock.advance(2 * computeAbsenceGrace)

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved {
		t.Fatalf("an ended run aged into a proof: %v", got)
	}
}

// A LAUNCH DISPATCHED AFTER THE BARRIER WAS ISSUED VOIDS ITS ANSWER.
//
// This is the fence the whole design rests on. The observation is taken against
// the host's dispatch generation BEFORE the question is asked, so a launch that
// advanced it makes the recording transaction match nothing — the answer is
// refused rather than an invalidation racing it.
func TestALaunchAfterTheBarrierVoidsTheAnswer(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	// The fence as it was when the question went out.
	fence := fenceOf(t, a, "host-a")

	if _, err := a.BumpDispatch(t.Context(), "host-a"); err != nil {
		t.Fatalf("BumpDispatch: %v", err)
	}

	// The host's answer arrives afterwards, carrying the old fence.
	if err := a.RecordBarrierObservation(t.Context(), BarrierObservation{
		Node: "host-a", BarrierID: barrier.ID, Fence: fence, Empty: true,
	}); err != nil {
		t.Fatalf("RecordBarrierObservation: %v", err)
	}

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved ||
		got == ClearanceSettling {
		t.Fatalf("an answer from before a launch was recorded as a run: %v", got)
	}

	if c := clearance(t, a); c.Clear() {
		t.Fatal("the fleet was reported clear with a launch dispatched after the question")
	}
}

// A LAUNCH DISPATCHED AFTER A RUN HAS ALREADY CROSSED THE GRACE DISCARDS IT.
// The fence is compared on every read, not only when the answer is stored.
func TestALaunchDiscardsAProofAlreadyEstablished(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if !clearance(t, a).Clear() {
		t.Fatal("the run did not establish a proof to discard")
	}

	if _, err := a.BumpDispatch(t.Context(), "host-a"); err != nil {
		t.Fatalf("BumpDispatch: %v", err)
	}

	if c := clearance(t, a); c.Clear() {
		t.Fatalf("a proof survived a launch dispatched under it: %+v", c)
	}
}

// A RECONNECT VOIDS A PROOF. The registration epoch is the other half of the
// fence: a host that re-registers is a different incarnation, and whatever the
// previous one said describes a machine in a state nothing here has seen.
func TestAReRegistrationVoidsAProof(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if !clearance(t, a).Clear() {
		t.Fatal("the run did not establish a proof to void")
	}

	registerBarrierHost(t, a, "host-a")

	if c := clearance(t, a); c.Clear() {
		t.Fatalf("a proof survived the host reconnecting: %+v", c)
	}

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved {
		t.Fatalf("a reconnected host is still proved: %v", got)
	}
}

// A LATE ANSWER FROM A SUPERSEDED INCARNATION STARTS NO RUN.
//
// THE WRITE SIDE OF THE EPOCH FENCE, WHICH IS NOT THE READ SIDE.
// TestAReRegistrationVoidsAProof above covers the reader: a run stored under one
// epoch stops counting when the host reconnects. This covers the writer, and
// mutation testing is what found it missing — dropping the epoch term from
// RecordBarrierObservation's refusal left that test green, because the run it
// discards was one the reader would have discarded anyway.
//
// What the writer's term prevents is worse than a stale proof: the row is
// written with the epoch read from the ledger, NOT the one the answer carried,
// so an answer taken under incarnation 1 would be recorded as a run belonging to
// incarnation 2 — a host that has since restarted, and may now be running
// something. Five quiet minutes later it reports proved clear on the strength of
// one observation of a machine in a state nothing has seen.
func TestALateAnswerFromASupersededIncarnationStartsNoRun(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	// What the barrier captured before it asked.
	stale := fenceOf(t, a, "host-a")

	// The host reconnects while its answer is in flight.
	registerBarrierHost(t, a, "host-a")

	if err := a.RecordBarrierObservation(t.Context(), BarrierObservation{
		Node: "host-a", BarrierID: barrier.ID, Fence: stale, Empty: true,
	}); err != nil {
		t.Fatalf("RecordBarrierObservation: %v", err)
	}

	// THE RUN MUST NOT EXIST AT ALL, and the way to see that is to let a
	// LEGITIMATE answer arrive a full grace later.
	//
	// Asserting only "not proved" here is too weak to catch anything: a stale
	// answer that WAS stored would start its run at this instant, so it would
	// read as settling either way. What separates the two is whether that run's
	// clock had already been ticking — if the superseded incarnation's answer
	// started it, this single later answer completes a window that began before
	// the current incarnation existed.
	clock.advance(computeAbsenceGrace)

	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved {
		t.Fatalf("an answer from a superseded incarnation started a run the current one was "+
			"credited with: %v", got)
	}

	if c := clearance(t, a); c.Clear() {
		t.Fatalf("the fleet was reported clear on a superseded incarnation's answer: %+v", c)
	}

	// AND THE CURRENT INCARNATION CAN STILL PROVE ITSELF from where it actually
	// began, so this is a discarded run rather than a poisoned host.
	clock.advance(computeAbsenceGrace)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("the current incarnation could not prove itself afterwards: %v", got)
	}
}

// A PROVED HOST THAT GOES SILENT STAYS PROVED, and liveness is deliberately not
// part of the fence. Nothing can be dispatched to it without moving its
// dispatch generation, and a reconnect moves its epoch — so silence alone
// changes nothing, while a liveness term would make a host that answered and
// then went quiet permanently unprovable.
func TestAProvedHostThatGoesSilentStaysProved(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if err := a.NodeGone(t.Context(), "host-a", fenceOf(t, a, "host-a").Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("a host that proved itself idle and then went quiet reports %v, want %v",
			got, ClearanceProved)
	}
}

// AN UNREACHABLE HOST THAT NEVER ANSWERED BLOCKS, and is named as unreachable
// rather than as merely pending — the two need different things from an
// operator.
func TestAnUnreachableHostBlocksAndIsNamedAsSuch(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)
	registerBarrierHost(t, a, "host-b")

	epoch := fenceOf(t, a, "host-b").Epoch
	if err := a.NodeGone(t.Context(), "host-b", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	barrier := requestBarrier(t, a)
	observe(t, a, barrier, true)

	c := clearance(t, a)

	if got := stateOf(t, c, "host-b"); got != ClearanceUnreachable {
		t.Fatalf("a host this deployment cannot reach reports %v, want %v",
			got, ClearanceUnreachable)
	}

	if c.Clear() {
		t.Fatal("the fleet was reported clear with a host nobody could ask")
	}
}

// A HOST BELOW THE BARRIER PROTOCOL CAN NEVER ANSWER, and saying so is the
// point: "did not answer" and "cannot answer" send an operator to different
// remedies, and neither is "is running nothing".
func TestAHostBelowTheBarrierProtocolIsNeverProved(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)

	old := testRegistration("host-old", config.ProviderFirecracker)
	old.WireMin = 12
	old.WireMax = BarrierWireVersion - 1
	old.WireVersion = BarrierWireVersion - 1

	if _, err := a.RegisterNode(t.Context(), old); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	barrier := requestBarrier(t, a)
	establishProof(t, a, clock, barrier)

	c := clearance(t, a)

	if got := stateOf(t, c, "host-old"); got != ClearanceBelowProtocol {
		t.Fatalf("a host on an older wire reports %v, want %v", got, ClearanceBelowProtocol)
	}

	if c.Clear() {
		t.Fatal("the fleet was reported clear with a host that cannot answer the question")
	}
}

// InvalidateBarrierRun ACTUALLY REMOVES THE ROW.
//
// The plane's own test for this counts calls to a fake BarrierStore, so a no-op
// body here would survive it — which adversarial review pointed out. This drives
// the real implementation and reads the ledger afterwards.
func TestInvalidatingARunRemovesItFromTheLedger(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if n := storedRuns(t, a); n != 1 {
		t.Fatalf("%d run(s) stored, want the one this test is about to discard", n)
	}

	if err := a.InvalidateBarrierRun(t.Context(), "host-a"); err != nil {
		t.Fatalf("InvalidateBarrierRun: %v", err)
	}

	if n := storedRuns(t, a); n != 0 {
		t.Fatalf("%d run(s) survived the invalidation", n)
	}

	if c := clearance(t, a); c.Clear() {
		t.Fatalf("the fleet is still clear after the host's proof was discarded: %+v", c)
	}
}

// AN UNREADABLE ARRIVAL DISCARDS EVERY STORED RUN.
//
// A loopback registration whose body will not decode names no host, so there is
// nothing to be selective about — and "billet could not tell who arrived" must
// not read as "nothing changed".
func TestAnUnreadableArrivalDiscardsEveryStoredRun(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	registerBarrierHost(t, a, "host-b")
	barrier := requestBarrier(t, a)

	// TWO RUNS, because "every" is the claim. establishProof only ever answers
	// for host-a, so a version of this that used it alone would pass against an
	// implementation that deleted exactly that one row.
	observeNode(t, a, barrier, "host-a", true)
	observeNode(t, a, barrier, "host-b", true)
	clock.advance(computeAbsenceGrace)
	observeNode(t, a, barrier, "host-a", true)
	observeNode(t, a, barrier, "host-b", true)

	if n := storedRuns(t, a); n != 2 {
		t.Fatalf("%d run(s) stored, want the two this test is about to discard", n)
	}

	if err := a.InvalidateEveryBarrierRun(t.Context()); err != nil {
		t.Fatalf("InvalidateEveryBarrierRun: %v", err)
	}

	if n := storedRuns(t, a); n != 0 {
		t.Fatalf("%d run(s) survived an arrival billet could not read", n)
	}
}

// AND IT IS HARMLESS FOR A HOST WITH NOTHING TO DISCARD, which is every first
// registration — the common case, and one that must not fail one.
func TestInvalidatingARunThatDoesNotExistIsHarmless(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)

	if err := a.InvalidateBarrierRun(t.Context(), "host-a"); err != nil {
		t.Fatalf("InvalidateBarrierRun on a host with no run: %v", err)
	}

	if err := a.InvalidateBarrierRun(t.Context(), "never-heard-of-it"); err != nil {
		t.Fatalf("InvalidateBarrierRun on an unknown host: %v", err)
	}

	if err := a.InvalidateEveryBarrierRun(t.Context()); err != nil {
		t.Fatalf("InvalidateEveryBarrierRun with nothing stored: %v", err)
	}
}

// A PROOF DOES NOT OUTLIVE THE SEAL IT WAS TAKEN UNDER.
//
// The plane drops a barrier whose admission generation has moved, but that is
// asynchronous cleanup rather than a fence. Between somebody resuming, the
// deployment taking work, somebody resealing, and the plane's next pass, every
// host's run is still stored and still fenced by an epoch and a dispatch
// generation nothing has moved — so without reading admission in the SAME
// snapshot, a drain exits 0 against a deployment that was open in between.
func TestAProofDoesNotSurviveAdmissionMoving(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if !clearance(t, a).Clear() {
		t.Fatal("the run did not establish a proof to invalidate")
	}

	// Somebody reopens the deployment and closes it again. Nothing touches any
	// host: no launch, no re-registration, no new answer.
	resumed, err := a.db.Resume(t.Context(), state.ResumeRequest{
		Expect: barrier.Generation, Clears: state.ProvenanceOperator, Actor: "someone else",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if _, err := a.db.Seal(t.Context(), state.SealRequest{
		Expect: resumed.Generation, Provenance: state.ProvenanceOperator, Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	c := clearance(t, a)

	if c.Clear() {
		t.Fatal("a proof taken under an earlier seal survived the deployment being reopened " +
			"and closed again; work could have been admitted in between")
	}

	if !c.Stale() {
		t.Errorf("the clearance does not report itself stale: %+v", c)
	}

	// AND THE HOST ITSELF IS STILL PROVED. The invalidation is about the
	// DEPLOYMENT's admission rather than about anything that host did, and
	// conflating the two would tell an operator to go and look at a machine that
	// is fine.
	if got := stateOf(t, c, "host-a"); got != ClearanceProved {
		t.Errorf("the host's own state was changed by admission moving: %v", got)
	}
}

// AN OPEN DEPLOYMENT IS NEVER CLEAR, even at the generation its barrier names.
func TestAnOpenDeploymentIsNeverClear(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)

	// A barrier requested at the OPEN generation, which no billet command does.
	barrier, err := a.RequestComputeBarrier(t.Context(), 0, "ops")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	// A FULL PROOF, so the ONLY thing standing between this and Clear() is the
	// admission guard. One observation would leave the host settling and the
	// assertion would hold with that guard deleted.
	establishProof(t, a, clock, barrier)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("the host is %v, so this test is not exercising the admission guard", got)
	}

	if c := clearance(t, a); c.Clear() {
		t.Fatalf("an open deployment reported itself clear: %+v", c)
	}
}

// A HOST THAT SAYS IT IS RUNNING SOMETHING BEATS A PROOF THAT HAS ALREADY BEEN
// ESTABLISHED.
//
// THIS IS THE DELAYED CREATE THE GRACE EXISTS FOR, ARRIVING BY THE OTHER ROUTE.
// A create the provider had accepted but not yet listed is invisible to the
// barrier's samples, so a run can complete around it — and then the host's
// ordinary sweep reports the instance. NOTHING MOVES EITHER FENCE: the launch
// was dispatched before the barrier, and the registration has not changed. So
// the completed run goes on reading as a proof while the host is explicitly
// saying otherwise, and the host can be decommissioned as PROVEN.
func TestAHostThatReportsComputeBeatsAnEstablishedProof(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if !clearance(t, a).Clear() {
		t.Fatal("the run did not establish a proof for the inventory to override")
	}

	// The host's ordinary sweep now says it holds one instance. No launch, no
	// re-registration: both fences are exactly where the proof left them.
	epoch := fenceOf(t, a, "host-a").Epoch
	if _, err := a.ResolveQuarantineFor(t.Context(), "host-a", []string{"l1"}, epoch); err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	c := clearance(t, a)

	if got := stateOf(t, c, "host-a"); got != ClearanceRunning {
		t.Fatalf("a host reporting compute over a completed run reports %v, want %v",
			got, ClearanceRunning)
	}

	if c.Clear() {
		t.Fatal("the fleet was reported clear while a host said it was running something")
	}

	// AND THE DECOMMISSION PATH AGREES, which is the half that is permanent: an
	// exclusion recorded as proven takes the host out of the expected set until
	// it registers again.
	proven, err := a.Decommission(t.Context(), DecommissionRequest{
		Node: "host-a", Actor: "ops", Force: true,
	})
	if err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	if proven {
		t.Fatal("a host that says it is running compute was excluded as PROVEN idle")
	}
}

// AND AN EMPTY BARRIER ANSWER CLEARS THAT AGAIN, so this is a block rather than
// a poisoning. Without it, one stale sweep would make a host unprovable for as
// long as the barrier lasted.
func TestAnEmptyBarrierAnswerSupersedesAnEarlierPositiveInventory(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	epoch := fenceOf(t, a, "host-a").Epoch
	if _, err := a.ResolveQuarantineFor(t.Context(), "host-a", []string{"l1"}, epoch); err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceRunning {
		t.Fatalf("a host reporting compute reports %v, want %v", got, ClearanceRunning)
	}

	// The barrier's own answers go through the same resolver, so an empty one
	// writes zero here as well as recording the run.
	if _, err := a.ResolveQuarantineFor(t.Context(), "host-a", nil, epoch); err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	establishProof(t, a, clock, barrier)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("a host that went empty again reports %v, want %v", got, ClearanceProved)
	}
}

// A HOST SAYING IT IS RUNNING SOMETHING IS NOT BURIED BY WHY IT CANNOT BE ASKED.
//
// An old-wire or unreachable host can still have said, through its ordinary
// sweep, that it holds compute. Both states block either way, so this is not a
// clearance bug — it is the strongest thing an operator could be told being
// rendered as the weakest.
func TestAHostThatSaysItIsRunningIsReportedAsSuchEvenWhenItCannotBeAsked(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)

	old := testRegistration("host-old", config.ProviderFirecracker)
	old.WireMin = 12
	old.WireMax = BarrierWireVersion - 1
	old.WireVersion = BarrierWireVersion - 1

	epoch, err := a.RegisterNode(t.Context(), old)
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// Its ordinary sweep says it is running one thing.
	if _, err := a.ResolveQuarantineFor(t.Context(), "host-old", []string{"l1"}, epoch); err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	requestBarrier(t, a)

	if got := stateOf(t, clearance(t, a), "host-old"); got != ClearanceRunning {
		t.Fatalf("a host that says it is running work reports %v, want %v — the reason it "+
			"cannot be asked buried the answer it already gave", got, ClearanceRunning)
	}
}

// A BARRIER NOBODY ASKED FOR IS NOT A CLEARANCE. Without a durable request no
// host has been asked, so there is nothing to be evidence of — and Clear() must
// not be satisfiable by an empty fleet with no barrier running.
func TestNoBarrierIsNeverClear(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)

	c := clearance(t, a)

	if c.Requested {
		t.Fatal("a deployment nobody drained reports a barrier in force")
	}

	if c.Clear() {
		t.Fatal("a deployment nobody asked about reported itself clear")
	}
}

// TWO WAITERS JOIN ONE BARRIER. A second request under the same admission
// generation must not mint a new id: doing so resets every host's continuous
// run, and two waiters would starve each other for as long as both waited.
func TestASecondWaiterJoinsTheBarrierInForce(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	first := requestBarrier(t, a)

	observe(t, a, first, true)
	clock.advance(computeAbsenceGrace / 2)

	second, err := a.RequestComputeBarrier(t.Context(), first.Generation, "somebody else")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("a second waiter minted a new barrier (%s, was %s), which resets every run",
			second.ID, first.ID)
	}

	// THE RUN THE FIRST WAITER STARTED IS STILL THE ONE BEING MEASURED. Half a
	// grace later, a sample lands at the far end of it — which it could not do if
	// the second request had reset empty_since.
	clock.advance(computeAbsenceGrace / 2)
	observe(t, a, second, true)

	if !clearance(t, a).Clear() {
		t.Fatal("the run the first waiter started did not survive the second joining it")
	}
}

// A BARRIER FROM ANOTHER ADMISSION GENERATION IS REPLACED, AND ITS RUNS GO WITH
// IT. Admission moving means the deployment was open in between, so every run
// under the old barrier describes a fleet that could have taken work.
func TestANewGenerationReplacesTheBarrierAndItsRuns(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	first := requestBarrier(t, a)

	establishProof(t, a, clock, first)

	if !clearance(t, a).Clear() {
		t.Fatal("the first barrier did not establish a proof to discard")
	}

	second, err := a.RequestComputeBarrier(t.Context(), first.Generation+1, "ops")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	if second.ID == first.ID {
		t.Fatal("a barrier for a different admission generation reused the old id")
	}

	// THE ROW ITSELF IS GONE, read directly. Neither Clear() nor the host's
	// reported state can see this: Clear() is already false from the admission
	// mismatch, and ComputeClear filters runs by the barrier in force, so a
	// superseded row is invisible to both whether or not anything deleted it.
	if n := storedRuns(t, a); n != 0 {
		t.Fatalf("%d run(s) from the previous admission generation survived", n)
	}
}

// AN ANSWER TO A BARRIER THAT HAS BEEN REPLACED IS DISCARDED, so a late reply
// cannot write itself into a run somebody else is establishing.
func TestAnAnswerToASupersededBarrierIsDiscarded(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	first := requestBarrier(t, a)

	// ADMISSION ACTUALLY MOVES, so the replacement barrier is the one in force
	// and Clear() can be true under it. Requesting at `first.Generation+1`
	// without this leaves admission behind, and every assertion below would hold
	// from that mismatch alone.
	second := rerequestBarrier(t, a, first.Generation)

	// A RUN UNDER THE BARRIER THAT IS ACTUALLY IN FORCE, so the stale answers
	// below have something they could damage. Without it the assertion holds
	// whether or not the refusal exists: a stale row keeps the OLD barrier id and
	// is filtered out of every read either way.
	establishProof(t, a, clock, second)

	if !clearance(t, a).Clear() {
		t.Fatal("the current barrier's own run was not established")
	}

	// Now the late answers for the barrier that was replaced.
	observe(t, a, first, true)
	clock.advance(computeAbsenceGrace)
	observe(t, a, first, true)

	// THE CURRENT RUN IS UNTOUCHED. A stale answer that got through would upsert
	// over this row — same node, different barrier — and reset its empty_since,
	// which is directly visible as the host no longer being proved.
	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("an answer for a replaced barrier overwrote the current run: %v", got)
	}

	if n := storedRuns(t, a); n != 1 {
		t.Fatalf("%d run(s) stored, want only the current barrier's", n)
	}
}

// DROPPING A BARRIER IS FENCED ON ITS ID, so the loop tidying away a request it
// was working on cannot delete one a waiter has just replaced it with.
func TestDroppingABarrierOnlyRemovesTheOneNamed(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)
	first := requestBarrier(t, a)

	second, err := a.RequestComputeBarrier(t.Context(), first.Generation+1, "ops")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	if err := a.DropComputeBarrier(t.Context(), first.ID); err != nil {
		t.Fatalf("DropComputeBarrier: %v", err)
	}

	current, found, err := a.ComputeBarrierInForce(t.Context())
	if err != nil {
		t.Fatalf("ComputeBarrierInForce: %v", err)
	}

	if !found || current.ID != second.ID {
		t.Fatalf("dropping a stale barrier removed the current one (found=%v, %+v)", found, current)
	}
}

// A DECOMMISSION NEEDS PROOF, OR AN EXPLICIT FORCE THAT RECORDS ITS ABSENCE.
// The whole risk of membership is that an exclusion launders uncertainty; what
// stops it is that the record says which it was, permanently.
func TestDecommissionRequiresProofOrRecordsItsAbsence(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)
	registerBarrierHost(t, a, "host-b")

	if err := a.NodeGone(t.Context(), "host-b", fenceOf(t, a, "host-b").Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	_, err := a.Decommission(t.Context(), DecommissionRequest{Node: "host-b", Actor: "ops"})
	if err == nil {
		t.Fatal("a host nothing had proved idle was decommissioned without --force")
	}
	if !strings.Contains(err.Error(), "nothing has proved") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}

	proven, err := a.Decommission(t.Context(), DecommissionRequest{
		Node: "host-b", Actor: "ops", Force: true,
	})
	if err != nil {
		t.Fatalf("a forced decommission was refused: %v", err)
	}

	if proven {
		t.Error("a forced decommission of a host nothing had proved reported itself proved")
	}

	c := clearance(t, a)

	unproven := c.Unproven()
	if len(unproven) != 1 || unproven[0] != "host-b" {
		t.Fatalf("a forced exclusion is not reported as unproven: %+v", c.Excluded)
	}

	for _, n := range c.Nodes {
		if n.Node == "host-b" {
			t.Fatal("a decommissioned host is still in the set the barrier expects to hear from")
		}
	}
}

// A DECOMMISSION DERIVES ITS PROOF ITSELF, AGAINST THE CURRENT INCARNATION.
//
// A caller cannot hand one in. `billet nodes decommission` used to read the
// fleet's clearance, reduce this host's state to a bool, and pass that to a
// separate write — and a host can re-register in between, which is exactly the
// change the epoch fence exists to catch. The exclusion would then be recorded
// as PROVEN about a machine that had just come back and might be running
// something, which is the laundering membership is built to prevent.
func TestAForcedDecommissionOfAReconnectedHostIsNotRecordedAsProven(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if !clearance(t, a).Clear() {
		t.Fatal("the run did not establish a proof for this test to invalidate")
	}

	// The host comes back — a different incarnation, which nothing has observed.
	registerBarrierHost(t, a, "host-a")

	proven, err := a.Decommission(t.Context(), DecommissionRequest{
		Node: "host-a", Actor: "ops", Force: true,
	})
	if err != nil {
		t.Fatalf("a forced decommission was refused: %v", err)
	}

	if proven {
		t.Fatal("a host that had just re-registered was recorded as PROVEN idle on the " +
			"strength of the previous incarnation's answer")
	}

	c := clearance(t, a)

	unproven := c.Unproven()
	if len(unproven) != 1 || unproven[0] != "host-a" {
		t.Fatalf("the exclusion is not recorded as unproven: %+v", c.Excluded)
	}
}

// AND A DECOMMISSION OF A GENUINELY PROVED HOST IS RECORDED AS PROVEN, which is
// the companion the test above needs: a derivation that always answered false
// would pass it and make every exclusion look like an admission of ignorance.
func TestADecommissionOfAProvedHostIsRecordedAsProven(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	establishProof(t, a, clock, barrier)

	if err := a.NodeGone(t.Context(), "host-a", fenceOf(t, a, "host-a").Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	proven, err := a.Decommission(t.Context(), DecommissionRequest{
		Node: "host-a", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("a proved host could not be decommissioned: %v", err)
	}

	if !proven {
		t.Fatal("a host proved to be running nothing was recorded as an unproven exclusion")
	}

	c := clearance(t, a)

	if len(c.Unproven()) != 0 {
		t.Errorf("a proved exclusion is reported as unproven: %+v", c.Excluded)
	}

	if len(c.Excluded) != 1 || !c.Excluded[0].Proven {
		t.Errorf("the exclusion does not record that it was proved: %+v", c.Excluded)
	}
}

// A LIVE HOST IS NOT GONE, and excluding one that is still talking to this
// control plane is a mistake rather than a decision about a retired machine.
func TestDecommissionRefusesAHostThatIsStillTalking(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)

	_, err := a.Decommission(t.Context(), DecommissionRequest{Node: "host-a", Actor: "ops"})
	if err == nil {
		t.Fatal("a host that is still registered live was decommissioned")
	}
	if !strings.Contains(err.Error(), "still talking") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// OUTSTANDING CAPACITY IS NOT OVERRIDABLE. A decommissioned host drops out of
// the floor arithmetic, so excluding one that still holds leases silently
// changes what every tier believes is already met — and the capacity stays
// charged either way.
func TestDecommissionRefusesWhileLeasesAreOutstandingEvenWithForce(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)

	lease, err := a.Reserve(t.Context(), "barrier-tier")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	target := lease.TargetNode
	if target == "" {
		t.Fatalf("the reservation named no host, so this test cannot stage its case: %+v", lease)
	}

	if err := a.NodeGone(t.Context(), target, fenceOf(t, a, target).Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	_, err = a.Decommission(t.Context(), DecommissionRequest{
		Node: target, Actor: "ops", Force: true,
	})
	if err == nil {
		t.Fatal("a host still holding a lease was decommissioned")
	}
	if !strings.Contains(err.Error(), "outstanding") {
		t.Errorf("the refusal does not name the capacity that is still charged: %v", err)
	}
}

// A HOST THAT COMES BACK REJOINS THE FLEET. An exclusion nobody remembers would
// otherwise hide a live machine from every later drain, forever — and the
// machine registering is the assertion behind the exclusion being contradicted
// by the machine itself.
// placeable asks the PLACEMENT QUERY whether a host is a candidate, rather than
// asking a report that happens to be about the same host.
func placeable(t *testing.T, a *Allocator, node string) bool {
	t.Helper()

	var found bool

	if err := a.db.View(t.Context(), func(tx querier) error {
		nodes, err := a.liveNodes(t.Context(), tx)
		if err != nil {
			return err
		}

		for i := range nodes {
			if nodes[i].name == node {
				found = true
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("liveNodes: %v", err)
	}

	return found
}

func TestRegisteringAgainUndoesADecommission(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)
	registerBarrierHost(t, a, "host-b")

	if err := a.NodeGone(t.Context(), "host-b", fenceOf(t, a, "host-b").Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if _, err := a.Decommission(t.Context(), DecommissionRequest{
		Node: "host-b", Actor: "ops", Force: true,
	}); err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	registerBarrierHost(t, a, "host-b")

	c := clearance(t, a)

	if len(c.Excluded) != 0 {
		t.Fatalf("a host that registered again is still excluded: %+v", c.Excluded)
	}

	found := false

	for _, n := range c.Nodes {
		if n.Node == "host-b" {
			found = true
		}
	}

	if !found {
		t.Fatal("a host that registered again is not back in the expected set")
	}

	// AND IT MAY BE PLACED ON AGAIN, ASKED OF THE PLACEMENT QUERY ITSELF.
	//
	// This assertion used to read RegisteredNodes and check decommissioned_at,
	// under a comment claiming it was about `drained` and the placement queries --
	// which that projection does not select at all. A row can be un-decommissioned
	// and still invisible to scheduling, so the claim and the check were about
	// different columns.
	if !placeable(t, a, "host-b") {
		t.Fatal("a host that registered again is not a placement candidate; " +
			"`drained` was left set, so scheduling cannot see it while every " +
			"other report says it is healthy")
	}

	nodes, err := a.RegisteredNodes(t.Context())
	if err != nil {
		t.Fatalf("RegisteredNodes: %v", err)
	}

	for _, n := range nodes {
		if n.Name == "host-b" && n.Decommissioned != "" {
			t.Fatalf("a host that registered again is still marked decommissioned: %+v", n)
		}
	}
}

// BumpDispatch ON A HOST THE LEDGER HAS NEVER HEARD OF IS NOT AN ERROR: nothing
// can be proved about a host the clearance query never walks, so there is no
// fence to advance and nothing that could later be believed.
func TestBumpDispatchOnAnUnknownHostIsHarmless(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)

	if _, err := a.BumpDispatch(t.Context(), "nobody"); err != nil {
		t.Fatalf("BumpDispatch on an unknown host: %v", err)
	}
}

// A cancelled context is reported, not swallowed.
func TestComputeClearReportsACancelledRead(t *testing.T) {
	t.Parallel()

	a, _ := barrierAllocator(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := a.ComputeClear(ctx); err == nil {
		t.Fatal("a cancelled clearance read reported success")
	}
}

// A HOST THAT STOPPED ANSWERING MID-RUN IS UNREACHABLE, NOT SETTLING.
//
// "Settling" tells an operator to wait, and the drain prints the moment it will
// clear. Both are claims about a future that needs ANOTHER answer from the host:
// the proof is two observations, so a run short of the grace is completed by a
// later empty answer or not at all. A host billet cannot reach gives none, and
// if it returns its epoch moves and this run is discarded — so no future turns
// THIS run into proof, and the printed ClearAt simply passes with the line
// unchanged.
//
// MEASURED ON THE REFERENCE HOST: a node killed with SIGKILL sat at "empty,
// still settling (clear at 05:28:21)" when read at 05:28:44.
//
// A PROVED run still outranks liveness, which is a different rule and is
// unchanged — see TestAProvedHostThatGoesSilentStaysProved.
func TestAHostThatStoppedAnsweringMidRunIsUnreachableRatherThanSettling(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)

	generation := sealBarrier(t, a)

	barrier, err := a.RequestComputeBarrier(t.Context(), generation, "ops")
	if err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	// One empty answer: the run has begun and is well short of the grace.
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceSettling {
		t.Fatalf("a LIVE host mid-run reports %v, want %v", got, ClearanceSettling)
	}

	// And then the machine goes away, without either fence moving.
	fence := fenceOf(t, a, "host-a")
	if err := a.NodeGone(t.Context(), "host-a", fence.Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	c := clearance(t, a)

	if got := stateOf(t, c, "host-a"); got != ClearanceUnreachable {
		t.Fatalf("a host that stopped answering mid-run reports %v, want %v — settling "+
			"promises a completion that only another answer can deliver",
			got, ClearanceUnreachable)
	}

	// THE RUN IS STILL THERE, which is what makes this the branch under test
	// rather than the ordinary no-run-at-all unreachable case one level down. An
	// implementation that discarded the run when the host went away would reach
	// that branch, report the same state, and prove nothing about this one.
	line := lineOf(t, c, "host-a")
	if line.EmptySince == "" || line.ClearAt == "" {
		t.Fatalf("the host's run was discarded rather than kept: %+v", line)
	}

	// AND PAST THE GRACE IT IS STILL UNREACHABLE, which is the acceptance
	// symptom exactly: the printed moment arrives, nothing has changed, and the
	// old code went on saying "settling" at a timestamp that had gone by. A fix
	// conditioned on the clock rather than on liveness passes the check above and
	// fails here.
	clock.advance(2 * computeAbsenceGrace)

	c = clearance(t, a)

	if got := stateOf(t, c, "host-a"); got != ClearanceUnreachable {
		t.Fatalf("past its own clear-at the host reports %v, want %v — no amount of "+
			"elapsed time turns one observation into a proof", got, ClearanceUnreachable)
	}

	// AND IT STILL BLOCKS. The point is the sentence an operator reads, not a
	// shortcut to clearance.
	if c.Clear() {
		t.Fatal("a fleet whose only host stopped answering mid-run was reported CLEAR")
	}
}

// A DISPATCHED LAUNCH RESTARTS THE RUN'S CLOCK, IT DOES NOT MERELY END ONE ROUND.
//
// The refusal in RecordBarrierObservation only covers an answer that CROSSED a
// launch: it compares the fence the caller captured against the ledger's. An
// answer taken entirely AFTER the launch matches, so it is stored -- and if the
// upsert kept the row's existing empty_since, the run would span the launch and
// the host could be proved clear over compute that launch created.
//
// THE CASE IN RecordBarrierRun IS WHAT PREVENTS IT, comparing the STORED fence
// against the incoming one and restarting the clock when they differ. Measured:
// making that CASE always keep the stored empty_since left every other test in
// this package green.
func TestALaunchRestartsTheRunItDidNotCross(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)
	barrier := requestBarrier(t, a)

	observe(t, a, barrier, true)

	// A LAUNCH, which advances the dispatch fence durably.
	if _, err := a.BumpDispatch(t.Context(), "host-a"); err != nil {
		t.Fatalf("BumpDispatch: %v", err)
	}

	// AND AN ANSWER TAKEN AFTER IT, carrying the NEW fence -- so nothing refuses
	// it and nothing deletes the row it lands on.
	clock.advance(computeAbsenceGrace + time.Minute)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved {
		t.Fatal("a host was proved clear on a run that spans a launch: the second " +
			"observation kept the first one's empty_since instead of restarting it")
	}

	// AND IT BECOMES PROVED ONCE THE NEW RUN ITSELF SPANS THE GRACE, so the
	// assertion above is about the restart rather than about proofs being
	// unreachable.
	clock.advance(computeAbsenceGrace + time.Minute)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("host-a is %s after a full run following the launch, want proved", got)
	}
}

// A HOST'S OWN REPORT ONLY BLOCKS UNDER THE REGISTRATION IN FORCE.
//
// Inventory is telemetry: a host SAYING it is running something blocks a proof,
// which is right while that word is current. A report from a PREVIOUS
// incarnation is not current -- the host has re-registered since and said
// nothing yet -- and treating it as current makes a host that once reported
// compute permanently unprovable, which is a drain that never completes and a
// decommission that can never be proved.
//
// Measured: dropping `i.node_epoch = n.epoch` from HostReportsCompute left every
// other test in this package green.
func TestAReportFromAPreviousIncarnationDoesNotBlockAProof(t *testing.T) {
	t.Parallel()

	a, clock := barrierAllocator(t)

	// THE HOST SAYS IT IS RUNNING SOMETHING, under its current registration.
	if _, err := a.Reconcile(t.Context(), "host-a", []string{"lease-x"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	barrier := requestBarrier(t, a)

	observe(t, a, barrier, true)
	clock.advance(computeAbsenceGrace + time.Minute)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got == ClearanceProved {
		t.Fatal("a host reporting compute under its current registration was proved clear")
	}

	// IT RECONNECTS, which bumps the epoch and makes that report a previous
	// incarnation's word. The run goes with it, so a fresh one has to be built.
	registerBarrierHost(t, a, "host-a")

	observe(t, a, barrier, true)
	clock.advance(computeAbsenceGrace + time.Minute)
	observe(t, a, barrier, true)

	if got := stateOf(t, clearance(t, a), "host-a"); got != ClearanceProved {
		t.Fatalf("host-a is %s; a report from before it reconnected is still blocking "+
			"the proof, so this host can never be drained or decommissioned", got)
	}

	// AND THE DECOMMISSION PATH AGREES, because it reads the same question
	// through provedTx rather than through the fleet report.
	if err := a.NodeGone(t.Context(), "host-a", fenceOf(t, a, "host-a").Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	proven, err := a.Decommission(t.Context(), DecommissionRequest{Node: "host-a", Actor: "ops"})
	if err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	if !proven {
		t.Error("the decommission recorded the exclusion as unproven while the fleet " +
			"report called the same host proved")
	}
}
