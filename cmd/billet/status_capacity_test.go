package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// STATUS MUST EXPOSE A DISCOVERY HOLD EVEN WITH ZERO ADDITIONAL HEADROOM. The
// command reads a live ledger through its own admin handle, so deleting its call
// to the capacity reporter cannot leave this test green.
func TestStatusShowsDiscoverySeparatelyFromHeadroom(t *testing.T) {
	stateDir := t.TempDir()
	path := writeCAConfig(t, stateDir)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, cfg.Tiers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "one-slot", Provider: config.ProviderDocker, VCPU: 2, Memory: 8 * config.GiB,
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := a.Reserve(t.Context(), cfg.Tiers[0].Label)
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	if err := a.RecordListenerCapacity(t.Context(), cfg.Tiers[0].Label, alloc.ListenerCapacity{
		Discovery: []string{lease.ID}, Sent: &one, Confirmed: &one, Exchange: "confirmed",
	}); err != nil {
		t.Fatal(err)
	}
	out := capture(t, func() {
		if err := cmdStatus(t.Context(), []string{"--config", path}); err != nil {
			t.Error(err)
		}
	})
	for _, want := range []string{
		"discovery 1, pending 0, launching 0, running 0, cleanup 0, unknown 0",
		"reserved floor 0, additional headroom 0",
		"advertisement last confirmed 1, sent 1, exchange confirmed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not contain %q:\n%s", want, out)
		}
	}
}
