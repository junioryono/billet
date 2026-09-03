package nodeplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// fenceRegistrar records the ORDER of everything that touches a host's launch
// fence and its queue, which is the only property the wire half of the compute
// barrier has.
//
// A COUNTER WOULD NOT DO. The invariant is not "a launch bumps the fence" but
// "it bumps it BEFORE that launch can be taken", so what has to be observable is
// the sequence, not the total.
type fenceRegistrar struct {
	countingRegistrar

	mu       sync.Mutex
	events   []string
	dispatch int64
	err      error
}

func (r *fenceRegistrar) BumpDispatch(_ context.Context, node string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return 0, r.err
	}

	r.dispatch++
	r.events = append(r.events, "bump:"+node)

	return r.dispatch, nil
}

func (r *fenceRegistrar) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

func (r *fenceRegistrar) trace() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.events))
	copy(out, r.events)

	return out
}

func (r *fenceRegistrar) generation() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.dispatch
}

// A LAUNCH CHARGES THE FENCE, AND DOES SO BEFORE ANY NODE CAN TAKE IT.
//
// A launch that reached a host without advancing its dispatch generation is a
// launch a barrier's acknowledgement cannot see, and the host would be reported
// clear with that launch about to run on it.
//
// WHAT THIS DOES NOT PROVE, said out loud rather than assumed: the ORDER of the
// bump and the append inside queueLocked is not observable from here, and a
// mutant that swaps them survives. It survives because it changes nothing — both
// are under one hold of p.mu, and a poller needs that mutex to take anything. The
// property that matters is that no other queueing interleaves between them, and
// what protects it is structural (the `Locked` suffix, and both call sites
// holding the mutex across it) rather than anything a test can drive.
func TestALaunchChargesItsFenceBeforeANodeCanTakeIt(t *testing.T) {
	t.Parallel()

	reg := &fenceRegistrar{}
	p := testPlane(t, WithRegistrar(reg))
	register(t, p, barrierHostName, config.ProviderDocker)

	launched := make(chan struct{})

	go func() {
		defer close(launched)

		if err := p.NewRunner().Launch(t.Context(), testLease(),
			server.Job{RequestID: 7}); err != nil {
			t.Errorf("Launch: %v", err)
		}
	}()

	// Poll until the node is handed the command; that is the first instant at
	// which anything on the far side could act on it.
	cmd, ok, err := p.Poll(t.Context(), barrierHostName, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !ok || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("the node was handed %+v, want a launch", cmd)
	}

	reg.record("taken")

	if err := p.Result(barrierHostName, "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("Result: %v", err)
	}

	<-launched

	trace := reg.trace()
	if len(trace) < 2 || trace[0] != "bump:"+barrierHostName || trace[1] != "taken" {
		t.Fatalf("the launch fence was not taken before the node could take the command: %v",
			trace)
	}
}

// AND A FENCE THAT CANNOT BE TAKEN FAILS THE LAUNCH.
//
// Dispatching unfenced would leave a launch running behind an answer somebody is
// about to call proof. The refusal is ErrNoNode — nothing was queued, so nothing
// started, and the caller may release the lease with a clear conscience.
func TestALaunchThatCannotTakeItsFenceIsNotSent(t *testing.T) {
	t.Parallel()

	reg := &fenceRegistrar{err: errors.New("the ledger is unavailable")}
	p := testPlane(t, WithRegistrar(reg))
	register(t, p, barrierHostName, config.ProviderDocker)

	err := p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	if err == nil {
		t.Fatal("a launch was sent to a host whose fence could not be advanced")
	}

	if !errors.Is(err, ErrNoNode) {
		t.Errorf("want ErrNoNode so the caller knows nothing started, got %v", err)
	}

	if errors.Is(err, server.ErrCustody) {
		t.Error("a launch that was never queued reported custody, which holds capacity for " +
			"compute that cannot exist")
	}

	if n := p.QueuedForTest(barrierHostName); n != 0 {
		t.Errorf("%d command(s) were queued despite the fence failing", n)
	}
}

