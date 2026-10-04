package nodeclient_test

import (
	"context"
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
