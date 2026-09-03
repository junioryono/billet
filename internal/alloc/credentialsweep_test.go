package alloc

import (
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// codeBuildSweepTier is a tier a codebuild node can serve, so a lease can be
// reserved and closed against one.
func codeBuildSweepTier() config.Tier {
	return config.Tier{
		Label:       "billet-4vcpu-codebuild",
		Provider:    config.ProviderCodeBuild,
		VCPU:        4,
		Memory:      7 * config.GiB,
		Image:       "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		GuestOS:     config.GuestLinux,
		Command:     []string{"./run.sh"},
		Trust:       config.WorkloadTrusted,
		Workflows:   []string{"acme/cloud/.github/workflows/ci.yml@refs/heads/main"},
		RunnerGroup: "trusted",
	}
}

func registerSweepingNode(t *testing.T, a *Allocator, name, region, path string) error {
	t.Helper()

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: name, Provider: config.ProviderCodeBuild,
		VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes:        codeBuildShapes(),
		CodeBuildJITPath: path, CodeBuildRegion: region,
	})

	return err
}

// A CODEBUILD NODE'S PATH IS RECORDED, LISTED, AND SURVIVES ITS DECOMMISSION.
//
// The control plane sweeps registrations a dead node left behind, and a host taken
// out of the fleet is exactly the one with nobody left to clean up after it. A host
// that registered before it could name its path is listed too, with an empty one,
// so `billet status` can say it is unswept rather than count it as clean.
func TestACodeBuildNodeRegistersWhereItStagesRegistrations(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB}, nil)

	if err := registerSweepingNode(t, a, "cb-1", "us-west-2", "/billet/linux/jit"); err != nil {
		t.Fatalf("register cb-1: %v", err)
	}

	// A node from before the wire carried the path.
	if err := registerSweepingNode(t, a, "cb-old", "", ""); err != nil {
		t.Fatalf("register cb-old: %v", err)
	}

	if _, err := a.Decommission(t.Context(), DecommissionRequest{
		Node: "cb-1", Actor: "test", Force: true,
	}); err != nil {
		t.Fatalf("decommission cb-1: %v", err)
	}

	paths, err := a.CodeBuildRegistrationPaths(t.Context())
	if err != nil {
		t.Fatalf("CodeBuildRegistrationPaths: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("%d paths listed, want both codebuild hosts: %+v", len(paths), paths)
	}

	got := paths[0]
	if got.Node != "cb-1" || got.Region != "us-west-2" || got.Path != "/billet/linux/jit" || !got.Decommissioned {
		t.Errorf("cb-1 listed as %+v; a decommissioned host's path must still be swept", got)
	}

	old := paths[1]
	if old.Node != "cb-old" || old.Path != "" || old.Region != "" || old.Decommissioned {
		t.Errorf("cb-old listed as %+v; a host that named no path is listed with none", old)
	}
}

// THE PATH IS A CODEBUILD FACT, RE-VALIDATED HERE BECAUSE THIS IS EXPORTED. A
// wildcard, a reserved namespace, another backend, or a path without its region
// would each send the control plane to list and delete somewhere it must not.
func TestOnlyACodeBuildNodeMayReportAWellFormedRegistrationPath(t *testing.T) {
	// NOT PARALLEL, and neither are its subtests: the assertion after the loop
	// reads the allocator the subtests wrote to, and parallel subtests run after
	// their parent's body has returned.
	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB}, nil)

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "docker-1", Provider: config.ProviderDocker,
		VCPU: 8, Memory: 32 * config.GiB,
		CodeBuildJITPath: "/billet/jit", CodeBuildRegion: "us-west-2",
	})
	if err == nil || !strings.Contains(err.Error(), "only a codebuild node") {
		t.Errorf("a docker node reporting a registration path got %v", err)
	}

	for name, tc := range map[string]struct{ region, path, want string }{
		"path without region": {"", "/billet/jit", "travel together"},
		"region without path": {"us-west-2", "", "travel together"},
		"wildcard path":       {"us-west-2", "/billet/*", "widens"},
		"reserved namespace":  {"us-west-2", "/aws/billet", "reserves"},
		"trailing slash":      {"us-west-2", "/billet/jit/", "no trailing slash"},
		"not a region":        {"uswest2", "/billet/jit", "does not look like an aws region"},
	} {
		t.Run(name, func(t *testing.T) {
			err := registerSweepingNode(t, a, "cb-"+strings.ReplaceAll(name, " ", "-"), tc.region, tc.path)
			if err == nil {
				t.Fatalf("region %q path %q was accepted", tc.region, tc.path)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q: %v", tc.want, err)
			}
		})
	}

	// AND NOTHING WAS RECORDED FOR ANY OF THEM.
	paths, err := a.CodeBuildRegistrationPaths(t.Context())
	if err != nil {
		t.Fatalf("CodeBuildRegistrationPaths: %v", err)
	}

	if len(paths) != 0 {
		t.Errorf("a refused registration left a path behind: %+v", paths)
	}
}

// THREE ANSWERS, NOT TWO. Unknown, open, and closed-at are different facts, and the
// sweep may act on the third alone — so a caller has to be able to tell them apart,
// and a read error must be an error rather than any of them.
func TestLeaseClosureHasThreeAnswers(t *testing.T) {
	t.Parallel()

	closedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB},
		[]config.Tier{codeBuildSweepTier()},
		WithClock(func() time.Time { return closedAt }))

	if err := registerSweepingNode(t, a, "cb-1", "us-west-2", "/billet/jit"); err != nil {
		t.Fatalf("register: %v", err)
	}

	unknown, err := a.LeaseClosure(t.Context(), "never-existed")
	if err != nil {
		t.Fatalf("LeaseClosure(unknown): %v", err)
	}

	if unknown.Known {
		t.Fatalf("a lease the ledger never issued reads as known: %+v", unknown)
	}

	lease, err := a.Reserve(t.Context(), codeBuildSweepTier().Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	open, err := a.LeaseClosure(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseClosure(open): %v", err)
	}

	if !open.Known || open.Terminal || !open.FinishedAt.IsZero() {
		t.Fatalf("an open lease reads as %+v", open)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}

	closed, err := a.LeaseClosure(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseClosure(closed): %v", err)
	}

	if !closed.Known || !closed.Terminal || !closed.FinishedAt.Equal(closedAt) {
		t.Fatalf("a closed lease reads as %+v, want terminal at %v", closed, closedAt)
	}
}
