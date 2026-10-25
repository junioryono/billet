package main

import (
	"path/filepath"
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
		JobID: "1", Owner: "acme", Repository: "repo", Event: "pull_request",
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
	if got.Cache.Image != "warm" || got.Cache.Generation != "g7" || got.Cache.StickyDisks != "cold" {
		t.Errorf("cache = %+v, want image warm from g7 and sticky disks cold", got.Cache)
	}

	if trust := jobs[orphan].Trust; trust != "" {
		t.Errorf("a job on a tier the config does not name has trust %q; the config says nothing about it", trust)
	}
	if image := jobs[orphan].Cache.Image; image != "cold" {
		t.Errorf("the second job's image cache = %q, want its own observation, cold", image)
	}
}
