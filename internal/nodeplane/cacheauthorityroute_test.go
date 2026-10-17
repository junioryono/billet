package nodeplane_test

import (
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeplane"
)

// AN UNPROVEN ANSWER NAMES THE JOB ONCE THE PLANE HAS BOUND IT, and names none
// before. A node waits for a lease whose answer names no job to be bound before
// it splices that job's first cache call, so a bound job's answer must never
// read as unbound, whichever branch answers it: here a tier that does not
// publish from its default branch, which is answered before GitHub is asked.
func TestAnUnprovenAuthorityNamesTheJobOnceBound(t *testing.T) {
	t.Parallel()

	store := &fakeStore{lease: &alloc.Lease{
		ID: "l1", Tier: "billet-2vcpu", Node: "n1", Epoch: 1, RequestID: 7,
	}}
	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog([]config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil))
	t.Cleanup(srv.Close)
	c := dial(t, srv.URL)

	unbound, err := c.CacheAuthority(t.Context(), "l1")
	if err != nil || unbound.LeaseID != "l1" || unbound.JobID != "" || unbound.RunID != 0 ||
		unbound.Proven {
		t.Fatalf("before the binding = %+v, err %v; want the lease, no job, unproven", unbound, err)
	}

	if err := store.RegisterPoolRunner(t.Context(), alloc.PoolRunner{
		RunnerName: "billet-l1", LeaseID: "l1", Tier: "billet-2vcpu", RunID: 31, JobID: "job-7",
	}); err != nil {
		t.Fatalf("bind the job: %v", err)
	}
	bound, err := c.CacheAuthority(t.Context(), "l1")
	if err != nil || bound.LeaseID != "l1" || bound.JobID != "job-7" || bound.RunID != 31 ||
		bound.Proven {
		t.Fatalf("once bound = %+v, err %v; want job-7 of run 31, unproven", bound, err)
	}
}
