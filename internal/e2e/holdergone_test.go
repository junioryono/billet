package e2e

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wiring"
)

// THE HOLDER-GONE REPRODUCTIONS, WRITTEN BEFORE ANY FIX.
//
// The acceptance record says a completion bound to a node incarnation that had
// been killed kept its capacity charged, and that the escape was a fresh ledger.
// The record's own hypothesis is a custody or teardown lease whose holder died:
// `billet leases release --force` sets a request only that holder can observe,
// so the lease would sit held with force=requested forever. These tests put a
// real node runtime over the real CodeBuild backend against a fake AWS, kill the
// process at the moment the hypothesis names, and record what actually happens.
//
// A KILLED HOLDER IS NOT A HOLDER THAT RENEWS. That is the whole of the answer
// to the hypothesis: a custody or teardown lease is renewed by the node's own
// janitor and by nothing else, so the process dying stops the renewals, the
// reaper quarantines the lease within one TTL, and quarantine is the phase the
// force command already resolves on the spot. Nothing here needs a timer or an
// operator escape; what it needs is to be SEEN, which is what the holder column
// on `billet leases` is for.
const (
	// cbTTL is short so "past its TTL" is one clock advance. The node's janitor
	// paces itself from it, but no janitor survives the kill these tests stage.
	cbTTL = 30 * time.Second
	// pastGrace is more than alloc's five-minute quarantine grace.
	pastGrace = 6 * time.Minute
)

// killedTeardownStack launches a job, asks the backend to stop it, and has the
// backend never confirm — so the lease is in teardown, held by this process's
// custody. Returns the lease and the build id.
func killedTeardownStack(t *testing.T) (*codeBuildStack, *offsetClock, *alloc.Lease, string) {
	t.Helper()

	clock := &offsetClock{}
	s := newCodeBuildStack(t, cbOnClock(clock, cbTTL))

	const requestID = 606

	lease := s.assignedLease(t, requestID)

	if err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	id, ok := s.fake.idOf(provider.InstanceName(lease.ID))
	if !ok {
		t.Fatal("no build was started")
	}

	// THE JOB RAN AND GITHUB REPORTED IT, before anything asked the backend to
	// stop: the listener records a completion's result ahead of the destroy it
	// dispatches, so the lease carries GitHub's word from here on.
	for _, phase := range []alloc.Phase{alloc.PhaseOnline, alloc.PhaseBusy} {
		if err := s.alloc.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("Advance to %s: %v", phase, err)
		}
	}
	if err := s.alloc.RecordJobResult(t.Context(), lease.ID, "succeeded", requestID); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	// A STOP IS A REQUEST, and this backend never confirms it: the node hands the
	// lease to custody and tells the caller so.
	s.fake.neverConfirmStops()

	err := s.runner.Destroy(t.Context(), requestID)
	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy of an unconfirmed stop = %v, want ErrCustody", err)
	}

	// One tend reports the durable phase the operator sees.
	if err := s.runner.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	current := readLease(t, s.alloc, lease.ID)
	if current.Phase != alloc.PhaseTeardown {
		t.Fatalf("after an unconfirmed stop the lease is %s, want teardown", current.Phase)
	}

	return s, clock, current, id
}

// restartAs registers a replacement process under the stack's node name, the
// way a restarted `billet node` does before it recovers anything, and returns
// the runtime that process would run.
func (s *codeBuildStack) restartAs(t *testing.T, incarnation string) *node.Runner {
	t.Helper()

	if _, err := s.alloc.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: s.host, Provider: config.ProviderCodeBuild,
		VCPU: 64, Memory: 256 * config.GiB, Incarnation: incarnation,
		EC2Shapes: []config.RemoteShape{
			{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 10000},
		},
	}); err != nil {
		t.Fatalf("register the replacement process: %v", err)
	}

	return node.New(s.alloc, s.host, s.jit, s.provider(t), nil)
}

func heldHolder(t *testing.T, a *alloc.Allocator, id string) alloc.Holder {
	t.Helper()

	held, err := a.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}

	for i := range held {
		if held[i].ID == id {
			return held[i].Holder
		}
	}

	t.Fatalf("lease %s is not held", id)

	return alloc.Holder{}
}

func readLease(t *testing.T, a *alloc.Allocator, id string) *alloc.Lease {
	t.Helper()

	lease, err := a.Lease(t.Context(), id)
	if err != nil {
		t.Fatalf("read lease %s: %v", id, err)
	}

	return lease
}

