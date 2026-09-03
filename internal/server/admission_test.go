package server

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// registerHost gives the allocator a machine to place on, since capacity is per
// host: a deployment with no registered node places nothing and every tier
// advertises zero, which would make these assertions about an empty fleet.
func registerHost(t *testing.T, a *alloc.Allocator) {
	t.Helper()

	for _, provider := range []config.ProviderKind{
		config.ProviderDocker, config.ProviderFirecracker, config.ProviderTart,
	} {
		if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
			Name:     "sealed-host-" + string(provider),
			Provider: provider,
			VCPU:     1 << 20,
			Memory:   1 << 20 * config.GiB,
		}); err != nil {
			t.Fatalf("registering a host: %v", err)
		}
	}
}

// A SEAL MUST NOT STOP THE LISTENER, and this is the most dangerous thing in the
// admission work.
//
// An escrow error returns from the listener; one listener returning cancels
// every other listener; their teardown destroys the compute they are holding and
// FAILS those builds. So a deliberate seal surfaced as an ordinary error would
// make `drain` the most destructive command billet has — it would kill exactly
// the jobs it exists to let finish.
//
// Having nothing to escrow is the right reading of a seal anyway: advertise
// nothing new, and carry on heartbeating and settling what is already running,
// which is what draining IS.
func TestASealedDeploymentDoesNotStopTheListener(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-sealed")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	// AN OPEN DEPLOYMENT ESCROWS, proved on its OWN ledger. Doing it on this one
	// would leave no headroom, and the sealed listener below would then return
	// early without ever reaching the allocator — a test that passes because
	// nothing happened, which is exactly the shape this suite keeps finding.
	proveOpenEscrows(t, tiers)

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Reason: "draining", Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// A LISTENER HOLDING NOTHING, against a ledger with room, so the refill
	// genuinely reaches Escrow rather than returning early on zero headroom.
	sealed := NewListener(a, tiers[0].Label, &fakeSession{})

	if room, err := a.Headroom(t.Context(), tiers[0].Label); err != nil {
		t.Fatalf("Headroom: %v", err)
	} else if room <= 0 {
		t.Fatalf("the fixture left no headroom, so the escrow path is never reached")
	}

	// THE POINT: this must not return an error. If it does, every listener in the
	// deployment is cancelled and every running job dies.
	if err := sealed.refillEscrow(t.Context()); err != nil {
		t.Fatalf("a sealed deployment stopped the listener, which would cancel every other "+
			"listener and destroy the jobs they are holding: %v", err)
	}

	// AND IT ESCROWED NOTHING, or the seal would be decorative.
	if got := sealed.capacity(); got != 0 {
		t.Errorf("a sealed deployment escrowed %d capacity", got)
	}

	// The same through the other entry point the poll loop uses.
	sealed.observed = &Statistics{TotalAssignedJobs: 4}
	if err := sealed.prepareEscrow(t.Context()); err != nil {
		t.Fatalf("prepareEscrow stopped the listener on a sealed deployment: %v", err)
	}
	if got := sealed.capacity(); got != 0 {
		t.Errorf("a sealed deployment escrowed %d capacity through prepareEscrow", got)
	}
}

// proveOpenEscrows shows the same listener code escrows on an open deployment,
// on a ledger of its own.
func proveOpenEscrows(t *testing.T, tiers []config.Tier) {
	t.Helper()

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	l := NewListener(a, tiers[0].Label, &fakeSession{})
	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("an open deployment failed to escrow: %v", err)
	}
	if l.capacity() == 0 {
		t.Fatal("an open deployment escrowed nothing, so the sealed assertion proves nothing")
	}
}

// AND A REAL LEDGER FAILURE STILL STOPS THE LISTENER, or the check above would
// have turned every ledger problem into silence.
//
// IT FAILS AT Headroom, NOT Escrow, and says so rather than claiming more: a
// closed database stops the poll before the escrow call is reached. What this
// pins is that a broken ledger is fatal and is not reported as a deliberate
// seal. Reaching Escrow specifically would need a failure seam the allocator
// does not expose.
func TestAGenuineLedgerFailureStillStopsTheListener(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-broken")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	l := NewListener(a, tiers[0].Label, &fakeSession{})
	l.observed = &Statistics{TotalAssignedJobs: 4}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err = l.prepareEscrow(t.Context())
	if err == nil {
		t.Fatal("a listener carried on against a ledger it could not use")
	}
	if strings.Contains(err.Error(), "not accepting new work") {
		t.Errorf("a broken ledger was reported as a deliberate seal: %v", err)
	}
}

