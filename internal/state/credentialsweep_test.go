package state

import (
	"testing"
	"time"
)

// THE RECORD ACCUMULATES WHAT WAS REMOVED AND REPLACES EVERYTHING ELSE, so a status
// line can say both "removed in total" and what the most recent pass found.
func TestACredentialSweepRecordAccumulatesRemovalsAcrossPasses(t *testing.T) {
	db := open(t)

	first := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

	if err := db.RecordCredentialSweep(t.Context(), CredentialSweepRecord{
		Region: "us-west-2", Path: "/billet/jit", SweptAt: first,
		Removed: 2, Kept: 5, Unaccounted: 1, ForeignNames: 0, Error: "",
	}); err != nil {
		t.Fatalf("first record: %v", err)
	}

	second := first.Add(30 * time.Second)

	if err := db.RecordCredentialSweep(t.Context(), CredentialSweepRecord{
		Region: "us-west-2", Path: "/billet/jit", SweptAt: second,
		Removed: 1, Kept: 4, Unaccounted: 0, ForeignNames: 1,
		Error: "the ledger could not say whether lease x is closed",
	}); err != nil {
		t.Fatalf("second record: %v", err)
	}

	// A SECOND PATH, so the listing is proved to keep them apart.
	if err := db.RecordCredentialSweep(t.Context(), CredentialSweepRecord{
		Region: "eu-central-1", Path: "/billet/jit", SweptAt: second, Removed: 7,
	}); err != nil {
		t.Fatalf("other region: %v", err)
	}

	sweeps, err := db.CredentialSweeps(t.Context())
	if err != nil {
		t.Fatalf("CredentialSweeps: %v", err)
	}

	if len(sweeps) != 2 {
		t.Fatalf("%d records, want 2 (one per region and path): %+v", len(sweeps), sweeps)
	}

	// Ordered by region, then path.
	if sweeps[0].Region != "eu-central-1" || sweeps[0].RemovedTotal != 7 {
		t.Errorf("first row = %+v, want the eu-central-1 pass with 7 removed", sweeps[0])
	}

	got := sweeps[1]

	switch {
	case got.Removed != 1:
		t.Errorf("Removed = %d, want the LAST pass's 1", got.Removed)
	case got.RemovedTotal != 3:
		t.Errorf("RemovedTotal = %d, want 2 + 1", got.RemovedTotal)
	case got.Kept != 4, got.Unaccounted != 0, got.ForeignNames != 1:
		t.Errorf("the last pass's counts were not replaced: %+v", got)
	case got.Error == "":
		t.Error("the last pass's error was dropped; a pass that stopped must say so")
	case !got.SweptAt.Equal(second):
		t.Errorf("SweptAt = %v, want %v", got.SweptAt, second)
	}
}

// A RECORD WITH NO PATH, NO REGION OR NO TIME IS REFUSED rather than stored as a
// row status would render as a sweep of nowhere.
func TestACredentialSweepRecordNeedsItsIdentity(t *testing.T) {
	db := open(t)

	now := time.Now()

	for name, rec := range map[string]CredentialSweepRecord{
		"no region": {Path: "/billet/jit", SweptAt: now},
		"no path":   {Region: "us-west-2", SweptAt: now},
		"no time":   {Region: "us-west-2", Path: "/billet/jit"},
		"negative":  {Region: "us-west-2", Path: "/billet/jit", SweptAt: now, Kept: -1},
	} {
		if err := db.RecordCredentialSweep(t.Context(), rec); err == nil {
			t.Errorf("%s: recorded %+v", name, rec)
		}
	}

	sweeps, err := db.CredentialSweeps(t.Context())
	if err != nil {
		t.Fatalf("CredentialSweeps: %v", err)
	}

	if len(sweeps) != 0 {
		t.Errorf("a refused record was stored: %+v", sweeps)
	}
}