func heldPhase(t *testing.T, a *alloc.Allocator, id string) (alloc.Phase, bool) {
	t.Helper()

	held, err := a.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}

	for i := range held {
		if held[i].ID == id {
			return held[i].State, true
		}
	}

	return "", false
}

// THE ISSUE'S OWN SCENARIO: kill a node mid-teardown and restart it, with the
// build having ended on its own in between.
//
// The restarted process adopts nothing — CodeBuild's inventory excludes terminal
// builds, which is right — and then the thing the hypothesis missed happens:
// nobody renews the lease, the reaper quarantines it within one TTL, and the
// restarted host's next inventory settles it after the grace. THE VERDICT IS
// THE JOB'S: the completion had already been delivered to the process that
// died, and the listener records GitHub's result before it dispatches a
// destroy, so what inventory settles is a job that finished — `done`, with no
// provisional failure for a later completion to correct, because no later
// completion is coming. A first version of this test recorded `failed` here as
// an observation; that observation is what this asserts against now.
func TestAKilledTeardownHolderIsQuarantinedAndSettledOnRestart(t *testing.T) {
	s, clock, lease, id := killedTeardownStack(t)

	// The process dies. Its custody map, its janitor and its tend loop go with
	// it; the ledger and the build do not.
	s.fake.finish(id)

	restarted := s.restartAs(t, secondProcess)
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// NOT RE-ADOPTED: a finished build is not in the inventory, so there is
	// nothing for a restart to hold. The lease is still charged and still says
	// teardown, and nothing is renewing it. What an operator CAN read is who held
	// it: the process that died, replaced by the one registered now.
	if phase := readLease(t, s.alloc, lease.ID).Phase; phase != alloc.PhaseTeardown {
		t.Fatalf("after a restart the lease is %s, want teardown still", phase)
	}

	if h := heldHolder(t, s.alloc, lease.ID); h.Incarnation != firstProcess || !h.Replaced() ||
		h.NodeIncarnation != secondProcess {
		t.Fatalf("held holder = %+v, want %s replaced by %s", h, firstProcess, secondProcess)
	}

	clock.advance(cbTTL + time.Second)

	if _, err := s.alloc.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	// QUARANTINED, WITHIN ONE TTL OF THE KILL. This is the fact the hypothesis
	// contradicts: the lease does not sit in teardown waiting for a holder.
	if phase, ok := heldPhase(t, s.alloc, lease.ID); !ok || phase != alloc.PhaseQuarantine {
		t.Fatalf("a TTL after its holder died the lease is %q (held=%v), want quarantine",
			phase, ok)
	}

	// The restarted host reports what it runs — nothing — and the grace has not
	// passed, so nothing is freed yet.
	if err := restarted.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := s.alloc.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("an absence inside the grace freed the lease: %v", err)
	}

	clock.advance(pastGrace)

	if err := restarted.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep after the grace: %v", err)
	}

	if _, err := s.alloc.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("after the grace the lease is still open (err=%v); its capacity never came back", err)
	}

	usage, err := s.alloc.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Leases != 0 {
		t.Fatalf("%d lease(s) still charge capacity after settlement", usage.Leases)
	}

	// THE VERDICT IS GITHUB'S, NOT INVENTORY'S GUESS. The build succeeded, the
	// completion reached a process that died holding it, and the result it
	// carried outlived that process in the ledger.
	outcome, err := s.alloc.HistoryOutcome(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryOutcome: %v", err)
	}

	reason, err := s.alloc.HistoryFailureReason(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryFailureReason: %v", err)
	}

	if outcome != string(alloc.PhaseDone) || reason != "" {
		t.Fatalf("a job GitHub reported succeeded settled as %q with reason %q after its "+
			"teardown holder died; want done with no reason", outcome, reason)
	}
}

