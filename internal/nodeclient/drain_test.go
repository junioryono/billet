package nodeclient_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
)

// A NODE FINISHES ITS CONTAINERS BEFORE IT STOPS, and it does NOT hand them to
// custody on the way out.
//
// The supersession drain looks like the same thing and is not. It opens with
// compute.Superseded(), which moves running work into custody BECAUSE the
// control plane now routes those completions to whichever process holds the
// name. On a local SIGTERM nobody is taking over: the completions still route
// here, and moving the work to custody would strand the very reports this drain
// is waiting for.
func TestASignalledNodeDrainsWithoutHandingOverCustody(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			Provider:     config.ProviderDocker,
			Deployment:   deployment,
			Log:          slog.New(slog.DiscardHandler),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: 20 * time.Second,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	// It keeps tending, which is what advances custody and lets held compute
	// finish. A node that simply returned would leave those leases renewed by
	// nobody.
	waitFor(t, func() bool { return compute.tended() > 0 })

	select {
	case <-done:
		t.Fatal("a signalled node stopped while it was still holding compute")
	case <-time.After(300 * time.Millisecond):
	}

	// AND IT DID NOT HAND OVER CUSTODY. This is the assertion that separates a
	// signal drain from a supersession drain, and the only one that fails if the
	// supersession path is reused wholesale.
	if n := compute.supersededCount(); n != 0 {
		t.Errorf("a signalled node moved %d running job(s) into custody; nobody is "+
			"taking over, so their completions still belong here", n)
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, ctx.Err()) {
			t.Errorf("want the cancellation once drained, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a drained node never stopped")
	}
}

// A node holding nothing stops at once, AND SAYS NOTHING ABOUT DRAINING.
//
// The early return looks redundant — waitForHolding's own loop condition is
// false immediately, so the node stops either way — and it is not. Without it
// every ordinary restart of an idle node announces "draining: waiting for the
// compute still running here" and then exits, which tells an operator reading
// the journal that work was in flight when none was. A log line that is only
// sometimes true is worse than no log line.
func TestANodeHoldingNothingStopsImmediatelyAndSaysNothingAboutDraining(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	compute := &fakeCompute{holding: false}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		logMu  sync.Mutex
		logged bytes.Buffer
	)

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log: slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logged},
				&slog.HandlerOptions{Level: slog.LevelInfo})),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: time.Hour,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a node with nothing running waited out a drain it did not need")
	}

	logMu.Lock()
	defer logMu.Unlock()

	if strings.Contains(logged.String(), "draining") {
		t.Errorf("an idle node announced a drain it did not perform:\n%s", logged.String())
	}
}

// lockedWriter serialises writes from the janitor goroutine and the loop, which
// both log.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.w.Write(p)
}

// The drain is bounded. A container that never reports must not keep the process
// alive forever — the teardown has to get its turn.
func TestANodeDrainIsBounded(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	// Holding, and never stops holding.
	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			Provider:     config.ProviderDocker,
			Deployment:   deployment,
			Log:          slog.New(slog.DiscardHandler),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: 200 * time.Millisecond,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a node drain with no bound never let the process stop")
	}
}

// A second signal ends the node's wait too. Without it, an operator who does not
// want to wait out a six-hour drain has only the signal that kills the process,
// which is the outcome the drain exists to avoid.
func TestASecondSignalEndsTheNodesDrain(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	// Holding, and never stops holding: only the hurry can end this.
	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	hurry := make(chan struct{})

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
			// An hour, so waiting it out is not something this test could do by
			// accident.
			DrainTimeout: time.Hour,
			Hurry:        hurry,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	waitFor(t, func() bool { return compute.tended() > 0 })

	select {
	case <-done:
		t.Fatal("the node stopped before it was hurried")
	case <-time.After(200 * time.Millisecond):
	}

	close(hurry)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second signal did not end the node's drain")
	}
}
