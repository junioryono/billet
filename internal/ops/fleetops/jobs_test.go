package fleetops

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

	return seedJob(t, stateDir, jobSeed{requestID: requestID, usage: usage, job: &forgedJob})
}

// forgedJob is the job seedMeasuredJob records: every string GitHub or a
// workflow chose carries a character that would forge a line unquoted.
var forgedJob = alloc.HistoryJob{
	// GITHUB'S JOB ID IS A STRING GITHUB CHOSE, quoted like the rest.
	JobID: "51001\rjob        forged", Owner: "acme", Repository: "api", Event: "push",
	WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	// A WORKFLOW CHOOSES ITS JOB'S NAME, newlines included.
	Name: "test\nlease      forged",
}

// jobSeed is what seedJob stages: the job GitHub named (nil names none), the
// host's usage report and its series.
type jobSeed struct {
	requestID int64
	job       *alloc.HistoryJob
	usage     *alloc.JobUsage
	series    *alloc.UsageSeries
}

// seedJob stages a job through the real allocator and returns its lease.
func seedJob(t *testing.T, stateDir string, seed jobSeed) string {
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
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, seed.requestID); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if seed.job != nil {
		if err := a.RecordJobIdentity(t.Context(), lease.ID, *seed.job); err != nil {
			t.Fatalf("RecordJobIdentity: %v", err)
		}
	}
	if seed.usage != nil {
		if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, *seed.usage, seed.series); err != nil {
			t.Fatalf("RecordLeaseUsage: %v", err)
		}
	}

	return lease.ID
}

func TestJobsShowPrintsWhoTheJobWasAndWhatTheHostMeasured(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)
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
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)
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