// THE OPERATOR ESCAPE ALREADY EXISTS, ONE PHASE OVER.
//
// While the dead holder's lease is still in teardown the force command can only
// leave a request, because it cannot tell a holder that is gone from one that is
// merely slow — and it must not: an epoch that moved can still be a superseded
// process draining. Once the reaper has quarantined the lease, the same command
// releases it on the spot. The hypothesis was that the first state lasts
// forever; it lasts one TTL.
func TestAKilledTeardownHolderIsReleasedByForceOnceQuarantined(t *testing.T) {
	s, clock, lease, id := killedTeardownStack(t)
	s.fake.finish(id)

	// The process is gone. An operator forces the lease while it still says
	// teardown: a request is recorded for a holder that will never read it.
	result, err := s.alloc.ForceRelease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("ForceRelease while teardown: %v", err)
	}

	if !result.Pending {
		t.Fatal("a teardown lease was released outright; only its holder may drop custody")
	}

	if _, err := s.alloc.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("a pending force request changed the lease: %v", err)
	}

	clock.advance(cbTTL + time.Second)

	if _, err := s.alloc.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if phase, ok := heldPhase(t, s.alloc, lease.ID); !ok || phase != alloc.PhaseQuarantine {
		t.Fatalf("a TTL later the lease is %q (held=%v), want quarantine", phase, ok)
	}

	// THE SAME COMMAND, NOW RESOLVES. No holder exists for a quarantined lease,
	// so the operator's assertion is acted on directly.
	result, err = s.alloc.ForceRelease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("ForceRelease while quarantined: %v", err)
	}

	if result.Pending {
		t.Fatal("a quarantined lease was handed to a holder that does not exist")
	}

	if _, err := s.alloc.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("the forced lease is still open: %v", err)
	}

	// AND THE HISTORY SAYS AN OPERATOR DID IT, rather than carrying a failure
	// nothing explains.
	if got, err := s.alloc.HistoryFailureReason(t.Context(), lease.ID); err != nil ||
		got != alloc.ForceReleasedReason {
		t.Fatalf("forced lease reason = %q err=%v, want %q", got, err, alloc.ForceReleasedReason)
	}
}

// THE OTHER HALF OF THE ISSUE'S SCENARIO: the build is still running when the
// node comes back.
//
// Then it IS in the inventory, the restart adopts it, and what the adoption has
// to get right is the phase it reports. A lease in teardown had a stop asked for
// it; the state machine allows teardown to end and never to go back to custody,
// so an adoption that called it custody would be refused on every tend, the
// lease would be renewed forever and the build never stopped. The adoption keeps
// the phase it found and finishes the teardown it inherited.
func TestAKilledTeardownHolderIsReadoptedWhenItsBuildStillRuns(t *testing.T) {
	s, _, lease, id := killedTeardownStack(t)

	restarted := s.restartAs(t, secondProcess)
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// ADOPTED, because the build is still running and its lease is still open.
	// Tending it is where the phase is reported, and a refused report is the
	// failure this test exists to catch.
	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after re-adopting a teardown: %v", err)
	}

	if phase := readLease(t, s.alloc, lease.ID).Phase; phase != alloc.PhaseTeardown {
		t.Fatalf("a re-adopted teardown reports %s, want teardown", phase)
	}

	// AND THE HOLDER IS THE PROCESS TENDING IT NOW. The report of a hold the
	// lease was already in names its holder, or the dead process would stay
	// the durable holder and the report would call a live one replaced.
	if h := heldHolder(t, s.alloc, lease.ID); h.Incarnation != secondProcess || h.Replaced() {
		t.Fatalf("re-adopted holder = %+v, want the tending process %s", h, secondProcess)
	}

	// The build ends, the tend proves it terminal, and the lease is released
	// with the outcome a completed job has.
	s.fake.finish(id)

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the build ended: %v", err)
	}

	if _, err := s.alloc.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("the re-adopted teardown is still open after its build ended: %v", err)
	}

	outcome, err := s.alloc.HistoryOutcome(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryOutcome: %v", err)
	}

	if outcome != string(alloc.PhaseDone) {
		t.Fatalf("a re-adopted completed job was recorded as %q, want done", outcome)
	}
}