// A SEAL DOES NOT RETIRE A POOL MEMBER GITHUB STILL COUNTS, and an earlier
// version of this branch did exactly that.
//
// `PoolRunnerIdle` means "no Started message has been processed locally", NOT
// "idle at GitHub". Between GitHub starting a job on a registered runner and
// this listener handling that message, the member reads idle while it is
// working — and forcing a desired count of zero retired it, removing its
// registration and destroying its compute. That fails somebody's build, and
// GitHub does not requeue a job a runner has already started.
//
// THE FIXTURE CARRIES A REAL MEMBER, because the first version of this test did
// not: with nothing registered, reconciliation could not destroy anything, so
// reintroducing the defect — or disabling retirement altogether — would have
// left it green.
func TestASealDoesNotRetireAPoolMemberGitHubStillCounts(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-pool")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	// A lease with compute behind it, registered as a pool member that reads
	// idle here — the state a runner is in between GitHub starting its job and
	// this listener hearing about it.
	lease, err := a.Reserve(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// ADVANCED PAST `capacity`, or ActiveRunnerLeases counts nothing and the
	// retirement path is never reached — which is how the first version of this
	// test stayed green with the defect back in place. A lease at `online` with a
	// pool member reading idle is exactly the dangerous state: registered, and
	// possibly already running a job this listener has not heard about.
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "sealed-host-firecracker"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 77); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	for _, phase := range []alloc.Phase{alloc.PhaseLaunching, alloc.PhaseOnline} {
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("Advance to %s: %v", phase, err)
		}
	}

	if err := a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{
		LeaseID: lease.ID, Tier: tiers[0].Label, LaunchRequestID: 77,
		RunnerName: "billet-" + lease.ID, RunnerID: 1234, Status: alloc.PoolRunnerIdle,
	}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		polls int
	)

	// DRIVEN THROUGH Run, not by calling reconcilePool directly. The defect this
	// guards against lived in what the LOOP passes as the desired count, so a
	// test that passes that count itself would stay green with the defect back.
	session := &fakeSession{}
	session.onPoll = func(int) {
		mu.Lock()
		polls++
		enough := polls >= 3
		mu.Unlock()

		if enough {
			cancel()
		}
	}

	// GitHub keeps saying one job is assigned, on every poll.
	session.onGet = func() (*Message, error) {
		return nil, ErrNoMessage
	}

	// ON THE SESSION, not on the listener: Run reads statistics from the session
	// at startup and overwrites whatever the listener was holding. Set on the
	// listener, the count was wiped before the loop began, reconcilePool was
	// skipped entirely for want of statistics, and this test passed with the
	// defect in place because nothing reconciled at all.
	session.stats = &Statistics{TotalAssignedJobs: 1}

	l := NewListener(a, tiers[0].Label, session)

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	// THE MEMBER IS STILL THERE. Retiring it would have removed the registration
	// and destroyed the compute of a job GitHub says is running.
	runners, err := a.PoolRunners(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("PoolRunners: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("the pool holds %d members, want the one GitHub still counts", len(runners))
	}
	if runners[0].Status == alloc.PoolRunnerRetiring {
		t.Error("a sealed deployment retired a member GitHub still counts as assigned; its " +
			"job would have been destroyed and never requeued")
	}
}

