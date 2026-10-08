package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

func metricsTestTier() config.Tier {
	return config.Tier{Label: "billet-2vcpu", Provider: config.ProviderDocker, VCPU: 2, Memory: 8 * config.GiB,
		Image: "ubuntu:24.04", GuestOS: config.GuestLinux, Reserved: 1}
}

// value finds one sample of a family by its label values.
func value(t *testing.T, snap metrics.Snapshot, family string, labels ...string) (float64, bool) {
	t.Helper()

	for _, s := range snap[family] {
		if len(s.Labels) != len(labels) {
			t.Fatalf("%s carries %d labels, want %d", family, len(s.Labels), len(labels))
		}

		match := true

		for i := range labels {
			if s.Labels[i] != labels[i] {
				match = false
			}
		}

		if match {
			return s.Value, true
		}
	}

	return 0, false
}

// THE GAUGES ARE THE LEDGER'S OWN REPORTS, read at the scrape: each tier's
// capacity report and the registered hosts.
func TestTheLedgerGaugesReadTheCapacityReportAndTheHosts(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tier := metricsTestTier()

	// THE LEDGER RECORDS AT recorded; THE SCRAPE LOOKS 30s LATER.
	recorded := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	now := recorded.Add(30 * time.Second)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{tier},
		alloc.WithClock(func() time.Time { return recorded }))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"docker-1", "docker-2"} {
		if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
			Name: name, Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// LIVE AND DECOMMISSIONED: the decommission is what it is counted as.
	if _, err := a.Decommission(t.Context(), alloc.DecommissionRequest{Node: "docker-2", Actor: "test", Force: true}); err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	leases, err := a.Escrow(t.Context(), tier.Label, 2)
	if err != nil || len(leases) != 2 {
		t.Fatalf("Escrow: %d leases, %v", len(leases), err)
	}

	confirmed := 3

	if err := a.RecordListenerCapacity(t.Context(), tier.Label, alloc.ListenerCapacity{
		Discovery: []string{leases[0].ID}, Confirmed: &confirmed,
		Waiting: 2, WaitingSince: recorded.Add(-90 * time.Second).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := ledgerSnapshot(a, []config.Tier{tier}, func() time.Time { return now })(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		family string
		labels []string
		want   float64
	}{
		{familyTierLeases, []string{tier.Label, "discovery"}, 1},
		{familyTierLeases, []string{tier.Label, "unknown"}, 1},
		{familyTierLeases, []string{tier.Label, "running"}, 0},
		{familyTierFloor, []string{tier.Label}, 1},
		// 16 vCPU and 64GiB, less the two escrowed 2-vCPU, 8GiB leases, is
		// room for six more.
		{familyTierHeadroom, []string{tier.Label}, 6},
		{familyTierAdvertised, []string{tier.Label}, 3},
		{familyTierWaiting, []string{tier.Label}, 2},
		{familyTierWaitingFor, []string{tier.Label}, 120},
		{familyTierReportAge, []string{tier.Label}, 30},
		{familyNodes, []string{"live"}, 1},
		{familyNodes, []string{"offline"}, 0},
		{familyNodes, []string{"decommissioned"}, 1},
	} {
		got, ok := value(t, snap, c.family, c.labels...)
		if !ok {
			t.Errorf("%s%v is missing", c.family, c.labels)

			continue
		}

		if got != c.want {
			t.Errorf("%s%v = %v, want %v", c.family, c.labels, got, c.want)
		}
	}

	// Every sample names a family the source declares, with its label count,
	// or the collector would drop it without a word.
	declared := map[string]int{}
	for _, f := range ledgerFamilies {
		declared[f.Name] = len(f.Labels)
	}

	for family, samples := range snap {
		n, ok := declared[family]
		if !ok {
			t.Errorf("the snapshot reports %s, which the source does not declare", family)
		}

		for _, s := range samples {
			if len(s.Labels) != n {
				t.Errorf("%s sample %v has %d labels, the family declares %d", family, s.Labels, len(s.Labels), n)
			}
		}
	}
}

// WHAT WAS NEVER RECORDED IS LEFT OUT, NOT ZERO: a tier whose listener has
// published nothing reports no advertisement, no wait and no report age.
func TestATierWithNoListenerReportHasNoAdvertisement(t *testing.T) {
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

	snap, err := ledgerSnapshot(a, []config.Tier{tier}, time.Now)(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, family := range []string{familyTierAdvertised, familyTierWaiting, familyTierWaitingFor, familyTierReportAge} {
		if v, ok := value(t, snap, family, tier.Label); ok {
			t.Errorf("%s = %v for a tier whose listener never reported", family, v)
		}
	}

	// A WAIT THAT HAS ENDED HAS NO DURATION, whatever WaitingSince still says.
	if err := a.RecordListenerCapacity(t.Context(), tier.Label, alloc.ListenerCapacity{
		Waiting: 0, WaitingSince: time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	snap, err = ledgerSnapshot(a, []config.Tier{tier}, time.Now)(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if v, ok := value(t, snap, familyTierWaiting, tier.Label); !ok || v != 0 {
		t.Errorf("a report recording no wait gives %s = %v, %v; want 0, present", familyTierWaiting, v, ok)
	}

	if v, ok := value(t, snap, familyTierWaitingFor, tier.Label); ok {
		t.Errorf("a wait that has ended reports %s = %v", familyTierWaitingFor, v)
	}
}

// A LEDGER THAT CANNOT BE READ IS AN ERROR, which the collector turns into
// billet_scrape_up 0 and no gauges, never a fleet of zeros.
func TestAnUnreadableLedgerIsAnErrorNotZeros(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}

	tier := metricsTestTier()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	snap, err := ledgerSnapshot(a, []config.Tier{tier}, time.Now)(t.Context())
	if err == nil {
		t.Fatalf("a closed ledger was read: %v", snap)
	}

	if snap != nil {
		t.Errorf("a failed read still reported %v", snap)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the read failed for the wrong reason: %v", err)
	}
}

// A REPORT FROM A CLOCK AHEAD OF THIS ONE IS AGE ZERO, never negative.
func TestAReportFromAheadIsAgeZero(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tier := metricsTestTier()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{tier},
		alloc.WithClock(func() time.Time { return now.Add(time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}

	if err := a.RecordListenerCapacity(t.Context(), tier.Label, alloc.ListenerCapacity{
		Waiting: 1, WaitingSince: now.Add(time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := ledgerSnapshot(a, []config.Tier{tier}, func() time.Time { return now })(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, family := range []string{familyTierReportAge, familyTierWaitingFor} {
		if v, ok := value(t, snap, family, tier.Label); !ok || v != 0 {
			t.Errorf("%s = %v, %v for a report a minute ahead; want 0, present", family, v, ok)
		}
	}
}

// THE SERVED ENDPOINT CARRIES BOTH HALVES: the ledger's gauges, read at the
// scrape, and its writes, told by the ledger as they happened.
func TestTheLedgerSourceServesGaugesAndWrites(t *testing.T) {
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

	m, err := ServeMetrics(t.Context(), "server", &config.MetricsConfig{Listen: "127.0.0.1:0"},
		ledgerSource(a, db, []config.Tier{tier}))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { m.Close(t.Context()) })

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "docker-1", Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB,
	}); err != nil {
		t.Fatal(err)
	}

	scrape := func() string {
		t.Helper()

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

		return string(body)
	}

	// A BASELINE AFTER THE HOST REGISTERED, so what moves next is the escrow's.
	before := scrape()

	if leases, err := a.Escrow(t.Context(), tier.Label, 1); err != nil || len(leases) != 1 {
		t.Fatalf("Escrow: %d leases, %v", len(leases), err)
	}

	after := scrape()

	for _, want := range []string{
		`billet_tier_leases{state="unknown",tier="billet-2vcpu"} 1`,
		`billet_scrape_up{source="ledger"} 1`,
		"billet_ledger_write_busy_retries_total 0",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("/metrics does not carry %q", want)
		}
	}

	delta := func(series string) float64 {
		return sampleValue(t, after, series) - sampleValue(t, before, series)
	}

	waits := delta("billet_ledger_write_wait_seconds_count")
	committed := delta(`billet_ledger_write_held_seconds_count{outcome="committed"}`)
	rolledBack := delta(`billet_ledger_write_held_seconds_count{outcome="rolled_back"}`)

	if waits < 1 || committed != waits || rolledBack != 0 {
		t.Errorf("the escrow moved the wait count by %v, the committed holds by %v and the rolled-back ones by %v; "+
			"want at least one write, each told as one wait and one committed hold, and nothing rolled back",
			waits, committed, rolledBack)
	}
}

// sampleValue is the value of one series in a scrape.
func sampleValue(t *testing.T, body, series string) float64 {
	t.Helper()

	for _, line := range strings.Split(body, "\n") {
		value, ok := strings.CutPrefix(line, series+" ")
		if !ok {
			continue
		}

		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("%s has value %q: %v", series, value, err)
		}

		return v
	}

	t.Fatalf("the scrape has no %s", series)

	return 0
}
