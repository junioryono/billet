package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
	"github.com/junioryono/billet/internal/usage"
)

// WHAT `billet jobs rightsize --json` WRITES IS WHAT `billet migrate workflows
// --rightsize` READS. The two are families that may not import each other, so each
// holds its own idea of the file and only this drives both, through the command tree:
// a field renamed on one side would otherwise leave each family's tests green and
// every recommendation silently unapplied.
func TestARightsizeReportFeedsTheWorkflowRewrite(t *testing.T) {
	stateDir := ledgertest.Dir(t)

	dir := t.TempDir()
	key := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(key, []byte("not read by these commands"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "billet.yaml")
	if err := os.WriteFile(cfg, []byte(`
server:
  listen: 127.0.0.1:7717
  state_dir: `+stateDir+`
  max_vcpu: 64
  max_memory: 256GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: `+key+`
tiers:
  - label: billet-2vcpu
    arch: x64
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
  - label: billet-8vcpu
    runs_on: eight
    arch: x64
    provider: docker
    vcpu: 8
    memory: 32GiB
    image: ubuntu:24.04
`), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	now := time.Now()
	stamp := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		// Twelve idle hours on eight vCPUs: 5% busy by the host's series over each job,
		// and 1 GiB at the host's peak, which proves the job small without its VM's word.
		for i := range 12 {
			lease := fmt.Sprintf("run-%d", i)
			started := now.Add(-time.Duration(24+i) * time.Hour)
			series, _, err := usage.EncodeSeriesGaps([]usage.Point{{}, {OffsetMillis: 3_602_000,
				CPUUsage: int64(0.05 * 8 * 3_602_000_000)}}, started.Add(-time.Second), 512<<10)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_series (lease_id, codec, series) VALUES (?, ?, ?)`,
				lease, usage.SeriesCodecGaps, base64.StdEncoding.EncodeToString(series)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
				(lease_id, tier, repo, workflow_ref, job_name, github_run_id, github_run_attempt, result, result_at,
				 queued_at, started_at, vcpu, memory)
				VALUES (?, 'billet-8vcpu', 'acme/api', 'acme/api/.github/workflows/ci.yml@refs/heads/main', 'build',
				        ?, 1, 'succeeded', ?, ?, ?, 8, 34359738368)`,
				lease, i+1, stamp(started.Add(time.Hour)), stamp(started), stamp(started)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_usage
				(lease_id, node, recorded_at, source, unmeasured, samples, interval_ms, window_ms, cpu_user_us,
				 cpu_system_us, guest_cpu_us, vmm_cpu_us, memory_peak_bytes, oom_kills, disk_read_bytes,
				 disk_write_bytes, net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets, cpu_some_us,
				 cpu_full_us, memory_some_us, memory_full_us, io_some_us, io_full_us, energy_active_uj,
				 energy_idle_uj, energy_source)
				VALUES (?, 'n', ?, 'host', '', 10, 1000, 3600000, 0, 0, 1440000000, 0, 1073741824, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				        0, 0, 0, 0, 0, 0, '')`, lease, stamp(started)); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("write the jobs: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	run := func(args ...string) (string, string) {
		t.Helper()

		var stdout, stderr bytes.Buffer
		env := cli.Env{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""), Getenv: os.Getenv}
		if err := cli.Run(args, env, commands, func(int) {}); err != nil {
			t.Fatalf("billet %v: %v\n%s", args, err, stderr.String())
		}

		return stdout.String(), stderr.String()
	}

	report, _ := run("jobs", "rightsize", "--config", cfg, "--json")
	if !strings.Contains(report, `"recommended_runs_on": "billet-2vcpu"`) {
		t.Fatalf("the report recommends nothing to act on:\n%s", report)
	}
	file := filepath.Join(dir, "rightsize.json")
	if err := os.WriteFile(file, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}

	checkout := t.TempDir()
	workflows := filepath.Join(checkout, ".github", "workflows")
	if err := os.MkdirAll(workflows, 0o750); err != nil {
		t.Fatal(err)
	}
	const before = "on: [push]\njobs:\n  build:\n    runs-on: eight # sized by hand\n    steps: []\n"
	target := filepath.Join(workflows, "ci.yml")
	if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	_, said := run("migrate", "workflows", "--config", cfg, "--path", checkout, "--repo", "acme/api", "--rightsize", file,
		"--write")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(before, "runs-on: eight", "runs-on: billet-2vcpu", 1); string(got) != want {
		t.Errorf("the workflow became %q, want %q; the command said:\n%s", got, want, said)
	}
	if !strings.Contains(said, "tier billet-2vcpu by the rightsize report") {
		t.Errorf("the command does not say the report moved it:\n%s", said)
	}
}
