package nodeclient_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/server"
)

// A registered host in these tests is deliberately larger than any budget they
// set, so the deployment-wide ceiling stays the binding constraint.
const (
	testNodeVCPU   = 1 << 20
	testNodeMemory = 1 << 20 * config.GiB
)

const deployment = "0123456789abcdef0123456789abcdef"

// fakeCompute records what the loop asked of it.
type fakeCompute struct {
	mu sync.Mutex

	launchErr  error
	destroyErr error
	recoverErr error

	launched  []int64
	destroyed []int64
	recovered int
	swept     int

	// order records the sequence of calls, which is what proves Recover ran
	// before any work was taken.
	order []string

	keptAlive   int
	tendedCount int
	assumed     []int64

	// ttlProbe is read by KeepAlive at the moment it starts, which is how a test
	// observes whether the janitor could already see the negotiated TTL.
	ttlProbe func() time.Duration
	ttlSeen  time.Duration

	// aliveReturned counts janitors that have exited, which is how a test
	// observes that none outlived the loop.
	aliveReturned int

	// holding reports compute this node is still responsible for.
	holding bool

	// supersededCalls counts hand-overs of running work into custody.
	supersededCalls int

	// launchGate lets a test hold a launch open. Launch closes launchStarted and
	// then waits, which is the only way to act on the world WHILE a launch is in
	// flight — polling for the launch to finish always loses to the report that
	// follows it microseconds later.
	launchGate    chan struct{}
	launchStarted chan struct{}
}

func (f *fakeCompute) KeepAlive(ctx context.Context) {
	f.mu.Lock()
	f.keptAlive++

	if f.ttlProbe != nil {
		f.ttlSeen = f.ttlProbe()
	}

	f.mu.Unlock()

	<-ctx.Done()

	f.mu.Lock()
	f.aliveReturned++
	f.mu.Unlock()
}

// holding lets a test say this node is still responsible for compute, which is
// what keeps a superseded process draining rather than exiting.
func (f *fakeCompute) Holding() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.holding
}

// Superseded records that the loop converted running work into custody, which
// is what lets a drain ever finish.
func (f *fakeCompute) Superseded() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.supersededCalls++
}

func (f *fakeCompute) supersededCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.supersededCalls
}

func (f *fakeCompute) ttlAtStart() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.ttlSeen
}

func (f *fakeCompute) aliveReturnedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.aliveReturned
}

func (f *fakeCompute) Tend(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tendedCount++

	return nil
}

func (f *fakeCompute) AssumeCustody(_ context.Context, _ *alloc.Lease, requestID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.assumed = append(f.assumed, requestID)

	return nil
}

func (f *fakeCompute) custodyTaken() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int64(nil), f.assumed...)
}

func (f *fakeCompute) tended() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.tendedCount
}

func (f *fakeCompute) aliveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.keptAlive
}

func (f *fakeCompute) Launch(_ context.Context, _ *alloc.Lease, job server.Job) error {
	f.mu.Lock()
	f.launched = append(f.launched, job.RequestID)
	f.order = append(f.order, "launch")
	started, gate, err := f.launchStarted, f.launchGate, f.launchErr
	f.mu.Unlock()

	if started != nil {
		close(started)
	}

	if gate != nil {
		<-gate
	}

	return err
}

func (f *fakeCompute) Destroy(_ context.Context, requestID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.destroyed = append(f.destroyed, requestID)
	f.order = append(f.order, "destroy")

	return f.destroyErr
}

func (f *fakeCompute) Recover(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recovered++
	f.order = append(f.order, "recover")

	return f.recoverErr
}

func (f *fakeCompute) Sweep(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.swept++

	return nil
}

func (f *fakeCompute) snapshot() ([]string, []int64, []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.order...),
		append([]int64(nil), f.launched...),
		append([]int64(nil), f.destroyed...)
}

type stubJIT struct{}

func (stubJIT) Describe(context.Context, string, string) (*nodeplane.JITSet, []string, error) {
	return nil, nil, nil
}

func (stubJIT) JITConfig(context.Context, int, string, string) (nodeplane.JITRegistration, error) {
	return nil, errors.New("no github in this test")
}

type stubStore struct{}

