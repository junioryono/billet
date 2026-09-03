package nodeplane_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	billetnode "github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/wirecert"
)

// A registered host in these tests is deliberately larger than any budget they
// set, so the deployment-wide ceiling stays the binding constraint.
const (
	testNodeVCPU   = 1 << 20
	testNodeMemory = 1 << 20 * config.GiB
)

const deployment = "0123456789abcdef0123456789abcdef"

// fakeRegistrar stands in for the allocator's node table.
type fakeRegistrar struct {
	// reconciled and reportedRunning record the quarantine reconciliation the
	// plane performs when a host comes back.
	reconciled      []string
	reportedRunning []string
	freed           int
	resolveErr      error
	lease           *alloc.Lease
	leaseErr        error

	mu       sync.Mutex
	err      error
	accepted []string
	// last is the whole registration, so a wire test can prove the fields
	// actually arrived rather than only that a name did.
	last alloc.NodeRegistration

	epoch     int64
	goneName  string
	goneEpoch int64
	forgotten bool
	// dispatched counts the launches that have taken the barrier fence.
	dispatched int
	// withdrawn records every withdrawal the plane recorded, with the fence it
	// presented; withdrawErr makes the ledger refuse to.
	withdrawn   []withdrawal
	withdrawErr error
}

// withdrawal is one host's withdrawal as the ledger was told it.
type withdrawal struct {
	name        string
	epoch       int64
	incarnation string
}

// NodeWithdrawn records the fence a withdrawal presented, so a wire test can
// prove the plane handed the ledger the registration it was talking to.
func (f *fakeRegistrar) NodeWithdrawn(
	_ context.Context, name string, epoch int64, incarnation string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.withdrawErr != nil {
		return f.withdrawErr
	}

	f.withdrawn = append(f.withdrawn, withdrawal{name: name, epoch: epoch, incarnation: incarnation})

	return nil
}

// withdrawals is what the ledger was told.
func (f *fakeRegistrar) withdrawals() []withdrawal {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]withdrawal(nil), f.withdrawn...)
}

func (f *fakeRegistrar) SettleCompletionOnTerminalLease(
	_ context.Context, leaseID string, _ int64, _ alloc.Phase,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseErr != nil {
		return false, f.leaseErr
	}
	if f.lease == nil || f.lease.ID != leaseID {
		return true, nil
	}

	return false, nil
}

// ResolveQuarantineFor records what a returning host reported running, so a test
// can assert the plane passed it on.
// reconciliation reports whether the plane asked, and with what.
func (f *fakeRegistrar) reconciliation() (bool, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.reconciled) > 0, f.reportedRunning
}

func (f *fakeRegistrar) ResolveQuarantineFor(
	_ context.Context, node string, running []string, _ int64,
) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reconciled = append(f.reconciled, node)
	f.reportedRunning = running

	return f.freed, f.resolveErr
}

// gone records which node the plane gave up on, and at which epoch.
func (f *fakeRegistrar) NodeGone(_ context.Context, name string, epoch int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.goneName, f.goneEpoch = name, epoch

	return nil
}

// ForgetEveryNode is what a restarted control plane calls before it trusts
// anything.
func (f *fakeRegistrar) ForgetEveryNode(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forgotten = true

	return nil
}

// BumpDispatch advances the fence a compute barrier is taken against. Counted
// rather than ignored, so a test can assert a launch charged it.
func (f *fakeRegistrar) BumpDispatch(_ context.Context, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.dispatched++

	return int64(f.dispatched), nil
}

// lastRegistration is what the plane most recently handed the ledger.
func (f *fakeRegistrar) lastRegistration() alloc.NodeRegistration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.last
}

func (f *fakeRegistrar) RegisterNode(_ context.Context, reg alloc.NodeRegistration) (int64, error) {
	name := reg.Name

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return 0, f.err
	}

	f.accepted = append(f.accepted, name)
	f.last = reg
	f.epoch++

	return f.epoch, nil
}

func (f *fakeRegistrar) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.accepted...)
}

// fakeStore is a ledger that answers whatever a test needs.
//
// The point of these tests is the WIRE, not the allocator: what has to be proved
// is that an error raised on one side arrives on the other as the same kind of
// error, and that requires being able to raise each kind on demand.
type fakeStore struct {
	mu sync.Mutex

	bindErr       error
	advanceErr    error
	heartbeatErr  error
	failureErr    error
	resizeErr     error
	releaseErr    error
	leaseErr      error
	deregisterErr error

	lease       *alloc.Lease
	launched    map[string]bool
	quarantined map[string]bool
	// historyNode is the host each ended lease's job was attributed to.
	historyNode map[string]string
	// launchedEntered/proceed stage registration while it is reading durable
	// ownership, after its intent must already have invalidated old inventory.
	launchedEntered chan struct{}
	launchedProceed <-chan struct{}

	bound        []string
	advanced     []alloc.Phase
	released     []alloc.Phase
	deregistered []string
	failures     []string
	pool         map[string]alloc.PoolRunner
	retired      []string
}

type cachePolicyFunc func(context.Context, string, string) (bool, error)

func (f cachePolicyFunc) ActionsCacheAllowed(
	ctx context.Context,
	owner, repository string,
) (bool, error) {
	return f(ctx, owner, repository)
}

func (f *fakeStore) Bind(_ context.Context, leaseID string, _ int64, node string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.bound = append(f.bound, node+"/"+leaseID)

	return f.bindErr
}

func (f *fakeStore) Advance(_ context.Context, _ string, _ int64, to alloc.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.advanced = append(f.advanced, to)

	return f.advanceErr
}

func (f *fakeStore) Heartbeat(context.Context, string, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.heartbeatErr
}

func (f *fakeStore) MarkFailure(_ context.Context, _ string, _ int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failures = append(f.failures, reason)

	return f.failureErr
}

func (f *fakeStore) Resize(context.Context, string, int64, string, int, config.ByteSize) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.resizeErr
}

func (f *fakeStore) MarkDeregistered(_ context.Context, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deregistered = append(f.deregistered, leaseID)

	return f.deregisterErr
}

func (f *fakeStore) Release(_ context.Context, _ string, _ int64, outcome alloc.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.released = append(f.released, outcome)

	return f.releaseErr
}

func (f *fakeStore) Lease(context.Context, string) (*alloc.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lease, f.leaseErr
}

func (f *fakeStore) RegisterPoolRunner(_ context.Context, runner alloc.PoolRunner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if runner.Status == "" {
		runner.Status = alloc.PoolRunnerIdle
	}
	if f.pool == nil {
		f.pool = make(map[string]alloc.PoolRunner)
	}
	if prior, ok := f.pool[runner.RunnerName]; ok {
		if prior.LeaseID != runner.LeaseID || prior.Tier != runner.Tier ||
			prior.LaunchRequestID != runner.LaunchRequestID {
			return alloc.ErrConflict
		}
		return nil
	}
	f.pool[runner.RunnerName] = runner
	return nil
}

func (f *fakeStore) PoolRunnerByName(_ context.Context, name string) (alloc.PoolRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runner, ok := f.pool[name]
	if !ok {
		return alloc.PoolRunner{}, alloc.ErrLeaseNotFound
	}
	return runner, nil
}

func (f *fakeStore) PoolRunnerByLease(_ context.Context, leaseID string) (alloc.PoolRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, runner := range f.pool {
		if runner.LeaseID == leaseID {
			return runner, nil
		}
	}
	return alloc.PoolRunner{}, alloc.ErrLeaseNotFound
}

func (f *fakeStore) RetirePoolRunner(_ context.Context, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, runner := range f.pool {
		if runner.LeaseID != leaseID {
			continue
		}
		runner.Status = alloc.PoolRunnerRetiring
		f.pool[name] = runner
		f.retired = append(f.retired, leaseID)
		return nil
	}
	return alloc.ErrLeaseNotFound
}

func (f *fakeStore) PreserveRecoveredBusyPoolRunner(
	_ context.Context, recovered alloc.PoolRunner,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pool == nil {
		f.pool = make(map[string]alloc.PoolRunner)
	}
	for name, runner := range f.pool {
		if runner.LeaseID != recovered.LeaseID {
			continue
		}
		runner.RunnerID = recovered.RunnerID
		runner.RunnerName = recovered.RunnerName
		runner.Status = alloc.PoolRunnerBusy
		delete(f.pool, name)
		f.pool[runner.RunnerName] = runner
		return nil
	}
	recovered.Status = alloc.PoolRunnerBusy
	f.pool[recovered.RunnerName] = recovered
	return nil
}

func (f *fakeStore) RetireRecoveredPoolRunner(
	_ context.Context, recovered alloc.PoolRunner,
) (alloc.PoolRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, runner := range f.pool {
		if runner.LeaseID != recovered.LeaseID {
			continue
		}
		if runner.Status == alloc.PoolRunnerBusy &&
			(runner.ActualRequestID != 0 || runner.RunID != 0 || runner.JobID != "") {
			return runner, nil
		}
		runner.Status = alloc.PoolRunnerRetiring
		f.pool[name] = runner
		f.retired = append(f.retired, recovered.LeaseID)
		return runner, nil
	}
	if f.pool == nil {
		f.pool = make(map[string]alloc.PoolRunner)
	}
	recovered.Status = alloc.PoolRunnerRetiring
	f.pool[recovered.RunnerName] = recovered
	f.retired = append(f.retired, recovered.LeaseID)
	return recovered, nil
}

