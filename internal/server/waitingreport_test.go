package server

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// WHAT A TIER IS WAITING FOR OUTLIVES THE PROCESS THAT KNOWS IT.
//
// The waiting order lives in the control plane's memory; `billet status` is
// another process reading the ledger. Without this an operator sees a tier that
// is running nothing, holding nothing and advertising its ceiling, with no way
// to tell "nobody has asked for this tier" from "five jobs are queued behind a
// shape that does not fit yet" — which is the question #140's whole design
// makes it possible to ask.
//
// IT RIDES IN THE OBSERVATION THAT ALREADY EXISTS (migration 52's JSON
// snapshot), so this carries no migration: an older binary ignores the fields
// it does not know, and a newer one reads an older snapshot as "not waiting".
func TestTheObservationCarriesWhatTheTierIsWaitingFor(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tierOf("a-small", 4), tierOf("b-large", 8)}
	a, _, listeners := orderedListeners(t, tiers, 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	// Two jobs assigned to the large tier and no room for either.
	if err := large.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	large.reportCapacity(t.Context(), nil, "")

	report, err := a.CapacityReport(t.Context(), "b-large")
	if err != nil {
		t.Fatalf("CapacityReport: %v", err)
	}

	if report.Listener.Waiting != 2 {
		t.Errorf("the observation says %d jobs are waiting on b-large, want 2: an operator "+
			"cannot tell a tier nobody wants from a tier that cannot fit", report.Listener.Waiting)
	}

	if report.Listener.WaitingSince == "" {
		t.Error("the observation carries no waiting-since, so status cannot say how long the " +
			"oldest job has been waiting")
	} else if _, err := time.Parse(time.RFC3339Nano, report.Listener.WaitingSince); err != nil {
		t.Errorf("waiting-since %q is not a timestamp: %v", report.Listener.WaitingSince, err)
	}
}

// A TIER THAT IS NOT WAITING SAYS SO, and says it with a zero rather than a
// stale timestamp: a record that outlived its demand would have status
// reporting a queue that does not exist.
func TestATierThatIsNotWaitingReportsNothingWaiting(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tierOf("a-small", 4), tierOf("b-large", 8)}
	a, _, listeners := orderedListeners(t, tiers, 16, config.AdmissionFair)
	small := listeners[0]

	if err := small.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	small.reportCapacity(t.Context(), nil, "")

	report, err := a.CapacityReport(t.Context(), "a-small")
	if err != nil {
		t.Fatalf("CapacityReport: %v", err)
	}

	if report.Listener.Waiting != 0 || report.Listener.WaitingSince != "" {
		t.Errorf("a tier whose work is running reports waiting=%d since=%q",
			report.Listener.Waiting, report.Listener.WaitingSince)
	}
}

// AN OLDER SNAPSHOT READS AS NOT WAITING, which is what makes this releasable
// without a migration: a control plane one release behind wrote no such field,
// and a reader that treated its absence as anything but zero would invent a
// queue.
func TestASnapshotWithoutTheWaitingFieldsReadsAsNotWaiting(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tierOf("a-small", 4)}
	a, _, _ := orderedListeners(t, tiers, 8, config.AdmissionFair)

	if err := a.RecordListenerCapacity(t.Context(), "a-small", alloc.ListenerCapacity{
		Exchange: "confirmed",
	}); err != nil {
		t.Fatalf("record an observation with no waiting fields: %v", err)
	}

	report, err := a.CapacityReport(t.Context(), "a-small")
	if err != nil {
		t.Fatalf("CapacityReport: %v", err)
	}

	if report.Listener.Waiting != 0 || report.Listener.WaitingSince != "" {
		t.Errorf("an older observation read as waiting=%d since=%q",
			report.Listener.Waiting, report.Listener.WaitingSince)
	}
}
