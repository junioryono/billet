package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
)

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

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
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

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()

		t.Error("the endpoint still answers after Close")
	}
}

// A STOPPING PROCESS'S CONTEXT IS USUALLY DONE ALREADY, and a scrape in flight
// still gets Close's own bounded window rather than being cut off by it.
func TestCloseGivesAScrapeInFlightItsOwnWindow(t *testing.T) {
	t.Parallel()

	entered, release := make(chan struct{}), make(chan struct{})

	srv, err := metrics.Listen(t.Context(), "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}

	m := &Metrics{srv: srv}
	answered := make(chan int, 1)

	go func() {
		req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodGet,
			"http://"+srv.Addr().String()+"/", http.NoBody)
		if err != nil {
			answered <- 0

			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- 0

			return
		}

		resp.Body.Close()
		answered <- resp.StatusCode
	}()

	<-entered

	stopping, cancel := context.WithCancel(t.Context())
	cancel()

	closed := make(chan struct{})

	go func() {
		m.Close(stopping)
		close(closed)
	}()

	// THE SCRAPE FINISHES AFTER CLOSE HAS BEGUN, which is when the listener
	// stops accepting: only a window of Close's own lets it finish.
	for {
		var d net.Dialer

		conn, err := d.DialContext(t.Context(), "tcp", srv.Addr().String())
		if err != nil {
			break
		}

		conn.Close()
		time.Sleep(5 * time.Millisecond)
	}

	close(release)

	if code := <-answered; code != http.StatusNoContent {
		t.Errorf("the scrape in flight answered %d, want 204: Close cut it off", code)
	}

	<-closed
}