// ONLY A LAUNCH TAKES THE FENCE. A destroy, sweep, tend or inventory cannot
// create compute, and charging them would void a proof for nothing — which on a
// deployment whose node sweeps every five minutes means no barrier ever
// completes.
func TestOnlyALaunchTakesTheFence(t *testing.T) {
	t.Parallel()

	reg := &fenceRegistrar{}
	p := testPlane(t, WithRegistrar(reg))
	register(t, p, barrierHostName, config.ProviderDocker)

	answered := make(chan struct{})

	go func() {
		defer close(answered)

		if err := p.NewRunner().Destroy(t.Context(), 7); err != nil {
			t.Errorf("Destroy: %v", err)
		}
	}()

	cmd, ok, err := p.Poll(t.Context(), barrierHostName, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !ok || cmd.Kind != nodeapi.CommandDestroy {
		t.Fatalf("the node was handed %+v, want a destroy", cmd)
	}

	if err := p.Result(barrierHostName, "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("Result: %v", err)
	}

	<-answered

	if got := reg.generation(); got != 0 {
		t.Fatalf("a destroy advanced the launch fence to %d; only a launch may", got)
	}
}

// barrierHostName is the fixture's one host. Named rather than passed, so the
// helpers below cannot drift from the plane barrierPlane actually registers.
const barrierHostName = "host"

// barrierFixtureID is the barrier newBarrierStore has in force. Named for the
// same reason as the host: what these tests vary is the ECHO, so the question's
// own id is fixture rather than subject.
const barrierFixtureID = "b1"

// barrierStore is a BarrierStore a test drives directly.
type barrierStore struct {
	mu sync.Mutex

	barrier    alloc.ComputeBarrier
	hasBarrier bool
	generation int64
	sealed     bool
	quiet      bool
	fences     map[string]alloc.NodeFence
	observed   []alloc.BarrierObservation
	reconciled [][]string
	dropped    []string
	// invalidated records the hosts whose stored run a registration discarded, and
	// invalidatedAll the arrivals too unreadable to name one.
	invalidated    []string
	invalidatedAll int
	invalidateErr  error
}

func newBarrierStore() *barrierStore {
	return &barrierStore{
		barrier:    alloc.ComputeBarrier{ID: barrierFixtureID, Generation: 3},
		hasBarrier: true,
		generation: 3,
		sealed:     true,
		quiet:      true,
		fences:     map[string]alloc.NodeFence{},
	}
}

func (s *barrierStore) ComputeBarrierInForce(context.Context) (alloc.ComputeBarrier, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.barrier, s.hasBarrier, nil
}

func (s *barrierStore) DropComputeBarrier(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.dropped = append(s.dropped, id)
	s.hasBarrier = false

	return nil
}

func (s *barrierStore) AdmissionGeneration(context.Context) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.generation, s.sealed, nil
}

func (s *barrierStore) Quiescence(context.Context) (alloc.Quiescence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Sealed with nothing outstanding is exactly Quiet(); an unquiet answer is
	// staged by leaving the seal off, which is what the real query would report
	// for a deployment still holding a lease.
	return alloc.Quiescence{Sealed: s.quiet, Generation: s.generation}, nil
}

func (s *barrierStore) InvalidateBarrierRun(_ context.Context, node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.invalidateErr != nil {
		return s.invalidateErr
	}

	s.invalidated = append(s.invalidated, node)

	return nil
}

func (s *barrierStore) InvalidateEveryBarrierRun(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.invalidateErr != nil {
		return s.invalidateErr
	}

	s.invalidatedAll++

	return nil
}

func (s *barrierStore) invalidations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, len(s.invalidated))
	copy(out, s.invalidated)

	return out
}

