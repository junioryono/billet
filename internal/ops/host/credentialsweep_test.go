package host

import (
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// STATUS SAYS EVERY SWEEP THE LEDGER RECORDED, including the host nothing sweeps
// after. The two passes are the ones internal/app's credential sweep test
// drives the sweep to record and pins field by field; this is the report
// another process prints from them.
func TestStatusReportsEveryRecordedCredentialSweep(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tier := config.Tier{
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

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	shapes := []config.RemoteShape{{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 10000}}

	for _, n := range []struct{ name, path string }{
		{"cb-a", "/billet/a/jit"}, {"cb-b", "/billet/b/jit"}, {"cb-old", ""},
	} {
		region := "us-west-2"
		if n.path == "" {
			region = ""
		}

		if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
			Name: n.name, Provider: config.ProviderCodeBuild,
			VCPU: 64, Memory: 256 * config.GiB, EC2Shapes: shapes,
			CodeBuildJITPath: n.path, CodeBuildRegion: region,
		}); err != nil {
			t.Fatalf("register %s: %v", n.name, err)
		}
	}

	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for _, rec := range []state.CredentialSweepRecord{
		{Region: "us-west-2", Path: "/billet/a/jit", SweptAt: at, Removed: 1, Unaccounted: 1},
		{Region: "us-west-2", Path: "/billet/b/jit", SweptAt: at, Error: "scripted refusal of path b"},
	} {
		if err := db.RecordCredentialSweep(t.Context(), rec); err != nil {
			t.Fatalf("record the sweep of %s: %v", rec.Path, err)
		}
	}

	out := capture(t, func() { PrintCredentialSweeps(t.Context(), processEnv(), a, db) })

	for _, want := range []string{
		"/billet/a/jit (us-west-2): 1 registration(s) removed in total",
		"1 naming leases this ledger has never seen",
		"the last pass stopped: scripted refusal of path b",
		"node cb-old registered without its registration path",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
}
