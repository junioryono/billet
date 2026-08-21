package alloc

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestPoolRunnerBindsTheActualJobWithoutChangingItsComputeLease(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	runner := PoolRunner{LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerName: "billet-" + lease.ID}
	if err := a.RegisterPoolRunner(t.Context(), runner); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	started, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		22, 202, "actual-job")
	if err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if started.LaunchRequestID != 11 || started.ActualRequestID != 22 ||
		started.Status != PoolRunnerBusy || started.RunnerID != 77 {
		t.Fatalf("started binding = %+v", started)
	}

	resolved, err := a.PoolRunnerByName(t.Context(), runner.RunnerName)
	if err != nil {
		t.Fatalf("PoolRunnerByName: %v", err)
	}
	if resolved.LeaseID != lease.ID || resolved.JobID != "actual-job" {
		t.Errorf("resolved binding = %+v", resolved)
	}

	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		33, 303, "different-job"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a busy runner accepted a second job: %v", err)
	}
}

func TestOnlyIdlePoolMembersAreScaleDownCandidates(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 4, MaxMemory: 8 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	for request := int64(11); request <= 12; request++ {
		lease := reserve(t, a, "linux")
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 100+request, request); err != nil {
			t.Fatalf("Assign(%d): %v", request, err)
		}
		name := "billet-" + lease.ID
		if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
			LaunchRequestID: request, RunnerName: name}); err != nil {
			t.Fatalf("RegisterPoolRunner(%d): %v", request, err)
		}
		if request == 11 {
			if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
				request, 111, "busy-job"); err != nil {
				t.Fatalf("StartPoolRunner: %v", err)
			}
		}
	}

	idle, err := a.IdlePoolRunners(t.Context(), "linux")
	if err != nil {
		t.Fatalf("IdlePoolRunners: %v", err)
	}
	if len(idle) != 1 || idle[0].LaunchRequestID != 12 {
		t.Fatalf("idle runners = %+v, want only request 12", idle)
	}
}

func TestRecoveredBusyPoolRunnerYieldsToAuthoritativeJobStarted(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	name := "billet-" + lease.ID
	if err := a.PreserveRecoveredBusyPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerID: 71, RunnerName: name,
	}); err != nil {
		t.Fatalf("PreserveRecoveredBusyPoolRunner: %v", err)
	}
	started, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
		22, 202, "actual-job")
	if err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if started.ActualRequestID != 22 || started.RunID != 202 || started.JobID != "actual-job" {
		t.Fatalf("started recovered binding = %+v", started)
	}
	settled, err := a.RetireRecoveredPoolRunner(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("RetireRecoveredPoolRunner: %v", err)
	}
	if settled.Status != PoolRunnerBusy || settled.ActualRequestID != 22 {
		t.Fatalf("recovery retired an authoritative started binding: %+v", settled)
	}
}

func TestRecoveredPlaceholderCanRetireAfterGitHubStopsReportingBusy(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.PreserveRecoveredBusyPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerID: 71, RunnerName: "billet-" + lease.ID,
	}); err != nil {
		t.Fatalf("PreserveRecoveredBusyPoolRunner: %v", err)
	}
	settled, err := a.RetireRecoveredPoolRunner(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("RetireRecoveredPoolRunner: %v", err)
	}
	if settled.Status != PoolRunnerRetiring {
		t.Fatalf("recovered placeholder status = %q, want retiring", settled.Status)
	}
}

func TestSettledPoolIdentitySurvivesUntilSourceAcknowledgement(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	name := "github-returned-name"
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 11, RunnerID: 71, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
		22, 202, "actual-job"); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if err := a.SettlePoolRunner(t.Context(), "linux", 11); err != nil {
		t.Fatalf("SettlePoolRunner: %v", err)
	}
	member, err := a.PoolRunnerByName(t.Context(), name)
	if err != nil || member.Status != PoolRunnerRetired || member.ActualRequestID != 22 {
		t.Fatalf("settled tombstone = %+v, err %v", member, err)
	}
	if err := a.AcknowledgePoolRunner(t.Context(), "linux", 11); err != nil {
		t.Fatalf("AcknowledgePoolRunner: %v", err)
	}
	if _, err := a.PoolRunnerByName(t.Context(), name); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("acknowledged pool identity still exists: %v", err)
	}
}
