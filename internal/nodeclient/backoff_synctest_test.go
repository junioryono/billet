package nodeclient_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// flakyTransport fails the first few requests to a path as an unreachable
// control plane does, and records when each request to every path was made.
type flakyTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	fail map[string]int
	seen map[string][]time.Time
}

func (ft *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ft.mu.Lock()
	ft.seen[req.URL.Path] = append(ft.seen[req.URL.Path], time.Now())
	failing := ft.fail[req.URL.Path] > 0

	if failing {
		ft.fail[req.URL.Path]--
	}
	ft.mu.Unlock()

	if failing {
		return nil, errors.New("injected: the control plane cannot be reached")
	}

	return ft.next.RoundTrip(req)
}

func (ft *flakyTransport) times(path string) []time.Time {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	return append([]time.Time(nil), ft.seen[path]...)
}

// THE LOOP WAITS ITS BACKOFF, AND NO LONGER, after a failure: a registration
// the plane could not be reached for is retried one Backoff later, and a poll
// that failed one poll backoff later, a fifth of Backoff above five seconds. In
// a synctest bubble the gaps are exact, so a retry that comes early (a hot loop
// against a plane that is down) or late (a node idle for longer than its
// operator configured) fails by the nanosecond rather than by a guess at the
// scheduler.
func TestTheLoopWaitsItsBackoffAfterAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			backoff     = 10 * time.Second
			pollBackoff = backoff / 5
			register    = "/v1/register"
			poll        = "/v1/nodes/n1/poll"
		)

		plane := nodeplane.New(slog.New(slog.DiscardHandler), deployment, time.Minute,
			nodeplane.WithCommandTimeout(5*time.Second),
			nodeplane.WithTierCatalog([]config.Tier{{
				Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
				VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
			}}))
		plane.SetPollWindowForTest(time.Minute)

		ft := &flakyTransport{
			next: &bubbleTransport{handler: nodeplane.Handler(slog.New(slog.DiscardHandler), plane, stubStore{}, stubJIT{})},
			fail: map[string]int{register: 2, poll: 2},
			seen: map[string][]time.Time{},
		}

		c, err := nodeclient.New(nodeclient.Options{Base: "127.0.0.1:7717", Node: "n1"})
		if err != nil {
			t.Fatal(err)
		}

		nodeclient.ReplaceTransportForTest(c, ft)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() {
			done <- nodeclient.Run(ctx, c, &fakeCompute{}, nodeclient.LoopOptions{
				VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
				GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
				Log: slog.New(slog.DiscardHandler), Backoff: backoff,
			})
		}()

		// Two registrations fail and the third is accepted; two polls fail and
		// the third is held by the plane for its window. Everything has
		// happened well inside this.
		time.Sleep(3*backoff + 3*pollBackoff)
		synctest.Wait()

		gaps := func(path string, want time.Duration, attempts int) {
			t.Helper()

			at := ft.times(path)
			if len(at) < attempts {
				t.Fatalf("%s was asked %d times, want at least %d", path, len(at), attempts)
			}

			for i := 1; i < attempts; i++ {
				if gap := at[i].Sub(at[i-1]); gap != want {
					t.Errorf("attempt %d at %s came %v after the failure before it, want %v",
						i+1, path, gap, want)
				}
			}
		}

		gaps(register, backoff, 3)
		gaps(poll, pollBackoff, 3)

		cancel()

		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop ended with %v", err)
		}
	})
}
