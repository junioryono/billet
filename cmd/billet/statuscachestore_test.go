package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func TestStatusSaysWhichHostsAreSkippingTheirStore(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)
	if err != nil {
		t.Fatal(err)
	}

	render := func() string {
		t.Helper()
		var out bytes.Buffer
		printCacheStores(t.Context(), cli.Env{Stdout: &out}, a, time.Date(2026, 11, 4, 8, 5, 0, 0, time.UTC))

		return out.String()
	}
	since := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	for _, node := range []string{"quiet", "healthy", "sick"} {
		if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
			Name: node, Provider: config.ProviderDocker, VCPU: 2, Memory: 8 * config.GiB,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// NOTHING REPORTED, NOTHING SAID: no line for a fleet with nothing to say.
	if got := render(); got != "" {
		t.Fatalf("with nothing reported: %q", got)
	}

	if err := a.RecordCacheStoreState(t.Context(), "healthy", alloc.CacheStoreReport{
		Store: "ceph", State: alloc.CacheStoreClosed, Since: since,
	}); err != nil {
		t.Fatal(err)
	}
	if got := render(); !strings.Contains(got, "cache     1 host(s) using their site's store") || strings.Contains(got, "OPEN") {
		t.Fatalf("with one healthy store: %q", got)
	}

	// A HOST WHOSE CLOCK IS AHEAD HAS NOT BEEN OPEN FOR LESS THAN NOTHING.
	if err := a.RecordCacheStoreState(t.Context(), "quiet", alloc.CacheStoreReport{
		Store: "ceph", State: alloc.CacheStoreOpen, Since: since.Add(time.Hour), Failures: 3,
		Reason: alloc.CacheStoreTimeout,
	}); err != nil {
		t.Fatal(err)
	}
	if got := render(); strings.Contains(got, "-55m") || !strings.Contains(got, "OPEN for 0s") {
		t.Fatalf("a host whose clock is ahead: %q", got)
	}
	if err := a.RecordCacheStoreState(t.Context(), "quiet", alloc.CacheStoreReport{
		Store: "ceph", State: alloc.CacheStoreClosed, Since: since,
	}); err != nil {
		t.Fatal(err)
	}
	if got := render(); !strings.Contains(got, "cache     2 host(s) using") {
		t.Fatalf("with two healthy stores: %q", got)
	}

	if err := a.RecordCacheStoreState(t.Context(), "sick", alloc.CacheStoreReport{
		Store: "ceph", State: alloc.CacheStoreOpen, Since: since, Failures: 3, Reason: "timeout",
	}); err != nil {
		t.Fatal(err)
	}
	got := render()
	for _, want := range []string{
		"cache     sick: ceph breaker OPEN for 5m0s (3 timeout failures); its new jobs start cold",
		"cache     2 host(s) using their site's store",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status does not say %q:\n%s", want, got)
		}
	}

	if err := a.RecordCacheStoreState(t.Context(), "sick", alloc.CacheStoreReport{
		Store: "ceph", State: alloc.CacheStoreProbation, Since: since,
	}); err != nil {
		t.Fatal(err)
	}
	if got := render(); !strings.Contains(got, "cache     sick: ceph breaker on probation for 5m0s") {
		t.Errorf("on probation: %q", got)
	}

	// A HOST THAT HAS LEFT IS NOT COUNTED AS USING ANYTHING.
	epoch, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "gone", Provider: config.ProviderDocker, VCPU: 2, Memory: 8 * config.GiB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RecordCacheStoreState(t.Context(), "gone", alloc.CacheStoreReport{
		Store: "ceph", State: alloc.CacheStoreClosed, Since: since,
	}); err != nil {
		t.Fatal(err)
	}
	if got := render(); !strings.Contains(got, "cache     3 host(s) using") {
		t.Fatalf("with a third healthy store: %q", got)
	}
	if err := a.NodeWithdrawn(t.Context(), "gone", epoch, ""); err != nil {
		t.Fatal(err)
	}
	if got := render(); !strings.Contains(got, "cache     2 host(s) using") {
		t.Errorf("a host that left was counted: %q", got)
	}
}
