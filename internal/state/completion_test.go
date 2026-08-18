package state

import (
	"slices"
	"testing"
)

func TestPendingCompletionsAreDurableAndScopedByTier(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	first := PendingCompletion{Tier: "linux", RequestID: 17, RunID: 31, Result: "Succeeded"}
	other := PendingCompletion{Tier: "macos", RequestID: 17, RunID: 32, Result: "Failed"}
	for _, completion := range []PendingCompletion{first, other} {
		if err := db.PutPendingCompletion(ctx, completion); err != nil {
			t.Fatalf("PutPendingCompletion(%+v): %v", completion, err)
		}
	}

	got, err := db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	if !slices.Equal(got, []PendingCompletion{first}) {
		t.Fatalf("linux pending completions = %+v, want %+v", got, first)
	}
	if err := db.DeletePendingCompletion(ctx, "linux", first.RequestID); err != nil {
		t.Fatalf("DeletePendingCompletion: %v", err)
	}
	got, err = db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted pending completions = %+v", got)
	}
}

func TestPendingCompletionRejectsAnUnusableIdentityOrResult(t *testing.T) {
	db := open(t)
	for _, completion := range []PendingCompletion{
		{Tier: "", RequestID: 1, Result: "Succeeded"},
		{Tier: "linux", RequestID: 0, Result: "Succeeded"},
		{Tier: "linux", RequestID: 1, Result: " "},
		{Tier: "linux", RequestID: 1, RunID: -1, Result: "Succeeded"},
	} {
		if err := db.PutPendingCompletion(t.Context(), completion); err == nil {
			t.Fatalf("PutPendingCompletion accepted %+v", completion)
		}
	}
}
