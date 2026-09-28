package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// seedMeasuredJob stages a job through the real allocator: assigned, named by
// GitHub, and, when usage is given, measured by the host.
func seedMeasuredJob(t *testing.T, stateDir string, requestID int64, usage *alloc.JobUsage) string {
	t.Helper()

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the ledger: %v", err)
		}
	}()

	tier := config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, requestID); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, alloc.HistoryJob{
		JobID: "51001", Owner: "acme", Repository: "api", Event: "push",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
		// A WORKFLOW CHOOSES ITS JOB'S NAME, newlines included.
		Name: "test\nlease      forged",
	}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}
	if usage != nil {
		if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, *usage, nil); err != nil {
			t.Fatalf("RecordLeaseUsage: %v", err)
		}
	}

	return lease.ID
}

func TestJobsShowPrintsWhoTheJobWasAndWhatTheHostMeasured(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	lease := seedMeasuredJob(t, stateDir, 77, &alloc.JobUsage{
		Source: alloc.UsageSourceHost, Unmeasured: []string{alloc.UsageMemory, alloc.UsageIO},
		Samples: 1200, IntervalMillis: 1000, WindowMillis: 1_200_000,
		CPUUserMicros: 3_600_000_000, CPUSystemMicros: 400_000_000,
		GuestCPUMicros: 3_900_000_000, VMMCPUMicros: 100_000_000,
		NetRxBytes: 202_408_832, NetTxBytes: 941_790,
		EnergyActiveMicrojoules: 90_000_000_000, EnergyIdleMicrojoules: 4_000_000_000,
		EnergySource: alloc.EnergyRAPL,
	})

	out := capture(t, func() {
		if err := cmdJobs(t.Context(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})

	for _, want := range []string{
		lease, `"test\nlease      forged"`, "github job 51001", "run 4242", "request 77",
		`"acme/api"`, `"acme/api/.github/workflows/ci.yml@refs/heads/main"`, `"push"`,
		"measured by the host epyc-1, 1200 samples every 1s over 20m0s",
		"user 3600.0s, system 400.0s", "guest vCPUs 3900.0s, VMM 100.0s",
		"received 193.0 MiB, sent 919.7 KiB",
		"90.0 kJ active, 4.0 kJ idle",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	for _, label := range []string{"memory", "disk"} {
		if !strings.Contains(out, label+strings.Repeat(" ", 11-len(label))+"not measured") {
			t.Errorf("%s was not reported as unmeasured:\n%s", label, out)
		}
	}
	if strings.Count(out, "\nlease ") != 0 {
		t.Errorf("a job name forged a line of the report:\n%s", out)
	}
}

func TestJobsShowSaysWhenNothingWasMeasured(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	lease := seedMeasuredJob(t, stateDir, -3, nil)

	out := capture(t, func() {
		if err := cmdJobs(t.Context(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})
	if !strings.Contains(out, "usage      not measured") {
		t.Errorf("an unmeasured job did not say so:\n%s", out)
	}
	if strings.Contains(out, "request -3") {
		t.Errorf("a pooled lease's internal request id was printed:\n%s", out)
	}

	err := cmdJobs(t.Context(), []string{"show", "--config", cfg, "no-such-lease"})
	if !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("an unknown lease = %v, want ErrLeaseNotFound", err)
	}
}