// A LAUNCH THAT FAILED AFTER STARTING SOMETHING IS ARCHIVED AS FAILED, EVEN BY
// THE PROCESS THAT INHERITS ITS TEARDOWN.
//
// The launching process knows the job never ran — the outcome lives in its
// custody entry — and the ledger records only the phase, which a completed
// job's teardown shares. Found by the review of the re-adoption above: a
// restart reading `teardown` with no reason reconstructed `done` for a runner
// that never registered. So the reason is written before the hold is first
// reported, and the restart reads it back.
func TestAKilledFailedLaunchTeardownIsArchivedAsFailedOnRestart(t *testing.T) {
	clock := &offsetClock{}
	s := newCodeBuildStack(t, cbOnClock(clock, cbTTL))
	lease := s.assignedLease(t, 607)

	// StartBuild commits a build and loses its id, and the stop that cleans it
	// up is never confirmed: a launch that failed with compute it cannot name.
	s.fake.startAmbiguously()
	s.fake.neverConfirmStops()

	err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: 607, Event: "push"})
	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("an ambiguous launch with an unconfirmed cleanup = %v, want ErrCustody", err)
	}

	id, ok := s.fake.idOf(provider.InstanceName(lease.ID))
	if !ok {
		t.Fatal("the ambiguous start left no build")
	}

	// One tend reports the hold. This is the moment the outcome has to become
	// durable, because it is the last thing the launching process does.
	if err := s.runner.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	current := readLease(t, s.alloc, lease.ID)
	if current.Phase != alloc.PhaseTeardown || current.FailureReason != alloc.LaunchFailedReason {
		t.Fatalf("after reporting the hold the lease is %s with reason %q, want teardown carrying %q",
			current.Phase, current.FailureReason, alloc.LaunchFailedReason)
	}

	// The process dies; a replacement adopts the build it finds and finishes
	// the teardown once the build ends.
	restarted := s.restartAs(t, secondProcess)
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after re-adopting: %v", err)
	}

	s.fake.finish(id)

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the build ended: %v", err)
	}

	if _, err := s.alloc.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("the inherited teardown is still open: %v", err)
	}

	outcome, err := s.alloc.HistoryOutcome(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryOutcome: %v", err)
	}
	if outcome != string(alloc.PhaseFailed) {
		t.Fatalf("a launch that never ran a job was archived as %q by the process that inherited "+
			"its teardown, want failed", outcome)
	}
}

// ------------------------------------------- a completion bound to a dead process --

// newCodeBuildProvider builds the real backend over a fake AWS, for both stacks.
func newCodeBuildProvider(
	t *testing.T, fake *fakeCodeBuild, deployment string, shapes []config.RemoteShape,
) *codebuild.Provider {
	t.Helper()

	prov, err := codebuild.New(deployment, config.CodeBuildConfig{
		Region:                     "us-west-2",
		Endpoint:                   fake.URL + "/",
		Project:                    "billet-cb",
		EnvironmentType:            config.CodeBuildLinuxContainer,
		PrivilegedMode:             true,
		AcceptExternalBuildCeiling: true,
		JITParameterPath:           "/billet/e2e/jit",
		BuildTimeoutMinutes:        60,
		QueuedTimeoutMinutes:       5,
		ComputeTypes:               shapes,
	},
		codebuild.WithHTTPClient(fake.Client()),
		codebuild.WithCredentials(cbCreds{}))
	if err != nil {
		t.Fatalf("codebuild.New: %v", err)
	}

	// Parameter Store's endpoint is DERIVED rather than configured — deliberately,
	// so an operator cannot aim a single-use credential at a host of their choosing
	// — so the test points it at the fake through the package's test seam.
	codebuild.SetSSMEndpointForTest(prov, fake.URL+"/")

	// A teardown the fake never confirms would otherwise pace its polls on the
	// wall clock; the wait proves nothing here and the context still ends it.
	codebuild.SetSleepForTest(prov, func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	})

	return prov
}

// wiredCodeBuild is the whole of billet over the CodeBuild backend: a real
// control plane and listener, a real node wire, a real node loop and the real
// provider, against a fake GitHub and a fake AWS — or, when a test supplies
// one, real AWS.
//
// THE ONLY STACK IN WHICH THE PLANE'S OWNER RECORD, THE LISTENER'S RENEWAL AND
// THE BACKEND'S INVENTORY ALL EXIST AT ONCE, which is where the holder-gone
// defect lives: each of them is correct alone, and the defect is in how they
// wait for each other.
type wiredCodeBuild struct {
	*stack
	// fake is the scripted AWS, nil when the stack runs against the real one.
	fake       *fakeCodeBuild
	clock      *offsetClock
	shapes     []config.RemoteShape
	deployment string
	// providers builds one backend per node process, over whatever AWS this
	// stack was assembled against, and cb is the control plane's own.
	providers func(t *testing.T) *codebuild.Provider
	cb        *codebuild.Provider
	// firstNode is the process wireUp started, so a test can wait for it to
	// stop on its own — which is what a superseded process draining does.
	firstNode *nodeLoop
	// liveName is the build a live run started, for its cleanup to reap.
	liveName string
}