// --json CARRIES THE SAME RECORD THROUGH THE SAME LEDGER READ, and a string
// GitHub or a workflow chose is data inside a JSON string, never structure.
func TestJobsShowJSONIsTheRecordForAProgram(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	lease := seedMeasuredJob(t, stateDir, 77, &alloc.JobUsage{
		Source: alloc.UsageSourceHost, Unmeasured: []string{alloc.UsageMemory, alloc.UsageIO},
		Samples: 1200, IntervalMillis: 1000, WindowMillis: 1_200_000,
		CPUUserMicros: 3_600_000_000, CPUSystemMicros: 400_000_000,
		NetRxBytes: 202_408_832, NetTxBytes: 941_790,
		EnergyActiveMicrojoules: 90_000_000_000, EnergyIdleMicrojoules: 4_000_000_000,
		EnergySource: alloc.EnergyRAPL,
	})

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--json", lease}); err != nil {
			t.Errorf("billet jobs show --json: %v", err)
		}
	})

	var got struct {
		Lease       string `json:"lease"`
		GitHubJobID string `json:"github_job_id"`
		RunID       int64  `json:"run_id"`
		RequestID   int64  `json:"request_id"`
		JobName     string `json:"job_name"`
		Repository  string `json:"repository"`
		Usage       *struct {
			Node       string          `json:"node"`
			Measured   map[string]bool `json:"measured"`
			Samples    int64           `json:"samples"`
			CPUUser    int64           `json:"cpu_user_us"`
			NetRx      int64           `json:"net_rx_bytes"`
			EnergyUJ   int64           `json:"energy_active_uj"`
			EnergyFrom string          `json:"energy_source"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the answer is not one JSON object: %v\n%s", err, out)
	}
	if got.Lease != lease || got.RunID != 4242 || got.RequestID != 77 || got.Repository != "acme/api" {
		t.Errorf("identity = %+v", got)
	}
	if got.GitHubJobID != "51001\rjob        forged" || got.JobName != "test\nlease      forged" {
		t.Errorf("GitHub's strings did not survive as data: job %q, name %q", got.GitHubJobID, got.JobName)
	}
	if got.Usage == nil {
		t.Fatalf("a measured job has no usage:\n%s", out)
	}
	u := got.Usage
	if u.Node != "epyc-1" || u.Samples != 1200 || u.CPUUser != 3_600_000_000 || u.NetRx != 202_408_832 ||
		u.EnergyUJ != 90_000_000_000 || u.EnergyFrom != alloc.EnergyRAPL {
		t.Errorf("usage = %+v", *u)
	}
	want := map[string]bool{
		alloc.UsageCPU: true, alloc.UsageThreads: true, alloc.UsageMemory: false, alloc.UsageOOM: false,
		alloc.UsageIO: false, alloc.UsageNet: true, alloc.UsagePressure: true, alloc.UsageEnergy: true,
	}
	if !maps.Equal(u.Measured, want) {
		t.Errorf("measured = %v, want %v (the OOM count goes with memory)", u.Measured, want)
	}
}

// A JOB WITH NO REPORT HAS A NULL USAGE, never an object of zeros a program
// would read as measured, and a pooled lease's internal id is not GitHub's.
func TestJobsShowJSONSaysWhenNothingWasMeasured(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	lease := seedMeasuredJob(t, stateDir, -3, nil)

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--json", lease}); err != nil {
			t.Errorf("billet jobs show --json: %v", err)
		}
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the answer is not one JSON object: %v\n%s", err, out)
	}
	if string(got["usage"]) != "null" {
		t.Errorf("usage = %s, want null", got["usage"])
	}
	if string(got["request_id"]) != "0" {
		t.Errorf("request_id = %s, want 0 for a pooled lease", got["request_id"])
	}
}

// knownAnswerFixtures is where the known-answer checker reads what this
// command prints.
var knownAnswerFixtures = filepath.Join("..", "..", "..", "scripts", "knownanswer", "testdata", "jobs-show")

// THE CHECKER'S FIXTURES ARE THIS COMMAND'S OWN ANSWERS. scripts/knownanswer
// decodes `billet jobs show --json` with a type of its own; it reads these
// files, and this comparison proves they are what the command prints today
// (BILLET_UPDATE_FIXTURES=1 rewrites them), so the two cannot drift apart.
func TestTheKnownAnswerFixturesAreJobsShowsOwn(t *testing.T) {
	record := alloc.JobRecord{
		LeaseID: "lease-cpu", Tier: "billet-8vcpu", Node: "ubuntu-01", RunID: 18000000001, RequestID: 9001,
		Job: alloc.HistoryJob{JobID: "52000000001", Name: "cpu", Event: "workflow_dispatch",
			WorkflowRef: "acme/bench/.github/workflows/known-answer.yml@refs/heads/main"},
		Repo: "acme/bench", Result: "succeeded", Conclusion: "succeeded", ChosenProvider: "firecracker",
		VCPU: 8, Memory: 16 << 30, QueuedAt: "2026-10-09T10:00:00Z", AssignedAt: "2026-10-09T10:00:01Z",
		StartedAt: "2026-10-09T10:00:05Z", FinishedAt: "2026-10-09T10:01:40Z",
	}
	measured := &alloc.RecordedUsage{Node: "ubuntu-01", RecordedAt: "2026-10-09T10:01:41Z", JobUsage: alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 95, IntervalMillis: 1000, WindowMillis: 95_000,
		CPUUserMicros: 251_000_000, CPUSystemMicros: 9_000_000, GuestCPUMicros: 255_000_000,
		VMMCPUMicros: 5_000_000, MemoryPeakBytes: 1_200_000_000, OOMKills: 0, DiskReadBytes: 40_000_000,
		DiskWriteBytes: 60_000_000, NetRxBytes: 52_000_000, NetTxBytes: 1_000_000, NetRxPackets: 40_000,
		NetTxPackets: 20_000, CPUSomeMicros: 1_000, IOSomeMicros: 2_000,
		EnergyActiveMicrojoules: 9_500_000_000, EnergyIdleMicrojoules: 600_000_000,
		EnergySource: alloc.EnergyRAPL,
	}}
	partial := *measured
	partial.Unmeasured = []string{alloc.UsageMemory, alloc.UsageIO, alloc.UsageEnergy}
	partial.MemoryPeakBytes, partial.DiskReadBytes, partial.DiskWriteBytes = 0, 0, 0
	partial.EnergyActiveMicrojoules, partial.EnergyIdleMicrojoules, partial.EnergySource = 0, 0, ""

	shapes := map[string]*alloc.RecordedUsage{"measured": measured, "partial": &partial, "no-report": nil}
	for _, name := range slices.Sorted(maps.Keys(shapes)) {
		var out bytes.Buffer
		if err := renderJobJSON(&out, record, shapes[name]); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		path := filepath.Join(knownAnswerFixtures, name+".json")
		if os.Getenv("BILLET_UPDATE_FIXTURES") == "1" {
			if err := os.MkdirAll(knownAnswerFixtures, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run with BILLET_UPDATE_FIXTURES=1 to write the fixtures)", name, err)
		}
		if !bytes.Equal(committed, out.Bytes()) {
			t.Errorf("%s differs from what billet jobs show --json prints; run with BILLET_UPDATE_FIXTURES=1:\n%s",
				path, out.String())
		}
	}
	entries, err := os.ReadDir(knownAnswerFixtures)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(shapes) {
		t.Errorf("%s holds %d files and this test writes %d: a file no producer writes is stale",
			knownAnswerFixtures, len(entries), len(shapes))
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