func (stubStore) Bind(context.Context, string, int64, string) error         { return nil }
func (stubStore) Advance(context.Context, string, int64, alloc.Phase) error { return nil }
func (stubStore) Heartbeat(context.Context, string, int64) error            { return nil }
func (stubStore) Release(context.Context, string, int64, alloc.Phase) error { return nil }
func (stubStore) Lease(context.Context, string) (*alloc.Lease, error)       { return nil, nil }
func (stubStore) LaunchedLeaseIDs(context.Context, string) (map[string]bool, error) {
	return nil, nil
}

func harness(t *testing.T) (*nodeplane.Plane, *nodeclient.Client) {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithCommandTimeout(5*time.Second))
	p.SetPollWindowForTest(60 * time.Millisecond)

	srv := httptest.NewServer(nodeplane.Handler(log, p, stubStore{}, stubJIT{}))
	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	return p, c
}

func runLoop(t *testing.T, c *nodeclient.Client, compute nodeclient.Compute) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		// Run only ends when the context does, so its error is always the
		// cancellation. Asserting anything else would assert the test's own
		// teardown.
		err := nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			GuestOS:    []config.GuestOS{config.GuestLinux},
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	}()

	// JOINED, so the loop cannot outlive its test and log into a finished one.
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// RECOVERY RUNS BEFORE ANY WORK IS TAKEN.
//
// The ordering is the point, not the fact that Recover is called. A node that
// starts launching while its previous incarnation's containers are unaccounted
// for double-counts the host: the ledger believes that capacity is free and it
// is not.
func TestRecoveryPrecedesTheFirstLaunch(t *testing.T) {
	t.Parallel()

	p, c := harness(t)
	compute := &fakeCompute{}

	runLoop(t, c, compute)

	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	lease := &alloc.Lease{
		ID:        "l1",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}

	if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	order, launched, _ := compute.snapshot()

	if len(launched) != 1 || launched[0] != 7 {
		t.Fatalf("the launch did not reach the compute: %v", launched)
	}

	if len(order) < 2 || order[0] != "recover" {
		t.Fatalf("recovery did not come first: %v", order)
	}
}

// A destroy reaches the compute and reports success.
func TestADestroyReachesTheCompute(t *testing.T) {
	t.Parallel()

	p, c := harness(t)
	compute := &fakeCompute{}

	runLoop(t, c, compute)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	if err := p.NewRunner().Destroy(t.Context(), 42); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	_, _, destroyed := compute.snapshot()

	if len(destroyed) != 1 || destroyed[0] != 42 {
		t.Fatalf("the destroy did not reach the compute: %v", destroyed)
	}
}

// CUSTODY SURVIVES THE ROUND TRIP AS A FLAG.
//
// The node's runner says "I may have started something" with server.ErrCustody;
// the server decides whether to release capacity on that basis. If it arrived as
// prose the server would release a lease whose container is still running.
func TestCustodyCrossesBackAsAFlag(t *testing.T) {
	t.Parallel()

	p, c := harness(t)
	compute := &fakeCompute{
		launchErr: errors.New("create ambiguous: " + server.ErrCustody.Error()),
	}

	// Wrapped so errors.Is finds it, exactly as the real runner does.
	compute.launchErr = errors.Join(server.ErrCustody, errors.New("create ambiguous"))

	runLoop(t, c, compute)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	lease := &alloc.Lease{
		ID:        "l1",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}

	err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	if err == nil {
		t.Fatal("an ambiguous launch reported success")
	}

	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("custody did not survive the wire: %v", err)
	}
}

// A clean failure does NOT become custody, or every failed launch would hold
// capacity forever.
func TestACleanFailureIsNotCustody(t *testing.T) {
	t.Parallel()

	p, c := harness(t)
	compute := &fakeCompute{launchErr: errors.New("image not found")}

	runLoop(t, c, compute)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	lease := &alloc.Lease{
		ID:        "l1",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}

	err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	if err == nil {
		t.Fatal("a failed launch reported success")
	}

	if errors.Is(err, server.ErrCustody) {
		t.Errorf("a clean failure was reported as custody, which holds capacity for compute "+
			"that never started: %v", err)
	}
}

