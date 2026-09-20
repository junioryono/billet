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

// STATUS SAYS WHAT A TIER IS WAITING FOR, and how long the oldest has waited.
//
// Nothing is reserved for an idle tier (#140), so in the ledger a tier with
// five queued jobs and a tier nobody wants look identical: both hold nothing
// and both advertise their ceiling. The listener's observation is the only
// place that difference exists, and an operator asking "why is nothing
// running" needs it printed rather than inferred.
func TestStatusSaysWhatATierIsWaitingFor(t *testing.T) {
	var waiting strings.Builder

	printTierCapacity(&waiting, "billet-64vcpu", alloc.TierCapacity{
		ObservedAt: "2026-09-20T12:00:00Z",
		Listener: alloc.ListenerCapacity{
			Exchange:     "confirmed",
			Waiting:      3,
			WaitingSince: "2026-09-20T11:40:00Z",
		},
	})

	for _, want := range []string{"waiting 3", "2026-09-20T11:40:00Z"} {
		if !strings.Contains(waiting.String(), want) {
			t.Errorf("the tier report does not say %q:\n%s", want, waiting.String())
		}
	}

	// AND SAYS NOTHING WHEN NOTHING IS WAITING, rather than printing a zero
	// beside every healthy tier: the line exists to be noticed.
	var quiet strings.Builder

	printTierCapacity(&quiet, "billet-2vcpu", alloc.TierCapacity{
		ObservedAt: "2026-09-20T12:00:00Z",
		Listener:   alloc.ListenerCapacity{Exchange: "confirmed"},
	})

	if strings.Contains(quiet.String(), "waiting") {
		t.Errorf("a tier with nothing waiting reported a queue:\n%s", quiet.String())
	}
}
