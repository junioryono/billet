package metrics

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// THE SAMPLER'S METRICS COUNT WHAT IT TELLS THEM: each cause from the start at
// zero, each gap under its own cause and a cause outside the list under
// other, each tick in the histogram, and each bounded read.
func TestTheSamplersMetricsCountWhatTheyAreTold(t *testing.T) {
	t.Parallel()

	reg, err := New("node")
	if err != nil {
		t.Fatal(err)
	}
	o, err := reg.UsageObserver([]string{"failed_read", "missed_tick"})
	if err != nil {
		t.Fatal(err)
	}
	o.Gap("failed_read")
	o.Gap("failed_read")
	o.Gap("a cause from somewhere else")
	o.TickTook(3 * time.Millisecond)
	o.TickTook(2 * time.Second)
	o.IOBounded()

	srv, err := Listen(t.Context(), "127.0.0.1:0", reg.Handler(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	code, body := get(t, srv, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics answered %d", code)
	}
	for _, want := range []string{
		`billet_node_usage_gaps_total{cause="failed_read"} 2`,
		`billet_node_usage_gaps_total{cause="missed_tick"} 0`,
		`billet_node_usage_gaps_total{cause="other"} 1`,
		"billet_node_usage_tick_seconds_count 2",
		"billet_node_usage_io_bounded_total 1",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics does not carry %q", want)
		}
	}
	if strings.Contains(body, "somewhere else") {
		t.Error("a cause outside the list became a label")
	}
}