type returnedJIT struct {
	removed    []string
	removeErr  error
	onRemove   func()
	recovery   nodeplane.JITRunnerRecovery
	recoverErr error
	recovered  []string
	onRecover  func()
}

func (*returnedJIT) Describe(context.Context, string, string) (*nodeplane.JITSet, []string, error) {
	return &nodeplane.JITSet{ID: 7, Name: "billet-2vcpu"}, nil, nil
}

func (*returnedJIT) JITConfig(context.Context, int, string, string) (nodeplane.JITRegistration, error) {
	return returnedRegistration{}, nil
}

func (j *returnedJIT) RemoveRunner(_ context.Context, _ int64, name string) error {
	j.removed = append(j.removed, name)
	if j.onRemove != nil {
		j.onRemove()
	}
	return j.removeErr
}

func (j *returnedJIT) RecoverRunner(
	_ context.Context, name string,
) (nodeplane.JITRunnerRecovery, error) {
	j.recovered = append(j.recovered, name)
	if j.onRecover != nil {
		j.onRecover()
	}
	return j.recovery, j.recoverErr
}

type returnedRegistration struct{}

func (returnedRegistration) Config() string     { return "single-use-credential" }
func (returnedRegistration) RunnerName() string { return "github-returned-name" }
func (returnedRegistration) ID() int64          { return 91 }

// QuarantinedLeaseIDs are the leases this fake says are holding capacity for
// compute nobody has accounted for.
func (f *fakeStore) EndedLeaseNode(_ context.Context, leaseID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	node, ok := f.historyNode[leaseID]
	if !ok {
		return "", alloc.ErrLeaseNotFound
	}

	return node, nil
}

func (f *fakeStore) QuarantinedLeaseIDs(context.Context, string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.quarantined, nil
}

func (f *fakeStore) LaunchedLeaseIDs(ctx context.Context, _ string) (map[string]bool, error) {
	f.mu.Lock()
	entered, proceed, launched := f.launchedEntered, f.launchedProceed, f.launched
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if proceed != nil {
		select {
		case <-proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return launched, nil
}

func serve(t *testing.T, store nodeplane.LeaseStore, opts ...nodeplane.Option) (*nodeplane.Plane, string) {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	opts = append([]nodeplane.Option{nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}})}, opts...)
	p := nodeplane.New(log, deployment, time.Minute, opts...)
	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil))

	t.Cleanup(srv.Close)

	return p, srv.URL
}

func dial(tb testing.TB, base string) *nodeclient.Client {
	tb.Helper()

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		tb.Fatalf("new client: %v", err)
	}

	if err := c.Register(tb.Context(), nodeclient.Registration{Provider: config.ProviderDocker,
		GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		tb.Fatalf("register: %v", err)
	}

	return c
}

func TestTheActionsCacheKillSwitchCrossesTheAuthenticatedNodeWire(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	var gotOwner, gotRepository string
	policy := cachePolicyFunc(func(_ context.Context, owner, repository string) (bool, error) {
		gotOwner, gotRepository = owner, repository
		return false, nil
	})
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil,
		nodeplane.WithCachePolicy(policy)))
	t.Cleanup(srv.Close)
	c := dial(t, srv.URL)

	allowed, err := c.ActionsCacheAllowed(t.Context(), "Acme", "API")
	if err != nil {
		t.Fatalf("ActionsCacheAllowed: %v", err)
	}
	if allowed {
		t.Fatal("a centrally blocked repository was allowed")
	}
	if gotOwner != "Acme" || gotRepository != "API" {
		t.Fatalf("policy scope = %q/%q, want Acme/API", gotOwner, gotRepository)
	}
}

// A COMMAND MAKES THE ROUND TRIP AND ITS RESULT COMES BACK.
//
// The whole wire in one test: the server queues a launch, the node's poll picks
// it up over HTTP, the node reports, and the caller of Launch sees the verdict.
// Everything else here tests an edge of this.
func TestACommandAndItsResultCrossTheWire(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{}, nodeplane.WithCommandTimeout(60*time.Second))
	c := dial(t, base)

	reported := make(chan error, 1)

	go func() {
		cmd, ok, err := c.Poll(t.Context())
		if err != nil || !ok {
			reported <- err

			return
		}

		reported <- c.Report(t.Context(), nodeapi.CommandResult{ID: cmd.ID, OK: true})
	}()

	lease := &alloc.Lease{
		ID:        "l1",
		Tier:      "billet-2vcpu",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}

	if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}); err != nil {
		t.Fatalf("Launch across the wire: %v", err)
	}

	// DRAINED, so the node's side is not still deciding when the test ends. An
	// unjoined goroutine calling t.Errorf after its test finishes panics the whole
	// run, which is exactly what this file did on its first green build.
	if err := <-reported; err != nil {
		t.Errorf("the node could not report: %v", err)
	}
}

