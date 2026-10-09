package fleetops

import (
	"bytes"
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
		// GITHUB'S JOB ID IS A STRING GITHUB CHOSE, quoted like the rest.
		JobID: "51001\rjob        forged", Owner: "acme", Repository: "api", Event: "push",
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
		Counters:     &alloc.JobCounters{Cycles: count(4_000_000_000), Instructions: count(4_840_000_000)},
	})

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})

	for _, want := range []string{
		lease, `"test\nlease      forged"`, `github job "51001\rjob        forged"`, "run 4242", "request 77",
		`"acme/api"`, `"acme/api/.github/workflows/ci.yml@refs/heads/main"`, `"push"`,
		"measured by the host epyc-1, 1200 samples every 1s over 20m0s",
		"user 3600.0s, system 400.0s", "guest vCPUs 3900.0s, VMM 100.0s",
		"received 193.0 MiB, sent 919.7 KiB",
		"90.0 kJ active, 4.0 kJ idle",
		"cycles 4,000,000,000, instructions 4,840,000,000", "1.21 instructions per cycle",
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
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Errorf("billet jobs show: %v", err)
		}
	})
	if !strings.Contains(out, "usage      no report recorded") {
		t.Errorf("an unmeasured job did not say so:\n%s", out)
	}
	if strings.Contains(out, "request -3") {
		t.Errorf("a pooled lease's internal request id was printed:\n%s", out)
	}

	err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "no-such-lease"})
	if !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("an unknown lease = %v, want ErrLeaseNotFound", err)
	}
}

// EACH GROUP SAYS "not measured" EXACTLY WHEN IT IS NAMED UNMEASURED, and no
// other group does, so the report never shows a zero it did not measure or
// hides one it did.
func TestEveryUsageGroupRendersItsOwnMeasurement(t *testing.T) {
	labels := map[string]string{
		alloc.UsageCPU: "cpu", alloc.UsageThreads: "vmm split", alloc.UsageMemory: "memory",
		alloc.UsageIO: "disk", alloc.UsageNet: "network", alloc.UsagePressure: "stalled",
		alloc.UsageEnergy: "energy",
	}
	for group := range labels {
		t.Run(group, func(t *testing.T) {
			usage := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: alloc.JobUsage{
				Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000,
				Unmeasured: []string{group}, EnergySource: alloc.EnergyRAPL,
			}}
			if group == alloc.UsageEnergy {
				usage.EnergySource = ""
			}
			var out bytes.Buffer
			renderJob(&out, alloc.JobRecord{LeaseID: "l1", Tier: "t"}, usage)

			for other, label := range labels {
				line := lineStarting(t, out.String(), label)
				unmeasured := strings.Contains(line, "not measured")
				if unmeasured != (other == group) {
					t.Errorf("with %s unmeasured, the %s line reads %q", group, label, line)
				}
			}
		})
	}
}

// lineStarting is the report line whose label is label.
func lineStarting(t *testing.T, report, label string) string {
	t.Helper()

	for line := range strings.SplitSeq(report, "\n") {
		if strings.HasPrefix(line, label+strings.Repeat(" ", 11-len(label))) {
			return line
		}
	}
	t.Fatalf("no %q line in:\n%s", label, report)

	return ""
}

// A VM MEASURED BY ITS PROCESS RENDERS WHAT ITS HOST KEPT: memory without an
// OOM count, and the kernel's energy estimate named as such.
func TestAProcessMeasuredJobRendersWhatItsHostKept(t *testing.T) {
	usage := &alloc.RecordedUsage{Node: "mac-1", JobUsage: alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 600, IntervalMillis: 1000,
		Unmeasured:      []string{alloc.UsageNet, alloc.UsageOOM, alloc.UsagePressure, alloc.UsageThreads},
		MemoryPeakBytes: 25_855_595_336, EnergyActiveMicrojoules: 7_400_903_879, EnergySource: alloc.EnergyProcess,
	}}
	var out bytes.Buffer
	renderJob(&out, alloc.JobRecord{LeaseID: "l1", Tier: "t"}, usage)

	if line := lineStarting(t, out.String(), "memory"); !strings.Contains(line, "peak 24.1 GiB, oom kills not measured") {
		t.Errorf("the memory line reads %q", line)
	}
	if line := lineStarting(t, out.String(), "energy"); !strings.Contains(line, "7.4 kJ (macOS's own estimate") {
		t.Errorf("the energy line reads %q", line)
	}
}

