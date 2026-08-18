package state

import (
	"slices"
	"testing"
)

func TestPendingCompletionsAreDurableAndScopedByTier(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	first := PendingCompletion{
		Tier: "linux", RequestID: 17, RunID: 31, Result: "Succeeded",
		LeaseID: "lease-17", LeaseEpoch: 4, Outcome: "done", ReleaseOnly: true,
	}
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
	if err := db.DeletePendingCompletion(ctx, "linux", first.RequestID, first.MessageID); err != nil {
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
		{Tier: "linux", RequestID: 1, Result: "Succeeded", LeaseEpoch: -1},
		{Tier: "linux", RequestID: 1, Result: "Succeeded", Outcome: "done"},
		{Tier: "linux", RequestID: 1, Result: "Succeeded", LeaseID: "lease-1"},
		{Tier: "linux", RequestID: 1, Result: "Succeeded", LeaseID: " "},
		{Tier: "linux", RequestID: 1, Result: "Succeeded", LeaseNode: "holder"},
		{Tier: "linux", RequestID: 1, Result: "Succeeded", LeaseID: "lease-1", Outcome: "lost"},
		{Tier: "linux", RequestID: 1, Result: "Succeeded", ReleaseOnly: true},
	} {
		if err := db.PutPendingCompletion(t.Context(), completion); err == nil {
			t.Fatalf("PutPendingCompletion accepted %+v", completion)
		}
	}
}

func TestPendingCompletionRetirementIsMonotonicPerMessage(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	completion := PendingCompletion{
		Tier: "linux", RequestID: 19, RunID: 41, Result: "Succeeded", MessageID: 10,
		LeaseID: "lease-19", LeaseEpoch: 3, LeaseNode: "holder", Outcome: "done",
	}
	if err := db.PutPendingCompletion(ctx, completion); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}
	releaseOnly := completion
	releaseOnly.ReleaseOnly = true
	if err := db.PutPendingCompletion(ctx, releaseOnly); err != nil {
		t.Fatalf("advance completion to release-only: %v", err)
	}
	redelivery := PendingCompletion{
		Tier: completion.Tier, RequestID: completion.RequestID, RunID: completion.RunID,
		Result: completion.Result, MessageID: completion.MessageID,
	}
	if err := db.PutPendingCompletion(ctx, redelivery); err != nil {
		t.Fatalf("redeliver initial completion: %v", err)
	}
	got, err := db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions after release-only redelivery: %v", err)
	}
	if len(got) != 1 || !got[0].ReleaseOnly || got[0].LeaseID != completion.LeaseID ||
		got[0].LeaseNode != completion.LeaseNode {
		t.Fatalf("release-only completion regressed: %+v", got)
	}
	if err := db.RetirePendingCompletion(ctx, "linux", 19, 10); err != nil {
		t.Fatalf("RetirePendingCompletion: %v", err)
	}
	if err := db.PutPendingCompletion(ctx, completion); err != nil {
		t.Fatalf("redeliver retired completion: %v", err)
	}
	older := completion
	older.MessageID = 9
	older.Result = "Failed"
	if err := db.PutPendingCompletion(ctx, older); err != nil {
		t.Fatalf("write older completion: %v", err)
	}
	got, err = db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	if len(got) != 1 || !got[0].Retired || got[0].MessageID != 10 || got[0].Result != "Succeeded" ||
		got[0].LeaseNode != "holder" {
		t.Fatalf("retired completion regressed: %+v", got)
	}

	newer := completion
	newer.MessageID = 11
	newer.RunID = 42
	newer.Result = "Failed"
	if err := db.PutPendingCompletion(ctx, newer); err != nil {
		t.Fatalf("write reused request completion: %v", err)
	}
	if err := db.DeletePendingCompletion(ctx, "linux", 19, 10); err != nil {
		t.Fatalf("delete stale completion identity: %v", err)
	}
	got, err = db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions after reuse: %v", err)
	}
	if len(got) != 1 || got[0].Retired || got[0].MessageID != 11 || got[0].Result != "Failed" {
		t.Fatalf("new completion was not isolated from stale retirement: %+v", got)
	}
}