func TestJITPersistsGitHubsReturnedIdentityBeforeGivingItToTheNode(t *testing.T) {
	store := &fakeStore{lease: &alloc.Lease{
		ID: "l1", Tier: "billet-2vcpu", Node: "n1", Epoch: 1, RequestID: 7,
	}}
	jit := &returnedJIT{}
	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}), nodeplane.WithCommandTimeout(time.Minute))
	srv := httptest.NewServer(nodeplane.Handler(log, p, store, jit))
	t.Cleanup(srv.Close)
	c := dial(t, srv.URL)

	lease := &alloc.Lease{ID: "l1", Tier: "billet-2vcpu", Node: "n1", Epoch: 1,
		RequestID: 7, VCPU: 2, Memory: 8 * config.GiB, GuestOS: config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker}}
	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.QueuedForTest("n1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("launch was never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok, err := c.Poll(t.Context()); err != nil || !ok {
		t.Fatalf("Poll = ok %v, err %v", ok, err)
	}
	reg, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work")
	if err != nil {
		t.Fatalf("JITConfig: %v", err)
	}
	if reg.RunnerName() != "github-returned-name" || reg.ID() != 91 {
		t.Fatalf("returned registration = name %q id %d", reg.RunnerName(), reg.ID())
	}
	member, err := store.PoolRunnerByName(t.Context(), "github-returned-name")
	if err != nil || member.LeaseID != "l1" || member.LaunchRequestID != 7 ||
		member.RunnerID != 91 || member.RunnerName != "github-returned-name" {
		t.Fatalf("durable GitHub identity = %+v, err %v", member, err)
	}
	store.lease.Phase = alloc.PhaseQuarantine
	jit.recovery = nodeplane.JITRunnerRecovery{RunnerID: 91, Present: true, Busy: true}
	recovery, err := c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryBusy ||
		!slices.Equal(jit.recovered, []string{"github-returned-name"}) {
		t.Fatalf("idle-to-busy recovery = %q, calls %v, err %v", recovery, jit.recovered, err)
	}
	member, err = store.PoolRunnerByLease(t.Context(), "l1")
	if err != nil || member.Status != alloc.PoolRunnerBusy || member.RunnerID != 91 {
		t.Fatalf("busy recovery did not preserve exact durable identity: %+v, err %v", member, err)
	}
	store.lease.Phase = ""
	jit.recovery = nodeplane.JITRunnerRecovery{}
	jit.removeErr = errors.New("github unavailable")
	if err := c.RemoveRunner(t.Context(), "l1", 91, "github-returned-name"); err == nil {
		t.Fatal("RemoveRunner succeeded while GitHub was unavailable")
	}
	member, err = store.PoolRunnerByName(t.Context(), "github-returned-name")
	if err != nil || member.Status != alloc.PoolRunnerRetiring ||
		!slices.Equal(store.retired, []string{"l1"}) {
		t.Fatalf("failed removal journal = %+v retired %v, err %v", member, store.retired, err)
	}
	jit.removeErr = nil
	if err := c.EnsureRunnerRemoved(t.Context(), "l1"); err != nil {
		t.Fatalf("EnsureRunnerRemoved after restart: %v", err)
	}
	if !slices.Equal(jit.removed, []string{"github-returned-name", "github-returned-name"}) {
		t.Fatalf("removed registrations = %v", jit.removed)
	}
	// The failed first removal must not have marked; the successful
	// EnsureRunnerRemoved must, so the lease stops counting as a live runner.
	if !slices.Equal(store.deregistered, []string{"l1"}) {
		t.Fatalf("deregistration marks = %v; want exactly [l1] (marked on success, not on "+
			"the earlier failure)", store.deregistered)
	}
	if _, err := c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1"); err == nil {
		t.Fatal("runner recovery was allowed outside quarantine")
	}
	store.lease.Phase = alloc.PhaseQuarantine
	recovery, err = c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryRetired || len(jit.recovered) != 1 ||
		!slices.Equal(jit.removed, []string{"github-returned-name", "github-returned-name", "github-returned-name"}) {
		t.Fatalf("durable recovery = %q, calls %v, err %v", recovery, jit.recovered, err)
	}
	store.mu.Lock()
	store.pool = nil
	store.mu.Unlock()
	jit.recovery = nodeplane.JITRunnerRecovery{RunnerID: 92, Present: true, Busy: true}
	recovery, err = c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryBusy {
		t.Fatalf("busy legacy recovery = %q, err %v", recovery, err)
	}
	member, err = store.PoolRunnerByLease(t.Context(), "l1")
	if err != nil || member.Status != alloc.PoolRunnerBusy || member.RunnerID != 92 ||
		member.RunnerName != "billet-l1" {
		t.Fatalf("legacy busy recovery identity = %+v, err %v", member, err)
	}
	jit.recovery = nodeplane.JITRunnerRecovery{}
	recovery, err = c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryRetired {
		t.Fatalf("idle legacy recovery = %q, err %v", recovery, err)
	}
	member, err = store.PoolRunnerByLease(t.Context(), "l1")
	if err != nil || member.Status != alloc.PoolRunnerRetiring {
		t.Fatalf("idle legacy recovery did not preserve retirement journal: %+v, err %v", member, err)
	}
	store.mu.Lock()
	store.pool = nil
	store.mu.Unlock()
	jit.recovery = nodeplane.JITRunnerRecovery{RunnerID: 93, Present: true}
	recovery, err = c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryRetired ||
		!slices.Equal(jit.removed, []string{"github-returned-name", "github-returned-name", "github-returned-name", "billet-l1"}) {
		t.Fatalf("idle registered recovery = %q, removed %v, err %v", recovery, jit.removed, err)
	}
	member, err = store.PoolRunnerByLease(t.Context(), "l1")
	if err != nil || member.Status != alloc.PoolRunnerRetiring || member.RunnerID != 93 {
		t.Fatalf("split no-row retirement fence = %+v, err %v", member, err)
	}
	if err := store.RegisterPoolRunner(t.Context(), alloc.PoolRunner{
		LeaseID: "l1", Tier: "billet-2vcpu", LaunchRequestID: 7, RunnerName: "billet-l1",
	}); err != nil {
		t.Fatalf("late split legacy registration: %v", err)
	}
	member, err = store.PoolRunnerByLease(t.Context(), "l1")
	if err != nil || member.Status != alloc.PoolRunnerRetiring {
		t.Fatalf("late split registration replaced retirement fence: %+v, err %v", member, err)
	}

	started := alloc.PoolRunner{LeaseID: "l1", Tier: "billet-2vcpu", LaunchRequestID: 7,
		RunnerID: 94, RunnerName: "billet-l1", Status: alloc.PoolRunnerBusy,
		ActualRequestID: 8, RunID: 9, JobID: "job"}
	store.mu.Lock()
	store.pool = nil
	store.mu.Unlock()
	jit.recovery = nodeplane.JITRunnerRecovery{}
	jit.onRecover = func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.pool = map[string]alloc.PoolRunner{"billet-l1": started}
	}
	recovery, err = c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryTracked {
		t.Fatalf("split recovery lost JobStarted during lookup: disposition %q, err %v", recovery, err)
	}

	store.mu.Lock()
	store.pool = map[string]alloc.PoolRunner{"billet-l1": {
		LeaseID: "l1", Tier: "billet-2vcpu", LaunchRequestID: 7,
		RunnerID: 94, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle,
	}}
	store.mu.Unlock()
	jit.onRecover = nil
	jit.recovery = nodeplane.JITRunnerRecovery{RunnerID: 94, Present: true}
	jit.onRemove = func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.pool = map[string]alloc.PoolRunner{"billet-l1": started}
	}
	recovery, err = c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryTracked {
		t.Fatalf("split recovery lost JobStarted after deletion: disposition %q, err %v", recovery, err)
	}

	store.mu.Lock()
	store.pool = nil
	store.mu.Unlock()
	jit.onRemove = nil
	jit.recoverErr = errors.New("github unavailable")
	if _, err := c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "billet-l1"); err == nil {
		t.Fatal("legacy recovery hid a GitHub error")
	}
	if _, err := c.RecoverRunner(t.Context(), "l1", "billet-2vcpu", 7, "wrong-name"); err == nil {
		t.Fatal("legacy recovery accepted a name unrelated to the lease")
	}
}

// The launch command arrives with everything a launch needs.
func TestTheLaunchArrivesIntact(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{}, nodeplane.WithCommandTimeout(60*time.Second))
	c := dial(t, base)

	got := make(chan nodeapi.Command, 1)

	go func() {
		cmd, ok, err := c.Poll(t.Context())
		if err != nil || !ok {
			close(got)

			return
		}

		got <- cmd

		if err := c.Report(t.Context(), nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			return
		}
	}()

	lease := &alloc.Lease{
		ID:        "l1",
		Tier:      "billet-2vcpu",
		VCPU:      4,
		Memory:    16 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     9,
		RequestID: 7,
	}

	if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7, RunID: 8, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	select {
	case cmd := <-got:
		if cmd.Lease == nil {
			t.Fatal("the command arrived with no lease")
		}

		if cmd.Lease.VCPU != 4 || cmd.Lease.Memory != 16*config.GiB {
			t.Errorf("size did not survive: %d vcpu / %v", cmd.Lease.VCPU, cmd.Lease.Memory)
		}

		if cmd.Lease.Epoch != 9 {
			t.Errorf("epoch did not survive (%d); the node fences its writes with it", cmd.Lease.Epoch)
		}

		if cmd.Job == nil || cmd.Job.Event != "push" {
			t.Errorf("the job's event did not survive: %+v", cmd.Job)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the node never saw the command")
	}
}

// AN IDLE POLL IS A 204 AND NOT AN ERROR.
func TestAnIdlePollIsQuiet(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{})
	p.SetPollWindowForTest(80 * time.Millisecond)

	c := dial(t, base)

	_, ok, err := c.Poll(t.Context())
	if err != nil {
		t.Fatalf("an idle poll failed: %v", err)
	}

	if ok {
		t.Error("an idle poll produced a command")
	}
}

// A FENCED LEASE ARRIVES AS alloc.ErrFenced, not as prose.
//
// This is the one the runner branches on: fenced means stop, something else owns
// this. It has to survive the wire as the same error value the in-process path
// produces, or every caller needs a second code path for remote nodes.
func TestLedgerErrorsKeepTheirKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		store *fakeStore
		want  error
		call  func(ctx context.Context, c *nodeclient.Client) error
	}{
		{
			name:  "fenced heartbeat",
			store: &fakeStore{heartbeatErr: alloc.ErrFenced},
			want:  alloc.ErrFenced,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				return c.Heartbeat(ctx, "l1", 1)
			},
		},
		{
			name:  "fallback exceeds capacity",
			store: &fakeStore{resizeErr: alloc.ErrNoCapacity},
			want:  alloc.ErrNoCapacity,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				return c.Resize(ctx, "l1", 1, "large", 16, 32*config.GiB)
			},
		},
		{
			name:  "operator force release",
			store: &fakeStore{heartbeatErr: alloc.ErrForceRelease},
			want:  alloc.ErrForceRelease,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				return c.Heartbeat(ctx, "l1", 1)
			},
		},
		{
			name:  "missing lease",
			store: &fakeStore{leaseErr: alloc.ErrLeaseNotFound},
			want:  alloc.ErrLeaseNotFound,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				_, err := c.Lease(ctx, "l1")

				return err
			},
		},
		{
			name:  "fenced release",
			store: &fakeStore{releaseErr: alloc.ErrFenced},
			want:  alloc.ErrFenced,
			call: func(ctx context.Context, c *nodeclient.Client) error {
				return c.Release(ctx, "l1", 1, alloc.PhaseDone)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, base := serve(t, tc.store)
			c := dial(t, base)

			if err := tc.call(t.Context(), c); !errors.Is(err, tc.want) {
				t.Fatalf("want %v across the wire, got %v", tc.want, err)
			}
		})
	}
}

// AN UNREGISTERED NODE IS TOLD TO REGISTER, which is what a restarted control
// plane looks like from a node that is still running.
func TestARestartedControlPlaneTellsTheNodeToRegister(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Never registered — the same state as a node whose server restarted.
	if err := c.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, nodeclient.ErrUnregistered) {
		t.Fatalf("want ErrUnregistered, got %v", err)
	}
}

// THE PATH DECIDES WHICH NODE, NOT THE BODY.
//
// They can disagree, and a node binding a lease under another node's name would
// place compute where the ledger does not expect it.
func TestTheBodyCannotRenameTheNode(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	// The client sends its own name in the body; pass a different one to prove
	// the server ignores it.
	if err := c.Bind(t.Context(), "l1", 1, "somebody-else"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.bound) != 1 || store.bound[0] != "n1/l1" {
		t.Fatalf("the ledger was told %v; the node's identity must come from the "+
			"authenticated path, not from a field it fills in", store.bound)
	}
}

// A phase billet does not know is refused, and the refusal names the value.
func TestAnUnknownPhaseIsRefusedOverTheWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	if err := c.Advance(t.Context(), "l1", 1, alloc.Phase("lauching")); err == nil {
		t.Fatal("a misspelled phase was accepted")
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.advanced) != 0 {
		t.Errorf("the ledger was asked to advance to %v", store.advanced)
	}
}