func (s *barrierStore) setFence(node string, fence alloc.NodeFence) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.fences[node] = fence
}

func (s *barrierStore) NodeFenceOf(_ context.Context, node string) (alloc.NodeFence, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fence, ok := s.fences[node]

	return fence, ok, nil
}

func (s *barrierStore) RecordBarrierObservation(
	_ context.Context, obs alloc.BarrierObservation,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.observed = append(s.observed, obs)

	return nil
}

func (s *barrierStore) ResolveQuarantineFor(
	_ context.Context, _ string, running []string, _ int64,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reconciled = append(s.reconciled, running)

	return 0, nil
}

func (s *barrierStore) observations() []alloc.BarrierObservation {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]alloc.BarrierObservation, len(s.observed))
	copy(out, s.observed)

	return out
}

func (s *barrierStore) reconciliations() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([][]string, len(s.reconciled))
	copy(out, s.reconciled)

	return out
}

// answerInventory plays the node: take the command, answer it, and say what was
// answered.
func answerInventory(t *testing.T, p *Plane, instances []string, ok bool, echo string) {
	t.Helper()

	const node = barrierHostName

	cmd, took, err := p.Poll(t.Context(), node, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !took {
		t.Fatal("nothing was queued for the node")
	}
	if cmd.Kind != nodeapi.CommandInventory {
		t.Fatalf("the node was handed %+v, want an inventory", cmd)
	}

	barrierID := cmd.BarrierID
	if echo != "" {
		barrierID = echo
	}

	res := nodeapi.CommandResult{
		ID: cmd.ID, OK: ok, BarrierID: barrierID, Instances: instances,
	}
	if !ok {
		res.Error = "the provider could not be read"
	}

	if err := p.Result(node, "", res); err != nil {
		t.Fatalf("Result: %v", err)
	}
}

// askInBackground runs one barrier round while the test plays the node.
func askInBackground(t *testing.T, p *Plane) chan struct{} {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := p.AskNodeForTest(t.Context(), barrierHostName, barrierFixtureID); err != nil {
			t.Errorf("AskNodeForTest: %v", err)
		}
	}()

	return done
}

// epochRegistrar hands out a real, advancing ledger epoch.
//
// countingRegistrar answers zero, which the plane treats as "nothing to
// install" — so a fixture built on it leaves n.ledgerEpoch at zero while the
// barrier store reports whatever the test invented, and the dispatch fence that
// compares the two would be skipped rather than exercised.
type epochRegistrar struct {
	countingRegistrar

	mu    sync.Mutex
	epoch int64
	// onRegister runs inside a registration, after it has begun and before its
	// epoch is installed in the plane.
	onRegister func()
}

// RegisterNode doubles as the one hook that runs INSIDE the window a
// registration opens: `beginRegistration` has set `p.registering`, and the new
// epoch has not been installed in the plane's map yet. `ArrivingForRegistration`
// is too early (it runs before the registration begins at all) and
// `afterRegisterNodeForTest` is too late for the `registering` guard, because by
// then the epoch has moved and either guard alone would refuse.
//
// The hook is called with the lock released, or anything it does that reaches
// back into this registrar would deadlock.
func (r *epochRegistrar) RegisterNode(context.Context, alloc.NodeRegistration) (int64, error) {
	r.mu.Lock()
	hook := r.onRegister
	r.mu.Unlock()

	// BEFORE THE EPOCH MOVES. A hook that ran after it would let an invalidation
	// moved to just after RegisterNode look correct to a test named for
	// happening before it.
	if hook != nil {
		hook()
	}

	r.mu.Lock()
	r.epoch++
	epoch := r.epoch
	r.mu.Unlock()

	return epoch, nil
}

func (r *epochRegistrar) current() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.epoch
}

