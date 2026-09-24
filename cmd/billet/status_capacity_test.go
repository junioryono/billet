package main

import (
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/server"
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
			Exchange:        "confirmed",
			Waiting:         3,
			WaitingSince:    "2026-09-20T11:40:00Z",
			WaitingProgress: "2026-09-20T11:59:30Z",
		},
	}, statusNow)

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
	}, statusNow)

	if strings.Contains(quiet.String(), "waiting") {
		t.Errorf("a tier with nothing waiting reported a queue:\n%s", quiet.String())
	}
}

// statusNow is the clock the status tests read the reports against.
var statusNow = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// A WAITER WHOSE LISTENER HAS STALLED IS SAID TO HAVE STOPPED HOLDING THE LINE.
// Its own report cannot say so, because a stalled listener publishes nothing;
// the reader compares the last progress it did publish with its own clock. On
// 2026-09-23 the report said only "waiting 1 job(s) for room" for eighteen
// minutes while that waiter's listener sat on a dead connection.
func TestStatusSaysAWaiterHasPublishedNoProgress(t *testing.T) {
	report := func(progress string) string {
		var b strings.Builder

		printTierCapacity(&b, "platform-8vcpu", alloc.TierCapacity{
			ObservedAt: "2026-09-20T11:40:00Z",
			Listener: alloc.ListenerCapacity{
				Exchange: "in flight", Waiting: 1,
				WaitingSince: "2026-09-20T11:30:00Z", WaitingProgress: progress,
			},
		}, statusNow)

		return b.String()
	}

	stalled := statusNow.Add(-server.WaiterAllowance - time.Minute).Format(time.RFC3339Nano)
	if out := report(stalled); !strings.Contains(out, "NO ADMISSION PROGRESS PUBLISHED") {
		t.Errorf("a waiter stalled past the allowance is reported as holding the line:\n%s", out)
	}

	live := statusNow.Add(-server.WaiterAllowance + time.Minute).Format(time.RFC3339Nano)
	for _, progress := range []string{live, "", "not a time"} {
		if out := report(progress); strings.Contains(out, "NO ADMISSION PROGRESS PUBLISHED") {
			t.Errorf("progress %q is reported as a stall:\n%s", progress, out)
		}
	}
}
