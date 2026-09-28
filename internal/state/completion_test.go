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
		if _, err := db.PutPendingCompletion(ctx, completion); err != nil {
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
	if err := db.AcknowledgePendingCompletion(ctx, "linux", first.RequestID, first.MessageID); err != nil {
		t.Fatalf("AcknowledgePendingCompletion: %v", err)
	}
	if err := db.RetirePendingCompletion(ctx, "linux", first.RequestID, first.MessageID); err != nil {
		t.Fatalf("RetirePendingCompletion: %v", err)
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
		if _, err := db.PutPendingCompletion(t.Context(), completion); err == nil {
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
	if _, err := db.PutPendingCompletion(ctx, completion); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}
	releaseOnly := completion
	releaseOnly.ReleaseOnly = true
	if _, err := db.PutPendingCompletion(ctx, releaseOnly); err != nil {
		t.Fatalf("advance completion to release-only: %v", err)
	}
	redelivery := PendingCompletion{
		Tier: completion.Tier, RequestID: completion.RequestID, RunID: completion.RunID,
		Result: completion.Result, MessageID: completion.MessageID,
	}
	if disposition, err := db.PutPendingCompletion(ctx, redelivery); err != nil {
		t.Fatalf("redeliver initial completion: %v", err)
	} else if disposition != PendingCompletionActionable {
		t.Fatalf("active redelivery disposition = %v, want actionable", disposition)
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
	if disposition, err := db.PutPendingCompletion(ctx, completion); err != nil {
		t.Fatalf("redeliver retired completion: %v", err)
	} else if disposition != PendingCompletionRetired {
		t.Fatalf("retired redelivery disposition = %v, want retired", disposition)
	}
	older := completion
	older.MessageID = 9
	older.Result = "Failed"
	if disposition, err := db.PutPendingCompletion(ctx, older); err != nil {
		t.Fatalf("write older completion: %v", err)
	} else if disposition != PendingCompletionStale {
		t.Fatalf("older completion disposition = %v, want stale", disposition)
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
	if disposition, err := db.PutPendingCompletion(ctx, newer); err != nil {
		t.Fatalf("write reused request completion: %v", err)
	} else if disposition != PendingCompletionActionable {
		t.Fatalf("newer completion disposition = %v, want actionable", disposition)
	}
	if err := db.AcknowledgePendingCompletion(ctx, "linux", 19, 10); err != nil {
		t.Fatalf("acknowledge stale completion identity: %v", err)
	}
	got, err = db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions after reuse: %v", err)
	}
	if len(got) != 1 || got[0].Retired || got[0].MessageID != 11 || got[0].Result != "Failed" {
		t.Fatalf("new completion was not isolated from stale retirement: %+v", got)
	}
}

func TestPendingCompletionDeletionWaitsForSettlementAndAcknowledgement(t *testing.T) {
	db := open(t)
	completion := PendingCompletion{
		Tier: "linux", RequestID: 20, RunID: 43, Result: "Succeeded", MessageID: 12,
	}
	if _, err := db.PutPendingCompletion(t.Context(), completion); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}
	if err := db.RetirePendingCompletion(t.Context(), "linux", 20, 12); err != nil {
		t.Fatalf("retire before acknowledgement: %v", err)
	}
	got, err := db.PendingCompletions(t.Context(), "linux")
	if err != nil {
		t.Fatalf("PendingCompletions before acknowledgement: %v", err)
	}
	if len(got) != 1 || !got[0].Retired {
		t.Fatalf("settlement deleted the pre-acknowledgement tombstone: %+v", got)
	}
	if err := db.AcknowledgePendingCompletion(t.Context(), "linux", 20, 12); err != nil {
		t.Fatalf("acknowledge retired completion: %v", err)
	}
	got, err = db.PendingCompletions(t.Context(), "linux")
	if err != nil {
		t.Fatalf("PendingCompletions after acknowledgement: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("settled and acknowledged completion remained: %+v", got)
	}
}

// WHAT THE COMPLETION SAID ABOUT ITS JOB SURVIVES WITH IT, every field read
// back, because a restored completion is compared against the binding and a
// field that did not round-trip would make every restored completion either
// unprovable or, worse, equal to an empty binding.
func TestAPendingCompletionKeepsItsJobIdentity(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	completion := PendingCompletion{
		Tier: "linux", RequestID: 18, RunID: 33, Result: "succeeded", MessageID: 4,
		JobID: "job-18", JobOwner: "acme", JobRepository: "api",
		JobWorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobEvent: "push",
	}
	if _, err := db.PutPendingCompletion(ctx, completion); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}

	got, err := db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	if !slices.Equal(got, []PendingCompletion{completion}) {
		t.Fatalf("pending completions = %+v, want %+v", got, completion)
	}
}

// A REDELIVERY OF THE SAME MESSAGE CANNOT ERASE OR REPLACE THE IDENTITY IT
// RECORDED, whether it carries none or another, because that identity is the
// completion's own evidence after a restart. A later message is a new
// obligation and replaces it with the rest of the row.
func TestARedeliveredCompletionKeepsTheIdentityItFirstRecorded(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	first := PendingCompletion{
		Tier: "linux", RequestID: 19, RunID: 34, Result: "succeeded", MessageID: 5,
		LeaseID: "lease-19", LeaseEpoch: 2, Outcome: "done", ReleaseOnly: true,
		JobID: "job-19", JobOwner: "acme", JobRepository: "api",
		JobWorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobEvent: "push",
	}
	if _, err := db.PutPendingCompletion(ctx, first); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}

	empty := first
	empty.JobID, empty.JobOwner, empty.JobRepository, empty.JobWorkflowRef, empty.JobEvent =
		"", "", "", "", ""
	other := first
	other.JobID, other.JobEvent = "job-20", "workflow_dispatch"
	for _, redelivery := range []PendingCompletion{empty, other} {
		if _, err := db.PutPendingCompletion(ctx, redelivery); err != nil {
			t.Fatalf("redelivered PutPendingCompletion: %v", err)
		}
		got, err := db.PendingCompletions(ctx, "linux")
		if err != nil {
			t.Fatalf("PendingCompletions: %v", err)
		}
		if len(got) != 1 || got[0].JobID != first.JobID || got[0].JobEvent != first.JobEvent ||
			got[0].JobWorkflowRef != first.JobWorkflowRef {
			t.Fatalf("after a redelivery carrying %q, identity = %+v, want the first", redelivery.JobID, got)
		}
	}

	later := other
	later.MessageID = 6
	if _, err := db.PutPendingCompletion(ctx, later); err != nil {
		t.Fatalf("later PutPendingCompletion: %v", err)
	}
	got, err := db.PendingCompletions(ctx, "linux")
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}
	if len(got) != 1 || got[0].JobID != "job-20" || got[0].JobEvent != "workflow_dispatch" {
		t.Fatalf("a later message did not replace the identity: %+v", got)
	}
}
