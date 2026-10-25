package fleetops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/jobtrace"
)

// THE EXPORT WRITES THE WINDOW'S TRACE AND THE FLEET IN THE REPLAY'S FORMAT,
// names replaced unless asked for, the hosts as they registered and the tiers
// and limits as configured, and refuses to replace an earlier export.
func TestJobsExportWritesTheTraceAndTheFleet(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	runtimeHistory(t, stateDir)

	// A JOB GITHUB NEVER REPORTED COMPLETE IS LEFT OUT, never exported as one
	// that succeeded, and A LEASE NEVER ASSIGNED A JOB IS NOT A JOB AT ALL: its
	// archive writes a history row too, which the export does not read, and
	// neither is A POOL SLOT RETIRED BEFORE A JOB REACHED IT, assigned to
	// GitHub's count and to no job.
	recent := time.Now().Add(-12 * time.Hour).Truncate(time.Hour)
	ranHistory(t, stateDir, []ranHistoryJob{
		{name: "build (ubuntu)", start: recent.Add(time.Minute), runtime: 3 * time.Minute, noResult: true},
		{start: recent.Add(10 * time.Minute), runtime: time.Minute, escrowOnly: true},
		{start: recent.Add(20 * time.Minute), runtime: time.Minute, poolSlot: true},
		// GitHub queued this one before every other, and billet was assigned
		// it last: THE STAND-INS ARE NUMBERED IN THE TRACE'S ORDER, so it is
		// job-1.
		{name: "deploy", start: recent.Add(30 * time.Minute), runtime: 2 * time.Minute,
			queued: recent.Add(-96 * time.Hour)},
	})

	out := t.TempDir()
	var stderr strings.Builder

	env, stdout := bufferedEnv()
	env.Stderr = &stderr
	if err := Jobs(t.Context(), env, []string{"export", "--config", cfg, "--out", out}); err != nil {
		t.Fatalf("billet jobs export: %v", err)
	}

	// THE LEDGER RAN EIGHT, one of them on billet-4vcpu, which the configuration
	// does not declare: its shape is not the fleet's to give, so it is left out
	// and said to be.
	if !strings.Contains(stdout.String(), "8 jobs and 1 hosts written") ||
		!strings.Contains(stderr.String(), "the window held 10 assigned jobs; left out 0 that never started, 1 "+
			"GitHub never reported complete, 0 whose completion is not after their start and 1 of tiers") {
		t.Errorf("the export says %q and %q; want 8 jobs and 1 host, and the one left out", stdout, &stderr)
	}

	for _, name := range []string{"trace.jsonl", "fleet.json"} {
		if info, err := os.Stat(filepath.Join(out, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v; want it written 0600", name, info, err)
		}
	}

	tf, err := os.Open(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer tf.Close()

	trace, err := jobtrace.ReadTrace(tf)
	if err != nil {
		t.Fatalf("the exported trace does not read back: %v", err)
	}

	if len(trace.Arrivals) != 8 {
		t.Fatalf("the trace has %d jobs, want the 8 of a declared tier the ledger ran", len(trace.Arrivals))
	}

	if a := trace.Arrivals[0]; a.Job != "job-1" || time.Duration(a.Duration) != 2*time.Minute {
		t.Errorf("the first arrival exported as %+v; want deploy, 2m, as job-1", a)
	}

	// EVERY JOB RAN IN RUN 4242, which without names is the first run stand-in.
	for _, a := range trace.Arrivals {
		if a.RunID != 1 {
			t.Errorf("job %d is in run %d; want run 4242 as its stand-in, 1", a.Seq, a.RunID)
		}
	}

	build := trace.Arrivals[1]
	if build.Owner != "owner-1" || build.Repository != "repo-1" ||
		build.WorkflowRef != "owner-1/repo-1/.github/workflows/workflow-1.yml" || build.Job != "job-2 (matrix-1)" ||
		build.Tier != "billet-2vcpu" || time.Duration(build.Duration) != 6*time.Minute {
		t.Errorf("the build job exported as %+v; want stand-ins for acme/api's build (ubuntu), 6m on billet-2vcpu",
			build)
	}

	for _, a := range trace.Arrivals {
		for _, name := range []string{"acme", "api", "build", "ubuntu", "lint"} {
			if strings.Contains(a.Owner+a.Repository+a.WorkflowRef+a.Job, name) {
				t.Errorf("an exported job carries the recorded %q: %+v", name, a)
			}
		}
	}

	ff, err := os.Open(filepath.Join(out, "fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer ff.Close()

	fleet, err := jobtrace.ReadFleet(ff)
	if err != nil {
		t.Fatalf("the exported fleet does not read back: %v", err)
	}

	if len(fleet.Hosts) != 1 || fleet.Hosts[0].Name != "epyc-1" || fleet.Hosts[0].VCPU != 8 ||
		fleet.Hosts[0].Memory != 32*config.GiB || fleet.Hosts[0].Provider != config.ProviderDocker {
		t.Errorf("the fleet's hosts are %+v; want epyc-1 as it registered", fleet.Hosts)
	}

	if len(fleet.Tiers) != 1 || fleet.Tiers[0] != (jobtrace.Tier{Label: "billet-2vcpu", VCPU: 2,
		Memory: 8 * config.GiB}) || fleet.MaxVCPU != 8 || fleet.MaxMemory != 32*config.GiB {
		t.Errorf("the fleet is %+v; want the configured tier and ceiling", fleet)
	}

	if err := Jobs(t.Context(), env, []string{"export", "--config", cfg, "--out", out}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("a second export into the same directory gave %v; want it refused", err)
	}

	// AN EXPORT REFUSED FOR ONE FILE WRITES NEITHER.
	half := t.TempDir()
	if err := os.WriteFile(filepath.Join(half, "fleet.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Jobs(t.Context(), env, []string{"export", "--config", cfg, "--out", half}); err == nil ||
		!strings.Contains(err.Error(), "fleet.json already exists") {
		t.Errorf("an export beside an earlier fleet.json gave %v; want it refused", err)
	}
	if _, err := os.Stat(filepath.Join(half, "trace.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused export left trace.jsonl behind (%v)", err)
	}

	// --names KEEPS WHAT WAS RECORDED.
	named := t.TempDir()
	if err := Jobs(t.Context(), env, []string{"export", "--config", cfg, "--out", named, "--names"}); err != nil {
		t.Fatalf("billet jobs export --names: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(named, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"repository":"api"`) || !strings.Contains(string(body), `"job":"build (ubuntu)"`) ||
		!strings.Contains(string(body), `"run_id":4242`) {
		t.Errorf("an export with --names lost the names:\n%s", body)
	}

	// A WINDOW WITH NOTHING TO REPLAY IS AN ERROR, not two empty files.
	empty := t.TempDir()
	if err := Jobs(t.Context(), env, []string{"export", "--config", cfg, "--out", empty, "--since", "1h"}); err == nil ||
		!strings.Contains(err.Error(), "nothing to replay") {
		t.Errorf("an export of an empty window gave %v; want it refused", err)
	}
}