// Every phase the ledger has can actually be sent.
func TestEveryPhaseCanBeAdvancedTo(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	want := []alloc.Phase{
		alloc.PhaseAssigned, alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy,
		alloc.PhaseCustody, alloc.PhaseTeardown,
	}

	for _, p := range want {
		if err := c.Advance(t.Context(), "l1", 1, p); err != nil {
			t.Errorf("phase %q could not be sent: %v", p, err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.advanced) != len(want) {
		t.Errorf("the ledger saw %v, want %v", store.advanced, want)
	}
}

func TestAFailureReasonCrossesTheNodeWire(t *testing.T) {
	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	const reason = "ec2 spot interruption: terminate"
	if err := c.MarkFailure(t.Context(), "l1", 7, reason); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.failures) != 1 || store.failures[0] != reason {
		t.Errorf("failure reasons = %v, want %q", store.failures, reason)
	}
}

// LaunchedLeaseIDs survives as a set, including when it is empty.
func TestLaunchedLeasesSurviveEmpty(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})
	c := dial(t, base)

	ids, err := c.LaunchedLeaseIDs(t.Context(), "n1")
	if err != nil {
		t.Fatalf("LaunchedLeaseIDs: %v", err)
	}

	if ids == nil {
		t.Error("an empty set arrived as nil, which a caller would range over as absent " +
			"rather than as empty")
	}
}

// The node learns the TTL from the server rather than inventing one.
func TestTheNodeTakesItsTimingsFromTheServer(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, 90*time.Second, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)

	if c.LeaseTTL() != 90*time.Second {
		t.Errorf("lease TTL = %v, want the server's 90s; a node that guesses is guessing at "+
			"somebody else's deadline", c.LeaseTTL())
	}
}

// A node from another deployment is refused at registration.
func TestAForeignNodeIsRefusedOverTheWire(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	err = c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: "ffffffffffffffffffffffffffffffff", VCPU: testNodeVCPU, Memory: testNodeMemory})
	if err == nil {
		t.Fatal("a node from another deployment registered")
	}
}

// The wire refuses a body carrying fields it does not understand, rather than
// silently dropping them.
func TestUnknownFieldsAreRefused(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	// AN OTHERWISE VALID REGISTRATION, differing only in the bogus field. The
	// version that stood here posted an empty body, which is refused for being
	// empty — so deleting DisallowUnknownFields left it green, and the check it
	// existed to guard was unprotected.
	valid := fmt.Sprintf(
		`{"version":%d,"node":"n1","provider":"docker","deployment":%q,`+
			`"vcpu":8,"memory":34359738368}`,
		nodeapi.Version, deployment)

	accepted, err := postRaw(t, srv.URL+"/v1/register", valid)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	if accepted != http.StatusOK {
		t.Fatalf("the control registration was rejected with %d, so this test cannot tell a "+
			"refused unknown field from a refused registration", accepted)
	}

	withBogus := fmt.Sprintf(
		`{"version":%d,"node":"n2","provider":"docker","deployment":%q,"turbo":true}`,
		nodeapi.Version, deployment)

	got, err := postRaw(t, srv.URL+"/v1/register", withBogus)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	if got == http.StatusOK {
		t.Error("a registration carrying a field this build does not understand was accepted: " +
			"the two sides are deployed separately, so an unknown field is a version mismatch " +
			"and silently dropping it hides one until something behaves oddly much later")
	}
}

// postRaw posts a literal body, which is the only way to send a field no Go type
// on this side has.
func postRaw(t *testing.T, url, body string) (int, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}

	defer func() { _ = res.Body.Close() }()

	return res.StatusCode, nil
}

// A CONTROL PLANE THAT ACCEPTS AND NEVER ANSWERS MUST NOT HANG THE NODE.
//
// The client has no client-wide timeout on purpose — a command poll is a long
// poll — and the first version replaced it with nothing, so every ordinary
// request could block forever against a server that completed the TCP handshake
// and then said nothing. A stuck heartbeat is worse than a failed one: it also
// stops the sweep, the custody tend, and every subsequent poll.
func TestARequestAgainstASilentServerGivesUp(t *testing.T) {
	t.Parallel()

	// Accepts the connection, reads nothing, writes nothing, ever.
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			// Held open deliberately; never answered.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	// A SHORT DEADLINE, so the suite does not spend the production constant
	// waiting. What is under test is that a deadline exists and fires at all —
	// the value itself is configuration, asserted separately.
	c, err := nodeclient.New(nodeclient.Options{
		Base:           "http://" + ln.Addr().String(),
		Node:           "n1",
		RequestTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Bounded by the test rather than by the client's own deadline, so a client
	// that hangs fails here instead of running until the whole suite times out.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- c.Heartbeat(ctx, "l1", 1)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request to a server that never answered reported success")
		}
	case <-ctx.Done():
		t.Fatal("the request never gave up; a node against a wedged control plane would " +
			"stop heartbeating, sweeping and polling forever")
	}
}

// THE LEDGER IS TOLD ABOUT A NODE, not just the in-memory plane.
//
// The plane's map decides where commands go; the allocator's node row is what
// Bind checks before placing a lease. Registering in one and not the other
// produced a node that took commands and then had every Bind refused — which
// reads as a broken node rather than a missing row.
func TestRegistrationReachesTheLedger(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistrar{}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(reg), nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	dial(t, srv.URL)

	if got := reg.names(); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("the ledger was told about %v, want [n1]", got)
	}
}

// recordingBarriers is the barrier store's registration-facing half.
//
// Only InvalidateBarrierRun is exercised here; the rest satisfies the interface
// and is never reached, because this test is about the REGISTRATION path.
type recordingBarriers struct {
	mu             sync.Mutex
	invalidated    []string
	invalidatedAll int
	// allErr makes the fleet-wide discard fail, which is the ledger blip that
	// must not be hidden behind a permanent refusal.
	allErr error
}

func (b *recordingBarriers) InvalidateBarrierRun(_ context.Context, node string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.invalidated = append(b.invalidated, node)

	return nil
}

func (b *recordingBarriers) InvalidateEveryBarrierRun(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.allErr != nil {
		return b.allErr
	}

	b.invalidatedAll++

	return nil
}

func (b *recordingBarriers) invalidatedEverything() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.invalidatedAll
}

func (b *recordingBarriers) invalidations() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.invalidated...)
}

func (b *recordingBarriers) ComputeBarrierInForce(
	context.Context,
) (alloc.ComputeBarrier, bool, error) {
	return alloc.ComputeBarrier{}, false, nil
}

func (b *recordingBarriers) DropComputeBarrier(context.Context, string) error { return nil }

func (b *recordingBarriers) AdmissionGeneration(context.Context) (int64, bool, error) {
	return 0, false, nil
}

func (b *recordingBarriers) Quiescence(context.Context) (alloc.Quiescence, error) {
	return alloc.Quiescence{}, nil
}

func (b *recordingBarriers) NodeFenceOf(context.Context, string) (alloc.NodeFence, bool, error) {
	return alloc.NodeFence{}, false, nil
}

func (b *recordingBarriers) RecordBarrierObservation(
	context.Context, alloc.BarrierObservation,
) error {
	return nil
}

func (b *recordingBarriers) ResolveQuarantineFor(
	context.Context, string, []string, int64,
) (int, error) {
	return 0, nil
}

// A REGISTRATION OVER THE WIRE DISCARDS WHAT THE HOST HAD PROVED, BEFORE IT
// COMMITS ITS EPOCH.
//
// DRIVEN THROUGH THE HTTP HANDLER, WHICH IS THE ONLY PATH A REAL NODE TAKES.
// `Plane.Register` is the in-process and test entry point; `handler.register`
// assembles the same sequence itself so it can read the launched leases in
// between, and an invalidation added to only the former is a fix in the path
// nobody uses. That is exactly what the first version of it was, and its test
// passed because the test called the wrong one.
//
// The pause inside LaunchedLeaseIDs is the window itself: past the fence, before
// the ledger's epoch moves.
func TestAWireRegistrationDiscardsAProofBeforeItsEpochMoves(t *testing.T) {
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	store := &fakeStore{launchedEntered: entered, launchedProceed: proceed}
	barriers := &recordingBarriers{}

	_, base := serve(t, store,
		nodeplane.WithRegistrar(&fakeRegistrar{}),
		nodeplane.WithBarrierStore(barriers))

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	registered := make(chan error, 1)

	go func() {
		registered <- c.Register(t.Context(), nodeclient.Registration{
			Provider: config.ProviderDocker, Deployment: deployment,
			VCPU: testNodeVCPU, Memory: testNodeMemory,
		})
	}()

	<-entered

	// INSIDE THE WINDOW. The epoch has not been written yet, so a reader in
	// another process still sees the old one — which is why the proof has to be
	// gone by now rather than when the registration lands.
	//
	// FLEET-WIDE, because this is the loopback wire: no certificate, so nothing
	// can name the arriving host until the body is decoded, and the discard runs
	// well before that. `TestAWireRegistrationDiscardsAProofBeforeItsEpochMoves`
	// has an mTLS sibling above that covers the per-host form.
	if n := barriers.invalidatedEverything(); n != 1 {
		t.Errorf("the fleet's proofs were discarded %d time(s) before the ledger write, "+
			"want 1", n)
	}

	// AND NOT A PER-HOST ONE AS WELL. There is exactly one discard per request;
	// a second would cost a read for nothing, and its failure would turn a
	// successful fleet-wide invalidation into a spurious 503.
	if got := barriers.invalidations(); len(got) != 0 {
		t.Errorf("a loopback registration also discarded per host: %v", got)
	}

	close(proceed)

	if err := <-registered; err != nil {
		t.Fatalf("register: %v", err)
	}
}

