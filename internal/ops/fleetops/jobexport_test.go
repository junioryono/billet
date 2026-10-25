package fleetops

import (
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
	if !strings.Contains(stdout.String(), "7 jobs and 1 hosts written") ||
		!strings.Contains(stderr.String(), "left out 1 jobs of tiers the configuration no longer declares") {
		t.Errorf("the export says %q and %q; want 7 jobs and 1 host, and the one left out", stdout, &stderr)
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

	if len(trace.Arrivals) != 7 {
		t.Fatalf("the trace has %d jobs, want the 7 of a declared tier the ledger ran", len(trace.Arrivals))
	}

	first := trace.Arrivals[0]
	if first.Owner != "owner-1" || first.Repository != "repo-1" ||
		first.WorkflowRef != "owner-1/repo-1/.github/workflows/workflow-1.yml" || first.Job != "job-1 (matrix-1)" ||
		first.Tier != "billet-2vcpu" || time.Duration(first.Duration) != 6*time.Minute {
		t.Errorf("the first job exported as %+v; want stand-ins for acme/api's build (ubuntu), 6m on billet-2vcpu",
			first)
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

	// --names KEEPS WHAT WAS RECORDED.
	named := t.TempDir()
	if err := Jobs(t.Context(), env, []string{"export", "--config", cfg, "--out", named, "--names"}); err != nil {
		t.Fatalf("billet jobs export --names: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(named, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"repository":"api"`) || !strings.Contains(string(body), `"job":"build (ubuntu)"`) {
		t.Errorf("an export with --names lost the names:\n%s", body)
	}

	// A WINDOW WITH NOTHING TO REPLAY IS AN ERROR, not two empty files.
	empty := t.TempDir()
	if err := Jobs(t.Context(), env, []string{"export", "--config", cfg, "--out", empty, "--since", "1h"}); err == nil ||
		!strings.Contains(err.Error(), "nothing to replay") {
		t.Errorf("an export of an empty window gave %v; want it refused", err)
	}
}