func barrierPlane(t *testing.T, store *barrierStore) (*Plane, *epochRegistrar) {
	t.Helper()

	reg := &epochRegistrar{}

	p := testPlane(t, WithRegistrar(reg), WithBarrierStore(store))
	register(t, p, barrierHostName, config.ProviderDocker)

	// THE FENCE THE LEDGER WOULD REPORT, matching what the plane just installed.
	store.setFence(barrierHostName, alloc.NodeFence{
		Epoch: reg.current(), Dispatch: 9,
		WireVersion: nodeapi.VersionComputeBarrier, Live: true,
	})

	return p, reg
}

// THE ANSWER IS RECORDED AGAINST THE FENCE CAPTURED BEFORE THE QUESTION.
//
// That ordering is what makes the answer causal rather than merely recent: a
// launch dispatched afterwards moves the fence, and the recording transaction
// then matches nothing. If the fence were read AFTER the reply, it would already
// include that launch and the answer would look current.
func TestAnInventoryIsRecordedAgainstTheFenceItWasAskedUnder(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, reg := barrierPlane(t, store)

	done := askInBackground(t, p)
	answerInventory(t, p, nil, true, "")
	<-done

	observed := store.observations()
	if len(observed) != 1 {
		t.Fatalf("want one observation, got %d", len(observed))
	}

	obs := observed[0]

	switch {
	case obs.Node != barrierHostName:
		t.Errorf("the observation names %q", obs.Node)
	case obs.BarrierID != barrierFixtureID:
		t.Errorf("the observation carries barrier %q, want b1", obs.BarrierID)
	case obs.Fence.Epoch != reg.current() || obs.Fence.Dispatch != 9:
		t.Errorf("the observation carries fence %+v, want the one read before the question",
			obs.Fence)
	case !obs.Empty:
		t.Error("a host that listed nothing was recorded as running something")
	}

	// AND THE EMPTY LIST REACHES THE RESOLVER, which is what writes zero into the
	// host's last-reported inventory.
	//
	// That matters because a positive inventory DOMINATES a completed run: a
	// version of askNode that reconciled only non-empty answers would leave one
	// stale sweep overriding every proof for the life of the barrier, and every
	// assertion about observations above would still pass.
	got := store.reconciliations()
	if len(got) != 1 {
		t.Fatalf("an empty inventory was reconciled %d time(s), want exactly 1", len(got))
	}

	if len(got[0]) != 0 {
		t.Errorf("the empty inventory reached the resolver as %v", got[0])
	}
}

// A HOST THAT REPORTS COMPUTE IS NOT EMPTY, and its answer still has to be
// recorded — that is what ends its run rather than letting it age.
func TestAHostThatReportsComputeEndsItsRun(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, _ := barrierPlane(t, store)

	done := askInBackground(t, p)
	answerInventory(t, p, []string{"l1"}, true, "")
	<-done

	observed := store.observations()
	if len(observed) != 1 {
		t.Fatalf("want one observation, got %d", len(observed))
	}

	if observed[0].Empty {
		t.Fatal("a host that listed a lease was recorded as empty")
	}

	// AND ITS LIST SETTLES REAL CAPACITY. A barrier inventory is a real
	// inventory, so it goes through the one path that already fences one rather
	// than being a second, parallel notion of what a host is running.
	if got := store.reconciliations(); len(got) != 1 || len(got[0]) != 1 || got[0][0] != "l1" {
		t.Errorf("the barrier's inventory was not reconciled against quarantine: %v", got)
	}
}

// "COULD NOT READ MY PROVIDER" IS NOT "RUNNING NOTHING", and the two arriving as
// the same message is the failure this whole mechanism exists to prevent.
func TestAHostThatCouldNotLookIsNotRecordedAsEmpty(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, _ := barrierPlane(t, store)

	done := askInBackground(t, p)
	answerInventory(t, p, nil, false, "")
	<-done

	observed := store.observations()
	if len(observed) != 1 {
		t.Fatalf("want one observation, got %d", len(observed))
	}

	if observed[0].Empty {
		t.Fatal("a host that could not read its provider was recorded as running nothing")
	}

	if got := store.reconciliations(); len(got) != 0 {
		t.Errorf("an unreadable inventory was reconciled against quarantine anyway: %v", got)
	}
}

