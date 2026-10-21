package server

import (
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
)

// A RUNNER THE LISTENER ADOPTS IS JOURNALED WITH ITS SESSION'S OWNER: GitHub
// reported it started on this listener's scale set, which is on that owner,
// so the plane can later remove or recover it there whatever the catalogue
// names by then (#93). Its id is kept on the job's history, and no
// registration time is invented for a runner billet did not mint.
func TestARunnerTheListenerAdoptsIsJournaledWithItsSessionsOwner(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 64 * config.GiB}, tiers)
	lease := poolLeaseForTier(t, a, tiers[0].Label)
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithGitHubTarget("acme"))
	name := provider.InstanceName(lease.ID)
	if _, err := l.identifyStarted(t.Context(), dispatch.Job{RequestID: 11, RunID: 101, JobID: "job-1",
		RunnerID: 71, RunnerName: name}); err != nil {
		t.Fatalf("identifyStarted: %v", err)
	}

	member, err := a.PoolRunnerByLease(t.Context(), lease.ID)
	if err != nil || member.GitHubTarget != "acme" {
		t.Fatalf("the adopted member is %+v (err %v), want it journaled on owner acme", member, err)
	}

	rec, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if rec.RunnerID != 71 || rec.RegisteredAt != "" {
		t.Errorf("the adopted runner's history has runner %d registered %q; want runner 71 and no "+
			"registration time, since billet did not mint it", rec.RunnerID, rec.RegisteredAt)
	}
}
