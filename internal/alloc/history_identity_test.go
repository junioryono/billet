package alloc

import (
	"errors"
	"testing"
	"time"
)

func assignedLease(t *testing.T, a *Allocator) *Lease {
	t.Helper()

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	fresh, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if err := a.Assign(t.Context(), fresh.ID, fresh.Epoch, 0, 77); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	return fresh
}

// A LATER MESSAGE COMPLETES THE RECORD AND NEVER REWRITES IT. JobStarted may
// omit a field a completion carries, and a completion about a different job
// (a swapped pool member) must not splice its name onto the one recorded.
func TestAJobsIdentityIsWrittenOnceAndOnlyForTheSameJob(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := assignedLease(t, a)

	started := HistoryJob{JobID: "51001", Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", Event: "push"}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, started); err != nil {
		t.Fatalf("RecordJobIdentity(started): %v", err)
	}

	other := HistoryJob{JobID: "99999", Owner: "evil", Repository: "fork",
		WorkflowRef: "evil/fork/.github/workflows/x.yml@refs/heads/x", Name: "spliced", Event: "pull_request"}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, other); err != nil {
		t.Fatalf("RecordJobIdentity(other): %v", err)
	}

	completed := started
	completed.Name, completed.Event = "test (ubuntu)", "workflow_dispatch"
	if err := a.RecordJobIdentity(t.Context(), lease.ID, completed); err != nil {
		t.Fatalf("RecordJobIdentity(completed): %v", err)
	}

	got, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}

	want := HistoryJob{JobID: "51001", WorkflowRef: started.WorkflowRef,
		Name: "test (ubuntu)", Event: "push"}
	if got.Job != want {
		t.Errorf("job = %+v, want %+v", got.Job, want)
	}
	if got.Repo != "acme/api" {
		t.Errorf("repo = %q, want acme/api", got.Repo)
	}
	if got.RequestID != 77 || got.Tier != "small" || got.Node != "epyc-1" {
		t.Errorf("row = %+v, want request 77 on small at epyc-1", got)
	}
}

// A lease the ledger never assigned has no row to write, and reading one is
// ErrLeaseNotFound rather than an empty record.
func TestAnUnassignedLeaseRecordsNoJob(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	if err := a.RecordJobIdentity(t.Context(), "no-such-lease",
		HistoryJob{JobID: "1", Owner: "acme", Repository: "api"}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}
	if _, err := a.Job(t.Context(), "no-such-lease"); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("Job = %v, want ErrLeaseNotFound", err)
	}
}

func TestARepositoryIsOwnerSlashName(t *testing.T) {
	for _, tc := range []struct {
		job  HistoryJob
		want string
	}{
		{HistoryJob{Owner: "acme", Repository: "api"}, "acme/api"},
		{HistoryJob{Repository: "api"}, "api"},
		{HistoryJob{Owner: "acme"}, "acme"},
		{HistoryJob{}, ""},
	} {
		if got := tc.job.Repo(); got != tc.want {
			t.Errorf("%+v.Repo() = %q, want %q", tc.job, got, tc.want)
		}
	}
}
