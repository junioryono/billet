package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func startedPoolLease(t *testing.T, a *Allocator) (Lease, PoolRunner) {
	t.Helper()

	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	runner := PoolRunner{LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerName: "billet-" + lease.ID}
	if err := a.RegisterPoolRunner(t.Context(), runner); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	return *lease, runner
}

// THE IDENTITY JOBSTARTED CARRIED IS DURABLE WITH THE BINDING, read back by
// both lookups a cache authority uses. Asserted on a fresh read, so a mapping
// that dropped a column fails here rather than in a publication that never
// happens.
func TestAStartedPoolRunnerKeepsTheJobIdentityGitHubReported(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease, runner := startedPoolLease(t, a)

	identity := JobIdentity{Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", Event: "push"}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		22, 202, "job-22", identity); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}

	byLease, err := a.PoolRunnerByLease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("PoolRunnerByLease: %v", err)
	}
	byName, err := a.PoolRunnerByName(t.Context(), runner.RunnerName)
	if err != nil {
		t.Fatalf("PoolRunnerByName: %v", err)
	}
	for _, read := range []PoolRunner{byLease, byName} {
		if read.Identity != identity {
			t.Errorf("recorded identity = %+v, want %+v", read.Identity, identity)
		}
	}
}

// A REPEATED START FILLS AN IDENTITY THAT WAS NEVER RECORDED AND NEVER REPLACES
// ONE THAT WAS. The first report is the one the ledger vouched for; a later
// message disagreeing with it is not evidence that the job changed.
func TestARepeatedStartFillsAMissingIdentityAndKeepsARecordedOne(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease, runner := startedPoolLease(t, a)

	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		22, 202, "job-22", JobIdentity{}); err != nil {
		t.Fatalf("StartPoolRunner without an identity: %v", err)
	}

	first := JobIdentity{Owner: "acme", Repository: "api", Event: "push",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		22, 202, "job-22", first); err != nil {
		t.Fatalf("redelivered StartPoolRunner: %v", err)
	}

	second := first
	second.Event = "workflow_dispatch"
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		22, 202, "job-22", second); err != nil {
		t.Fatalf("a third report of the same job: %v", err)
	}

	read, err := a.PoolRunnerByLease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("PoolRunnerByLease: %v", err)
	}
	if read.Identity != first {
		t.Errorf("identity = %+v, want the first recorded %+v", read.Identity, first)
	}
}
