package nodeplane

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

type presentLeaseRegistrar struct {
	countingRegistrar
	lease *alloc.Lease
}

func (r *presentLeaseRegistrar) SettleCompletionOnTerminalLease(
	_ context.Context, leaseID string, _ int64, _ alloc.Phase,
) (bool, error) {
	if r.lease != nil && r.lease.ID == leaseID {
		return false, nil
	}

	return true, nil
}

type blockingInventoryRegistrar struct {
	presentLeaseRegistrar
	entered chan struct{}
	proceed chan struct{}
}

type blockingAllocatorRegistrar struct {
	*alloc.Allocator
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	proceed chan struct{}
}

type observingAllocatorRegistrar struct {
	*alloc.Allocator
	secondEntered chan struct{}
}

func (r *blockingAllocatorRegistrar) RegisterNode(
	ctx context.Context,
	registration alloc.NodeRegistration,
) (int64, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 2 {
		select {
		case r.entered <- struct{}{}:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		select {
		case <-r.proceed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	return r.Allocator.RegisterNode(ctx, registration)
}

func (r *observingAllocatorRegistrar) RegisterNode(
	ctx context.Context,
	registration alloc.NodeRegistration,
) (int64, error) {
	select {
	case r.secondEntered <- struct{}{}:
	default:
	}

	return r.Allocator.RegisterNode(ctx, registration)
}

func (r *blockingInventoryRegistrar) ResolveQuarantineFor(
	ctx context.Context, _ string, _ []string, _ int64,
) (int, error) {
	select {
	case r.entered <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case <-r.proceed:
		return 0, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestBoundCompletionWaitsForItsPersistedNode(t *testing.T) {
	p := testPlane(t)
	runner := p.NewRunner()
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("absent holder destroy = %v, want only holder unavailable", err)
	}
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "unrelated", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "unrelated-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register unrelated node: %v", err)
	}
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("unrelated live fleet destroy = %v, want only holder unavailable", err)
	}
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register unreconciled holder: %v", err)
	}
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("unreconciled holder destroy = %v, want only holder unavailable", err)
	}
}

func TestBoundCompletionUsesTheHolderAfterItAdoptsTheLease(t *testing.T) {
	p := testPlane(t)
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	p.AdoptOwnership("holder", "holder-2", []string{"l1"})

	done := make(chan error, 1)
	go func() {
		done <- p.NewRunner().DestroyCompletedBound(
			t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone)
	}()
	cmd, took, err := p.Poll(t.Context(), "holder", "holder-2")
	if err != nil || !took {
		t.Fatalf("poll adopted holder: took=%v err=%v", took, err)
	}
	if cmd.Kind != nodeapi.CommandDestroy || cmd.RequestID != 7 || cmd.JobResult != "Succeeded" {
		t.Fatalf("adopted holder received %+v", cmd)
	}
	if err := p.Result("holder", "holder-2", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("complete destroy: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("bound destroy: %v", err)
	}
}

func TestBoundCompletionIsNotTakenByAReplacementIncarnation(t *testing.T) {
	p := testPlane(t)
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	p.AdoptOwnership("holder", "holder-1", []string{"l1"})
	done := make(chan error, 1)
	go func() {
		done <- p.NewRunner().DestroyCompletedBound(
			t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone)
	}()
	waitFor(t, "bound destroy to queue", func() bool { return p.QueuedForTest("holder") == 1 })
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register replacement: %v", err)
	}
	cmd, took, err := p.Poll(t.Context(), "holder", "holder-2")
	if err != nil {
		t.Fatalf("poll replacement: %v", err)
	}
	if took || cmd.Kind == nodeapi.CommandDestroy {
		t.Fatalf("replacement received the old holder's destroy: %+v", cmd)
	}
	if err := <-done; !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("replacement incarnation destroy = %v, want only holder unavailable", err)
	}
}

func TestBoundCompletionAcceptsTheHoldersKnownEmptyInventory(t *testing.T) {
	p := testPlane(t, WithRegistrar(&countingRegistrar{}))
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
		InventoryKnown: true,
	}); err != nil {
		t.Fatalf("register empty holder: %v", err)
	}
	p.AdoptOwnershipWithInventory("holder", "holder-2", nil, true)
	if err := p.NewRunner().DestroyCompletedBound(
		t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); err != nil {
		t.Fatalf("durably absent holder destroy: %v", err)
	}
}