func count(n int64) *int64 { return &n }

// countedJob renders a measured job carrying counters.
func countedJob(t *testing.T, counters *alloc.JobCounters) string {
	t.Helper()

	usage := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000,
		Unmeasured: []string{alloc.UsageEnergy}, Counters: counters,
	}}
	var out bytes.Buffer
	renderJob(&out, alloc.JobRecord{LeaseID: "l1", Tier: "t"}, usage)

	return out.String()
}

// THE COUNTERS AND WHAT THEY IMPLY ARE PRINTED AS COUNTED: IPC, cache and
// branch misses per 1,000 instructions, and the frontend's stalled share, each
// from the counts it needs.
func TestJobsShowPrintsTheCountersAndTheirRatios(t *testing.T) {
	t.Parallel()

	out := countedJob(t, &alloc.JobCounters{
		Cycles: count(4_000_000_000), Instructions: count(4_840_000_000),
		CacheReferences: count(120_000_000), CacheMisses: count(35_816_000),
		BranchMisses: count(9_680_000), FrontendStallCycles: count(1_000_000_000),
	})
	for label, want := range map[string]string{
		"counters": "cycles 4,000,000,000, instructions 4,840,000,000",
		"ipc":      "1.21 instructions per cycle",
		"cache":    "7.4 misses per 1,000 instructions",
		"branches": "2.0 mispredicted per 1,000 instructions",
		"frontend": "stalled 25.0% of cycles",
	} {
		if line := lineStarting(t, out, label); !strings.Contains(line, want) {
			t.Errorf("the %s line reads %q, want it to say %q", label, line, want)
		}
	}
	for _, want := range []string{"cache references 120,000,000, cache misses 35,816,000",
		"branch misses 9,680,000, frontend stall cycles 1,000,000,000"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// AN EVENT THAT WAS NOT COUNTED SAYS SO, AND SO DOES EVERY RATIO THAT NEEDS
// IT, while a ratio of counted events is still printed; a job with no counters
// says that once. A ratio over a counted zero is undefined, not infinite.
func TestJobsShowSaysWhichCountersWereNotMeasured(t *testing.T) {
	t.Parallel()

	out := countedJob(t, &alloc.JobCounters{
		Cycles: count(4_000_000_000), Instructions: count(4_840_000_000), BranchMisses: count(9_680_000),
	})
	for label, want := range map[string]string{
		"ipc":      "1.21 instructions per cycle",
		"cache":    "not measured",
		"branches": "2.0 mispredicted per 1,000 instructions",
		"frontend": "not measured",
	} {
		if line := lineStarting(t, out, label); !strings.Contains(line, want) {
			t.Errorf("the %s line reads %q, want it to say %q", label, line, want)
		}
	}
	if !strings.Contains(out, "cache references not measured, cache misses not measured") {
		t.Errorf("uncounted cache events were not named:\n%s", out)
	}

	if line := lineStarting(t, countedJob(t, nil), "counters"); !strings.HasSuffix(line, "not measured") {
		t.Errorf("a job with no counters reads %q", line)
	}
	idle := countedJob(t, &alloc.JobCounters{Cycles: count(0), Instructions: count(0)})
	if line := lineStarting(t, idle, "ipc"); !strings.Contains(line, "undefined (no cycles counted)") {
		t.Errorf("IPC over zero cycles reads %q", line)
	}
}