// AND IT REFUSES OFFERS LOCALLY, because lowering the advertisement is a request
// rather than a guard: a queued message still arrives, an unacknowledged one is
// still redelivered, and either would be acquired against escrow the refill
// takes straight back.
func TestASealedListenerDeclinesOffers(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-decline")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	session := &fakeSession{}
	l := NewListener(a, tiers[0].Label, session)

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("initial refill: %v", err)
	}

	held := l.capacity()
	if held == 0 {
		t.Fatal("the fixture escrowed nothing, so there is no offer to decline")
	}

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Reason: "draining", Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// MARKED WITHOUT HANDING THE CAPACITY BACK, which is the only arrangement
	// that tests the refusal at all. An earlier version of this called
	// observeAdmission — mark AND release — then asserted capacity was already
	// zero before presenting the offer. That made the decline inevitable: `assign`
	// declines when nothing is held, so the test passed with the refusal in
	// `handle` deleted. Measured; the mutant survived.
	//
	// Here the escrow is still held when the offer arrives, so the ONLY thing that
	// can decline it is the guard under test.
	if sealed, _ := l.markAdmission(t.Context()); !sealed {
		t.Fatal("the listener did not observe the seal")
	}
	if !l.isQuiesced() {
		t.Fatal("the listener is not quiesced after observing a sealed deployment")
	}

	if got := l.capacity(); got != held {
		t.Fatalf("the fixture holds %d capacity, want the %d it escrowed; the offer below "+
			"would be declined for want of escrow rather than by the seal", got, held)
	}

	// THE OFFER IS ACTUALLY DECLINED, which is the part lowering the
	// advertisement cannot do: a queued message still arrives and an
	// unacknowledged one is still redelivered.
	if err := l.handle(t.Context(), &Message{
		MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}},
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := session.acquiredIDs(); len(got) > 0 {
		t.Errorf("a sealed listener acquired %v against escrow it was still holding; "+
			"accepting an offer takes the escrow straight back, so the quiesce would "+
			"never converge", got)
	}

	// AND THE CAPACITY ONLY GOES BACK WHEN IT IS HANDED BACK, not as a side effect
	// of the offer.
	if got := l.capacity(); got != held {
		t.Errorf("the listener holds %d capacity after declining, want the %d it started "+
			"with", got, held)
	}

	l.handBackIdleEscrow(t.Context(), true)

	if got := l.capacity(); got != 0 {
		t.Errorf("a quiesced listener still holds %d capacity, want it handed back", got)
	}
}

// AND AN OPEN ONE ACCEPTS THE SAME OFFER, so the refusal above is not a listener
// that declines everything.
func TestAnOpenListenerAcceptsTheSameOffer(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-accept")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	session := &fakeSession{}
	l := NewListener(a, tiers[0].Label, session)

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refill: %v", err)
	}
	if sealed, _ := l.markAdmission(t.Context()); sealed {
		t.Fatal("an open deployment was observed as sealed")
	}

	if err := l.handle(t.Context(), &Message{
		MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}},
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := session.acquiredIDs(); len(got) == 0 {
		t.Fatal("an open listener declined an offer, so the sealed assertion proves nothing")
	}
}

