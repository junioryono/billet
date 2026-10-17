package fleetops

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

var waitTier = config.Tier{
	Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
	VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
}

// startWaitedJob runs one job through the real allocator the way the listener
// does: bought, assigned to a pool slot, registered, started by GitHub's
// JobStarted and named with the message's times. It returns the lease.
func startWaitedJob(t *testing.T, a *alloc.Allocator, request int64, job alloc.HistoryJob) string {
	t.Helper()

	lease, err := a.Reserve(t.Context(), waitTier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, request); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	name := "billet-" + lease.ID
	if err := a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: lease.ID,
		Tier: waitTier.Label, LaunchRequestID: request, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, waitTier.Label, request, name, request, 9,
		job.JobID, alloc.JobIdentity{}); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, job); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}

	return lease.ID
}

func waitAllocator(t *testing.T, stateDir string, now func() time.Time) *alloc.Allocator {
	t.Helper()

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	opts := []alloc.Option{}
	if now != nil {
		opts = append(opts, alloc.WithClock(now))
	}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{waitTier}, opts...)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return a
}

// ON A FIXED CLOCK: each part is computed only where both of its ends were
// recorded, the unknown job is counted and kept out, the negative wait is
// printed as negative, and the longest names its lease.
func TestTheQueueWaitReportSplitsEachTiersWaits(t *testing.T) {
	stateDir := t.TempDir()
	seedLedger(t, stateDir)

	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	a := waitAllocator(t, stateDir, func() time.Time { return now })
	since := now

	long := startWaitedJob(t, a, 1, alloc.HistoryJob{JobID: "long",
		QueuedAt: now.Add(-52 * time.Minute), ScaleSetAssignedAt: now.Add(-51 * time.Minute)})
	startWaitedJob(t, a, 2, alloc.HistoryJob{JobID: "short", QueuedAt: now.Add(-30 * time.Second)})
	startWaitedJob(t, a, 3, alloc.HistoryJob{JobID: "skewed", QueuedAt: now.Add(4 * time.Second)})
	startWaitedJob(t, a, 4, alloc.HistoryJob{JobID: "unrecorded"})

	report, err := buildJobWaits(t.Context(), a, []config.Tier{waitTier}, "", since)
	if err != nil {
		t.Fatalf("buildJobWaits: %v", err)
	}
	if len(report.Groups) != 1 {
		t.Fatalf("groups = %+v, want one for the tier's one shape", report.Groups)
	}

	g := report.Groups[0]
	if g.Tier != waitTier.Label || g.VCPU != 2 || g.Started != 4 || g.Unknown != 1 {
		t.Errorf("group = %s/%d with %d started and %d unknown; want billet-2vcpu/2, 4 and 1",
			g.Tier, g.VCPU, g.Started, g.Unknown)
	}
	if g.Wait.Count != 3 || g.Wait.Longest != 52*time.Minute || g.Wait.LongestLease != long ||
		g.Wait.Negative != 1 || g.Wait.P50 != 30*time.Second {
		t.Errorf("wait = %+v; want 3 jobs, p50 30s, the longest 52m on %s, one below zero", g.Wait, long)
	}
	if g.AtGitHub.Count != 1 || g.AtGitHub.P50 != time.Minute || g.ForRoom.P50 != 51*time.Minute {
		t.Errorf("at github %+v, for room %+v; want only the job GitHub said it assigned, 1m and 51m",
			g.AtGitHub, g.ForRoom)
	}
	if g.ToStart.Count != 4 || g.ToStart.Longest != 0 {
		t.Errorf("to start = %+v; want all four, bought and started on one frozen clock", g.ToStart)
	}

	var out bytes.Buffer
	renderJobWaits(&out, report)

	for _, want := range []string{
		"jobs that started since 2026-10-16T08:00:00Z",
		"billet-2vcpu, 2 vCPU: 4 started, 1 with no GitHub queue time on record (unknown, not counted)",
		"52m0s  " + long, "1 below zero, down to -4s: GitHub's clock and billet's disagreed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
}

// THROUGH THE COMMAND, AND AS JSON: a part no job could be measured for is
// null, never a summary of zeros, and --tier keeps out what it does not name.
func TestJobsWaitPrintsTheLedgersWaits(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	seedLedger(t, stateDir)

	a := waitAllocator(t, stateDir, nil)
	lease := startWaitedJob(t, a, 1, alloc.HistoryJob{JobID: "j1", QueuedAt: time.Now().Add(-20 * time.Minute)})

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"wait", "--config", cfg, "--json"}); err != nil {
			t.Errorf("billet jobs wait --json: %v", err)
		}
	})

	var report struct {
		Groups []struct {
			Tier     string           `json:"tier"`
			Started  int              `json:"started"`
			Wait     *waitSummaryJSON `json:"wait"`
			AtGitHub *waitSummaryJSON `json:"at_github"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out)
	}
	if len(report.Groups) != 1 || report.Groups[0].Started != 1 || report.Groups[0].Wait == nil {
		t.Fatalf("groups = %+v, want the one started job with a wait", report.Groups)
	}
	if w := report.Groups[0].Wait; w.LongestLease != lease || w.P99 < 1200 || w.P99 > 1260 {
		t.Errorf("wait = %+v, want about 20 minutes on %s", w, lease)
	}
	if report.Groups[0].AtGitHub != nil {
		t.Errorf("at github = %+v for a job GitHub gave no scale-set time; want null", report.Groups[0].AtGitHub)
	}

	other := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"wait", "--config", cfg, "--tier", "nobody"}); err != nil {
			t.Errorf("billet jobs wait --tier nobody: %v", err)
		}
	})
	if !strings.Contains(other, "no job started in the window") || strings.Contains(other, lease) {
		t.Errorf("--tier nobody reported another tier's job:\n%s", other)
	}
}