// A REGISTRATION THAT IS PERMANENTLY REFUSED STILL DISCARDS THE PROOF.
//
// The request carries the host's own live inventory, and an authenticated host
// saying what it is running does not stop being true because billet refuses the
// registration on its merits. Worse, a PERMANENT refusal is one the node does
// not retry — so a proof left standing here stays standing indefinitely, and
// `local down` can stop services on the strength of it.
//
// Rolling a control plane past an old node's protocol range is exactly this
// shape, and it is the operation during which somebody is most likely to be
// running a drain.
func TestARefusedRegistrationStillDiscardsTheProof(t *testing.T) {
	t.Parallel()

	barriers := &recordingBarriers{}

	_, base := serve(t, &fakeStore{},
		nodeplane.WithRegistrar(&fakeRegistrar{}),
		nodeplane.WithBarrierStore(barriers))

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// A CONTRIBUTION OF NOTHING is refused by checkRegistration, permanently,
	// before the registration is considered to have begun — while the request
	// still reports live compute.
	err = c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: 0, Memory: 0,
		InventoryKnown: true, Instances: []string{"l1"},
	})
	if err == nil {
		t.Fatal("a registration contributing nothing was accepted")
	}

	// FLEET-WIDE, for the same reason: this is the loopback wire, and the discard
	// happens before anything could name the host.
	if n := barriers.invalidatedEverything(); n != 1 {
		t.Fatalf("a refused registration discarded the fleet's proofs %d time(s), want 1", n)
	}

	if got := barriers.invalidations(); len(got) != 0 {
		t.Errorf("a loopback registration also discarded per host: %v", got)
	}
}

// A REGISTRATION THE STRICT DECODER REFUSES STILL DISCARDS THE PROOF.
//
// `decode` uses DisallowUnknownFields, so a node rolled ahead of this control
// plane is answered CodeRefused before ANY semantic check runs — and a node
// stops retrying a permanent refusal. That is the documented node-first-upgrade
// case, and it is a host that may be holding compute right now.
//
// DRIVEN AS RAW BYTES OVER A REAL mTLS WIRE, and both halves matter. The field
// this is about cannot be produced by a well-behaved client — the whole point is
// a body from a build that knows something this one does not — and the
// CERTIFICATE is what makes the host identifiable before that body is read. On a
// loopback wire, which requires no certificate, the body is the only identity
// there is and this case cannot be closed at all.
func TestARegistrationRefusedByTheDecoderStillDiscardsTheProof(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	barriers := &recordingBarriers{}

	srv := httptest.NewUnstartedServer(nodeplane.Handler(log,
		nodeplane.New(log, deployment, time.Minute,
			nodeplane.WithRegistrar(&fakeRegistrar{}),
			nodeplane.WithBarrierStore(barriers)),
		&fakeStore{}, nil, nodeplane.RequireClientCert()))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientConf}}

	// AN OTHERWISE VALID REGISTRATION. The version, capacity, provider and
	// deployment are all ones this build accepts, so the ONLY thing that can
	// refuse this request is the unknown field — without that, a later semantic
	// refusal would produce the same status and the same invalidation, and the
	// test would pass while proving nothing about the decoder.
	body := fmt.Sprintf(
		`{"version":%d,"min_version":%d,"node":"epyc-1","provider":"docker",`+
			`"deployment":"%s","vcpu":8,"memory":34359738368,`+
			`"a_field_from_a_newer_build":true}`,
		nodeapi.Version, nodeapi.MinVersion, deployment)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/register", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defer resp.Body.Close()

	// THE DECODER'S OWN REFUSAL, named. 400 with the field in the message is what
	// DisallowUnknownFields produces; a semantic refusal is 403.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field was answered %d, want %d — so this test is not "+
			"exercising the decoder", resp.StatusCode, http.StatusBadRequest)
	}

	said, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the refusal: %v", err)
	}

	if !strings.Contains(string(said), "a_field_from_a_newer_build") {
		t.Errorf("the refusal does not name the unknown field: %s", said)
	}

	if got := barriers.invalidations(); len(got) != 1 || got[0] != "epyc-1" {
		t.Fatalf("a registration refused by the decoder left the host's proof standing: %v",
			got)
	}
}

// A LOOPBACK REGISTRATION THE DECODER REFUSES DISCARDS EVERY PROOF.
//
// That wire requires no certificate, so a body that will not decode names NOBODY
// — and the refusals that get there are permanent ones a node does not retry:
// an unknown field from a node rolled ahead of this control plane, an oversized
// body. Something arrived and billet cannot say what, which must not read as
// "nothing changed".
//
// Over-invalidation is the point. A loopback deployment is one machine, so the
// cost is one host's barrier round.
func TestAnUnreadableLoopbackRegistrationDiscardsEveryProof(t *testing.T) {
	t.Parallel()

	barriers := &recordingBarriers{}

	_, base := serve(t, &fakeStore{},
		nodeplane.WithRegistrar(&fakeRegistrar{}),
		nodeplane.WithBarrierStore(barriers))

	// AN OTHERWISE VALID REGISTRATION, so the unknown field is the only thing
	// that can refuse it.
	body := fmt.Sprintf(
		`{"version":%d,"min_version":%d,"node":"n1","provider":"docker",`+
			`"deployment":"%s","vcpu":8,"memory":34359738368,`+
			`"a_field_from_a_newer_build":true}`,
		nodeapi.Version, nodeapi.MinVersion, deployment)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/v1/register", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field was answered %d, want %d", resp.StatusCode,
			http.StatusBadRequest)
	}

	if n := barriers.invalidatedEverything(); n != 1 {
		t.Fatalf("a registration this wire could not read discarded the fleet's proofs %d "+
			"time(s), want 1", n)
	}

	// AND NOT ONE HOST'S, because there is no host to name.
	if got := barriers.invalidations(); len(got) != 0 {
		t.Errorf("an unreadable registration named a host anyway: %v", got)
	}
}

// A REGISTRATION WITH THE WRONG METHOD IS STILL AN ARRIVAL.
//
// Routing this path as `POST /v1/register` lets ServeMux answer 405 before the
// handler runs at all — and a caller holding this host's certificate has
// arrived, whatever verb it used, contradicting whatever that host last proved.
// The route is deliberately not method-qualified for that reason, and this is
// what says so.
func TestARegistrationWithTheWrongMethodStillDiscardsTheProof(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	barriers := &recordingBarriers{}

	srv := httptest.NewUnstartedServer(nodeplane.Handler(log,
		nodeplane.New(log, deployment, time.Minute,
			nodeplane.WithRegistrar(&fakeRegistrar{}),
			nodeplane.WithBarrierStore(barriers)),
		&fakeStore{}, nil, nodeplane.RequireClientCert()))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientConf}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.URL+"/v1/register", http.NoBody)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a GET to the registration route was answered %d, want %d",
			resp.StatusCode, http.StatusMethodNotAllowed)
	}

	if got := barriers.invalidations(); len(got) != 1 || got[0] != "epyc-1" {
		t.Fatalf("an arrival with the wrong method left the host's proof standing: %v", got)
	}
}

// AND ON LOOPBACK, WHERE THERE IS NO HOST TO NAME, EVERY PROOF GOES.
//
// The mTLS half above passes on the certificate-name discard, which happens
// before the method check — so it says nothing about the wire that has no
// certificate. There the arrival cannot be attributed at all, and the fleet-wide
// discard is what has to run before the 405.
func TestAWrongMethodLoopbackRegistrationDiscardsEveryProof(t *testing.T) {
	t.Parallel()

	barriers := &recordingBarriers{}

	_, base := serve(t, &fakeStore{},
		nodeplane.WithRegistrar(&fakeRegistrar{}),
		nodeplane.WithBarrierStore(barriers))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/v1/register", http.NoBody)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a GET to the registration route was answered %d, want %d",
			resp.StatusCode, http.StatusMethodNotAllowed)
	}

	if n := barriers.invalidatedEverything(); n != 1 {
		t.Fatalf("an unattributable arrival discarded the fleet's proofs %d time(s), want 1", n)
	}
}

