package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// sweepingRunner is a Runner that also implements Sweeper, recording what the
// control plane asked of it and when.
type sweepingRunner struct {
	fakeRunner

	keepAliveStarted chan struct{}
	keepAliveExited  chan struct{}
	startedOnce      atomic.Bool

	// keepAliveCancelled is closed once the keep-alive has seen its context end,
	// and holdAfterCancel, when set, keeps it from returning until it is closed:
	// a keep-alive still finishing a renewal as the control plane stops.
	keepAliveCancelled chan struct{}
	holdAfterCancel    chan struct{}

	tends  atomic.Int64
	sweeps atomic.Int64
}

func (s *sweepingRunner) KeepAlive(ctx context.Context) {
	if s.startedOnce.CompareAndSwap(false, true) {
		close(s.keepAliveStarted)
	}

	<-ctx.Done()

	if s.keepAliveCancelled != nil {
		close(s.keepAliveCancelled)
	}

	if s.holdAfterCancel != nil {
		<-s.holdAfterCancel
	}

	if s.keepAliveExited != nil {
		close(s.keepAliveExited)
	}
}

// RUN DOES NOT RETURN WHILE ITS KEEP-ALIVE IS STILL RUNNING. A control plane
// whose Run came back with a loop still live would have the ledger closed under
// that loop by its caller's next defer. The keep-alive here, once cancelled,
// holds until released: Run must still be inside when the keep-alive has seen
// its cancellation, and return only after the release.
func TestRunReturnsOnlyOnceItsKeepAliveHas(t *testing.T) {
	t.Parallel()

	runner := &sweepingRunner{
		keepAliveStarted:   make(chan struct{}),
		keepAliveExited:    make(chan struct{}),
		keepAliveCancelled: make(chan struct{}),
		holdAfterCancel:    make(chan struct{}),
	}

	prov := &fakeProvisioner{
		onEnsure: func(string) error {
			return errors.New("stopping the test here")
		},
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("billet-4vcpu-a")})

	srv := New(a, prov, []config.Tier{tier("billet-4vcpu-a")}, "billet-test", nil,
		WithNodeRunner(runner))

	done := make(chan error, 1)

	go func() { done <- srv.Run(t.Context()) }()

	select {
	case <-runner.keepAliveCancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the keep-alive was never cancelled; Run did not reach its return")
	}

	// THE KEEP-ALIVE IS HELD, SO A Run THAT JOINS IT CANNOT HAVE RETURNED. One
	// that does not join returns at once, and this window is how long it is
	// given to show it: a correct Run is never failed by it.
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) while its keep-alive was still running", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(runner.holdAfterCancel)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return once its keep-alive had")
	}

	select {
	case <-runner.keepAliveExited:
	default:
		t.Error("Run returned before its keep-alive did")
	}
}

func (s *sweepingRunner) Tend(context.Context) error {
	s.tends.Add(1)

	return nil
}

func (s *sweepingRunner) Sweep(context.Context) error {
	s.sweeps.Add(1)

	return nil
}

// The control plane starts the keep-alive BEFORE it reconciles scale sets.
//
// Reconciliation is a network round trip per tier against GitHub, and it sits
// between recovery — which renews each adopted lease once, as it adopts — and
// the startup reap. On a bad day it takes longer than a lease TTL, and the
// reaper then terminalizes a lease billet is deliberately holding, hands its
// capacity back, and lets a listener advertise it while the container runs on.
//
// So this asserts the ORDER, using a provisioner that blocks: if the keep-alive
// were started after reconciliation, it would never run at all here.
func TestKeepAliveStartsBeforeScaleSetReconciliation(t *testing.T) {
	t.Parallel()

	blocked := make(chan struct{})
	runner := &sweepingRunner{keepAliveStarted: make(chan struct{})}

	prov := &fakeProvisioner{
		onEnsure: func(string) error {
			// Stands in for a slow round trip to GitHub.
			<-blocked

			return errors.New("stopping the test here")
		},
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("billet-4vcpu-a")})

	srv := New(a, prov, []config.Tier{tier("billet-4vcpu-a")}, "billet-test", nil,
		WithNodeRunner(runner))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- srv.Run(ctx) }()

	select {
	case <-runner.keepAliveStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the keep-alive was not started before scale-set reconciliation; a slow " +
			"reconcile would let the reaper reclaim capacity billet is holding")
	}

	close(blocked)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the control plane did not stop")
	}
}

// The keep-alive GOROUTINE exits when the control plane returns.
//
// The first version of this waited for Run to return and called that proof.
// It is not: Run returning says nothing about the goroutine it started, and
// removing the deferred cancel would have left the goroutine alive until the
// test's own context was torn down — passing either way. This waits for the
// goroutine itself.
func TestTheKeepAliveGoroutineExitsWithTheControlPlane(t *testing.T) {
	t.Parallel()

	runner := &sweepingRunner{
		keepAliveStarted: make(chan struct{}),
		keepAliveExited:  make(chan struct{}),
	}

	prov := &fakeProvisioner{
		onEnsure: func(string) error {
			return errors.New("stopping the test here")
		},
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("billet-4vcpu-a")})

	srv := New(a, prov, []config.Tier{tier("billet-4vcpu-a")}, "billet-test", nil,
		WithNodeRunner(runner))

	// A context of its OWN, never cancelled by this test. If Run does not cancel
	// the keep-alive itself, nothing here will, and the wait below times out.
	done := make(chan error, 1)

	// DERIVED FROM Background, NOT from t.Context(), and the linter is wrong to
	// want the latter. t.Context() is cancelled during teardown, which would
	// cancel the keep-alive for us — so the assertion below would pass whether or
	// not Run cancels it, which is the vacuity this test exists to close.
	//
	// The cancel is deferred rather than absent, so a BROKEN implementation leaks
	// its goroutine only until this test returns instead of until the process
	// does. Deferring it does not weaken the assertion: it runs after the wait
	// below has already succeeded or failed.
	//nolint:usetesting // a context the test does not cancel until afterwards is the point
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	go func() { done <- srv.Run(runCtx) }()

	select {
	case <-runner.keepAliveStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the keep-alive never started")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the control plane did not stop")
	}

	select {
	case <-runner.keepAliveExited:
	case <-time.After(10 * time.Second):
		t.Fatal("the keep-alive goroutine outlived the control plane; it will heartbeat " +
			"leases for a process that has stopped")
	}
}