// A SEAL IS NOT A SHUTDOWN. `draining` ends with the listener returning, which
// cancels every other listener and destroys the compute they hold. An operator
// who seals a deployment expects to be able to resume it.
func TestQuiescingIsNotDraining(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-notdrain")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	l := NewListener(a, tiers[0].Label, &fakeSession{})

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if sealed, _ := l.markAdmission(t.Context()); !sealed {
		t.Fatal("the listener did not observe the seal")
	}

	// ASSERTED IN BOTH DIRECTIONS. Without this, deleting the flag write leaves
	// quiesced false throughout and every remaining assertion here still passes.
	if !l.isQuiesced() {
		t.Fatal("a sealed deployment did not quiesce the listener")
	}

	if l.isDraining() {
		t.Fatal("a sealed deployment put the listener into its shutdown drain, which ends " +
			"with it returning and every other listener being cancelled")
	}

	// AND IT GOES BACK TO WORK when the seal lifts, which a drain never does.
	current, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}
	if _, err := db.Resume(t.Context(), state.ResumeRequest{
		Expect: current.Generation, Actor: "ops",
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if sealed, _ := l.markAdmission(t.Context()); sealed {
		t.Fatal("the listener still reports the deployment sealed after a resume")
	}
	if l.isQuiesced() {
		t.Fatal("the listener stayed quiesced after the deployment was resumed")
	}
}

// AN UNREADABLE ADMISSION STATE QUIESCES THE LISTENER WITHOUT STOPPING IT.
//
// SCOPED TO WHAT IT PROVES: that the loop survives, and that an unreadable state
// leaves the listener quiesced. It delivers no offer, so it says nothing about
// the decline itself — TestASealedListenerDeclinesOffers covers that, against
// escrow still held. An earlier header claimed both halves.
//
// The surviving half cannot be seen by calling observeAdmission: that can show
// the flag is set, and it cannot show that Run stays alive. Any fatal wiring on either side of the observation would survive
// that. So this drives Run, breaks the ledger while it is polling, and asserts
// the loop keeps going — because returning here cancels every other listener and
// destroys the compute they hold, over a transient database error.
func TestAnUnreadableAdmissionStateDoesNotStopTheLoop(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-unreadable")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		polls  int
		broken bool
	)

	session := &fakeSession{}
	session.onPoll = func(int) {
		mu.Lock()
		polls++
		count := polls
		mu.Unlock()

		// ONLY THE ADMISSION READ BREAKS, mid-flight. Closing the whole ledger
		// would prove nothing about this change: refreshAdoptedCapacity returns a
		// fatal error on a dead database and always has, so the listener would
		// stop for a reason that predates any of this. Dropping the one table
		// isolates the read under test.
		if count == 1 {
			if err := db.Tx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(context.WithoutCancel(ctx), `DROP TABLE admission`)

				return err
			}); err != nil {
				t.Errorf("drop admission: %v", err)
			}

			mu.Lock()
			broken = true
			mu.Unlock()
		}

		// IT KEPT POLLING past the break, which is the property. A listener that
		// returned would never reach this count.
		if count >= 4 {
			cancel()
		}
	}
	session.onGet = func() (*Message, error) { return nil, ErrNoMessage }

	l := NewListener(a, tiers[0].Label, session)

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an unreadable admission state stopped the listener, which cancels every "+
			"other listener and destroys the compute they hold: %v", err)
	}

	mu.Lock()
	total, wasBroken := polls, broken
	mu.Unlock()

	if !wasBroken {
		t.Fatal("the fixture never broke the ledger")
	}
	if total < 4 {
		t.Errorf("the loop polled %d times; it stopped rather than carrying on past an "+
			"unreadable admission state", total)
	}

	// AND IT TREATED THE UNREADABLE STATE AS SEALED rather than as permission.
	if !l.isQuiesced() {
		t.Error("an admission state billet could not read left the listener taking work")
	}
}

// A SEAL LANDING DURING THE POLL IS SEEN BEFORE THE MESSAGE IS HANDLED.
//
// GetMessage blocks for most of a minute, and a seal arrives from another
// process. Observed only before the poll, a seal landing while the listener was
// blocked would not be seen until the next iteration — and the message in hand
// can carry an offer, which would then be accepted against escrow still held.
func TestASealLandingDuringThePollIsObservedBeforeHandling(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-midpoll")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	session := &fakeSession{}
	l := NewListener(a, tiers[0].Label, session)

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refill: %v", err)
	}

	// Open when the poll began.
	if sealed, _ := l.markAdmission(t.Context()); sealed {
		t.Fatal("an open deployment was observed as sealed")
	}
	if l.capacity() == 0 {
		t.Fatal("the fixture holds no escrow, so an offer would find nothing to back it")
	}

	// The seal lands while the poll is blocked.
	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The observation the poll loop makes when GetMessage returns, before it
	// hands the message to handle. MARK ONLY, which is what the loop does — the
	// escrow has to outlive handle so an assignment in the same message keeps its
	// backing. Calling observeAdmission here would empty `held` first, and the
	// offer below would then be declined for want of escrow rather than by the
	// guard under test.
	if sealed, _ := l.markAdmission(t.Context()); !sealed {
		t.Fatal("a seal that landed during the poll was not seen when it returned")
	}

	if err := l.handle(t.Context(), &Message{
		MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}},
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := session.acquiredIDs(); len(got) > 0 {
		t.Errorf("an offer that arrived with the poll was acquired after the seal: %v", got)
	}
}

