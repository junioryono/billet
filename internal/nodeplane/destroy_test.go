package nodeplane

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

func TestBoundCompletionWaitsForItsPersistedNode(t *testing.T) {
	p := testPlane(t)
	runner := p.NewRunner()
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder"); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("absent holder destroy = %v, want custody", err)
	}
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "unrelated", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "unrelated-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register unrelated node: %v", err)
	}
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder"); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("unrelated live fleet destroy = %v, want custody", err)
	}
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register unreconciled holder: %v", err)
	}
	if err := runner.DestroyCompletedBound(t.Context(), 7, "Succeeded", "l1", "holder"); !errors.Is(err, server.ErrCustody) {
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
			t.Context(), 7, "Succeeded", "l1", "holder")
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
			t.Context(), 7, "Succeeded", "l1", "holder")
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
	p := testPlane(t)
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "holder", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "holder-2", VCPU: 8, Memory: 32 * config.GiB,
		InventoryKnown: true,
	}); err != nil {
		t.Fatalf("register empty holder: %v", err)
	}
	p.AdoptOwnershipWithInventory("holder", "holder-2", nil, true)
	if err := p.NewRunner().DestroyCompletedBound(
		t.Context(), 7, "Succeeded", "l1", "holder"); err != nil {
		t.Fatalf("known-empty holder destroy: %v", err)
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
			t.Context(), 7, "Succeeded", "l1", "holder")
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