// A FLEET-WIDE DISCARD THAT FAILS REFUSES THE REGISTRATION, RETRYABLY.
//
// `decode` writes its own 400 before returning, and a node reads that as
// PERMANENT and stops retrying — so an invalidation attempted after it could
// only be logged, and a transient ledger blip would leave every proof standing
// with nobody ever coming back. That is why the discard is ahead of everything
// and answers 503 itself.
func TestAFailedFleetWideDiscardRefusesTheRegistrationRetryably(t *testing.T) {
	t.Parallel()

	barriers := &recordingBarriers{allErr: errors.New("the ledger is unavailable")}
	reg := &fakeRegistrar{}

	p, base := serve(t, &fakeStore{},
		nodeplane.WithRegistrar(reg),
		nodeplane.WithBarrierStore(barriers))

	body := fmt.Sprintf(
		`{"version":%d,"min_version":%d,"node":"n1","provider":"docker",`+
			`"deployment":"%s","vcpu":8,"memory":34359738368}`,
		nodeapi.Version, nodeapi.MinVersion, deployment)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/v1/register", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defer resp.Body.Close()

	// 503, NOT 400 OR 403. A node retries the first and stops on the others, and
	// stopping is what strands the proof.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a failed fleet-wide discard was answered %d, want %d — anything else is "+
			"permanent and the node stops retrying",
			resp.StatusCode, http.StatusServiceUnavailable)
	}

	// AND THE REGISTRATION ACTUALLY STOPPED. The status alone proves nothing:
	// the 503 is already written by the time the handler returns, so a version
	// that carried on would supersede the node and move its ledger epoch while
	// the client still saw exactly this response — which is the side effect the
	// refusal exists to prevent.
	if got := reg.names(); len(got) != 0 {
		t.Errorf("a registration refused for a failed discard reached the ledger anyway: %v",
			got)
	}

	if got := p.Nodes(); len(got) != 0 {
		t.Errorf("a registration refused for a failed discard installed a node anyway: %v",
			got)
	}
}

// revokedCerts refuses everything, which is what an operator running
// `billet nodes revoke` leaves behind.
type revokedCerts struct{}

func (revokedCerts) CertRevokedFor(context.Context, string, string, time.Time) (bool, error) {
	return true, nil
}

func (revokedCerts) CertRevoked(context.Context, string) (bool, error) { return true, nil }

func (revokedCerts) RecordIssuedCert(context.Context, alloc.IssuedCert) error { return nil }

func (revokedCerts) RecordRenewedCert(
	context.Context, alloc.IssuedCert, string, time.Time,
) error {
	return nil
}

// A REVOKED HOST'S ARRIVAL STILL DISCARDS ITS PROOF.
//
// Revocation is an operator taking a machine's credentials back, and it says
// NOTHING about whether that machine is still running compute. The refusal is
// permanent, so the node stops retrying — and neither its epoch nor its dispatch
// generation moves, so nothing else would ever invalidate the run either.
func TestARevokedRegistrationStillDiscardsTheProof(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	barriers := &recordingBarriers{}

	srv := httptest.NewUnstartedServer(nodeplane.Handler(log,
		nodeplane.New(log, deployment, time.Minute,
			nodeplane.WithRegistrar(&fakeRegistrar{}),
			nodeplane.WithBarrierStore(barriers)),
		&fakeStore{}, nil,
		nodeplane.RequireClientCert(),
		nodeplane.WithRevocations(revokedCerts{})))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	clientConf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientConf}}

	body := fmt.Sprintf(
		`{"version":%d,"min_version":%d,"node":"epyc-1","provider":"docker",`+
			`"deployment":"%s","vcpu":8,"memory":34359738368,`+
			`"inventory_known":true,"instances":["l1"]}`,
		nodeapi.Version, nodeapi.MinVersion, deployment)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/register", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("a revoked certificate registered (status %d)", resp.StatusCode)
	}

	if got := barriers.invalidations(); len(got) != 1 || got[0] != "epyc-1" {
		t.Fatalf("a revoked host's arrival left its proof standing: %v", got)
	}
}

func TestRegistrationIntentInvalidatesAbsenceBeforeTheOwnershipRead(t *testing.T) {
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	store := &fakeStore{launchedEntered: entered, launchedProceed: proceed}
	reg := &fakeRegistrar{}
	p, base := serve(t, store, nodeplane.WithRegistrar(reg))
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "old", VCPU: testNodeVCPU,
		Memory: testNodeMemory, InventoryKnown: true,
	}); err != nil {
		t.Fatalf("register old incarnation: %v", err)
	}
	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	registered := make(chan error, 1)
	go func() {
		registered <- c.Register(t.Context(), nodeclient.Registration{
			Provider: config.ProviderDocker, Deployment: deployment,
			VCPU: testNodeVCPU, Memory: testNodeMemory,
			InventoryKnown: true, Instances: []string{"l1"},
		})
	}()
	<-entered

	if err := p.NewRunner().DestroyCompletedBound(
		t.Context(), 7, "Succeeded", "l1", "n1", 1, alloc.PhaseDone,
	); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("completion during ownership read = %v, want only holder unavailable", err)
	}
	close(proceed)
	if err := <-registered; err != nil {
		t.Fatalf("register replacement: %v", err)
	}
}

// A LEDGER THAT REFUSES LEAVES THE PLANE UNCHANGED.
//
// Otherwise the node believes it registered, starts polling, and has every lease
// operation refused — with no way to discover that the row it needs was never
// written.
func TestALedgerRefusalFailsTheRegistration(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistrar{err: errors.New("no such node in the fleet configuration")}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(reg), nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err == nil {
		t.Fatal("a node the ledger refused was registered anyway")
	}

	if len(p.Nodes()) != 0 {
		t.Errorf("the plane kept %v after the ledger refused", p.Nodes())
	}
}

// A CUSTODY HEARTBEAT IS EVIDENCE THE NODE IS ALIVE.
//
// The node's command loop is synchronous: if Recover, Sweep or Tend wedges — a
// hung Docker call is enough — the loop never reaches Poll again. Its custody
// janitor is a separate goroutine and keeps heartbeating perfectly well.
//
// When only Poll and Result counted as life, that heartbeat asked whether the
// node was registered, the question ran expiry, and the same call that proved
// the node alive was the one that declared it dead. Every heartbeat afterwards
// was refused as unregistered, the wedged loop could not reach Poll to
// re-register, and the leases it was holding — for compute that may still be
// running — expired.
func TestACustodyHeartbeatKeepsANodeRegistered(t *testing.T) {
	t.Parallel()

	// A clock this test moves. Atomic because the plane reads it from its own
	// goroutines, and a closure over a plain variable is a data race the moment
	// anything but the test consults it.
	var nanos atomic.Int64

	nanos.Store(time.Now().UnixNano())

	clock := func() time.Time { return time.Unix(0, nanos.Load()) }

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithClock(clock))
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Silent on the POLL for well past the window — the wedged-loop case — while
	// the janitor keeps renewing.
	for range 3 {
		nanos.Add(int64(10 * time.Minute))

		if err := c.Heartbeat(t.Context(), "l1", 1); err != nil {
			t.Fatalf("a heartbeat from a node whose command loop is stuck was refused, so the "+
				"leases it holds expire while its compute may still be running: %v", err)
		}
	}

	if got := p.Nodes(); len(got) != 1 {
		t.Errorf("a node that heartbeated throughout was forgotten: %v", got)
	}
}

// A LEDGER OUTAGE IS RETRYABLE; A VERDICT IS NOT.
//
// The node STOPS on a refusal, so the classification decides whether a database
// blip takes a host down permanently. A ledger that could not write the node row
// will succeed once it answers again; a protocol mismatch never will.
func TestOnlyAVerdictIsPermanent(t *testing.T) {
	t.Parallel()

	t.Run("a ledger outage is retryable", func(t *testing.T) {
		t.Parallel()

		reg := &fakeRegistrar{err: errors.New("database is locked")}

		log := slog.New(slog.DiscardHandler)
		p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(reg), nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
		}}))
		srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

		t.Cleanup(srv.Close)

		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}

		err = c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory})
		if err == nil {
			t.Fatal("a registration the ledger could not record was accepted")
		}

		if errors.Is(err, nodeclient.ErrRefused) {
			t.Errorf("a ledger outage was reported as a permanent refusal, so the node would "+
				"give up and stay down after the database recovered: %v", err)
		}
	})

	t.Run("a foreign deployment is permanent", func(t *testing.T) {
		t.Parallel()

		log := slog.New(slog.DiscardHandler)
		p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
		}}))
		srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

		t.Cleanup(srv.Close)

		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}

		err = c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: "ffffffffffffffffffffffffffffffff", VCPU: testNodeVCPU, Memory: testNodeMemory})
		if !errors.Is(err, nodeclient.ErrRefused) {
			t.Errorf("a foreign deployment identity must be permanent, or the node retries "+
				"something that will never be accepted: %v", err)
		}
	})
}

