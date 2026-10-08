package app

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
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