// DRAINING AND QUIESCED TOGETHER IS A SHUTDOWN, and the drain's rules win:
// what is running still finishes, and the listener still stops when it has.
func TestAQuiescedListenerStillDrainsOnShutdown(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-both")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	l := NewListener(a, tiers[0].Label, &fakeSession{})

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed, _ := l.markAdmission(t.Context()); !sealed {
		t.Fatal("the listener did not observe the seal")
	}

	// A shutdown now begins on top of the seal.
	drainCtx, endDrain := l.beginDrain(t.Context())
	defer endDrain()

	if !l.isDraining() {
		t.Fatal("beginDrain did not mark the listener draining")
	}
	if !l.isQuiesced() {
		t.Error("the seal was forgotten when the shutdown began")
	}
	if drainCtx.Err() != nil {
		t.Error("the drain began already cancelled, so nothing running would be waited for")
	}
}

// THE POLL LOOP RE-OBSERVES THE SEAL, driven through Run rather than by calling
// observeAdmission by hand.
//
// A hand-driven test proves the function works and says nothing about whether
// the loop calls it — deleting the call from the loop left the earlier version
// of this green. GetMessage blocks for most of a minute in production, so a seal
// landing mid-poll and an offer arriving with the response is the ordinary
// racing shape rather than an exotic one.
func TestTheLoopReObservesTheSealBeforeHandlingAMessage(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-loop")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		polls  int
		sealed bool
	)

	session := &fakeSession{}

	// The seal lands DURING the poll, which is where it lands in production:
	// another process writes it while this one is blocked in GetMessage.
	session.onPoll = func(int) {
		mu.Lock()
		polls++
		first := polls == 1
		mu.Unlock()

		if !first {
			return
		}

		if _, err := db.Seal(context.WithoutCancel(ctx), state.SealRequest{
			Provenance: state.ProvenanceOperator, Actor: "ops",
		}); err != nil {
			t.Errorf("Seal: %v", err)

			return
		}

		mu.Lock()
		sealed = true
		mu.Unlock()
	}

	// And the response carries an offer, so the message in hand is one the
	// listener must decline rather than acquire.
	session.onGet = func() (*Message, error) {
		mu.Lock()
		haveSealed := sealed
		enough := polls >= 3
		mu.Unlock()

		if enough {
			cancel()

			return nil, ErrNoMessage
		}

		if !haveSealed {
			return nil, ErrNoMessage
		}

		return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
	}

	l := NewListener(a, tiers[0].Label, session)

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	if got := session.acquiredIDs(); len(got) > 0 {
		t.Errorf("the loop acquired %v from a message that arrived with a seal; the "+
			"observation before the poll had already been made and was stale", got)
	}
}

// A SEAL LANDING DURING THE POLL MUST NOT STRIP THE BACKING FROM AN ASSIGNMENT
// IN THE MESSAGE THAT POLL RETURNED.
//
// GitHub can assign work this listener holds no in-memory promise for — after a
// restart, or on the direct-assignment path — and `assign` backs such a job from
// `held`. So the order in the loop is load-bearing: mark, handle, THEN hand the
// idle escrow back. Handing it back first empties `held`, and the assignment is
// declined for want of escrow that existed when GitHub made it.
//
// That is revoking a commitment already made, which is the one thing a seal must
// not do. It refuses NEW work; a job already assigned is not new work.
func TestASealDuringThePollKeepsBackingAnAssignmentAlreadyMade(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-midpollassign")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		mu       sync.Mutex
		polls    int
		sealedAt int
		assigned bool
		launched []int64
	)

	session := &fakeSession{}

	// THE SEAL LANDS WHILE THE POLL IS BLOCKED — after the escrow was taken and
	// advertised, before the message comes back. That is the window the fix is
	// about, and onPoll is exactly it.
	session.onPoll = func(int) {
		mu.Lock()
		polls++
		first := polls == 1
		count := polls
		mu.Unlock()

		if first {
			if _, err := db.Seal(t.Context(), state.SealRequest{
				Provenance: state.ProvenanceOperator, Actor: "ops",
			}); err != nil {
				t.Errorf("Seal: %v", err)
			}

			mu.Lock()
			sealedAt = count
			mu.Unlock()
		}

		if count >= 4 {
			cancel()
		}
	}

	// NO PROMISE ON FILE for this request, which is the case `assign` backs from
	// `held` — the restart and direct-assignment path.
	session.onGet = func() (*Message, error) {
		mu.Lock()
		defer mu.Unlock()

		if !assigned {
			assigned = true

			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		return nil, ErrNoMessage
	}

	// THE EVIDENCE IS TAKEN LIVE. Asserting on the ledger afterwards would race
	// the shutdown drain, which terminalises the very lease the assertion looks
	// for — the job being launched at all is the property, and it is observable
	// exactly when it happens.
	runner := &fakeRunner{onLaunch: func(requestID int64) error {
		mu.Lock()
		launched = append(launched, requestID)
		mu.Unlock()

		return nil
	}}

	l := NewListener(a, tiers[0].Label, session, WithRunner(runner),
		stopsWithoutWaiting())

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	when, delivered := sealedAt, assigned
	mu.Unlock()

	if when != 1 || !delivered {
		t.Fatalf("the fixture did not stage the window: sealed at poll %d, assignment "+
			"delivered %v", when, delivered)
	}

	// THE JOB IS BACKED — the runner was asked to launch it. Declined, `assign`
	// returns before anything reaches the runner.
	mu.Lock()
	got := append([]int64(nil), launched...)
	mu.Unlock()

	if !slices.Contains(got, 11) {
		t.Errorf("a seal landing mid-poll declined an assignment GitHub had already made "+
			"against capacity advertised before the seal; launched: %v", got)
	}
}