// Loopback is the only address the wire may serve on without TLS.
//
// THIS TEST USED TO ASSERT THE VULNERABILITY. It listed ":7717" as safe, which is
// the wildcard — net.Listen binds every interface for an empty host — so the
// unauthenticated wire, JIT credential endpoint included, was served to the whole
// network by a guard that reported it had not. The hole was pinned by its own
// test rather than caught by it.
func TestOnlyLoopbackIsSafeWithoutTLS(t *testing.T) {
	t.Parallel()

	for addr, want := range map[string]bool{
		"127.0.0.1:7717":  true,
		"localhost:7717":  true,
		"[::1]:7717":      true,
		"127.0.0.53:7717": true,

		// AN IPv4-MAPPED LOOPBACK, which is loopback and which no amount of
		// prefix-matching on "127." recognises. Included because without it the
		// whole test passes against a string-comparison implementation, and this
		// function's first version was exactly that.
		"[::ffff:127.0.0.1]:7717": true,

		// THE WILDCARD, in every spelling. An empty host is not "unspecified so
		// probably local"; it is every interface this machine has.
		":7717":         false,
		"0.0.0.0:7717":  false,
		"[::]:7717":     false,
		"10.0.0.5:7717": false,

		// A name that might resolve to loopback is still not loopback: what it
		// resolves to is not knowable here and can change under the process.
		"example.com:443":       false,
		"localhost.evil.com:80": false,

		// Not an address at all. Refusing beats guessing.
		"127.0.0.1":   false,
		"":            false,
		"garbage":     false,
		"1.2.3.4.5:1": false,
	} {
		if got := nodeplane.LoopbackOnly(addr); got != want {
			t.Errorf("LoopbackOnly(%q) = %v, want %v", addr, got, want)
		}
	}
}

// THE TIER'S SHAPE TRAVELS WITH THE LAUNCH, so a node keeps no catalogue.
//
// A node that read its own copy needed that copy to agree with the control
// plane's, and nothing checked. A tier missing from the node's file refused the
// launch loudly; a tier whose `image:` had drifted ran the WRONG image, with no
// error anywhere and nothing to compare against.
//
// The lease already carries what was decided when the reservation was made —
// vCPU, memory, guest OS, acceptable providers. This is the rest of it: the
// fields a node cannot get from the lease and previously had to look up.
func TestALaunchCarriesTheTierShape(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{}, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu",
		Providers: []config.ProviderKind{
			config.ProviderEC2,
			config.ProviderDocker,
		},
		GuestOS:     config.GuestLinux,
		VCPU:        2,
		Memory:      8 * config.GiB,
		Disk:        40 * config.GiB,
		SHM:         512 * config.MiB,
		RunnerGroup: "billet",
		Launch: map[config.ProviderKind]config.TierLaunch{
			config.ProviderEC2: {
				Image:   "ami-cloud",
				Command: []string{"/usr/local/bin/billet-runner"},
			},
			config.ProviderDocker: {
				Image:   "ghcr.io/actions/actions-runner:latest",
				Command: []string{"/entrypoint.sh"},
			},
		},
	}}))

	c := dial(t, base)

	got := make(chan nodeapi.Command, 1)

	go func() {
		cmd, took, err := c.Poll(t.Context())
		if err == nil && took {
			got <- cmd
		}
	}()

	lease := &alloc.Lease{
		ID: "l1", Tier: "billet-2vcpu", VCPU: 2, Memory: 8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderEC2, config.ProviderDocker},
		Epoch:     1,
	}

	// Buffered and never read: the node never answers, so this returns custody
	// long after the assertions below. The test is about what was DELIVERED.
	launched := make(chan error, 1)

	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}) }()

	select {
	case cmd := <-got:
		if cmd.Tier == nil {
			t.Fatal("the launch carried no tier shape, so the node has nothing to start")
		}

		if cmd.Tier.Image != "ghcr.io/actions/actions-runner:latest" {
			t.Errorf("image = %q, want the catalogue's", cmd.Tier.Image)
		}

		if cmd.Tier.Disk != 40*config.GiB || cmd.Tier.SHM != 512*config.MiB {
			t.Errorf("disk/shm = %s/%s, want the catalogue's", cmd.Tier.Disk, cmd.Tier.SHM)
		}

		// The runner group is part of a tier's ADDRESS: without it the node
		// resolves the scale set in GitHub's default group and its registrations
		// are refused.
		if cmd.Tier.RunnerGroup != "billet" {
			t.Errorf("runner_group = %q, want %q", cmd.Tier.RunnerGroup, "billet")
		}

		if len(cmd.Tier.Command) != 1 || cmd.Tier.Command[0] != "/entrypoint.sh" {
			t.Errorf("command = %v, want the catalogue's", cmd.Tier.Command)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the launch never reached the node")
	}
}

// AND THE LEDGER DECIDES FOR A LEASE NOTHING HAS CLAIMED YET.
//
// The owners map only holds leases the plane has DELIVERED, so escrow a listener
// is still holding — and any lease at all in the window after a control-plane
// restart, before the fleet re-adopts — has no entry, and the ownership check
// has nothing to refuse with. Fleet membership was all that was left, and every
// registered node passes that.
//
// So the ledger answers instead, the way the rest of the arithmetic does:
// COALESCE(node, target_node). Escrow chose the machine long before a bind fills
// `node` in, and that choice is what billet advertised against — releasing it
// from another host desynchronises the advertisement from the ledger, and the
// assignment GitHub makes against the difference fails.
//
// A lease the ledger has not placed at all is still allowed through, because
// that is the recovery path: after a restart a node re-adopts what it is running
// and must be able to maintain it while it does.
func TestANodeCannotReleaseALeaseTheLedgerGaveToAnotherHost(t *testing.T) {
	t.Parallel()

	// The ledger says this lease was escrowed for another machine and never bound.
	store := &fakeStore{lease: &alloc.Lease{ID: "l1", TargetNode: "other", Epoch: 1}}

	_, base := serve(t, store)
	c := dial(t, base)

	err := c.Release(t.Context(), "l1", 1, alloc.PhaseDone)
	if err == nil {
		t.Fatal("a node released a lease the ledger had escrowed for a different machine; " +
			"the advertisement and the ledger now disagree")
	}

	store.mu.Lock()
	released := len(store.released)
	store.mu.Unlock()

	if released != 0 {
		t.Errorf("the release reached the ledger anyway: %d", released)
	}
}

// WHAT A HOST REPORTS RUNNING REACHES THE LEDGER, and an absent report does not.
//
// This is the wiring that frees capacity held for compute nobody has accounted
// for, and it is also the wiring that must NOT free capacity for a container
// that is still there. Both halves turn on one flag: a node that could read its
// provider vouches for the list, and one that could not sends nothing rather
// than an empty list — because an unreadable provider knows nothing, and
// treating its silence as "running nothing" would hand back a live container's
// slot.
func TestARegistrationPassesOnWhatTheHostReportsRunning(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		reg        nodeclient.Registration
		wantCalled bool
		wantIDs    []string
	}{
		{
			name:       "a host that vouches for what it is running",
			reg:        nodeclient.Registration{Instances: []string{"l1", "l2"}, InventoryKnown: true},
			wantCalled: true,
			wantIDs:    []string{"l1", "l2"},
		},
		{
			name:       "a host that is genuinely running nothing",
			reg:        nodeclient.Registration{InventoryKnown: true},
			wantCalled: true,
		},
		{
			name: "a host whose provider could not be read",
			reg:  nodeclient.Registration{Instances: nil, InventoryKnown: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := &fakeRegistrar{}

			log := slog.New(slog.DiscardHandler)
			p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(reg))
			srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

			t.Cleanup(srv.Close)

			c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
			if err != nil {
				t.Fatalf("new client: %v", err)
			}

			full := tc.reg
			full.Provider, full.Deployment = config.ProviderDocker, deployment
			full.VCPU, full.Memory = testNodeVCPU, testNodeMemory

			if err := c.Register(t.Context(), full); err != nil {
				t.Fatalf("register: %v", err)
			}

			called, ids := reg.reconciliation()

			if called != tc.wantCalled {
				t.Fatalf("the ledger was asked to reconcile: %v, want %v — an unreadable "+
					"provider must not free capacity, and a host that is running nothing must",
					called, tc.wantCalled)
			}

			if tc.wantCalled && !slices.Equal(ids, tc.wantIDs) {
				t.Errorf("the ledger was told this host runs %v, want %v", ids, tc.wantIDs)
			}
			for _, id := range tc.wantIDs {
				owner, ok := p.OwnerOfLease(id)
				if !ok || owner.Node != "n1" || !owner.Current {
					t.Errorf("reported live lease %s has owner %+v, present=%v", id, owner, ok)
				}
			}
		})
	}
}