// AN ECHO THAT DOES NOT MATCH IS NOT AN ANSWER TO THIS QUESTION.
func TestAnInventoryForAnotherBarrierIsNotBelieved(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, _ := barrierPlane(t, store)

	done := askInBackground(t, p)
	answerInventory(t, p, nil, true, "someone-elses-barrier")
	<-done

	observed := store.observations()
	if len(observed) != 1 {
		t.Fatalf("want one observation, got %d", len(observed))
	}

	if observed[0].Empty {
		t.Fatal("an answer echoing a different barrier was recorded as this barrier's proof")
	}
}

// A REGISTRATION DISCARDS WHAT THE HOST HAD PROVED, AT THE MOMENT IT BEGINS.
//
// THE LEDGER'S EPOCH DOES NOT MOVE UNTIL THE REGISTRATION COMMITS, and `billet
// drain` reads that ledger from ANOTHER PROCESS where `p.registering` is
// invisible. So between a replacement arriving and its write landing, a
// completed run reads as current — and the fleet can be reported clear about a
// host whose new incarnation may be holding compute the old one never saw, which
// is what two hosts under one name looks like.
//
// The dispatch fence beside this is a different guarantee: it stops a superseded
// process ANSWERING. It cannot invalidate a proof already stored.
func TestARegistrationDiscardsAStoredProofBeforeItsEpochMoves(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, reg := barrierPlane(t, store)

	// FROM A BASELINE, because the fixture's own registration already discarded
	// once. What is being asserted is that THIS registration's discard has
	// happened by the time the ledger write does — a total would be satisfied by
	// the earlier one.
	before := len(store.invalidations())

	// SAMPLED AT THE ENTRY TO THE LEDGER WRITE, not after it.
	// afterRegisterNodeForTest fires once RegisterNode has RETURNED — the epoch
	// has already moved by then, so an invalidation moved to just after it would
	// leave this green while reopening the race the test is named for.
	var whenWritten []string

	reg.mu.Lock()
	reg.onRegister = func() { whenWritten = store.invalidations() }
	reg.mu.Unlock()

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: barrierHostName, Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "second", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if len(whenWritten) != before+1 {
		t.Fatalf("the stored run was not discarded before the registration's ledger write "+
			"(%d discards before, %d by the time it landed)", before, len(whenWritten))
	}

	if whenWritten[before] != barrierHostName {
		t.Errorf("the discard named %q, want %q", whenWritten[before], barrierHostName)
	}
}

// AND A DISCARD THAT CANNOT BE MADE FAILS THE REGISTRATION.
//
// The alternative is a proof surviving the incarnation it describes. The node
// retries, exactly as it does when the ledger cannot record the registration.
func TestARegistrationIsRefusedIfAStoredProofCannotBeDiscarded(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, reg := barrierPlane(t, store)

	store.mu.Lock()
	store.invalidateErr = errors.New("the ledger is unavailable")
	store.mu.Unlock()

	before := reg.current()

	_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: barrierHostName, Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "second", VCPU: 8, Memory: 32 * config.GiB,
	})
	if err == nil {
		t.Fatal("a registration succeeded while a host's idle proof could not be discarded")
	}

	// NOT PERMANENT. A ledger that cannot write is an outage, and a node told
	// ErrRefused stops rather than retrying.
	if errors.Is(err, ErrRefused) {
		t.Errorf("an outage was reported as a permanent refusal, so the node will stop "+
			"retrying: %v", err)
	}

	// AND THE REGISTRATION ACTUALLY STOPPED. The error alone proves nothing about
	// what else happened: a version that returned it and carried on would
	// supersede the incumbent and move its ledger epoch, which is the side effect
	// the refusal exists to prevent.
	if got := reg.current(); got != before {
		t.Errorf("a registration refused for a failed discard moved the ledger epoch from "+
			"%d to %d", before, got)
	}
}

