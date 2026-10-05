package nodeclient_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
)

// A NODE SET TO HAND OVER STOPS AT ONCE AND LEAVES ITS COMPUTE RUNNING (#374).
//
// The compute below never stops holding, so a drain would never end; the handoff
// returns without waiting, destroys nothing, and does not move the work into
// custody either, because nobody is superseding this node: the next process of it
// adopts what it finds when it registers. It does withdraw, so the plane stops
// placing on it at once rather than when its silence runs out, and the cache it
// was asked to start serving was started, once, after it registered.
func TestANodeSetToHandOverStopsWithoutWaitingOrDestroying(t *testing.T) {
	t.Parallel()

	plane, c := harness(t)

	compute := &fakeCompute{holding: true}
	var ready atomic.Int32

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
			// An hour, so a drain could not end on its own inside this test.
			DrainTimeout:   time.Hour,
			HandOverOnStop: true,
			Ready:          func() { ready.Add(1) },
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })
	waitFor(t, func() bool { return ready.Load() == 1 })

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a handoff ended the node with %v; a stop that did what it was asked "+
				"exits cleanly", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a node set to hand over waited for the compute it holds")
	}

	if nodes := plane.Nodes(); len(nodes) != 0 {
		t.Errorf("the handoff did not withdraw; the plane still places on %v", nodes)
	}
	if got := ready.Load(); got != 1 {
		t.Errorf("Ready ran %d times, want once", got)
	}

	_, _, destroyed := compute.snapshot()
	if len(destroyed) != 0 {
		t.Errorf("the handoff destroyed %v; the compute must be left for the next process", destroyed)
	}

	compute.mu.Lock()
	superseded := compute.supersededCalls
	compute.mu.Unlock()
	if superseded != 0 {
		t.Error("the handoff moved the running work into custody as if superseded")
	}
}

// A DRAIN REQUEST OVERRIDES HANDOFF (#374). A stop that removes the node or its
// guests' networking asks for a drain first, and the node then waits for the
// compute it holds instead of leaving it behind.
func TestADrainRequestOverridesHandoff(t *testing.T) {
	t.Parallel()

	plane, c := harness(t)
	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:           testNodeVCPU,
			Memory:         testNodeMemory,
			Provider:       config.ProviderDocker,
			Deployment:     deployment,
			Log:            slog.New(slog.DiscardHandler),
			Backoff:        20 * time.Millisecond,
			SweepEvery:     10 * time.Millisecond,
			DrainTimeout:   time.Hour,
			HandOverOnStop: true,
			DrainRequested: func() bool { return true },
		})
	}()

	waitFor(t, func() bool { return len(plane.Nodes()) == 1 })
	cancel()

	select {
	case err := <-done:
		t.Fatalf("a node asked to drain handed over instead (returned %v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end once the compute was gone")
	}
}

// READY WAITS FOR A RECOVERY THAT SUCCEEDED, and a drain that never saw one
// still answers the guests it waits for. The cache a handed-over guest reaches
// must be served by a process that can answer for it, but a stop during
// recovery that keeps failing must not leave those guests unanswered while it
// waits for them.
func TestReadyWaitsForRecoveryAndADrainStillAnswers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		recover bool
	}{
		{name: "recovery succeeds", recover: true},
		{name: "a drain stops a node whose recovery keeps failing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, c := harness(t)
			compute := &fakeCompute{holding: true, recoverErr: errors.New("heartbeat failed")}
			var ready atomic.Int32

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)

			go func() {
				done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
					VCPU:         testNodeVCPU,
					Memory:       testNodeMemory,
					Provider:     config.ProviderDocker,
					Deployment:   deployment,
					Log:          slog.New(slog.DiscardHandler),
					Backoff:      20 * time.Millisecond,
					SweepEvery:   10 * time.Millisecond,
					DrainTimeout: time.Hour,
					Ready:        func() { ready.Add(1) },
				})
			}()

			waitFor(t, func() bool {
				compute.mu.Lock()
				defer compute.mu.Unlock()

				return compute.recovered >= 3
			})
			if got := ready.Load(); got != 0 {
				t.Fatalf("Ready ran %d times while recovery was failing", got)
			}

			if tc.recover {
				compute.mu.Lock()
				compute.recoverErr = nil
				compute.mu.Unlock()
			}
			if !tc.recover {
				cancel()
			}
			waitFor(t, func() bool { return ready.Load() == 1 })

			cancel()
			compute.mu.Lock()
			compute.holding = false
			compute.mu.Unlock()

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the node did not stop")
			}
			if got := ready.Load(); got != 1 {
				t.Errorf("Ready ran %d times, want once", got)
			}
		})
	}
}

