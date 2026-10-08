package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/version"
)

func serve(t *testing.T, role string, pprofOn bool) *Server {
	t.Helper()

	reg, err := New(role)
	if err != nil {
		t.Fatal(err)
	}

	srv, err := Listen(t.Context(), "127.0.0.1:0", reg.Handler(pprofOn))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		// t.Context() is already done when cleanups run.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()

		if err := srv.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	return srv
}

func get(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.Addr().String()+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}

// The endpoint serves the build, the runtime and the process, labelled with
// the role.
func TestTheEndpointServesTheBuildAndTheRuntime(t *testing.T) {
	t.Parallel()

	srv := serve(t, "node", false)

	code, body := get(t, srv, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics answered %d", code)
	}

	build := `billet_build_info{revision="` + version.Revision() + `",role="node",version="` + version.Version() + `"} 1`
	for _, want := range []string{build, "go_goroutines ", "go_memstats_heap_alloc_bytes "} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not carry %q", want)
		}
	}
}

// THE PROFILER IS A SEPARATE SWITCH: off, its paths are not served at all.
func TestTheProfilerIsServedOnlyWhenAskedFor(t *testing.T) {
	t.Parallel()

	for _, on := range []bool{false, true} {
		srv := serve(t, "server", on)

		for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/symbol"} {
			code, _ := get(t, srv, path)

			if on && code != http.StatusOK {
				t.Errorf("pprof on: %s answered %d", path, code)
			}

			if !on && code != http.StatusNotFound {
				t.Errorf("pprof off: %s answered %d, want 404", path, code)
			}
		}
	}
}

// A PORT ALREADY TAKEN IS AN ERROR FROM Listen, not a server that never
// serves.
func TestATakenPortIsRefusedAtListen(t *testing.T) {
	t.Parallel()

	srv := serve(t, "server", false)

	reg, err := New("server")
	if err != nil {
		t.Fatal(err)
	}

	if second, err := Listen(t.Context(), srv.Addr().String(), reg.Handler(false)); err == nil {
		_ = second.Close(t.Context())

		t.Fatal("a second endpoint bound the first one's port")
	}
}

// CLOSE STOPS SERVING before it returns.
func TestCloseStopsServing(t *testing.T) {
	t.Parallel()

	reg, err := New("server")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := Listen(t.Context(), "127.0.0.1:0", reg.Handler(false))
	if err != nil {
		t.Fatal(err)
	}

	addr := srv.Addr().String()

	if code, _ := get(t, srv, "/metrics"); code != http.StatusOK {
		t.Fatalf("/metrics answered %d before Close", code)
	}

	if err := srv.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	var d net.Dialer

	conn, err := d.DialContext(t.Context(), "tcp", addr)
	if err == nil {
		conn.Close()

		t.Fatal("the endpoint still accepts after Close")
	}

	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("dialling a closed endpoint failed for another reason: %v", err)
	}
}

// A SCRAPE SHUTDOWN CANNOT FINISH IS CUT OFF: past Close's context the
// connections are closed, and Close returns once the serving goroutine has.
func TestCloseForcesWhatShutdownCannotFinish(t *testing.T) {
	t.Parallel()

	entered, release, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})

	srv, err := Listen(t.Context(), "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(handled)

		close(entered)
		<-release
	}))
	if err != nil {
		t.Fatal(err)
	}

	// The handler is released and joined whatever the test concluded.
	t.Cleanup(func() {
		close(release)
		<-handled
	})

	bound, cancelBound := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	defer cancelBound()

	answered := make(chan error, 1)

	go func() {
		req, err := http.NewRequestWithContext(bound, http.MethodGet, "http://"+srv.Addr().String()+"/", http.NoBody)
		if err != nil {
			answered <- err

			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}

		answered <- err
	}()

	select {
	case <-entered:
	case err := <-answered:
		t.Fatalf("the scrape ended (%v) before its handler began", err)
	case <-bound.Done():
		t.Fatal("the scrape never reached its handler")
	}

	expired, cancel := context.WithCancel(t.Context())
	cancel()

	closed := make(chan error, 1)

	go func() { closed <- srv.Close(expired) }()

	select {
	case err := <-closed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Close past its context returned %v, want the context's error", err)
		}
	case <-bound.Done():
		t.Fatal("Close did not return while a scrape held its connection")
	}

	// THE REQUEST'S OWN BOUND IS LONGER THAN THIS WAIT CAN TAKE, so an answer
	// here is the forced close, not the client giving up.
	select {
	case err := <-answered:
		if err == nil {
			t.Error("the scrape in flight was answered, not cut off")
		}

		if bound.Err() != nil {
			t.Error("the scrape ended only when its own bound did, not when Close cut it off")
		}
	case <-bound.Done():
		t.Fatal("the scrape in flight was never cut off")
	}
}