// THE OPEN CONTROL FOR THE TEST ABOVE. That one is negative — it asserts a
// member is NOT retired — so it would pass just as well if reconciliation were
// disabled entirely, or if the fixture never reached the retirement path at all.
//
// This drives the same fixture with the deployment OPEN and GitHub reporting no
// assigned jobs, where the member SHOULD be retired. If this stops passing, the
// negative assertion above stops meaning anything.
func TestTheSamePoolMemberIsRetiredWhenGitHubStopsCountingIt(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-poolcontrol")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	lease, err := a.Reserve(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "sealed-host-firecracker"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 77); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	for _, phase := range []alloc.Phase{alloc.PhaseLaunching, alloc.PhaseOnline} {
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("Advance to %s: %v", phase, err)
		}
	}

	if err := a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{
		LeaseID: lease.ID, Tier: tiers[0].Label, LaunchRequestID: 77,
		RunnerName: "billet-" + lease.ID, RunnerID: 1234, Status: alloc.PoolRunnerIdle,
	}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	// NOT SEALED, and GitHub counts nothing assigned — so this member is surplus.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		mu       sync.Mutex
		polls    int
		gone     bool
		observed bool
	)

	session := &fakeSession{}

	// OBSERVED WHILE THE LISTENER IS STILL RUNNING, and the property is that the
	// member is GONE rather than merely not idle. Two measurements shaped this:
	//
	// Reading the pool after Run returns proves nothing — the shutdown drain
	// destroys and forgets what is left, so the member disappears whether
	// reconciliation retired it or not.
	//
	// And asserting "no IDLE member" proves nothing either. Retirement marks the
	// row `retiring` before it removes routing and compute, and that mark survives
	// with the retirement itself deleted — so both the healthy and the broken
	// listener report zero idle members. Only completion discriminates.
	session.onPoll = func(int) {
		mu.Lock()
		polls++
		count := polls
		mu.Unlock()

		runners, err := a.PoolRunners(t.Context(), tiers[0].Label)
		if err != nil {
			t.Errorf("PoolRunners: %v", err)
		}

		present := false

		for _, r := range runners {
			if r.LeaseID == lease.ID {
				present = true
			}
		}

		mu.Lock()
		observed = true
		if !present {
			gone = true
		}
		done := gone
		mu.Unlock()

		if done || count >= 8 {
			cancel()
		}
	}
	session.onGet = func() (*Message, error) { return nil, ErrNoMessage }
	session.stats = &Statistics{TotalAssignedJobs: 0}

	l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{}),
		stopsWithoutWaiting())

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	retired, sampled := gone, observed
	mu.Unlock()

	if !sampled {
		t.Fatal("the fixture never sampled the pool while the listener was running")
	}
	if !retired {
		t.Fatal("a pool member GitHub no longer counts was never retired while the listener " +
			"ran; the sealed test's negative assertion proves nothing if this path never " +
			"actually retires anything")
	}
}