func TestKnownEmptyInventoryDoesNotReleaseALiveDurableLease(t *testing.T) {
	reg := &presentLeaseRegistrar{lease: &alloc.Lease{ID: "l1", Node: "holder"}}
	p := testPlane(t, WithRegistrar(reg))
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
		InventoryKnown: true,
	}); err != nil {
		t.Fatalf("register empty holder: %v", err)
	}
	if err := p.NewRunner().DestroyCompletedBound(
		t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("live durable lease destroy = %v, want only holder unavailable", err)
	}
}

func TestPeriodicReconciliationInstallsOwnershipBeforeCompletionCanUseAbsence(t *testing.T) {
	reg := &blockingInventoryRegistrar{
		presentLeaseRegistrar: presentLeaseRegistrar{lease: &alloc.Lease{ID: "l1", Node: "holder"}},
		entered:               make(chan struct{}, 1),
		proceed:               make(chan struct{}),
	}
	p := testPlane(t, WithRegistrar(reg))
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register holder: %v", err)
	}

	reconciled := make(chan error, 1)
	go func() {
		_, err := p.ReconcileInventory(t.Context(), "holder", "holder-1", []string{"l1"})
		reconciled <- err
	}()
	<-reg.entered

	destroyed := make(chan error, 1)
	go func() {
		destroyed <- p.NewRunner().DestroyCompletedBound(
			t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone)
	}()
	select {
	case err := <-destroyed:
		t.Fatalf("completion crossed an unfinished inventory decision: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(reg.proceed)
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile inventory: %v", err)
	}

	cmd, took, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !took || cmd.Kind != nodeapi.CommandDestroy {
		t.Fatalf("poll reconciled holder: command=%+v took=%v err=%v", cmd, took, err)
	}
	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("complete destroy: %v", err)
	}
	if err := <-destroyed; err != nil {
		t.Fatalf("bound destroy: %v", err)
	}
}

func TestReplacementRegistrationInvalidatesAbsenceBeforeItsEpochWrite(t *testing.T) {
	now := time.Now().UTC()
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tier := testTier()
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier}, alloc.WithClock(func() time.Time { return now }),
		alloc.WithLeaseTTL(30*time.Second))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	reg := &blockingAllocatorRegistrar{
		Allocator: a,
		entered:   make(chan struct{}, 1),
		proceed:   make(chan struct{}),
	}
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithRegistrar(reg), WithTierCatalog([]config.Tier{tier}))
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register first incarnation: %v", err)
	}
	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "holder"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	for _, phase := range []alloc.Phase{
		alloc.PhaseAssigned, alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy,
	} {
		lease, err = a.Lease(t.Context(), lease.ID)
		if err != nil {
			t.Fatalf("read lease before %s: %v", phase, err)
		}
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}
	now = now.Add(31 * time.Second)
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, err := p.ReconcileInventory(t.Context(), "holder", "holder-1", nil); err != nil {
		t.Fatalf("install first empty inventory: %v", err)
	}

	replacement := make(chan error, 1)
	go func() {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
			InventoryKnown: true, Instances: []string{lease.ID},
		})
		replacement <- err
	}()
	<-reg.entered

	if err := p.NewRunner().DestroyCompletedBound(
		t.Context(), 7, "Succeeded", lease.ID, "holder", lease.Epoch, alloc.PhaseDone,
	); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("completion during replacement registration = %v, want only holder unavailable", err)
	}
	if current, err := a.Lease(t.Context(), lease.ID); err != nil ||
		current.Phase != alloc.PhaseQuarantine {
		t.Fatalf("live replacement lease = %+v err=%v, want charged quarantine", current, err)
	}
	close(reg.proceed)
	if err := <-replacement; err != nil {
		t.Fatalf("register replacement: %v", err)
	}
	owner, ok := p.OwnerOfLease(lease.ID)
	if !ok || owner.Node != "holder" || owner.Incarnation != "holder-2" || !owner.Current {
		t.Fatalf("replacement did not adopt live lease: owner=%+v present=%v", owner, ok)
	}
}

