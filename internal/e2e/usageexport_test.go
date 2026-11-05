package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/ops/fleetops"
	"github.com/junioryono/billet/internal/provider"
)

// `billet usage export` READS WHAT THE REAL STACK WROTE, through the operator's own
// command over the ledger a control plane kept: a job GitHub offered, started and
// completed comes out with its owner, its repository, its charged shape and its
// minutes, and a second run past its cursor prints nothing.
func TestUsageExportReportsAJobTheStackRan(t *testing.T) {
	s, _, _ := simulatedStack(t, overTheWire)

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 5101, "push", simulatedTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 5101, "push", simulatedTier))

	names := s.awaitOneRunning(t)

	leaseID, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("instance %q does not carry a lease", names[0])
	}

	started := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0]}
	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		started.Apply(fakeactions.JobJSON("JobStarted", 5101, "push", simulatedTier)))

	deadline = time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), leaseID)
		if err == nil && rec.StartedAt != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's start was never recorded: %+v, err %v", rec, err)
		}

		time.Sleep(50 * time.Millisecond)
	}

	// GitHub's own times, 150 s apart: what a real job's messages carry, and nothing near
	// how long this test took between sending them.
	assigned := time.Date(2026, 11, 4, 9, 48, 27, 0, time.UTC)
	completed := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0], Result: "succeeded",
		RunnerAssignedAt: assigned, FinishedAt: assigned.Add(150 * time.Second)}
	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		completed.Apply(fakeactions.JobJSON("JobCompleted", 5101, "push", simulatedTier)))

	s.awaitGone(t)

	deadline = time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), leaseID)
		if err == nil && rec.Conclusion != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's lease never ended: %+v, err %v", rec, err)
		}

		time.Sleep(100 * time.Millisecond)
	}

	var out, errs bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	cfg := operatorConfig(t, s.dir)

	if err := fleetops.Usage(t.Context(), env, []string{"export", "--config", cfg, "--json"}); err != nil {
		t.Fatalf("billet usage export: %v\n%s", err, errs.String())
	}

	// THE ONE LEASE THAT RAN A JOB, read as a consumer reads it: from the JSON, by
	// the names the format promises.
	var record map[string]any

	for line := range strings.Lines(out.String()) {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a line is not a record: %v\n%s", err, line)
		}

		if r["lease_id"] == leaseID {
			record = r
		}
	}

	if record == nil {
		t.Fatalf("the export does not name lease %s:\n%s", leaseID, out.String())
	}

	for field, want := range map[string]any{
		"target": "acme", "repository": "acme/billet-test", "tier": simulatedTier,
		"provider": "simulated", "ownership": "owned", "conclusion": "done", "result": "succeeded",
	} {
		if record[field] != want {
			t.Errorf("%s = %v, want %v\n%s", field, record[field], want, out.String())
		}
	}

	if record["vcpu"] != float64(2) {
		t.Errorf("vcpu = %v, want the 2 charged", record["vcpu"])
	}
	// THE OPERATOR'S FILE HERE DECLARES NO TIER, so this is the stand-in, saying so.
	if record["minute_weight"] != float64(1) || record["weight_source"] != "charged_vcpu_undeclared_tier" {
		t.Errorf("weight = %v from %v, want 1 from the vCPU charged, for a tier this config does not declare",
			record["minute_weight"], record["weight_source"])
	}

	// GITHUB'S 150 SECONDS, whatever this run's timing, rounded up to three minutes: both
	// ends came through the real message path and neither is the time it arrived.
	if record["billed_minutes"] != float64(3) || record["job_clock"] != "github" {
		t.Errorf("billed_minutes = %v on %v, want 3 on github\n%s", record["billed_minutes"], record["job_clock"], out.String())
	}

	if unknown, ok := record["unknown"].(map[string]any); !ok || len(unknown) != 0 {
		t.Errorf("a job the stack ran from start to finish has unknowns: %v", record["unknown"])
	}

	// AND IT IS THE LAST: past the newest cursor there is nothing, on the same ledger.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")

	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("read the last record: %v", err)
	}

	cursor, ok := last["cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("the last record has no cursor: %v", last)
	}

	out.Reset()

	if err := fleetops.Usage(t.Context(), env, []string{"export", "--config", cfg, "--json", "--since", cursor}); err != nil {
		t.Fatalf("billet usage export --since: %v\n%s", err, errs.String())
	}
	if out.Len() != 0 {
		t.Errorf("past the last cursor the export printed:\n%s", out.String())
	}
}
