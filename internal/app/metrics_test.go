package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
)

// metricsTestWait bounds every wait these tests make, so a regression fails
// rather than hangs.
const metricsTestWait = 10 * time.Second

// NO BLOCK, NOTHING SERVED, and closing that nothing is safe.
func TestNoMetricsBlockServesNothing(t *testing.T) {
	t.Parallel()

	m, err := ServeMetrics(t.Context(), "server", nil)
	if err != nil {
		t.Fatal(err)
	}

	if m != nil {
		t.Fatalf("a config without a metrics block started an endpoint: %+v", m)
	}

	m.Close(t.Context())
}

// A block serves the role's metrics where it says, until Close.
func TestAMetricsBlockServesTheRole(t *testing.T) {
	t.Parallel()

	m, err := ServeMetrics(t.Context(), "node", &config.MetricsConfig{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}

	url := "http://" + m.srv.Addr().String() + "/metrics"

	ctx, cancel := context.WithTimeout(t.Context(), metricsTestWait)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), `role="node"`) {
		t.Errorf("the endpoint does not label its build with the role:\n%s", body)
	}

	m.Close(t.Context())

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()

		t.Error("the endpoint still answers after Close")
	}
}

// CLOSE WAITS ON A CONTEXT OF ITS OWN: a stopping process's context is usually
// done already, and the endpoint is handed a live one bounded at
// metricsCloseWait instead.
func TestCloseHandsTheEndpointALiveBoundedContext(t *testing.T) {
	t.Parallel()

	var (
		called   bool
		deadline time.Time
		live     error
	)

	m := &Metrics{stop: func(ctx context.Context) error {
		called = true
		deadline, _ = ctx.Deadline()
		live = ctx.Err()

		return nil
	}}

	stopping, cancel := context.WithCancel(t.Context())
	cancel()

	before := time.Now()
	m.Close(stopping)
	after := time.Now()

	if !called {
		t.Fatal("Close did not stop the endpoint")
	}

	if live != nil {
		t.Errorf("Close handed the endpoint a context already done (%v): the parent's cancellation reached it", live)
	}

	if deadline.Before(before.Add(metricsCloseWait)) || deadline.After(after.Add(metricsCloseWait)) {
		t.Errorf("Close's context has deadline %v, want %v after the call, between %v and %v",
			deadline, metricsCloseWait, before.Add(metricsCloseWait), after.Add(metricsCloseWait))
	}
}

// AND THE WINDOW IS USED: a scrape in flight when Close begins is answered.
func TestCloseLetsAScrapeInFlightFinish(t *testing.T) {
	t.Parallel()

	entered, release, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})

	srv, err := metrics.Listen(t.Context(), "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(handled)

		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}

	// THE ENDPOINT IS CLOSED ONCE, by whichever of Close and the cleanup gets
	// there first: metrics.Server.Close consumes its serving result, so a
	// second call would wait forever.
	var (
		stopOnce sync.Once
		stopErr  error
	)

	stopEndpoint := func(ctx context.Context) error {
		stopOnce.Do(func() { stopErr = srv.Close(ctx) })

		return stopErr
	}

	m := &Metrics{srv: srv, stop: stopEndpoint}
	answered, clientDone := make(chan int, 1), make(chan struct{})

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), metricsTestWait)
	defer cancel()

	go func() {
		defer close(clientDone)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr().String()+"/", http.NoBody)
		if err != nil {
			answered <- -1

			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- -1

			return
		}

		resp.Body.Close()
		answered <- resp.StatusCode
	}()

	var released, started bool

	closed := make(chan struct{})

	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}

	// ONE OWNER FOR WHAT THE TEST STARTED, whatever it concluded: the handler
	// is released, the endpoint closed (or its close joined), and the handler,
	// the client and Close's goroutine joined, each wait bounded.
	t.Cleanup(func() {
		releaseOnce()

		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), metricsTestWait)
		defer cancel()

		if err := stopEndpoint(ctx); err != nil {
			t.Logf("the endpoint closed with %v", err)
		}

		joins := []struct {
			what string
			done <-chan struct{}
		}{{"the client", clientDone}, {"Close", closed}}
		if started {
			joins = append(joins, struct {
				what string
				done <-chan struct{}
			}{"the handler", handled})
		}

		for _, j := range joins {
			select {
			case <-j.done:
			case <-ctx.Done():
				t.Errorf("%s never returned", j.what)
			}
		}
	})

	select {
	case <-entered:
		started = true
	case code := <-answered:
		t.Fatalf("the scrape ended (%d) before its handler began", code)
	case <-ctx.Done():
		t.Fatal("the scrape never reached its handler")
	}

	stopping, stop := context.WithCancel(t.Context())
	stop()

	go func() {
		defer close(closed)
		m.Close(stopping)
	}()

	// THE SCRAPE FINISHES AFTER CLOSE HAS BEGUN, which is when the listener
	// turns new connections away: refused, or, for one the kernel had already
	// queued when the listener closed, reset (measured on darwin, 2026-10-08).
	for refused := false; !refused; {
		var d net.Dialer

		conn, err := d.DialContext(ctx, "tcp", srv.Addr().String())

		switch {
		case err == nil:
			conn.Close()
		case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET):
			refused = true
		case ctx.Err() != nil:
			t.Fatal("the listener never closed")
		default:
			t.Fatalf("dialling the endpoint failed for another reason: %v", err)
		}
	}

	releaseOnce()

	select {
	case code := <-answered:
		if code != http.StatusNoContent {
			t.Errorf("the scrape in flight answered %d, want 204: Close cut it off", code)
		}
	case <-ctx.Done():
		t.Fatal("the scrape in flight was never answered")
	}

	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("Close did not return")
	}
}