// A NODE THAT CANNOT RECONCILE DOES NOT TAKE WORK.
//
// Recover failing means the node does not know what is already running on it.
// Serving commands anyway is how a host ends up double-counted.
func TestANodeThatCannotRecoverTakesNoWork(t *testing.T) {
	t.Parallel()

	p, c := harness(t)
	compute := &fakeCompute{recoverErr: errors.New("docker is not answering")}

	runLoop(t, c, compute)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	// It registered, but it must never poll — so a command sits undelivered and
	// the caller is told nothing started.
	lease := &alloc.Lease{
		ID:        "l1",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()

	err := p.NewRunner().Launch(ctx, lease, server.Job{RequestID: 7})
	if err == nil {
		t.Fatal("a node that cannot reconcile accepted work")
	}

	if errors.Is(err, server.ErrCustody) {
		t.Errorf("nothing was ever delivered, so this must not be custody: %v", err)
	}

	_, launched, _ := compute.snapshot()
	if len(launched) != 0 {
		t.Errorf("the compute was asked to launch %v despite failing to reconcile", launched)
	}
}

// An unknown command is refused rather than ignored.
//
// Ignoring it would leave the caller waiting for a result that never comes, and
// its launch would eventually be assumed to be in custody — holding capacity for
// compute that was never started.
func TestAnUnknownCommandIsRefused(t *testing.T) {
	t.Parallel()

	res := nodeclient.ExecuteForTest(t.Context(), &fakeCompute{}, nodeapi.Command{
		ID:   "c1",
		Kind: nodeapi.CommandKind("teleport"),
	})

	if res.OK {
		t.Fatal("an unknown command reported success")
	}

	if res.Error == "" {
		t.Error("an unknown command was refused with no reason")
	}

	if res.Custody {
		t.Error("an unknown command claimed custody; nothing was started")
	}
}

// A launch with no lease cannot start anything and says so.
func TestALaunchWithoutALeaseIsRefused(t *testing.T) {
	t.Parallel()

	res := nodeclient.ExecuteForTest(t.Context(), &fakeCompute{}, nodeapi.Command{
		ID:   "c1",
		Kind: nodeapi.CommandLaunch,
	})

	if res.OK {
		t.Fatal("a launch with no lease reported success")
	}
}

// THE CUSTODY JANITOR RUNS, and without it the whole custody design is broken
// across the split.
//
// In one process the runner renews the leases of compute it could not confirm
// gone. A node that never starts that janitor answers the server with "I am
// holding this" while holding nothing: the server stops heartbeating, the reaper
// releases the capacity a TTL later, and a container keeps running on capacity
// that has been sold to somebody else.
func TestTheCustodyJanitorIsStarted(t *testing.T) {
	t.Parallel()

	_, c := harness(t)
	compute := &fakeCompute{ttlProbe: c.LeaseTTL}

	runLoop(t, c, compute)

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	// STARTED AFTER THE FIRST REGISTRATION, not before it. The janitor picks its
	// renewal cadence from the TTL, and that value arrives with the registration
	// response — starting first meant reading a zero and renewing on a fallback
	// clock for the process's whole life, while racing the write that would have
	// told it the truth.
	if compute.ttlAtStart() <= 0 {
		t.Error("the janitor started before it could know the lease TTL, so it renews on a " +
			"cadence nobody chose")
	}
}

// THE JANITOR STOPS WITH THE LOOP, and stopping is the whole point.
//
// It was started on the loop's own context, which outlives Run: a node that
// exits because it was refused, or a `server --dev` that shuts one node down,
// would leave a goroutine heartbeating leases for a runtime nobody is driving
// any more. Nothing crashes, so nothing draws attention.
func TestTheCustodyJanitorDoesNotOutliveTheLoop(t *testing.T) {
	t.Parallel()

	_, c := harness(t)
	compute := &fakeCompute{}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		defer close(done)

		err := nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not return after its context was cancelled")
	}

	// Read AFTER Run returned, deliberately. Run waits for the janitor before
	// returning, so a poll here would let a janitor that merely exits soon look
	// identical to one the loop actually waited for.
	if compute.aliveReturnedCount() != 1 {
		t.Error("the loop returned while its custody janitor was still running, so the " +
			"goroutine outlives the node it belongs to")
	}
}