// AN IDLE SEALED DEPLOYMENT REACHES QUIET ON ITS OWN.
//
// This is the end-to-end property `billet drain --wait` rests on: nothing is
// running, so nothing will ever finish, and reaching quiet has to come from the
// listener handing back what it holds rather than from work completing.
//
// WHAT ACTUALLY MAKES IT TRUE, established by mutation rather than by reading:
//
//   - Handing back EVERY held lease. Trimming to targetCapacity instead — which
//     keeps one discovery slot — leaves a capacity-phase lease the barrier
//     counts, and Quiet() never becomes true. Killed this test.
//   - Handing back at all. Killed this test.
//
// And what does NOT make it true, said out loud because both mutations SURVIVED
// and it would be easy to claim otherwise: skipping prepareEscrow while
// quiesced, and dropping the discovery slot from the advertisement. Neither is
// load-bearing HERE, because alloc.Escrow refuses at the ledger while sealed, so
// a refill cannot recreate escrow whether or not the loop attempts one, and an
// advertisement is a number sent to GitHub rather than a lease. Both are still
// right to keep — one stops billet asking for what it will be refused, the other
// stops GitHub being told about capacity that is not there — but the guarantee
// rests on the allocator, not on the loop remembering.
//
// OBSERVED WHILE THE LISTENER RUNS, not after. The shutdown drain releases
// escrow too, so a sample taken after Run returns would report quiet whether or
// not sealing achieved anything.
func TestAnIdleSealedDeploymentReachesQuietWithoutAnotherJob(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-idlequiet")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	session := &fakeSession{}
	l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{}),
		stopsWithoutWaiting())

	// ESCROWED AND ADVERTISED FIRST, so the deployment genuinely holds something
	// when the seal lands. Sealing an allocator that never escrowed would reach
	// quiet trivially.
	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refill: %v", err)
	}
	if l.capacity() == 0 {
		t.Fatal("the fixture holds no escrow, so reaching quiet would prove nothing")
	}

	before, err := a.Quiescence(t.Context())
	if err != nil {
		t.Fatalf("Quiescence: %v", err)
	}
	if len(before.Outstanding) == 0 {
		t.Fatal("the ledger records no outstanding lease for the escrow the listener holds")
	}

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops", Reason: "cutover",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		quiet bool
		polls int
	)

	session.onPoll = func(int) {
		mu.Lock()
		polls++
		count := polls
		mu.Unlock()

		q, err := a.Quiescence(context.WithoutCancel(ctx))
		if err != nil {
			t.Errorf("Quiescence: %v", err)

			return
		}

		if q.Quiet() {
			mu.Lock()
			quiet = true
			mu.Unlock()

			cancel()

			return
		}

		if count >= 10 {
			cancel()
		}
	}
	session.onGet = func() (*Message, error) { return nil, ErrNoMessage }

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	reached := quiet
	mu.Unlock()

	if !reached {
		q, qErr := a.Quiescence(t.Context())
		t.Fatalf("an idle sealed deployment never reached quiet while the listener ran "+
			"(outstanding=%+v, err=%v); a drain on it would wait forever for a job that is "+
			"never going to arrive", q.Outstanding, qErr)
	}
}

