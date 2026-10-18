package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE NODE'S SAMPLER REPORTS TO ITS METRICS ENDPOINT: serving the node's usage
// source hands the node the observer its sampler is built with, every cause
// the sampler can mark is reported from the start, and the source reads
// nothing at a scrape, so it has no scrape_up to report.
func TestTheNodesSamplerReportsToItsEndpoint(t *testing.T) {
	t.Parallel()

	n := &Node{}
	m, err := ServeMetrics(t.Context(), "node", &config.MetricsConfig{Listen: "127.0.0.1:0"}, n.UsageMetrics())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(t.Context()) })
	if n.sampled == nil {
		t.Fatal("serving the node's usage metrics gave its sampler nothing to report to")
	}
	n.sampled.Gap("slow_read")
	n.sampled.IOBounded()

	ctx, cancel := context.WithTimeout(t.Context(), metricsTestWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.srv.Addr().String()+"/metrics",
		http.NoBody)
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

	for _, want := range []string{
		`billet_node_usage_gaps_total{cause="slow_read"} 1`,
		`billet_node_usage_gaps_total{cause="handover"} 0`,
		`billet_node_usage_gaps_total{cause="failed_read"} 0`,
		"billet_node_usage_io_bounded_total 1",
	} {
		if !strings.Contains(string(body), want+"\n") {
			t.Errorf("the node's endpoint does not carry %q", want)
		}
	}
	if strings.Contains(string(body), `billet_scrape_up{source="usage"}`) {
		t.Error("a source with nothing to read reports a scrape")
	}
}
