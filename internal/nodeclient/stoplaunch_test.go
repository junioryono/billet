package nodeclient_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// stoppableLaunch holds a launch open until release closes or its context
// ends, and records whether that context had ended when the launch returned.
type stoppableLaunch struct {
	*fakeCompute

	started chan struct{}
	release chan struct{}
	opened  sync.Once

	mu        sync.Mutex
	cancelled error
}

// newStoppableLaunch is a stoppableLaunch whose held launch is let go when the
// test ends, so a failed assertion cannot leave the loop's join waiting on it.
func newStoppableLaunch(t *testing.T) *stoppableLaunch {
	t.Helper()

	s := &stoppableLaunch{fakeCompute: &fakeCompute{holding: true},
		started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(s.open)

	return s
}

// open lets the held launch finish; safe to call more than once.
func (s *stoppableLaunch) open() { s.opened.Do(func() { close(s.release) }) }

func (s *stoppableLaunch) Launch(
	ctx context.Context, lease *alloc.Lease, tier *nodeapi.TierSpec, job dispatch.Job,
) error {
	close(s.started)

	select {
	case <-s.release:
	case <-ctx.Done():
	}

	s.mu.Lock()
	s.cancelled = ctx.Err()
	s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	return s.fakeCompute.Launch(ctx, lease, tier, job)
}

func (s *stoppableLaunch) launchCancelled() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cancelled
}

// runStopping runs the loop with a handoff on stop, cancelled by the returned
// function, and answers what Run returned on the channel.
func runStopping(
	t *testing.T, c *nodeclient.Client, compute nodeclient.Compute, concurrency int, hurry <-chan struct{},
) (context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:              testNodeVCPU,
			Memory:            testNodeMemory,
			Provider:          config.ProviderDocker,
			GuestOS:           []config.GuestOS{config.GuestLinux},
			Deployment:        deployment,
			Log:               slog.New(slog.DiscardHandler),
			Backoff:           20 * time.Millisecond,
			DrainTimeout:      time.Hour,
			HandOverOnStop:    true,
			LaunchConcurrency: concurrency,
			Hurry:             hurry,
		})
	}()

	return cancel, done
}

// A LAUNCH ALREADY RUNNING WHEN A HANDOFF STOP ARRIVES FINISHES (#393). The
// first live handover's stop cancelled a launch while it sized its root disk;
// the launch is seconds of work, so the stop lets it finish, the plane hears
// that it started, and only then does the node withdraw and hand over. Both
// the overlapped launches of a Firecracker node and the inline one of every
// other backend.
func TestALaunchAlreadyRunningFinishesAcrossAHandoffStop(t *testing.T) {
	t.Parallel()

	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency %d", concurrency), func(t *testing.T) {
			t.Parallel()

			plane, c := harness(t)
			compute := newStoppableLaunch(t)
			stop, done := runStopping(t, c, compute, concurrency, nil)

			waitFor(t, func() bool { return len(plane.Nodes()) == 1 })

			launched := make(chan error, 1)
			go func() {
				launched <- plane.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 7})
			}()

			<-compute.started
			stop()

			select {
			case err := <-done:
				t.Fatalf("the node stopped (%v) while a launch it had begun was still running", err)
			case <-time.After(200 * time.Millisecond):
			}
			if nodes := plane.Nodes(); len(nodes) != 1 {
				t.Errorf("the node withdrew while its launch was running; the plane places on %v", nodes)
			}

			compute.open()

			if err := <-launched; err != nil {
				t.Errorf("the plane heard the launch fail (%v); the stop cancelled it", err)
			}
			if err := compute.launchCancelled(); err != nil {
				t.Errorf("the launch ran under a context the stop ended: %v", err)
			}

			select {
			case err := <-done:
				if err != nil {
					t.Errorf("the handoff ended the node with %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the node did not hand over once its launch had finished")
			}
			if nodes := plane.Nodes(); len(nodes) != 0 {
				t.Errorf("the handoff did not withdraw; the plane still places on %v", nodes)
			}
		})
	}
}

// A SECOND SIGNAL CANCELS A LAUNCH THE STOP IS WAITING FOR. The operator's
// "stop waiting" reaches the launch too, which then unwinds as any cancelled
// launch does, and the node stops.
func TestASecondSignalCancelsALaunchInFlight(t *testing.T) {
	t.Parallel()

	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency %d", concurrency), func(t *testing.T) {
			t.Parallel()

			plane, c := harness(t)
			compute := newStoppableLaunch(t)
			hurry := make(chan struct{})
			stop, done := runStopping(t, c, compute, concurrency, hurry)

			waitFor(t, func() bool { return len(plane.Nodes()) == 1 })

			launched := make(chan error, 1)
			go func() {
				launched <- plane.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 7})
			}()

			<-compute.started
			stop()
			close(hurry)

			select {
			case err := <-launched:
				if err == nil {
					t.Error("the plane heard the launch succeed although the second signal cancelled it")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the second signal did not reach the launch the stop was waiting for")
			}
			if err := compute.launchCancelled(); !errors.Is(err, context.Canceled) {
				t.Errorf("the launch's context ended with %v, want the second signal's cancellation", err)
			}

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the node did not stop after the second signal")
			}
		})
	}
}