// A SUPERSEDED PROCESS CANNOT ANSWER UNDER ITS REPLACEMENT'S EPOCH.
//
// REGISTRATION COMMITS THE LEDGER EPOCH BEFORE IT INSTALLS THE NEW INCARNATION,
// and deliberately so: the ledger write must not happen under Plane.mu. That
// leaves a window in which the ledger reads the NEW epoch while the plane still
// routes to the OLD process — so a barrier that captured its fence there would
// credit that process's answer to a registration it never belonged to, and with
// two hosts sharing one name the run's two ends would come from two machines.
//
// Staged through afterRegisterNodeForTest, which exists for exactly this gap.
// SPLIT INTO TWO, so each guard is separately observable. Asked after the ledger
// write, BOTH hold — the registration is in flight AND the epoch has moved — so
// a single test proves only that at least one exists, and either could be
// deleted without failing it.
func TestABarrierIsNotAskedWhileARegistrationIsInFlight(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, reg := barrierPlane(t, store)

	asked := make(chan error, 1)

	// INSIDE THE LEDGER WRITE, which is the only moment where `registering` is
	// set and the plane's installed epoch has NOT moved. Earlier and the
	// registration has not begun; later and the epoch guard would refuse too, so
	// either alone would pass.
	// ON A BOUNDED CONTEXT, because the failure mode otherwise is a hang rather
	// than an assertion. If the guard regresses, the command is queued to a node
	// with no poller and dispatch waits out the production ten-minute timeout —
	// which the package's own test deadline turns into a suite-wide panic naming
	// the wrong thing. A second is far longer than a refusal needs and far
	// shorter than a dispatch that got through.
	reg.mu.Lock()
	reg.onRegister = func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		asked <- p.AskNodeForTest(ctx, barrierHostName, barrierFixtureID)
	}
	reg.mu.Unlock()

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: barrierHostName, Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "second", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := <-asked; err != nil {
		t.Fatalf("AskNodeForTest: %v", err)
	}

	assertNoBarrierGotThrough(t, p, store)
}

// AND NOT AFTER THE EPOCH HAS MOVED EITHER, with no registration in flight —
// which is the other guard, and the one that catches a fence captured before a
// registration this plane has already finished.
func TestABarrierIsNotAskedAgainstAStaleEpoch(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, reg := barrierPlane(t, store)

	// The ledger has moved on; the plane installed that epoch long ago.
	store.setFence(barrierHostName, alloc.NodeFence{
		Epoch: reg.current() + 7, Dispatch: 9,
		WireVersion: nodeapi.VersionComputeBarrier, Live: true,
	})

	if err := p.AskNodeForTest(t.Context(), barrierHostName, barrierFixtureID); err != nil {
		t.Fatalf("AskNodeForTest: %v", err)
	}

	assertNoBarrierGotThrough(t, p, store)
}

// assertNoBarrierGotThrough is the shared half of the two tests above.
//
// IT ASSERTS THE REFUSAL HAPPENED, not merely that nothing bad did. Checking
// only "nothing was queued" and "nothing empty was recorded" is satisfied by a
// dispatch that WENT THROUGH and then timed out — which is exactly what the
// unguarded code does under the bounded context above, so three mutants survived
// until this was tightened.
//
// The discriminator is the observation. A dispatch REFUSED returns immediately
// with a live context, so askNode logs it and records one non-empty answer,
// ending the host's run. A dispatch that got through and expired returns with a
// dead context and records nothing at all.
func assertNoBarrierGotThrough(t *testing.T, p *Plane, store *barrierStore) {
	t.Helper()

	if n := p.QueuedForTest(barrierHostName); n != 0 {
		t.Fatalf("%d inventory command(s) were queued", n)
	}

	observed := store.observations()
	if len(observed) != 1 {
		t.Fatalf("%d observation(s) recorded, want exactly one — the refusal itself. Zero "+
			"means the dispatch went through and merely ran out of time", len(observed))
	}

	// AND IT MUST NOT BE EMPTY. A round that could not be completed ends the
	// host's run, which is conservative; an empty one is the only kind that
	// builds toward a proof.
	if observed[0].Empty {
		t.Fatalf("an answer that should never have been asked for was recorded as empty: %+v",
			observed[0])
	}
}