// A REQUESTED DRAIN RECOVERS BEFORE IT DECIDES (#374). A process stopped before
// its first recovery knows of nothing it holds while guests a previous process
// left are still running, so a stop that must drain recovers first, for as long
// as that takes, and only then judges whether it holds anything.
func TestARequestedDrainRecoversBeforeItDecides(t *testing.T) {
	t.Parallel()

	_, c := harness(t)
	compute := &fakeCompute{recoverErr: errors.New("the provider could not list its guests")}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:           testNodeVCPU,
			Memory:         testNodeMemory,
			Provider:       config.ProviderDocker,
			Deployment:     deployment,
			Log:            slog.New(slog.DiscardHandler),
			Backoff:        20 * time.Millisecond,
			SweepEvery:     10 * time.Millisecond,
			DrainTimeout:   time.Hour,
			HandOverOnStop: true,
			DrainRequested: func() bool { return true },
		})
	}()

	attempts := func() int {
		compute.mu.Lock()
		defer compute.mu.Unlock()

		return compute.recovered
	}

	waitFor(t, func() bool { return attempts() >= 2 })
	cancel()
	before := attempts()

	select {
	case err := <-done:
		t.Fatalf("a stop asked to drain returned (%v) before this process ever recovered", err)
	case <-time.After(300 * time.Millisecond):
	}
	if attempts() <= before {
		t.Error("a stop asked to drain did not keep recovering")
	}

	compute.mu.Lock()
	compute.recoverErr = nil
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop did not end once recovery succeeded and nothing was held")
	}
}

// A HANDOVER WHOSE WITHDRAWAL DID NOT LAND DRAINS (#374). The next process takes
// over only what a process the plane recorded as withdrawn owned; without that
// record the leases stay with an exited process and a completion's destroy never
// reaches the guest, so the node keeps serving its compute instead.
func TestAHandoverWhoseWithdrawalFailsDrains(t *testing.T) {
	t.Parallel()

	_, c, b := breakableHarness(t)
	b.failWithdraw.Store(true)
	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:           testNodeVCPU,
			Memory:         testNodeMemory,
			Provider:       config.ProviderDocker,
			Deployment:     deployment,
			Log:            slog.New(slog.DiscardHandler),
			Backoff:        20 * time.Millisecond,
			SweepEvery:     10 * time.Millisecond,
			DrainTimeout:   time.Hour,
			HandOverOnStop: true,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })
	cancel()
	waitFor(t, func() bool { return b.withdrawAttempts.Load() >= 3 })

	select {
	case err := <-done:
		t.Fatalf("the node handed over (returned %v) though its withdrawal never landed", err)
	case <-time.After(300 * time.Millisecond):
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end once the compute was gone")
	}
}

// A FIRST REGISTRATION MADE BY A REQUESTED DRAIN STARTS THE JANITOR (#374). A
// process that never registered before its stop registers to recover what its
// host runs, and the leases recovery adopts are renewed by the janitor from then
// on, exactly as after an ordinary registration, for as long as the drain lasts.
func TestADrainThatRegistersFirstKeepsItsLeasesAlive(t *testing.T) {
	t.Parallel()

	_, c, b := breakableHarness(t)
	b.failRegister.Store(true)
	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:           testNodeVCPU,
			Memory:         testNodeMemory,
			Provider:       config.ProviderDocker,
			Deployment:     deployment,
			Log:            slog.New(slog.DiscardHandler),
			Backoff:        20 * time.Millisecond,
			SweepEvery:     10 * time.Millisecond,
			DrainTimeout:   time.Hour,
			HandOverOnStop: true,
			DrainRequested: func() bool { return true },
		})
	}()

	waitFor(t, func() bool { return b.registerAttempts.Load() >= 2 })
	cancel()
	if got := compute.aliveCount(); got != 0 {
		t.Fatalf("the janitor started (%d) before any registration succeeded", got)
	}

	b.failRegister.Store(false)
	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end once the compute was gone")
	}
}