// wiredOpt varies how the wired CodeBuild stack is assembled.
type wiredOpt func(*wiredConfig)

type wiredConfig struct {
	// providers, when set, replaces the fake AWS with backends of the caller's
	// making; the stack then has no fake to script.
	providers func(t *testing.T, shapes []config.RemoteShape, deployment string) *codebuild.Provider
	// supersedable lets the first node process end with ErrSuperseded without
	// that counting as a failure: the test is about a process being replaced.
	supersedable bool
	// reapEvery replaces the harness's 200ms reaper tick. Against real AWS every
	// tick is an inventory walk, and a walk every 200ms is a throttled account.
	reapEvery time.Duration
}

// withRealProviders assembles the wired stack over backends the caller builds,
// which is how the same scenario runs against real AWS.
func withRealProviders(
	build func(t *testing.T, shapes []config.RemoteShape, deployment string) *codebuild.Provider,
) wiredOpt {
	return func(c *wiredConfig) { c.providers = build }
}

// supersedableFirstNode tolerates the first node process being superseded.
func supersedableFirstNode(c *wiredConfig) { c.supersedable = true }

// withWiredReapInterval replaces the reaper tick.
func withWiredReapInterval(d time.Duration) wiredOpt {
	return func(c *wiredConfig) { c.reapEvery = d }
}

// cbWireTTL is the lease TTL of the wired stack: short enough that a lease
// nobody renews is quarantined within seconds, long enough that a heartbeat
// every third of it is not a busy loop.
const cbWireTTL = 3 * time.Second

func newWiredCodeBuild(t *testing.T, opts ...wiredOpt) *wiredCodeBuild {
	t.Helper()

	var wc wiredConfig
	for _, o := range opts {
		o(&wc)
	}

	p := newPlane(t)
	dir := t.TempDir()

	client, err := scaleset.New(scaleset.Config{
		ConfigURL:      p.URL + "/acme",
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     p.PrivateKeyPEM(),
		Org:            "acme",
		AppID:          12345,
		APIURL:         p.URL + "/api/v3",
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	closeDB := sync.OnceFunc(func() { _ = db.Close() })
	t.Cleanup(closeDB)

	// THE FAKE GITHUB SERVES ONE SCALE SET, under testTier's label, so the tier
	// carries that label whatever backend runs it.
	tiers := []config.Tier{{
		Label:       testTier,
		Provider:    config.ProviderCodeBuild,
		VCPU:        4,
		Memory:      7 * config.GiB,
		Image:       "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		RunnerGroup: testGroup,
		GuestOS:     config.GuestLinux,
		Trust:       config.WorkloadTrusted,
		Workflows:   []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},
		Command:     []string{"./run.sh"},
	}}

	clock := &offsetClock{}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, tiers,
		alloc.WithClock(clock.now), alloc.WithLeaseTTL(cbWireTTL))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	deployment, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	shapes := []config.RemoteShape{
		{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 10000},
	}

	var (
		fake      *fakeCodeBuild
		providers func(t *testing.T) *codebuild.Provider
	)

	if wc.providers != nil {
		providers = func(t *testing.T) *codebuild.Provider {
			t.Helper()

			return wc.providers(t, shapes, deployment)
		}
	} else {
		fake = newFakeCodeBuild(t)
		providers = func(t *testing.T) *codebuild.Provider {
			t.Helper()

			return newCodeBuildProvider(t, fake, deployment, shapes)
		}
	}

	prov := providers(t)
	log := testLogger(t)

	const host = "aws-cb-wire"

	// THE NODE'S OWN SECOND SIGNAL, separate from the server's: this stack stops
	// a node process while the control plane keeps running, and a channel shared
	// with the server would end the plane's drain later for the wrong reason.
	nodeHurry := make(chan struct{})
	serverHurry := make(chan struct{})

	// A SHORT COMMAND TIMEOUT, because a destroy queued for a process that is
	// gone otherwise waits ten minutes before the plane will say so.
	first, wire, wireAddr, serverOpts := wireUp(
		t, log, a, client, prov, config.ProviderCodeBuild, shapes, tiers, deployment,
		host, nodeHurry, wireOptions{
			plane:        []nodeplane.Option{nodeplane.WithCommandTimeout(10 * time.Second)},
			supersedable: wc.supersedable,
		})

	stopWithHurry := sync.OnceFunc(func() {
		close(nodeHurry)
		first.stop()
	})

	reapEvery := wc.reapEvery
	if reapEvery == 0 {
		reapEvery = 200 * time.Millisecond
	}

	serverOpts = append(serverOpts,
		server.WithReapInterval(reapEvery),
		server.WithDrainTimeout(200*time.Millisecond),
		server.WithHurry(serverHurry),
		// A completion whose holder is gone settles on a LATER attempt, and the
		// default pacing would wait fifteen seconds and then minutes for it.
		server.WithCleanupRetry(200*time.Millisecond, time.Second))

	srv := server.New(a, wiring.Provisioner{Client: client}, tiers, "billet-test", log, serverOpts...)

	return &wiredCodeBuild{
		stack: &stack{
			hurry: serverHurry,
			dir:   dir, closeDB: closeDB, plane: p, alloc: a, db: db,
			runner: first.runner, server: srv, provider: prov, node: host, tiers: tiers,
			wire: wire, stopNode: stopWithHurry, wireAddr: wireAddr,
		},
		fake:       fake,
		clock:      clock,
		shapes:     shapes,
		deployment: deployment,
		providers:  providers,
		cb:         prov,
		firstNode:  first,
	}
}

