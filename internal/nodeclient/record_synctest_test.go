package nodeclient_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// R4, THE QUIESCENCE PROOF: a failed record write is retried by NOTHING but
// another registration. The loop runs in a synctest bubble against the plane
// behind an in-memory transport, so its clock is the bubble's: ten minutes
// pass with every goroutine durably blocked, and the installation-attempt
// count does not move; then the next accepted registration is released and
// exactly one further attempt follows, logging a second failure.

// bubbleTransport answers in memory through the plane's handler, holding a
// path's requests behind a gate the fixture releases.
type bubbleTransport struct {
	handler http.Handler
	mu      sync.Mutex
	gate    chan struct{}
	gated   string
}

func (bt *bubbleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	bt.mu.Lock()
	gate, gated := bt.gate, bt.gated
	bt.mu.Unlock()

	if gate != nil && req.URL.Path == gated {
		select {
		case <-gate:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}

	rec := httptest.NewRecorder()
	bt.handler.ServeHTTP(rec, req)

	return rec.Result(), nil
}

func (bt *bubbleTransport) hold(path string) chan struct{} {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	bt.gate, bt.gated = make(chan struct{}), path

	return bt.gate
}

func TestAFailedRecordWriteIsNotRetriedWithoutAnotherRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "registration")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(dir, "current")

		var attempts atomic.Int32

		boom := errors.New("injected: the record cannot be written")

		t.Cleanup(nodeclient.SetInstallRecordForTest(func(string, string, func(io.Writer) error) (string, error) {
			attempts.Add(1)

			return "", boom
		}))

		log := &capturingLog{}
		plane := nodeplane.New(slog.New(slog.DiscardHandler), deployment, time.Minute,
			nodeplane.WithCommandTimeout(5*time.Second),
			nodeplane.WithTierCatalog([]config.Tier{{
				Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
				VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
			}}))
		plane.SetPollWindowForTest(60 * time.Millisecond)

		bt := &bubbleTransport{handler: nodeplane.Handler(slog.New(slog.DiscardHandler), plane, stubStore{}, stubJIT{})}

		c, err := nodeclient.New(nodeclient.Options{Base: "127.0.0.1:7717", Node: "n1"})
		if err != nil {
			t.Fatal(err)
		}

		nodeclient.ReplaceTransportForTest(c, bt)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() {
			done <- nodeclient.Run(ctx, c, &fakeCompute{}, nodeclient.LoopOptions{
				VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
				GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
				Log: slog.New(log), Backoff: 20 * time.Millisecond, RegistrationRecordPath: path,
			})
		}()

		// The first registration is accepted and its record write fails once.
		for attempts.Load() < 1 {
			synctest.Wait()

			if attempts.Load() < 1 {
				time.Sleep(time.Millisecond)
			}
		}

		// FURTHER REGISTRATION HELD, ten minutes pass with the bubble quiescent,
		// and the attempt count does not move.
		release := bt.hold("/v1/register")

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		if got := attempts.Load(); got != 1 {
			t.Fatalf("the record write was attempted %d times without another registration", got)
		}

		if log.count("could not record this registration") != 1 {
			t.Errorf("the failure was logged %d times, want once", log.count("could not record this registration"))
		}

		// THE NEXT ACCEPTED REGISTRATION: the plane forgets the node, the loop
		// re-registers through the released gate, and exactly one further
		// attempt follows, logged again.
		plane.ForgetForTest("n1")
		close(release)

		for attempts.Load() < 2 {
			synctest.Wait()

			if attempts.Load() < 2 {
				time.Sleep(time.Millisecond)
			}
		}

		synctest.Wait()

		if got := attempts.Load(); got != 2 {
			t.Errorf("the record write was attempted %d times, want exactly one more", got)
		}

		if log.count("could not record this registration") != 2 {
			t.Errorf("the second failure was not logged: %d lines", log.count("could not record this registration"))
		}

		cancel()

		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop ended with %v", err)
		}
	})
}