// A LATE RESULT MAKES THE NODE ASSUME CUSTODY TOO, and this is the path the
// first version missed.
//
// The other test forces the report itself to FAIL, which is one of the two ways
// a launch and its lease can come apart. The other is worse because everything
// appears to work: the launch takes longer than the command timeout, the plane
// abandons it and tells the listener the node has custody, the listener stops
// heartbeating — and THEN the provider returns successfully and the node reports
// it. The report arrives, the plane says 204, and the node files the instance in
// its ordinary running set. Nothing renews that lease. The reaper releases its
// capacity while the container runs, and the same capacity is sold twice.
//
// So a report for a launch the plane has already abandoned is answered with
// custody rather than with a shrug.
func TestALateResultMakesTheNodeAssumeCustody(t *testing.T) {
	t.Parallel()

	p, c := harness(t)

	gate := make(chan struct{})
	compute := &fakeCompute{
		launchGate:    gate,
		launchStarted: make(chan struct{}),
	}

	runLoop(t, c, compute)

	// Registered first, or the launch below finds no node and fails instantly for
	// a reason that has nothing to do with what this test is about.
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	// The launch is dispatched, and the fake holds it open past the plane's
	// command timeout.
	launched := make(chan error, 1)

	go func() {
		lease := &alloc.Lease{
			ID:        "l1",
			VCPU:      2,
			Memory:    8 * config.GiB,
			GuestOS:   config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderDocker},
			Epoch:     1,
		}

		launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	select {
	case <-compute.launchStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the node never started the launch")
	}

	// The plane stops waiting and hands the listener custody.
	select {
	case err := <-launched:
		if !errors.Is(err, server.ErrCustody) {
			t.Fatalf("an abandoned in-flight launch must report custody, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the launch never returned")
	}

	// Only now does the provider succeed, so the report is late rather than lost.
	close(gate)

	waitFor(t, func() bool { return len(compute.custodyTaken()) == 1 })

	if got := compute.custodyTaken(); len(got) != 1 || got[0] != 7 {
		t.Errorf("custody taken for %v, want the request the plane abandoned; without this "+
			"the container runs under a lease nothing renews", got)
	}
}

// A RE-REGISTRATION HANDS OVER CUSTODY TOO, and it left no record that it had.
//
// The timeout path tombstones an abandoned launch so a late report is answered
// with "this is yours". Re-registration does the same handover — every in-flight
// command is failed with custody, and the listener stops heartbeating on the
// strength of it — and recorded nothing. A launch still running across that
// moment, on a partitioned host or one whose provider outlived its billet
// process, would report success afterwards, find neither an inflight entry nor a
// tombstone, and be answered 204. Nothing held the lease at all.
func TestARegistrationHandsOverCustodyOfWhatWasInFlight(t *testing.T) {
	t.Parallel()

	p, c := harness(t)

	gate := make(chan struct{})
	compute := &fakeCompute{
		launchGate:    gate,
		launchStarted: make(chan struct{}),
	}

	runLoop(t, c, compute)

	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	launched := make(chan error, 1)

	go func() {
		lease := &alloc.Lease{
			ID:        "l1",
			VCPU:      2,
			Memory:    8 * config.GiB,
			GuestOS:   config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderDocker},
			Epoch:     1,
		}

		launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	select {
	case <-compute.launchStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the node never started the launch")
	}

	// The node re-registers while its launch is still running, which is what a
	// reconnect after a partition looks like from the plane's side.
	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	select {
	case err := <-launched:
		if !errors.Is(err, server.ErrCustody) {
			t.Fatalf("a launch in flight across a re-registration must report custody, got %v",
				err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the launch never returned")
	}

	// Only now does the provider finish, so the report arrives after the handover.
	close(gate)

	waitFor(t, func() bool { return len(compute.custodyTaken()) == 1 })

	if got := compute.custodyTaken(); len(got) != 1 || got[0] != 7 {
		t.Errorf("custody taken for %v, want the request the plane handed over; without it "+
			"the container runs under a lease nothing renews", got)
	}
}

// A LOST RESULT MAKES THE NODE TAKE CUSTODY, because the server already has.
//
// The failure this prevents: the launch succeeds, the report is lost, the plane
// times the command out and reports custody to the listener — which stops
// heartbeating, since custody means the node holds it. The node meanwhile
// believes it merely launched something and holds the instance in its ordinary
// running set, which nothing renews. The lease is reaped while the container
// runs and its capacity is sold twice.
//
// The handoff has to be CAUSED rather than hoped for, so the party that could
// not report is the party that takes custody.
func TestALostResultMakesTheNodeAssumeCustody(t *testing.T) {
	t.Parallel()

	p, c := harness(t)
	compute := &fakeCompute{}

	runLoop(t, c, compute)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	lease := &alloc.Lease{
		ID:        "l1",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
		RequestID: 7,
	}

	// The launch is delivered and executed; the REPORT is what fails. Forgetting
	// the node makes its result rejected with "register again", which is exactly
	// the state a restarted control plane produces.
	//
	// Held open while that happens, because a report follows its launch by
	// microseconds and any attempt to interleave by polling loses the race.
	compute.mu.Lock()
	compute.launchStarted = make(chan struct{})
	compute.launchGate = make(chan struct{})
	started, gate := compute.launchStarted, compute.launchGate
	compute.mu.Unlock()

	go func() {
		<-started
		p.ForgetForTest("n1")
		close(gate)
	}()

	// The launch's own outcome is not what is under test — the plane reports
	// custody, correctly — so what matters is what the NODE did about it.
	if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}); err == nil {
		t.Fatal("a launch whose result was lost reported success")
	}

	waitFor(t, func() bool {
		for _, id := range compute.custodyTaken() {
			if id == 7 {
				return true
			}
		}

		return false
	})
}

// CUSTODY IS ADVANCED, not merely held.
//
// The janitor renews the leases of compute that could not be confirmed gone;
// Tend is what ENDS those entries once it is. Without it a node holds every
// custody lease forever and the capacity behind them is never returned — the
// mirror of the failure that starting the janitor prevents.
func TestCustodyIsAdvancedOnTheSweepCadence(t *testing.T) {
	t.Parallel()

	_, c := harness(t)
	compute := &fakeCompute{}

	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		err := nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	waitFor(t, func() bool { return compute.tended() > 0 })
}

// A SUPERSEDED NODE DRAINS WHAT IT HOLDS BEFORE IT STOPS.
//
// The server keeps the heartbeat and result routes open for a superseded process
// precisely so a launch it began can finish — and that permission is worthless if
// the process exits the moment its poll is refused, which is what the first
// version did. Exiting cancels the janitor, and the replacement cannot adopt what
// it cannot see, because the container is on THIS machine. The lease is then
// renewed by nobody and its capacity is resold under a running job.
func TestASupersededNodeDrainsBeforeStopping(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	compute := &fakeCompute{holding: true}

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(t.Context(), c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
		})
	}()

	// A second process takes the name.
	other, err := nodeclient.New(nodeclient.Options{Base: c.BaseForTest(), Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	if err := other.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("the second process could not register: %v", err)
	}

	// FIRST IT TAKES CUSTODY OF WHAT IT IS RUNNING, because after supersession the
	// control plane routes those completions to the replacement — which cannot see
	// them, reports the destroy as done, and lets the lease go. Tend only ever
	// looks at custody, so running work left where it was would keep this process
	// holding forever and never finish.
	waitFor(t, func() bool { return compute.supersededCount() > 0 })

	// THEN IT KEEPS TENDING while it is holding something. Tend is what releases a
	// lease once its compute is confirmed gone, so this is the work that lets the
	// hand-over ever complete.
	waitFor(t, func() bool { return compute.tended() > 0 })

	select {
	case <-done:
		t.Fatal("a superseded node stopped while it was still holding compute; nothing else " +
			"can renew those leases, because the containers are on this machine")
	case <-time.After(300 * time.Millisecond):
	}

	// Once it is holding nothing, there is nothing left that depends on it.
	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case err := <-done:
		if !errors.Is(err, nodeclient.ErrSuperseded) {
			t.Errorf("want ErrSuperseded once drained, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a drained node never stopped")
	}
}

// A NODE THAT CANNOT JOIN STOPS, rather than retrying forever.
//
// A protocol mismatch or a foreign deployment identity is refused identically
// every time. Retrying it every few seconds produces a process that looks alive,
// never works, and crashes nothing that would draw attention — the failure mode
// nobody notices. Stopping makes it visible: a supervisor restarting the node
// meets the same wall and says so again.
func TestANodeThatIsRefusedStops(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute)
	srv := httptest.NewServer(nodeplane.Handler(log, p, stubStore{}, stubJIT{}))

	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	done := make(chan error, 1)

	go func() {
		// A deployment identity this control plane will never accept.
		done <- nodeclient.Run(t.Context(), c, &fakeCompute{}, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: "ffffffffffffffffffffffffffffffff",
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a refused node reported a clean exit")
		}

		if !errors.Is(err, nodeclient.ErrRefused) {
			t.Errorf("the node stopped for the wrong reason: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a node the control plane will never accept is still retrying")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("condition never became true")
}
