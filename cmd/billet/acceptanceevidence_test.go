package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// THE EVIDENCE SAYS WHICH POOL RAN EACH JOB, WHAT GITHUB SAID IT WAS, WHERE IT
// RAN AND WHAT ITS CACHES DID. An acceptance run about untrusted work is read
// from outside the job, and this is the outside. Two jobs are staged through the
// real allocator: one on the derived config's tier, whose trust class comes from
// that config, and one on a tier the config does not name, which has none.
func TestTheEvidenceSaysEachJobsTrustOriginPlacementAndCaches(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
	stateDir := filepath.Join(filepath.Dir(ws.ConfigPath), "server")
	ledgertest.Seed(t, stateDir)

	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("load the derived config: %v", err)
	}
	derived := cfg.Tiers[0]
	gone := derived
	gone.Label = "a-tier-the-config-does-not-name"

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{derived, gone})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: derived.Node, Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	stage := func(tier string, requestID int64, job alloc.HistoryJob, cache alloc.CacheObservation) string {
		t.Helper()

		lease, err := a.Reserve(t.Context(), tier)
		if err != nil {
			t.Fatalf("Reserve %s: %v", tier, err)
		}
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, requestID); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, derived.Node); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if err := a.RecordJobIdentity(t.Context(), lease.ID, job); err != nil {
			t.Fatalf("RecordJobIdentity: %v", err)
		}
		if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch, cache); err != nil {
			t.Fatalf("RecordCacheObservation: %v", err)
		}

		return lease.ID
	}
	fork := stage(derived.Label, 7, alloc.HistoryJob{
		JobID: "1", RunID: 9001, Owner: "acme", Repository: "repo", Event: "pull_request",
		WorkflowRef: "acme/repo/.github/workflows/probe.yml@refs/pull/1/merge", Name: "probe",
	}, alloc.CacheObservation{
		ImageCache: alloc.ImageCacheWarm, CacheGeneration: "g7",
		BuildCaches: alloc.BuildCaches{Sticky: alloc.BuildCacheCold},
	})
	orphan := stage(gone.Label, 8, alloc.HistoryJob{
		JobID: "2", Owner: "acme", Repository: "repo", Event: "push",
		WorkflowRef: "acme/repo/.github/workflows/probe.yml@refs/heads/main", Name: "probe",
	}, alloc.CacheObservation{ImageCache: alloc.ImageCacheCold})
	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	doc, err := collectAcceptanceEvidence(t.Context(), ws)
	if err != nil {
		t.Fatalf("collectAcceptanceEvidence: %v", err)
	}
	jobs := map[string]acceptanceJob{}
	for _, job := range doc.Jobs {
		jobs[job.LeaseID] = job
	}

	got, ok := jobs[fork]
	if !ok {
		t.Fatalf("the evidence has no job for lease %s: %+v", fork, doc.Jobs)
	}
	if want := string(derived.Trust.Effective()); got.Trust != want || want == "" {
		t.Errorf("trust = %q, want the derived tier's %q", got.Trust, want)
	}
	if got.Repository != "acme/repo" || got.Event != "pull_request" ||
		got.WorkflowRef != "acme/repo/.github/workflows/probe.yml@refs/pull/1/merge" || got.JobName != "probe" {
		t.Errorf("origin = %q %q %q %q, want what GitHub's message recorded",
			got.Repository, got.Event, got.WorkflowRef, got.JobName)
	}
	// A POOL RUNNER RAN ANOTHER RUN'S JOB: its lease was bought by run 4242.
	if got.RunID != 4242 || got.GitHubRunID != 9001 {
		t.Errorf("runs = bought by %d, started %d; want 4242 and 9001", got.RunID, got.GitHubRunID)
	}
	if got.Cache.Image != "warm" || got.Cache.Generation != "g7" || got.Cache.StickyDisks != "cold" {
		t.Errorf("cache = %+v, want image warm from g7 and sticky disks cold", got.Cache)
	}

	if trust := jobs[orphan].Trust; trust != "" {
		t.Errorf("a job on a tier the config does not name has trust %q; the config says nothing about it", trust)
	}
	if image := jobs[orphan].Cache.Image; image != "cold" {
		t.Errorf("the second job's image cache = %q, want its own observation, cold", image)
	}
	for id, job := range jobs {
		if job.Unread != "" {
			t.Errorf("job %s says it could not be read: %s", id, job.Unread)
		}
	}
}

// A HISTORY ROW THAT CANNOT BE READ IS SAID SO ON ITS JOB, and the rest of the
// evidence is still written: a lease listed in one read and gone by the next
// must not cost the run the document that says what it held.
func TestTheEvidenceNamesAJobWhoseHistoryCouldNotBeRead(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
	stateDir := filepath.Join(filepath.Dir(ws.ConfigPath), "server")
	ledgertest.Seed(t, stateDir)

	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("load the derived config: %v", err)
	}
	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the ledger: %v", err)
		}
	})

	tier := cfg.Tiers[0]
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: tier.Node, Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, 7); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, tier.Node); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.RecordCacheObservation(t.Context(), lease.ID, lease.Epoch,
		alloc.CacheObservation{ImageCache: alloc.ImageCacheWarm, CacheGeneration: "g1"}); err != nil {
		t.Fatalf("RecordCacheObservation: %v", err)
	}

	// THE UNREADABLE JOB COMES FIRST, so a describe that stopped at it would
	// leave the readable one after it bare.
	jobs := []acceptanceJob{
		{LeaseID: "a-lease-with-no-history", Tier: tier.Label},
		{LeaseID: lease.ID, Tier: tier.Label},
	}
	describeAcceptanceJobs(t.Context(), db, cfg, jobs)

	if !strings.HasPrefix(jobs[0].Unread, "its history could not be read: ") {
		t.Errorf("unread = %q, want the job to say its history could not be read", jobs[0].Unread)
	}
	if want := string(tier.Trust.Effective()); jobs[0].Trust != want {
		t.Errorf("trust = %q, want the config's %q even with the history unread", jobs[0].Trust, want)
	}
	if jobs[1].Unread != "" || jobs[1].Cache.Image != "warm" || jobs[1].Cache.Generation != "g1" {
		t.Errorf("the readable job after it = unread %q, image %q from %q; want it read whole, warm from g1",
			jobs[1].Unread, jobs[1].Cache.Image, jobs[1].Cache.Generation)
	}

	body, err := json.Marshal(jobs)
	if err != nil {
		t.Fatalf("render the jobs: %v", err)
	}
	if n := strings.Count(string(body), `"unread":`); n != 1 {
		t.Errorf("the rendered jobs carry %d unread fields, want only the unreadable job's:\n%s", n, body)
	}
}
