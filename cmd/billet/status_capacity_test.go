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
		if err := cmdStatus(t.Context(), processEnv(), []string{"--config", path}); err != nil {
			t.Error(err)
		}
	})
	for _, want := range []string{
		"discovery 1, pending 0, launching 0, idle 0, running 0, cleanup 0, unknown 0",
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

// STATUS SAYS WHICH OF THE TWO REFUSES A WAITING TIER (#248): the admission
// order holding freed room for an older waiter, named, or the allocator having
// none, with its reason. On 2026-09-25 the report said only that a tier was
// waiting, and that a starved tier and a full fleet read the same.
func TestStatusSaysWhatHoldsAWaitingTierBack(t *testing.T) {
	report := func(l alloc.ListenerCapacity) string {
		var b strings.Builder

		l.Exchange, l.Waiting, l.WaitingSince = "confirmed", 10, "2026-09-25T17:21:36Z"
		l.WaitingProgress = statusNow.Format(time.RFC3339Nano)
		printTierCapacity(&b, "org-2vcpu", alloc.TierCapacity{ObservedAt: "2026-09-25T17:55:00Z", Listener: l}, statusNow)

		return b.String()
	}

	behind := report(alloc.ListenerCapacity{HeldBy: "repo-8vcpu", HeldBySince: "2026-09-25T17:20:00Z"})
	if !strings.Contains(behind, "behind repo-8vcpu, waiting since 2026-09-25T17:20:00Z") {
		t.Errorf("status does not name the waiter that holds the line:\n%s", behind)
	}

	refused := report(alloc.ListenerCapacity{Refusal: "the allocator has no room: no room on its hosts (ubuntu-01)"})
	if !strings.Contains(refused, "last purchase refused: the allocator has no room: no room on its hosts (ubuntu-01)") {
		t.Errorf("status does not say why the allocator refused:\n%s", refused)
	}

	// A TIER THAT NEVER WAITS IN LINE STILL SAYS WHY: at its own cap it has no
	// waiting line, and the refusal is the only line about its work.
	var capped strings.Builder

	printTierCapacity(&capped, "repo-capped", alloc.TierCapacity{
		ObservedAt: "2026-09-25T17:55:00Z",
		Listener: alloc.ListenerCapacity{
			Exchange: "confirmed", Refusal: "the allocator has no room: the tier holds its max_concurrent of 4",
		},
	}, statusNow)

	if !strings.Contains(capped.String(), "last purchase refused: the allocator has no room: the tier holds its "+
		"max_concurrent of 4") || strings.Contains(capped.String(), "waiting") {
		t.Errorf("a tier refused at its own cap does not say why, or claims a place in line:\n%s", capped.String())
	}

	// A REPORT FROM BEFORE THE FIELDS SAYS NEITHER, which is not the same as
	// neither being true.
	older := report(alloc.ListenerCapacity{})
	if strings.Contains(older, "behind") || strings.Contains(older, "refused") {
		t.Errorf("a report without the fields claimed a reason:\n%s", older)
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

		printTierCapacity(&b, "org-8vcpu", alloc.TierCapacity{
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