// THE RECONCILE ROUTE EXISTS AND CARRIES THE INVENTORY, which the node-side test
// cannot show: it injects the allocator directly, so removing the HTTP route or
// making the client method a no-op leaves it green.
func TestReconcileReachesTheLedgerOverTheWire(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistrar{}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(reg))
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)

	if _, err := c.Reconcile(t.Context(), "n1", []string{"l1", "l2"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	called, ids := reg.reconciliation()

	if !called {
		t.Fatal("the report never reached the ledger, so quarantined capacity is reclaimed " +
			"only at registration — which is the one moment it cannot work")
	}

	if !slices.Equal(ids, []string{"l1", "l2"}) {
		t.Errorf("the ledger was told this host runs %v", ids)
	}

	// AND AN EMPTY REPORT IS STILL A REPORT: a host running nothing is exactly
	// the one whose quarantined capacity should come back.
	if _, err := c.Reconcile(t.Context(), "n1", nil); err != nil {
		t.Fatalf("empty reconcile: %v", err)
	}

	_, ids = reg.reconciliation()

	if len(ids) != 0 {
		t.Errorf("an empty report was recorded as %v", ids)
	}
}

// AND A SUPERSEDED PROCESS CANNOT RECONCILE, because its inventory describes a
// machine somebody else now owns.
func TestASupersededIncarnationCannotReconcile(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistrar{}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(reg))
	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))

	t.Cleanup(srv.Close)

	old := dial(t, srv.URL)

	// A second process takes the name.
	dial(t, srv.URL)

	if _, err := old.Reconcile(t.Context(), "n1", nil); err == nil {
		t.Fatal("a superseded process reported an inventory for a node it no longer owns; " +
			"its stale list can free capacity the current one is using")
	}

	if called, _ := reg.reconciliation(); called {
		t.Error("the stale report reached the ledger anyway")
	}
}

// A SUPERSEDED PROCESS ASKING ABOUT A LEASE THAT IS OVER GETS THE LEDGER'S
// ANSWER. The ownership record ends with the lease, so a draining process whose
// custody outlived the lease's settlement would otherwise be refused as
// superseded on every renew and release — a refusal it cannot act on, and a
// drain that never ends. "Not found" is what the ledger says and what custody
// treats as done. A lease the ledger still holds OPEN keeps the refusal: only
// the process given the work may touch a lease that is charged.
func TestASupersededProcessIsToldAFinishedLeaseIsGone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		store *fakeStore
		want  error
	}{
		{"a lease the ledger has ended", &fakeStore{leaseErr: alloc.ErrLeaseNotFound}, alloc.ErrLeaseNotFound},
		{"a lease the ledger still holds open", &fakeStore{lease: &alloc.Lease{ID: "l1", Node: "n1"}}, nodeclient.ErrSuperseded},
		{"a ledger that could not answer", &fakeStore{leaseErr: errors.New("busy")}, nodeclient.ErrSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, base := serve(t, tc.store)

			old := dial(t, base)
			// A second process takes the name; the first is now draining.
			dial(t, base)

			if err := old.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, tc.want) {
				t.Fatalf("superseded heartbeat = %v, want %v", err, tc.want)
			}
			if err := old.Release(t.Context(), "l1", 1, alloc.PhaseDone); !errors.Is(err, tc.want) {
				t.Fatalf("superseded release = %v, want %v", err, tc.want)
			}

			// NOTHING REACHED THE STORE ON EITHER ANSWER: the wire decided.
			tc.store.mu.Lock()
			released := len(tc.store.released)
			tc.store.mu.Unlock()
			if released != 0 {
				t.Fatalf("a superseded process released through the wire: %v", tc.store.released)
			}
		})
	}
}

// A SUPERSEDED PROCESS MAY STILL WITHDRAW THE REGISTRATION OF A LEASE THE LEDGER
// HAS ENDED — and the wire PERFORMS it rather than answering "not found". A
// replacement's inventory can settle a lease while the superseded process still
// runs the guest, and that process must remove the registration GitHub can route
// work to before it destroys the guest. An answer it could read as "already
// removed" would let it skip exactly that.
func TestASupersededProcessStillWithdrawsTheRegistrationOfAnEndedLease(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		leaseErr:    alloc.ErrLeaseNotFound,
		historyNode: map[string]string{"l1": "n1"},
		pool: map[string]alloc.PoolRunner{"github-returned-name": {
			LeaseID: "l1", Tier: "billet-2vcpu", LaunchRequestID: 7, RunnerID: 91,
			RunnerName: "github-returned-name", Status: alloc.PoolRunnerBusy,
		}},
	}
	jit := &returnedJIT{}
	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, store, jit))
	t.Cleanup(srv.Close)

	old := dial(t, srv.URL)
	// A second process takes the name; the first is draining what it holds.
	dial(t, srv.URL)

	if err := old.EnsureRunnerRemoved(t.Context(), "l1"); err != nil {
		t.Fatalf("a superseded process could not withdraw the registration of its ended lease: %v", err)
	}

	store.mu.Lock()
	retired, deregistered := store.retired, store.deregistered
	store.mu.Unlock()

	if !slices.Equal(jit.removed, []string{"github-returned-name"}) {
		t.Fatalf("GitHub removals = %v, want the durable registration withdrawn", jit.removed)
	}
	if !slices.Equal(retired, []string{"l1"}) || !slices.Equal(deregistered, []string{"l1"}) {
		t.Fatalf("durable journal: retired %v deregistered %v, want [l1] for both", retired, deregistered)
	}

	// EVERYTHING ELSE ABOUT THAT LEASE IS STILL "NOT FOUND" for the superseded
	// process: it may withdraw a registration, never touch the capacity.
	if err := old.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("superseded heartbeat on an ended lease = %v, want not found", err)
	}
}

// AND SO MAY THE CURRENT PROCESS WHOSE OWNER RECORD IS GONE. A control-plane
// restart forgets every delivery, and an inventory drops an adoption the ledger
// no longer lists; the ledger then answers "not found" for the ended lease's
// placement, which the node must not read as a removal. The removal route is
// admitted on the ledger's own word that the lease is over.
func TestACurrentProcessWithNoOwnerRecordStillWithdrawsTheRegistrationOfAnEndedLease(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		leaseErr:    alloc.ErrLeaseNotFound,
		historyNode: map[string]string{"l1": "n1"},
		pool: map[string]alloc.PoolRunner{"github-returned-name": {
			LeaseID: "l1", Tier: "billet-2vcpu", LaunchRequestID: 7, RunnerID: 91,
			RunnerName: "github-returned-name", Status: alloc.PoolRunnerBusy,
		}},
	}
	jit := &returnedJIT{}
	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, store, jit))
	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)

	if err := c.EnsureRunnerRemoved(t.Context(), "l1"); err != nil {
		t.Fatalf("the current process could not withdraw the registration of an ended lease "+
			"it holds no record for: %v", err)
	}
	if !slices.Equal(jit.removed, []string{"github-returned-name"}) {
		t.Fatalf("GitHub removals = %v, want the durable registration withdrawn", jit.removed)
	}

	// The capacity routes still answer from the ledger's placement, which for
	// an ended lease is not found.
	if err := c.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("heartbeat on an ended lease with no record = %v, want not found", err)
	}
}

// AND NEVER ANOTHER HOST'S. Membership is not permission: a lease id is not
// secret, and the route that admits an ended lease must still require the
// history to attribute that lease's job to the asking node. A job never
// attributed to any host refuses too.
func TestARegisteredNodeCannotWithdrawAnotherHostsEndedRegistration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		history map[string]string
	}{
		{"attributed to another host", map[string]string{"l1": "n2"}},
		{"attributed to no host", map[string]string{"l1": ""}},
		{"no history at all", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeStore{
				leaseErr:    alloc.ErrLeaseNotFound,
				historyNode: tc.history,
				pool: map[string]alloc.PoolRunner{"github-returned-name": {
					LeaseID: "l1", Tier: "billet-2vcpu", LaunchRequestID: 7, RunnerID: 91,
					RunnerName: "github-returned-name", Status: alloc.PoolRunnerBusy,
				}},
			}
			jit := &returnedJIT{}
			log := slog.New(slog.DiscardHandler)
			p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
				Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
				VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
			}}))
			srv := httptest.NewServer(nodeplane.Handler(log, p, store, jit))
			t.Cleanup(srv.Close)

			c := dial(t, srv.URL)

			if err := c.EnsureRunnerRemoved(t.Context(), "l1"); err == nil {
				t.Fatal("a registered node withdrew the registration of an ended lease that was never its own")
			}
			if len(jit.removed) != 0 {
				t.Fatalf("GitHub removals = %v, want none", jit.removed)
			}
		})
	}
}

// THE QUARANTINED SET CROSSES THE WIRE, and dropping it is silent.
//
// A remote node reads this to tell an orphan from a job whose listener died. If
// the field never arrives the node sees an empty set, `open || held` is false
// for a live container, and it DESTROYS the job the quarantine exists to
// protect — the round-2 P0, reachable again through a response field nothing
// asserted.
func TestQuarantinedLeasesCrossTheWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		launched:    map[string]bool{"running": true},
		quarantined: map[string]bool{"held": true},
	}

	_, base := serve(t, store)
	c := dial(t, base)

	held, err := c.QuarantinedLeaseIDs(t.Context(), "n1")
	if err != nil {
		t.Fatalf("QuarantinedLeaseIDs: %v", err)
	}

	if !held["held"] {
		t.Error("the quarantined set did not reach the node, so it will treat a job whose " +
			"listener died as an orphan and destroy it")
	}

	if held["running"] {
		t.Error("a launched lease was reported as quarantined")
	}

	// And the launched set is still its own answer.
	open, err := c.LaunchedLeaseIDs(t.Context(), "n1")
	if err != nil {
		t.Fatalf("LaunchedLeaseIDs: %v", err)
	}

	if !open["running"] || open["held"] {
		t.Errorf("the launched set is %v; the two answers have been conflated", open)
	}
}
