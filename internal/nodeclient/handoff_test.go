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
