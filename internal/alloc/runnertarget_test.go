package alloc

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A LEASE'S OWNER IS WRITTEN ONCE, BY ITS HOLDER: the same owner again is a
// retry, another owner is a conflict that leaves the first standing, a holder
// whose epoch was reclaimed writes nothing, and a lease the ledger has ended
// takes no owner at all (#93).
func TestALeasesGitHubTargetIsWrittenOnceByItsHolder(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 4, MaxMemory: 8 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")

	if got, err := a.LeaseGitHubTarget(t.Context(), lease.ID); err != nil || got != "" {
		t.Fatalf("a fresh lease's owner = %q, err %v; want none recorded", got, err)
	}

	if err := a.RecordLeaseGitHubTarget(t.Context(), lease.ID, lease.Epoch+1, "acme"); !errors.Is(err, ErrFenced) {
		t.Fatalf("a stale holder recorded an owner: %v, want ErrFenced", err)
	}

	if err := a.RecordLeaseGitHubTarget(t.Context(), lease.ID, lease.Epoch, "acme"); err != nil {
		t.Fatalf("RecordLeaseGitHubTarget: %v", err)
	}

	if err := a.RecordLeaseGitHubTarget(t.Context(), lease.ID, lease.Epoch, "acme"); err != nil {
		t.Fatalf("a retry naming the same owner was refused: %v", err)
	}

	if err := a.RecordLeaseGitHubTarget(t.Context(), lease.ID, lease.Epoch, "someone/widgets"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second owner was accepted: %v, want ErrConflict", err)
	}

	if got, err := a.LeaseGitHubTarget(t.Context(), lease.ID); err != nil || got != "acme" {
		t.Fatalf("the lease's owner = %q, err %v; want the first, acme", got, err)
	}

	ended := reserve(t, a, "linux")
	if err := a.Release(t.Context(), ended.ID, ended.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := a.RecordLeaseGitHubTarget(t.Context(), ended.ID, ended.Epoch, "acme"); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("an ended lease took an owner: %v, want ErrLeaseNotFound", err)
	}

	if _, err := a.LeaseGitHubTarget(t.Context(), "no-such-lease"); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("an absent lease's owner: %v, want ErrLeaseNotFound", err)
	}
}

// A POOL MEMBER KEEPS THE OWNER IT WAS REGISTERED ON, and a retry naming
// another owner is refused on every path that journals one, while a record
// made without an owner contradicts nothing.
func TestAPoolMemberKeepsTheOwnerItWasRegisteredOn(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	runner := PoolRunner{LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11, RunnerID: 91,
		RunnerName: "billet-" + lease.ID, GitHubTarget: "acme"}
	if err := a.RegisterPoolRunner(t.Context(), runner); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	for name, read := range map[string]func() (PoolRunner, error){
		"by lease": func() (PoolRunner, error) { return a.PoolRunnerByLease(t.Context(), lease.ID) },
		"by name":  func() (PoolRunner, error) { return a.PoolRunnerByName(t.Context(), runner.RunnerName) },
		"in its tier": func() (PoolRunner, error) {
			members, err := a.PoolRunners(t.Context(), "linux")
			if err != nil || len(members) != 1 {
				return PoolRunner{}, errors.Join(err, errors.New("not exactly one member"))
			}
			return members[0], nil
		},
	} {
		got, err := read()
		if err != nil || got.GitHubTarget != "acme" {
			t.Errorf("the member read %s has owner %q, err %v; want acme", name, got.GitHubTarget, err)
		}
	}

	unrecorded := runner
	unrecorded.GitHubTarget = ""
	if err := a.RegisterPoolRunner(t.Context(), unrecorded); err != nil {
		t.Errorf("a retry recording no owner was refused: %v", err)
	}

	other := runner
	other.GitHubTarget = "someone/widgets"
	if err := a.RegisterPoolRunner(t.Context(), other); !errors.Is(err, ErrConflict) {
		t.Errorf("a registration retry on another owner: %v, want ErrConflict", err)
	}
	if err := a.PreserveRecoveredBusyPoolRunner(t.Context(), other); !errors.Is(err, ErrConflict) {
		t.Errorf("a busy recovery on another owner: %v, want ErrConflict", err)
	}
	if _, err := a.RetireRecoveredPoolRunner(t.Context(), other); !errors.Is(err, ErrConflict) {
		t.Errorf("a retiring recovery on another owner: %v, want ErrConflict", err)
	}

	if got, err := a.PoolRunnerByLease(t.Context(), lease.ID); err != nil || got.GitHubTarget != "acme" ||
		got.Status != PoolRunnerIdle {
		t.Errorf("after the refused retries the member is %+v, err %v; want it idle on acme", got, err)
	}
}