// A SUCCESSOR STOPPED BEFORE IT RECOVERED, WHOSE WITHDRAWAL FAILS, RECOVERS AND
// DRAINS (#374). It registered, so the plane may already have handed it the
// guests it reported, while its own Holding() still says nothing; exiting on that
// would leave those leases with a process that never withdrew.
func TestAnUnrecoveredHandoverWhoseWithdrawalFailsRecoversAndDrains(t *testing.T) {
	t.Parallel()

	_, c, b := breakableHarness(t)
	b.failWithdraw.Store(true)
	// THE FIRST RECOVERY IS HELD until the stop, so this process registers and
	// never recovers; the one after the stop is released below.
	compute := &fakeCompute{recoverGate: make(chan struct{}), recoverStarted: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:           testNodeVCPU,
			Memory:         testNodeMemory,
			Provider:       config.ProviderDocker,
			Deployment:     deployment,
			Log:            slog.New(slog.DiscardHandler),
			Backoff:        20 * time.Millisecond,
			SweepEvery:     10 * time.Millisecond,
			DrainTimeout:   time.Hour,
			HandOverOnStop: true,
		})
	}()

	<-compute.recoverStarted
	cancel()
	waitFor(t, func() bool { return b.withdrawAttempts.Load() >= 3 })

	select {
	case err := <-done:
		t.Fatalf("an unrecovered node whose withdrawal failed exited (%v) as if it held nothing", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Recovery is let through and finds the guest the previous process left.
	compute.setHolding(true)
	close(compute.recoverGate)

	select {
	case err := <-done:
		t.Fatalf("the node stopped (%v) while it held a guest it had just recovered", err)
	case <-time.After(300 * time.Millisecond):
	}

	compute.setHolding(false)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end once the compute was gone")
	}
}

// AND SO DOES ONE THE PLANE HAS SINCE FORGOTTEN (#374). A registration the plane
// forgot clears the wire version, which used to read as "never registered"; but
// what that registration was given is still this process's to account for, so
// it registers again and recovers before it believes it holds nothing.
func TestAForgottenUnrecoveredHandoverRecoversAndDrains(t *testing.T) {
	t.Parallel()

	plane, c, _ := breakableHarness(t)
	compute := &fakeCompute{recoverGate: make(chan struct{}), recoverStarted: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:           testNodeVCPU,
			Memory:         testNodeMemory,
			Provider:       config.ProviderDocker,
			Deployment:     deployment,
			Log:            slog.New(slog.DiscardHandler),
			Backoff:        20 * time.Millisecond,
			SweepEvery:     10 * time.Millisecond,
			DrainTimeout:   time.Hour,
			HandOverOnStop: true,
		})
	}()

	<-compute.recoverStarted
	// The plane forgets this registration, so the withdrawal is answered
	// "unregistered" and clears the client's wire version.
	plane.ForgetForTest("n1")
	cancel()

	select {
	case err := <-done:
		t.Fatalf("a forgotten, unrecovered node exited (%v) as if it held nothing", err)
	case <-time.After(300 * time.Millisecond):
	}
	if !c.EverRegistered() {
		t.Fatal("the client forgot it had ever registered")
	}

	compute.setHolding(true)
	close(compute.recoverGate)

	select {
	case err := <-done:
		t.Fatalf("the node stopped (%v) while it held a guest it had just recovered", err)
	case <-time.After(300 * time.Millisecond):
	}

	compute.setHolding(false)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end once the compute was gone")
	}
}