// restartNode starts a replacement process under the stack's node name over
// the same backend, and waits until the plane's commands for that name reach
// EXACTLY that process.
//
// THE WAIT IS FOR THE REPLACEMENT'S OWN INCARNATION, not for the old one to be
// gone: "the owner is not current" is equally true of an original that merely
// expired before its replacement registered, and a completion delivered in that
// window would exercise "no live holder" rather than the replaced holder this
// scenario is about.
func (w *wiredCodeBuild) restartNode(t *testing.T) string {
	t.Helper()

	hurry := make(chan struct{})

	replacement := startNodeLoop(t, testLogger(t), w.wireAddr, nodeProcess{
		host: w.node, deployment: w.deployment,
		provider: w.providers(t),
		kind:     config.ProviderCodeBuild, shapes: w.shapes, hurry: hurry,
	})

	t.Cleanup(func() {
		close(hurry)
		replacement.stop()
	})

	waitUntil(t, "the replacement process to be the one the plane talks to", func() bool {
		return w.wire.CurrentIncarnationForTest(w.node) == replacement.incarnation
	})

	return replacement.incarnation
}

// runJobToCompletionOnADeadHolder drives one job through the real dispatch path,
// kills the node that launched it, lets the build finish on its own, registers a
// replacement process, and delivers GitHub's completion to a control plane whose
// record of the holder names the dead process. Returns the job's lease id.
func (w *wiredCodeBuild) runJobToCompletionOnADeadHolder(t *testing.T) string {
	t.Helper()

	const requestID = 7001

	// REGISTERED BEFORE THE CONTROL PLANE'S STOP, so it runs after it: cleanups
	// run last-in-first-out, and a reap that talks to real AWS for seconds while
	// the plane's context is already cancelled would let Run return before the
	// harness asked it to, which the harness rightly reports as a failure.
	if w.fake == nil {
		w.reapLiveBuild(t)
	}

	w.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", requestID, "push", testTier))

	stop := sync.OnceFunc(w.run(t))
	t.Cleanup(stop)

	deadline := time.Now().Add(30 * time.Second)
	for len(w.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	w.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", requestID, "push", testTier))

	names := w.awaitOneRunning(t)

	leaseID, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("build %q does not carry a billet lease name", names[0])
	}

	w.liveName = names[0]

	// THE PROCESS THAT LAUNCHED IT DIES, without destroying anything: the build
	// keeps running on AWS and the control plane keeps the lease in its running
	// set, renewed on the listener's own clock.
	w.stopNode()

	// The runner inside finishes the job on its own and the build ends.
	w.finishBuild(t, names[0])

	// A replacement process registers under the same name. Its inventory is the
	// backend's, which no longer lists a finished build, so it adopts nothing.
	replacement := w.restartNode(t)

	owner, ok := w.wire.OwnerOfLease(leaseID)
	if !ok || owner.Current || owner.Incarnation == replacement {
		t.Fatalf("owner = %+v (present=%v), want the dead process recorded and not current", owner, ok)
	}

	// GitHub reports the job complete, with the result the runner produced.
	done := fakeactions.JobJSON("JobCompleted", requestID, "push", testTier)
	done["result"] = "Succeeded"
	w.plane.queue(fakeactions.StatisticsJSON(0, 0), done)

	// Message 3: available, assigned, completed.
	awaitAck(t, w.stack, 3)

	return leaseID
}