func TestSameNodeRegistrationSerializesTheRealAllocatorCommit(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tier := testTier()
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	reg := &observingAllocatorRegistrar{
		Allocator:     a,
		secondEntered: make(chan struct{}, 2),
	}
	committed := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute, WithRegistrar(reg),
		func(p *Plane) {
			p.afterRegisterNodeForTest = func(ctx context.Context, _ string, _ int64) {
				once.Do(func() {
					close(committed)
					select {
					case <-proceed:
					case <-ctx.Done():
					}
				})
			}
		})

	first := make(chan error, 1)
	go func() {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: "holder-1", VCPU: 8, Memory: 32 * config.GiB,
		})
		first <- err
	}()
	<-committed
	// Discard the observation for A. A is now past its durable commit and held
	// immediately before its plane install.
	<-reg.secondEntered
	if !registrationGuardHeldForTest(p, "holder") {
		t.Fatal("registration guard was released in the post-commit, pre-install gap")
	}

	second := make(chan error, 1)
	go func() {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "holder", Provider: config.ProviderFirecracker,
			Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
		})
		second <- err
	}()
	waitFor(t, "the second registration to queue behind the first", func() bool {
		return registrationCountForTest(p, "holder") == 2
	})

	select {
	case <-reg.secondEntered:
		t.Fatal("second registration reached the allocator before the first committed and installed")
	default:
	}

	close(proceed)
	if err := <-first; err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second Register: %v", err)
	}

	var ledgerEpoch int64
	var ledgerProvider string
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT epoch, provider FROM nodes WHERE name = ?`, "holder").
		Scan(&ledgerEpoch, &ledgerProvider); err != nil {
		t.Fatalf("read durable registration: %v", err)
	}
	if got := p.epochForTest("holder"); got != ledgerEpoch {
		t.Errorf("plane epoch = %d, durable epoch = %d", got, ledgerEpoch)
	}
	if got := p.providerForTest("holder"); got != config.ProviderKind(ledgerProvider) {
		t.Errorf("plane provider = %q, durable provider = %q", got, ledgerProvider)
	}
	if got := p.incarnationForTest("holder"); got != "holder-2" {
		t.Errorf("plane incarnation = %q, want second serialized process", got)
	}
}

func TestPeriodicInventoryAdoptsItsLiveBoundLease(t *testing.T) {
	p := testPlane(t, WithRegistrar(&countingRegistrar{}))
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := p.ReconcileInventory(t.Context(), "holder", "holder-1", []string{"l1"}); err != nil {
		t.Fatalf("reconcile inventory: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- p.NewRunner().DestroyCompletedBound(
			t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone)
	}()
	cmd, took, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !took {
		t.Fatalf("poll reconciled holder: took=%v err=%v", took, err)
	}
	if cmd.Kind != nodeapi.CommandDestroy {
		t.Fatalf("reconciled holder received %+v, want destroy", cmd)
	}
	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("complete destroy: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("bound destroy: %v", err)
	}
}

// A DESTROY IS ADDRESSED, NOT SHOUTED.
//
// It used to go to every live node at once, because the plane did not track
// which one held which request — and the cost is not merely traffic. Every
// dispatch waits up to the command timeout, so the listener serialised teardown
// to one destroy at a time to stop a fleet of twenty turning a shutdown into
// twenty timeouts. Teardown scaled with the size of the fleet rather than with
// the work being torn down.
//
// The plane does know: it recorded the owner when it handed the launch over.
//
// ASSERTED ON WHAT WAS DISPATCHED rather than by racing two pollers for the
// command. A poll can be answered by a redelivered in-flight launch as easily as
// by the destroy, which made the obvious version of this test flaky under load:
// it passed alone and failed in the full run, which is the worst way for a test
// to be wrong.
func TestADestroyGoesOnlyToTheNodeHoldingTheJob(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(2*time.Second))

	// WITH INCARNATIONS, because ownership is only recorded for a node that
	// identified its process — the plane will not attribute a container to a name
	// two hosts could be sharing.
	for _, name := range []string{"holder", "bystander"} {
		if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version:     nodeapi.Version,
			Node:        name,
			Provider:    config.ProviderDocker,
			Deployment:  deployment,
			Incarnation: name + "-1",
			VCPU:        8,
			Memory:      32 * config.GiB,
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	// The holder takes the launch and never answers it, so ownership stands and
	// the container is — as far as the plane knows — running there.
	taken := make(chan struct{})

	go func() {
		if _, _, err := p.Poll(t.Context(), "holder", "holder-1"); err == nil {
			close(taken)
		}
	}()

	// PARKED BEFORE THE LAUNCH IS DISPATCHED, which is what makes this
	// deterministic. A dispatched command is discarded once the command timeout
	// elapses, so starting the poller and the launch together was a race against
	// the scheduler: under the full suite's parallelism the poll goroutine could
	// simply not run inside that window, the launch would expire unclaimed, and
	// the test failed for a reason that had nothing to do with addressing.
	waitFor(t, "the holder to park on a poll",
		func() bool { return p.WaitersForTest("holder") == 1 })

	launched := make(chan error, 1)

	go func() {
		lease := testLease()
		lease.Node = "holder"

		launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	select {
	case <-taken:
	case <-time.After(10 * time.Second):
		t.Fatal("the holder never took the launch")
	}

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 7) }()

	// Nobody is polling now, so a dispatched destroy sits in a queue where it can
	// be counted.
	waitFor(t, "the machine holding the job to be asked to destroy it",
		func() bool { return p.QueuedForTest("holder") > 0 })

	// NOTHING ELSE WAS ASKED. A bystander receiving a destroy for a container it
	// has never heard of answers "not found", which is harmless in itself and is
	// exactly what made teardown wait on every machine in the fleet.
	if got := p.QueuedForTest("bystander"); got != 0 {
		t.Errorf("a machine that never had this job was asked to destroy it (%d queued)", got)
	}

	<-destroyed
	<-launched
}

// A BOUND LEASE GOES TO THE MACHINE IT IS BOUND TO, and `pick` was only asking
// where it was AIMED.
//
// The two answer different questions. target_node is where placement decided the
// work should go; node is where it actually went, written at Bind. Resolving only
// the target meant a lease already running somewhere could be launched again on a
// different host — and with the target empty, the choice fell through to the
// fallback, which walks a MAP. So the second host was picked by hash order: the
// same lease landed on a different machine from run to run.
//
// Everywhere else on this path attributes a lease as COALESCE(node, target_node),
// because a reservation aimed at a machine has already spent its capacity. This
// is the one place that did not.
func TestALeaseGoesToTheNodeItIsBoundTo(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	for _, name := range []string{"a-holder", "z-bystander"} {
		if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: name, Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: name + "-1",
			VCPU: 8, Memory: 32 * config.GiB,
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	// Bound, not merely aimed: target_node is deliberately empty.
	lease := testLease()
	lease.Node = "z-bystander"
	lease.TargetNode = ""

	n, err := p.PickForTest(lease)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}

	if n != "z-bystander" {
		t.Errorf("a lease bound to %q was aimed at %q; the node it is BOUND to is where its "+
			"container already is", lease.Node, n)
	}
}

// AND AN UNBOUND, UNAIMED LEASE PICKS THE SAME HOST EVERY TIME.
//
// The fallback walks the lease's provider preference, which is ordered — but for
// each provider it iterated the node MAP, so among equally acceptable hosts the
// answer came out in hash order. Two identical fleets then place the same lease
// differently, which cannot be reproduced from a log or asserted in a test.
func TestAnUnpinnedLeasePicksDeterministically(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	for _, name := range []string{"m", "a", "z"} {
		if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: name, Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: name + "-1",
			VCPU: 8, Memory: 32 * config.GiB,
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	lease := testLease()
	lease.Node, lease.TargetNode = "", ""

	first, err := p.PickForTest(lease)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}

	for range 50 {
		got, err := p.PickForTest(lease)
		if err != nil {
			t.Fatalf("pick: %v", err)
		}

		if got != first {
			t.Fatalf("the same lease on the same fleet was aimed at %q and then %q", first, got)
		}
	}

	if first != "a" {
		t.Errorf("the fallback chose %q; with nothing else to separate them the name decides, "+
			"so it should be the first in order", first)
	}
}

// A BOUND COMPLETION WHOSE HOLDER WAS REPLACED CORRECTS A SETTLED LEASE AND
// SETTLES NOTHING ITSELF.
//
// The owner record names the process that was handed the launch, and a
// replacement is refused the destroy — that stays. What this adds is the way
// out: when the recorded holder is not the process this node's commands reach,
// the completion asks the ledger whether something else has ALREADY settled the
// lease and, if inventory did so provisionally, corrects the verdict to
// GitHub's. Every state short of that still answers "holder unavailable" — a
// lease still open, a lease quarantined but not yet observed absent after the
// grace — because this plane's cached inventory snapshot has no ordering
// relationship to the quarantine and must not be what frees the capacity.
func TestABoundCompletionSettlesFromTheReplacementsInventoryOnceQuarantined(t *testing.T) {
	now := time.Now().UTC()
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tier := testTier()
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier}, alloc.WithClock(func() time.Time { return now }),
		alloc.WithLeaseTTL(30*time.Second))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithRegistrar(a), WithTierCatalog([]config.Tier{tier}))
	register := func(incarnation string, known bool, instances []string) {
		t.Helper()
		if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: incarnation, VCPU: 8, Memory: 32 * config.GiB,
			InventoryKnown: known, Instances: instances,
		}); err != nil {
			t.Fatalf("register %s: %v", incarnation, err)
		}
	}
	register("holder-1", false, nil)

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "holder"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// THE LAUNCH IS DELIVERED TO holder-1, which is what records it as the owner
	// — a delivery record, the kind no re-registration ever drops.
	const requestID = 7
	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: requestID})
	}()
	cmd, took, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !took || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll launch: took=%v kind=%s err=%v", took, cmd.Kind, err)
	}
	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("answer launch: %v", err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
	for _, phase := range []alloc.Phase{
		alloc.PhaseAssigned, alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy,
	} {
		lease, err = a.Lease(t.Context(), lease.ID)
		if err != nil {
			t.Fatalf("read lease before %s: %v", phase, err)
		}
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}
	if !p.OwnsForTest(lease.ID, "holder", "holder-1") {
		t.Fatal("the delivered launch did not record holder-1 as the owner")
	}

	settle := func() error {
		return p.NewRunner().DestroyCompletedBound(
			t.Context(), requestID, "Succeeded", lease.ID, "holder", lease.Epoch, alloc.PhaseDone)
	}
	unavailable := func(what string) {
		t.Helper()
		if err := settle(); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
			t.Fatalf("%s: bound destroy = %v, want only holder unavailable", what, err)
		}
	}

	// holder-1 DIES, AND A REPLACEMENT WHOSE INVENTORY STILL LISTS THE LEASE
	// REGISTERS. That is a build still running on the backend, and a replacement
	// can never confirm its destroy: the ledger is not asked.
	register("holder-2", true, []string{lease.ID})
	unavailable("a replacement whose inventory lists the lease")

	// A REPLACEMENT WHOSE INVENTORY DOES NOT LIST IT registers. The lease has not
	// expired — nothing has been proved — so the answer is unchanged.
	register("holder-3", true, nil)
	if owner, ok := p.OwnerOfLease(lease.ID); !ok || owner.Current {
		t.Fatalf("owner = %+v (present=%v), want the dead holder-1 recorded as not current", owner, ok)
	}
	unavailable("a replacement with an empty inventory before the lease expired")

	// NOBODY RENEWS, so the reaper quarantines the lease; inside the grace the
	// answer is still unchanged and the capacity is still charged.
	now = now.Add(31 * time.Second)
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if current, err := a.Lease(t.Context(), lease.ID); err != nil || current.Phase != alloc.PhaseQuarantine {
		t.Fatalf("after the TTL the lease is %+v err=%v, want quarantine", current, err)
	}
	unavailable("a quarantined lease inside the grace")

	// PAST THE GRACE AND STILL UNAVAILABLE, because nothing has OBSERVED the
	// absence since: the replacement's registration-time snapshot is all the
	// plane holds, and a snapshot with no ordering against the quarantine is
	// not what may free capacity.
	now = now.Add(6 * time.Minute)
	unavailable("a quarantined lease past the grace with no fresh inventory")
	if _, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("a completion settled the lease from a cached snapshot: %v", err)
	}

	// THE HOST'S OWN INVENTORY, TAKEN NOW, settles it provisionally...
	if _, err := a.Reconcile(t.Context(), "holder", nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("a fresh empty inventory past the grace left the lease open: %v", err)
	}
	if outcome, err := a.HistoryOutcome(t.Context(), lease.ID); err != nil ||
		outcome != string(alloc.PhaseFailed) {
		t.Fatalf("provisional outcome = %q err=%v, want inventory's failed", outcome, err)
	}

	// ...AND THE COMPLETION CORRECTS THAT VERDICT TO GITHUB'S.
	if err := settle(); err != nil {
		t.Fatalf("bound destroy after inventory settled the lease: %v", err)
	}
	outcome, err := a.HistoryOutcome(t.Context(), lease.ID)
	if err != nil || outcome != string(alloc.PhaseDone) {
		t.Fatalf("history outcome = %q err=%v, want done", outcome, err)
	}

	// THE RECORD ENDS WITH THE LEASE. While the lease was open the record kept
	// holder-1 entitled to drain what it held; now that the ledger says the
	// lease is terminal nothing can be entitled to anything about it, and a
	// record kept past that point is one map entry per dead holder for the life
	// of the plane.
	if p.OwnsForTest(lease.ID, "holder", "holder-1") {
		t.Fatal("settlement kept the owner record of a lease the ledger says is over")
	}
}

// A HOLDER NOBODY REPLACED IS AS UNREACHABLE AS ONE THAT WAS. A host that died
// and never came back leaves its lease to an operator's forced release, and
// the completion's retry must be able to finish on that rather than answer
// "holder unavailable" for the life of the process.
func TestABoundCompletionFinishesOnAForcedReleaseWhenItsHostNeverReturns(t *testing.T) {
	now := time.Now().UTC()
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tier := testTier()
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier}, alloc.WithClock(func() time.Time { return now }),
		alloc.WithLeaseTTL(30*time.Second))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	// THE DEFAULT POLL WINDOW, WHICH EVERY OTHER TEST IN THIS FILE USES. This one
	// set 20ms, and that is what made it flaky: the launch below is dispatched
	// from a GOROUTINE and the poll that collects it runs immediately, so the two
	// race, and under -race with the whole package in parallel the goroutine
	// loses. MEASURED rather than reasoned about — driving the window to 1ns
	// reproduces the CI failure exactly, `poll launch: took=false kind= err=<nil>`
	// at the same line.
	//
	// A LONG POLL RETURNS THE INSTANT A COMMAND IS QUEUED, so the window is a
	// ceiling on a wait and never a delay: the default costs this test nothing in
	// the ordinary case and only bounds how long it would take to report a
	// launch that never arrives.
	//
	// AND NOTHING HERE DEPENDS ON IT EXPIRING, which is the question that has to
	// be asked before widening any deadline — the nodeclient harness was widened
	// on that assumption and the package stopped finishing. Checked: this test
	// polls exactly once and never for a command that does not come, and it
	// forgets the host with ForgetForTest rather than waiting one out. The window
	// also sets staleAfter (4x), so 20ms gave an 80ms stale window and the risk of
	// the holder being expired out from under the launch; the default removes
	// that too.
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithRegistrar(a), WithTierCatalog([]config.Tier{tier}))
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register holder: %v", err)
	}

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "holder"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	const requestID = 7
	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: requestID})
	}()
	cmd, took, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !took || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll launch: took=%v kind=%s err=%v", took, cmd.Kind, err)
	}
	if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("answer launch: %v", err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
	for _, phase := range []alloc.Phase{
		alloc.PhaseAssigned, alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy,
	} {
		lease, err = a.Lease(t.Context(), lease.ID)
		if err != nil {
			t.Fatalf("read lease before %s: %v", phase, err)
		}
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}

	// THE HOST DIES AND THE PLANE FORGETS IT. Nothing replaces it.
	p.ForgetForTest("holder")

	settle := func() error {
		return p.NewRunner().DestroyCompletedBound(
			t.Context(), requestID, "Succeeded", lease.ID, "holder", lease.Epoch, alloc.PhaseDone)
	}
	if err := settle(); !errors.Is(err, server.ErrHolderUnavailable) || errors.Is(err, server.ErrCustody) {
		t.Fatalf("a dead, unreplaced holder: bound destroy = %v, want only holder unavailable", err)
	}

	now = now.Add(31 * time.Second)
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if err := settle(); !errors.Is(err, server.ErrHolderUnavailable) {
		t.Fatalf("a quarantined lease with no host to observe it: bound destroy = %v, want holder unavailable", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("a completion settled a lease no host observed absent: %v", err)
	}

	// THE OPERATOR SETTLES IT, and the completion's retry finishes on that
	// verdict rather than rewriting it: the operator's assertion stands.
	if _, err := a.ForceRelease(t.Context(), lease.ID); err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}
	if err := settle(); err != nil {
		t.Fatalf("bound destroy after the operator released the lease: %v", err)
	}
	if outcome, err := a.HistoryOutcome(t.Context(), lease.ID); err != nil ||
		outcome != string(alloc.PhaseFailed) {
		t.Fatalf("history outcome = %q err=%v, want the operator's failed preserved", outcome, err)
	}
	if p.OwnsForTest(lease.ID, "holder", "holder-1") {
		t.Fatal("the owner record of a settled lease was kept")
	}
}
