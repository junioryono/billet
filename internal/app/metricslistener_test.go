package app

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// WHAT A LISTENER TELLS REACHES THE ENDPOINT, labelled by its tier, and the source still
// serves what the ledger source serves.
func TestTheControlPlaneServesItsListenersTimes(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tier := metricsTestTier()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatal(err)
	}

	timings := &server.ListenerTimings{}
	cp := &ControlPlane{allocator: a, db: db, cfg: &config.Config{Tiers: []config.Tier{tier}}, listenerTimings: timings}

	m, err := ServeMetrics(t.Context(), "server", &config.MetricsConfig{Listen: "127.0.0.1:0"}, cp.LedgerMetrics())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { m.Close(t.Context()) })

	// Through the listener's own path would need a session; the observer is what connects.
	observed := observedTimings(t, timings)
	observed.PollTook(tier.Label, 50*time.Second)
	observed.PollGapTook(tier.Label, 70*time.Second)
	observed.MessageHandled(tier.Label, 3*time.Second)
	observed.TeardownTook(tier.Label, 200*time.Second)

	ctx, cancel := context.WithTimeout(t.Context(), metricsTestWait)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.srv.Addr().String()+"/metrics", http.NoBody)
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

	got := string(body)

	for sample, want := range map[string]float64{
		`billet_listener_poll_seconds_bucket{tier="billet-2vcpu",le="45"}`:              0,
		`billet_listener_poll_seconds_bucket{tier="billet-2vcpu",le="60"}`:              1,
		`billet_listener_poll_gap_seconds_bucket{tier="billet-2vcpu",le="60"}`:          0,
		`billet_listener_poll_gap_seconds_bucket{tier="billet-2vcpu",le="90"}`:          1,
		`billet_listener_message_handling_seconds_bucket{tier="billet-2vcpu",le="2.5"}`: 0,
		`billet_listener_message_handling_seconds_bucket{tier="billet-2vcpu",le="5"}`:   1,
		`billet_listener_teardown_seconds_bucket{tier="billet-2vcpu",le="180"}`:         0,
		`billet_listener_teardown_seconds_bucket{tier="billet-2vcpu",le="300"}`:         1,
	} {
		if v := sampleValue(t, got, sample); v != want {
			t.Errorf("%s is %v, want %v", sample, v, want)
		}
	}

	if sampleValue(t, got, `billet_scrape_up{source="ledger"}`) != 1 {
		t.Error("the ledger source stopped serving")
	}
}

// observedTimings is what the source connected these timings to.
func observedTimings(t *testing.T, timings *server.ListenerTimings) server.ListenerObserver {
	t.Helper()

	o := timings.Current()
	if o == nil {
		t.Fatal("serving the source connected nothing to the listeners' timings")
	}

	return o
}