// finishBuild ends the build carrying a lease name the way the runner exiting
// does: scripted on the fake, and WAITED FOR on real AWS, where the runner
// refuses the fake GitHub's registration and the build fails on its own.
func (w *wiredCodeBuild) finishBuild(t *testing.T, name string) {
	t.Helper()

	if w.fake != nil {
		id, ok := w.fake.idOf(name)
		if !ok {
			t.Fatalf("no build carries %q", name)
		}

		w.fake.finish(id)

		return
	}

	waitUntilWithin(t, 6*time.Minute, "the real build to end on its own", func() bool {
		inst, found, err := w.cb.Find(t.Context(), name)

		return err == nil && found && inst.Terminal
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	waitUntilWithin(t, 30*time.Second, what, cond)
}

func waitUntilWithin(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(limit)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// THE STATE THE ACCEPTANCE RECORD DESCRIBES, END TO END, and what now ends it.
//
// The completion is bound to the process that launched the build. That process
// is dead and a replacement holds its name, so the plane will not hand the
// destroy to the replacement — right, because a replacement truthfully knows
// nothing — and answers that the holder is unavailable. The listener used to
// read that as an ordinary failed destroy: keep the lease, keep RENEWING it,
// retry. The lease then never expired, so it was never quarantined, so the
// replacement's inventory could never settle it, so every retry got the same
// answer, forever. `billet leases` showed nothing held and `--force` refused the
// lease as busy. The escape used at acceptance was a fresh ledger.
//
// WHAT CHANGED IS ONLY WHO RENEWS. A listener whose destroy cannot reach the
// holder holds no compute, so it stops renewing and keeps the obligation. If the
// dead process's replacement had adopted the build it would renew the lease
// itself; here nothing does, the reaper quarantines it within a TTL — the same
// state a killed custody holder's lease reaches — and the completion's retry
// settles it from the replacement's known-empty inventory after the grace, with
// GitHub's own outcome rather than inventory's provisional one.
func TestACompletionBoundToAKilledIncarnationIsSettledOnceItsHolderIsReplaced(t *testing.T) {
	w := newWiredCodeBuild(t)
	leaseID := w.runJobToCompletionOnADeadHolder(t)

	// QUARANTINED WITHIN A TTL. This is the assertion the defect fails: with the
	// listener still renewing, the lease stays busy for as long as the control
	// plane runs.
	waitUntil(t, "the orphaned completion's lease to be quarantined", func() bool {
		phase, held := heldPhase(t, w.alloc, leaseID)

		return held && phase == alloc.PhaseQuarantine
	})

	// STILL CHARGED, deliberately: quarantine is capacity held until proof.
	if _, err := w.alloc.Lease(t.Context(), leaseID); err != nil {
		t.Fatalf("quarantine released the lease before any proof: %v", err)
	}

	// The replacement's inventory has excluded the build since it registered,
	// and the grace is what turns that absence into settlement.
	w.clock.advance(pastGrace)

	waitUntil(t, "the completion to settle the quarantined lease", func() bool {
		_, err := w.alloc.Lease(t.Context(), leaseID)

		return errors.Is(err, alloc.ErrLeaseNotFound)
	})

	// WITH GITHUB'S OUTCOME. The inventory path alone would have recorded the
	// provisional failure a killed teardown holder gets; the completion's retry
	// is what knows the job succeeded.
	waitUntil(t, "the history to carry the completion's outcome", func() bool {
		outcome, err := w.alloc.HistoryOutcome(t.Context(), leaseID)

		return err == nil && outcome == string(alloc.PhaseDone)
	})

	if phase, held := heldPhase(t, w.alloc, leaseID); held {
		t.Fatalf("the settled lease is still reported held as %s", phase)
	}
}

// THE OTHER WAY A HOLDER STOPS BEING REACHABLE: A SECOND PROCESS TAKES ITS
// NAME WHILE THE FIRST IS STILL RUNNING THE JOB.
//
// A superseded process drains — it keeps the lease of the build it launched
// renewed from custody and finishes what it holds — while its replacement,
// whose inventory can see the same build, adopts it into custody too. The
// completion arrives bound to the first process, which the plane's commands no
// longer reach, so the listener parks it without the lease. What has to hold
// throughout: the lease is NEVER quarantined while the build runs, because a
// live holder renews it; the lease settles as `done` through the holder's own
// tend once the build ends; the completion's retry then finds nothing to
// correct; the owner record ends with the lease; and the superseded process's
// drain ENDS on its own — which it can only do if the wire tells a superseded
// process that a lease it still holds custody of is over (the plane's record
// is gone by then, and "superseded" is a refusal a drain cannot act on).
func TestASupersededProcessDrainsTheJobItHoldsWhileTheCompletionWaits(t *testing.T) {
	w := newWiredCodeBuild(t, supersedableFirstNode)

	const requestID = 7002

	w.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", requestID, "push", testTier))

	stop := sync.OnceFunc(w.run(t))
	t.Cleanup(stop)

	waitUntil(t, "billet to bid for the available job", func() bool {
		return len(w.plane.acquiredIDs()) > 0
	})

	w.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", requestID, "push", testTier))

	names := w.awaitOneRunning(t)

	leaseID, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("build %q does not carry a billet lease name", names[0])
	}

	// A SECOND PROCESS REGISTERS UNDER THE SAME NAME. The first is not stopped:
	// it learns on its next poll that it was superseded and drains.
	replacement := w.restartNode(t)

	owner, ok := w.wire.OwnerOfLease(leaseID)
	if !ok || owner.Current || owner.Incarnation == replacement {
		t.Fatalf("owner = %+v (present=%v), want the superseded process recorded and not current", owner, ok)
	}

	// Both processes hold the build in custody now and report it. The ledger
	// says so, and names the host's current process as the holder.
	waitUntil(t, "the superseded process and its replacement to report custody", func() bool {
		lease, err := w.alloc.Lease(t.Context(), leaseID)

		return err == nil && lease.Phase == alloc.PhaseCustody
	})

	// GitHub reports the job complete. The plane binds the destroy to the
	// superseded process, which no longer polls, and the listener parks the
	// obligation without the lease.
	done := fakeactions.JobJSON("JobCompleted", requestID, "push", testTier)
	done["result"] = "Succeeded"
	w.plane.queue(fakeactions.StatisticsJSON(0, 0), done)
	awaitAck(t, w.stack, 3)

	// NEVER QUARANTINED WHILE THE BUILD RUNS. The listener has let go, the TTL
	// is three seconds, and the reaper ticks five times a second: if nothing
	// renewed the lease it would be quarantined well inside this window.
	deadline := time.Now().Add(3 * cbWireTTL)
	for time.Now().Before(deadline) {
		lease, err := w.alloc.Lease(t.Context(), leaseID)
		if err != nil {
			t.Fatalf("the lease of a build a live process holds was settled: %v", err)
		}

		if lease.Phase == alloc.PhaseQuarantine {
			t.Fatal("a lease held in custody by a draining process was quarantined; nothing renewed it")
		}

		time.Sleep(100 * time.Millisecond)
	}

	// The build ends. The holder's tend proves it terminal and releases the
	// lease with the outcome a completed job has.
	w.finishBuild(t, names[0])

	waitUntil(t, "the holder to settle the lease", func() bool {
		_, err := w.alloc.Lease(t.Context(), leaseID)

		return errors.Is(err, alloc.ErrLeaseNotFound)
	})

	if outcome, err := w.alloc.HistoryOutcome(t.Context(), leaseID); err != nil ||
		outcome != string(alloc.PhaseDone) {
		t.Fatalf("history outcome = %q err=%v, want done", outcome, err)
	}

	// THE COMPLETION'S RETRY FINDS THE LEASE OVER AND THE RECORD GOES WITH IT.
	waitUntil(t, "the completion's obligation to clear and the owner record to end", func() bool {
		_, present := w.wire.OwnerOfLease(leaseID)

		return !present
	})

	// AND THE SUPERSEDED PROCESS FINISHES DRAINING ON ITS OWN. Its custody entry
	// ends when the wire tells it the lease is gone — through the answer a
	// superseded process gets about a lease nothing holds any more.
	select {
	case <-w.firstNode.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the superseded process is still draining a lease the ledger has settled")
	}

	held, err := w.alloc.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("%d lease(s) still held after the build ended: %+v", len(held), held)
	}
}