// A HOST BELOW THE BARRIER PROTOCOL IS NEVER ASKED.
//
// Sending it the command would burn its single command slot for the full command
// timeout on every round, and the answer would be a refusal — which is not an
// inventory. It blocks in the ledger instead, where an operator can see why.
func TestAHostBelowTheBarrierProtocolIsNotAsked(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p := testPlane(t, WithRegistrar(&countingRegistrar{}), WithBarrierStore(store))

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		MinVersion: nodeapi.MinVersion,
		Version:    nodeapi.VersionComputeBarrier - 1,
		Node:       "old", Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register an older node: %v", err)
	}

	targets := p.barrierTargets()

	for _, n := range targets {
		if n.name == "old" {
			t.Fatal("a host whose wire has no inventory command was still going to be asked")
		}
	}
}

// A BARRIER WHOSE ADMISSION GENERATION MOVED IS DROPPED, along with the runs
// under it: admission moving means the deployment was open in between, so every
// run describes a fleet that could have taken work.
func TestABarrierIsDroppedWhenAdmissionMoves(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, _ := barrierPlane(t, store)

	store.mu.Lock()
	store.generation = 4 // somebody resumed and resealed
	store.mu.Unlock()

	p.barrierPass(t.Context())

	store.mu.Lock()
	dropped := strings.Join(store.dropped, ",")
	store.mu.Unlock()

	if dropped != barrierFixtureID {
		t.Fatalf("a barrier from a superseded admission generation was not dropped (dropped %q)",
			dropped)
	}
}

// AND ONE AGAINST AN OPEN DEPLOYMENT IS DROPPED TOO. Nothing billet ships asks
// for that, and servicing it would produce answers about a fleet that is free to
// take work at any moment.
func TestABarrierAgainstAnOpenDeploymentIsDropped(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, _ := barrierPlane(t, store)

	store.mu.Lock()
	store.sealed = false
	store.mu.Unlock()

	p.barrierPass(t.Context())

	store.mu.Lock()
	dropped := len(store.dropped)
	store.mu.Unlock()

	if dropped == 0 {
		t.Fatal("a barrier against an open deployment was serviced")
	}
}

// THE LEDGER BARRIER COMES FIRST. While a lease is open a launch may legitimately
// be dispatched, which moves a host's fence and discards its run — so asking then
// burns commands to produce answers that can never add up.
func TestNoHostIsAskedWhileTheLedgerStillHoldsSomething(t *testing.T) {
	t.Parallel()

	store := newBarrierStore()
	p, _ := barrierPlane(t, store)

	store.mu.Lock()
	store.quiet = false
	store.mu.Unlock()

	p.barrierPass(t.Context())

	if n := p.QueuedForTest(barrierHostName); n != 0 {
		t.Fatalf("%d inventory command(s) were queued while the ledger still held work", n)
	}

	if got := store.observations(); len(got) != 0 {
		t.Fatalf("an observation was recorded before the ledger was quiet: %+v", got)
	}
}

// A PLANE WITH NO BARRIER STORE PROVES NOTHING AND DOES NOTHING, which is the
// correct behaviour for a wiring that was never asked to — not a degraded one.
func TestALoopWithNoBarrierStoreReturns(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		p.BarrierLoop(ctx)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("BarrierLoop did not return on a plane with nothing to prove")
	}
}
