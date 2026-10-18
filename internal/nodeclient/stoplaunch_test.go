package nodeclient_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// stoppableLaunch holds a launch open until release closes or its context
// ends, and records whether that context had ended when the launch returned.
type stoppableLaunch struct {
	*fakeCompute

	started   chan struct{}
	startOnce sync.Once
	release   chan struct{}
	opened    sync.Once

	mu        sync.Mutex
	cancelled error
}

// newStoppableLaunch is a stoppableLaunch that holds compute until the test
// lets it go.
func newStoppableLaunch() *stoppableLaunch {
	return &stoppableLaunch{fakeCompute: &fakeCompute{holding: true},
		started: make(chan struct{}), release: make(chan struct{})}
}

func (s *stoppableLaunch) Launch(
	ctx context.Context, lease *alloc.Lease, tier *nodeapi.TierSpec, job dispatch.Job,
) error {
	s.startOnce.Do(func() { close(s.started) })

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

// open lets the held launch finish; safe to call more than once.
func (s *stoppableLaunch) open() { s.opened.Do(func() { close(s.release) }) }

func (s *stoppableLaunch) launchCancelled() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cancelled
}

// logBook keeps every message the loop logs, so a test can wait for one.
type logBook struct {
	mu   sync.Mutex
	msgs []string
}

func (b *logBook) Enabled(context.Context, slog.Level) bool { return true }
func (b *logBook) WithAttrs([]slog.Attr) slog.Handler       { return b }
func (b *logBook) WithGroup(string) slog.Handler            { return b }

func (b *logBook) Handle(_ context.Context, r slog.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, r.Message)

	return nil
}

func (b *logBook) said(prefix string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.msgs {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}

	return false
}

// waitingForLaunches is how the loop says, as the stop arrives, that it is
// waiting for a launch: the moment a test knows the stop reached the loop
// while the launch still runs.
const waitingForLaunches = "waiting for the launches already running to finish"

// runStopping runs the loop with a handoff on stop, cancelled by the returned
// function, and answers what Run returned on the channel. When the test ends,
// the held launch is let go, the compute stops holding and the loop is joined,
// before the plane's server closes, so a failed assertion leaves nothing
// running into the next test.
func runStopping(
	t *testing.T, c *nodeclient.Client, compute *stoppableLaunch, concurrency int, hurry <-chan struct{},
) (context.CancelFunc, <-chan error, *logBook) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	book := &logBook{}
	done := make(chan error, 1)
	exited := make(chan struct{})

	go func() {
		defer close(exited)
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:              testNodeVCPU,
			Memory:            testNodeMemory,
			Provider:          config.ProviderDocker,
			GuestOS:           []config.GuestOS{config.GuestLinux},
			Deployment:        deployment,
			Log:               slog.New(book),
			Backoff:           20 * time.Millisecond,
			DrainTimeout:      time.Hour,
			HandOverOnStop:    true,
			LaunchConcurrency: concurrency,
			Hurry:             hurry,
		})
	}()
	t.Cleanup(func() {
		cancel()
		compute.open()
		compute.setHolding(false)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("the node loop outlived its test")
		}
	})

	return cancel, done, book
}

// launchOn asks the plane to launch on the node and answers its error on the
// channel, once the node is registered.
func launchOn(t *testing.T, plane *nodeplane.Plane, compute *stoppableLaunch) <-chan error {
	t.Helper()

	launched := make(chan error, 1)
	go func() {
		launched <- plane.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 7})
	}()

	select {
	case <-compute.started:
	case err := <-launched:
		t.Fatalf("the launch ended (%v) before it reached the compute", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the launch never reached the compute")
	}

	return launched
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
			compute := newStoppableLaunch()
			stop, done, book := runStopping(t, c, compute, concurrency, nil)
			waitFor(t, func() bool { return len(plane.Nodes()) == 1 })
			launched := launchOn(t, plane, compute)

			stop()
			// THE STOP HAS REACHED THE LOOP and the launch is still running.
			waitFor(t, func() bool { return book.said(waitingForLaunches) })
			if err := compute.launchCancelled(); err != nil {
				t.Fatalf("the launch's context ended (%v) once the stop arrived", err)
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
// "stop waiting" reaches the launch too, which then fails as any cancelled
// launch does and is reported so, and the node stops. The first signal alone
// does not: the launch is still held when the second arrives.
func TestASecondSignalCancelsALaunchInFlight(t *testing.T) {
	t.Parallel()

	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency %d", concurrency), func(t *testing.T) {
			t.Parallel()

			plane, c := harness(t)
			compute := newStoppableLaunch()
			hurry := make(chan struct{})
			stop, done, book := runStopping(t, c, compute, concurrency, hurry)
			waitFor(t, func() bool { return len(plane.Nodes()) == 1 })
			launched := launchOn(t, plane, compute)

			stop()
			waitFor(t, func() bool { return book.said(waitingForLaunches) })
			select {
			case err := <-launched:
				t.Fatalf("the first signal ended the launch (%v)", err)
			default:
			}
			if err := compute.launchCancelled(); err != nil {
				t.Fatalf("the first signal cancelled the launch: %v", err)
			}

			close(hurry)

			select {
			case err := <-launched:
				// THE NODE REPORTED THE CANCELLATION, rather than the plane giving up
				// on a result that never came and assuming custody.
				if err == nil || errors.Is(err, dispatch.ErrCustody) ||
					!strings.Contains(err.Error(), "could not launch lease l1: context canceled") {
					t.Errorf("the plane heard %v, want the node's report of a cancelled launch", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the second signal did not reach the launch the stop was waiting for")
			}
			if err := compute.launchCancelled(); !errors.Is(err, context.Canceled) {
				t.Errorf("the launch's context ended with %v, want the second signal's cancellation", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("the node stopped with %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the node did not stop after the second signal")
			}
		})
	}
}

// THE BOUNDARY IS THE POLL'S CONTEXT, AND ONLY A LAUNCH TAKES THE OTHER ONE. A
// launch whose poll context ended before it began is refused as draining
// whatever the launch context says; one that began runs under the launch
// context alone; and every other command keeps the poll's.
func TestOnlyALaunchRunsUnderTheLaunchContext(t *testing.T) {
	t.Parallel()

	live := t.Context()
	ended, end := context.WithCancel(t.Context())
	end()
	launch := nodeapi.Command{ID: "c1", Kind: nodeapi.CommandLaunch, Lease: testLease(1),
		Job: &nodeapi.Job{RequestID: 7}}

	refused := newStoppableLaunch()
	refused.open()
	if res := nodeclient.ExecutePolledForTest(ended, live, refused, launch); res.OK || !res.Draining {
		t.Errorf("a launch after the stop = %+v, want it refused as draining", res)
	}
	select {
	case <-refused.started:
		t.Error("a launch after the stop reached the compute")
	default:
	}

	began := newStoppableLaunch()
	if res := nodeclient.ExecutePolledForTest(live, ended, began, launch); res.OK {
		t.Errorf("a launch under an ended launch context = %+v, want it cancelled", res)
	}
	if err := began.launchCancelled(); !errors.Is(err, context.Canceled) {
		t.Errorf("the launch ran under %v, want the launch context's cancellation", err)
	}

	destroy := &fakeCompute{rejectCanceledDestroy: true}
	if res := nodeclient.ExecutePolledForTest(live, ended, destroy,
		nodeapi.Command{ID: "c2", Kind: nodeapi.CommandDestroy, RequestID: 7}); !res.OK {
		t.Errorf("a destroy ran under the launch context: %+v", res)
	}
}