// AN UNREADABLE ADMISSION STATE DECLINES OFFERS AND KEEPS WHAT IT HOLDS.
//
// The two are different kinds of act and only one is fail-closed. REFUSING costs
// a poll and cannot admit work billet is unable to back, so doing it on a failed
// read is the safe direction. HANDING THE ESCROW BACK is premised on knowing the
// deployment is sealed — which is exactly what just failed to be established —
// and a listener that returns its escrow on a transient database blip hands the
// gap to another tier and retakes it next poll, which is the flapping the escrow
// exists to prevent.
//
// DRIVEN THROUGH Run, and that is the whole point of this version. The earlier
// one called a helper that composed mark-and-release, with a comment claiming it
// was "the composition the pre-poll site uses". A later change made the loop
// mark and release in two steps and dropped the `known` bit between them — so
// production released escrow on an unreadable read while this test stayed green,
// because the helper it called was by then reachable from nothing but itself.
// A test that names an invariant has to exercise the code that holds it.
func TestAnUnreadableAdmissionStateKeepsTheEscrowItCannotJustify(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-keephold")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		polls  int
		broke  bool
		afterB []int
	)

	session := &fakeSession{}
	l := NewListener(a, tiers[0].Label, session)

	session.onPoll = func(int) {
		mu.Lock()
		polls++
		count := polls
		mu.Unlock()

		// Escrow first, then break ONLY the admission read — the rest of the
		// ledger still answers, the way a timed-out read on a busy database does.
		if count == 2 {
			if err := db.Tx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(context.WithoutCancel(ctx), `DROP TABLE admission`)

				return err
			}); err != nil {
				t.Errorf("drop admission: %v", err)
			}

			mu.Lock()
			broke = true
			mu.Unlock()

			return
		}

		// Every poll after the break, record what is still held. These are full
		// iterations: each one marked, polled, and reached the release decision.
		mu.Lock()
		if broke {
			afterB = append(afterB, l.idleEscrow())
		}
		mu.Unlock()

		if count >= 6 {
			cancel()
		}
	}
	session.onGet = func() (*Message, error) { return nil, ErrNoMessage }

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	held, brokeIt := append([]int(nil), afterB...), broke
	mu.Unlock()

	if !brokeIt || len(held) < 2 {
		t.Fatalf("the fixture never completed a poll with an unreadable admission state "+
			"(broke=%v, samples=%d)", brokeIt, len(held))
	}

	// STILL HOLDING, on every poll after the read began failing. Handing it back
	// is an action taken on a fact that was never established.
	for i, n := range held {
		if n == 0 {
			t.Fatalf("the listener handed its escrow back on poll %d after a read that "+
				"FAILED; that gap goes to another tier and is retaken next poll: %v",
				i+1, held)
		}
	}

	// AND IT DECLINED, which is the fail-closed half and must still happen.
	if !l.isQuiesced() {
		t.Error("an unreadable admission state left the listener taking work")
	}
}

func TestWithdrawingLowersTheNumberBeforeHandingTheEscrowBack(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-withdraw")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registerHost(t, a)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		polls int
		// Each poll's advertised number paired with what was still held when it
		// went out. The pairing is the point: a snapshot of either alone cannot
		// show which came first.
		sent []struct{ advertised, held int }
	)

	session := &fakeSession{}
	l := NewListener(a, tiers[0].Label, session)

	session.onPoll = func(capacity int) {
		mu.Lock()
		polls++
		count := polls
		sent = append(sent, struct{ advertised, held int }{capacity, l.idleEscrow()})
		mu.Unlock()

		// Sealed after the first poll has advertised the escrow, so there IS a
		// live advertisement to withdraw.
		if count == 1 {
			if _, err := db.Seal(t.Context(), state.SealRequest{
				Provenance: state.ProvenanceOperator, Actor: "ops",
			}); err != nil {
				t.Errorf("Seal: %v", err)
			}
		}

		if count >= 4 {
			cancel()
		}
	}
	session.onGet = func() (*Message, error) { return nil, ErrNoMessage }

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	got := append([]struct{ advertised, held int }(nil), sent...)
	mu.Unlock()

	if len(got) < 3 {
		t.Fatalf("only %d polls; the fixture never reached a withdrawal", len(got))
	}
	if got[0].advertised == 0 {
		t.Fatalf("the first poll advertised nothing, so there was no advertisement to "+
			"withdraw: %+v", got)
	}

	// THE POLL THAT FIRST CARRIES THE LOWER NUMBER STILL HOLDS THE ESCROW. That
	// is the whole invariant: at no instant is GitHub's live number larger than
	// what this listener can back.
	var withdrew bool

	for _, p := range got[1:] {
		if p.advertised < got[0].advertised {
			withdrew = true

			if p.held == 0 {
				t.Errorf("the escrow was handed back before, or in the same breath as, the "+
					"poll that lowered the advertisement: %+v", got)
			}

			break
		}
	}

	if !withdrew {
		t.Errorf("no poll ever advertised less than the first, so the seal never withdrew "+
			"anything: %+v", got)
	}

	// AND IT DOES GO BACK, so this is a deferral rather than a leak.
	if l.idleEscrow() != 0 {
		t.Errorf("the listener still holds %d idle leases after several sealed polls",
			l.idleEscrow())
	}
}
