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
	f.mu.Unlock()

	<-ctx.Done()
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
	compute := &fakeCompute{}

	runLoop(t, c, compute)

	waitFor(t, func() bool { return compute.aliveCount() == 1 })
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
