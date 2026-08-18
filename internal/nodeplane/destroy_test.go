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

func (r *presentLeaseRegistrar) ResolveQuarantineForCompletion(
	_ context.Context, _ string, leaseID string, _, _ int64, _ alloc.Phase,
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
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("absent holder destroy = %v, want custody", err)
	}
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "unrelated", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "unrelated-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register unrelated node: %v", err)
	}
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("unrelated live fleet destroy = %v, want custody", err)
	}
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register unreconciled holder: %v", err)
	}
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("unreconciled holder destroy = %v, want custody", err)
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
	if err := <-done; err == nil {
		t.Fatal("bound destroy accepted a replacement incarnation as its holder")
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
		t.Context(), 7, "Succeeded", "l1", "holder", 1, alloc.PhaseDone); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("live durable lease destroy = %v, want custody", err)
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
	); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("completion during replacement registration = %v, want custody", err)
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
