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

// A HANDOVER KEEPS WHAT IS KNOWN OF ITS JOBS, ONCE, AND ONLY ONCE IT HAS
// WITHDRAWN (#384). The next process takes the jobs over only from a process
// the plane recorded as withdrawn, so the checkpoints are written then; a
// stop asked to drain, or one whose withdrawal did not land, drains and
// measures its jobs to the end itself, and keeps nothing.
func TestAHandoverKeepsWhatItMeasuredOnceWithdrawn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		drain         bool
		failWithdraw  bool
		wantHandOvers int
	}{
		{name: "handed over", wantHandOvers: 1},
		{name: "asked to drain", drain: true},
		{name: "withdrawal refused", failWithdraw: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plane, c, b := breakableHarness(t)
			b.failWithdraw.Store(tc.failWithdraw)
			compute := &fakeCompute{holding: true}
			var placedAtHandOver atomic.Int32
			placedAtHandOver.Store(-1)
			compute.onHandOver = func() { placedAtHandOver.Store(int32(len(plane.Nodes()))) }

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
					DrainRequested: func() bool { return tc.drain },
				})
			}()

			waitFor(t, func() bool { return compute.aliveCount() == 1 })
			cancel()
			if !tc.drain && !tc.failWithdraw {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("the node did not hand over")
				}
			} else {
				// A DRAIN WAITS FOR ITS COMPUTE; let it go once the stop is under way.
				if tc.failWithdraw {
					waitFor(t, func() bool { return b.withdrawAttempts.Load() >= 3 })
				}
				compute.setHolding(false)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("the drain did not end once the compute was gone")
				}
			}

			if got := compute.handOverCount(); got != tc.wantHandOvers {
				t.Errorf("HandOver ran %d times, want %d", got, tc.wantHandOvers)
			}
			if tc.wantHandOvers == 1 && placedAtHandOver.Load() != 0 {
				t.Errorf("HandOver ran while the plane still placed on %d nodes; it must follow the withdrawal",
					placedAtHandOver.Load())
			}
		})
	}
}
