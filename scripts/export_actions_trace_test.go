package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/replay"
)

// THE EXPORTER IS EXECUTED, NOT PATTERN-MATCHED. It is the one way a real
// workload reaches the replay harness, and every rule in it is a shell rule: a
// failed page must fail the run rather than shorten the trace, a job that never
// ran must be left out, GitHub's conclusion must arrive spelled as the wire
// spells it. So it runs here against a fake `gh` that serves fixture pages and
// evaluates the script's own jq, and what it emits is read back by the reader
// the replay uses.

const exportScript = "export-actions-trace.sh"

// fakeGH stands in for the CLI. It answers `api` calls from $FAKE_GH_DIR: the
// runs page from runs.json and each run's jobs from jobs-<id>.json, and refuses
// any flag but --paginate, because `gh api` has no --arg and takes --jq's filter
// as the next word, so a script that shaped its answer inside gh would be tested
// against a jq the real CLI never runs. FAKE_GH_FAIL makes every call fail, the
// way an expired token would.
const fakeGH = `#!/bin/bash
set -euo pipefail
if [ -n "${FAKE_GH_FAIL:-}" ]; then
  echo "gh: HTTP 401: Bad credentials" >&2
  exit 1
fi
[ "$1" = "api" ] || { echo "fake gh: unexpected subcommand $1" >&2; exit 90; }
shift
endpoint=""
while [ $# -gt 0 ]; do
  case "$1" in
    --paginate) shift ;;
    -*) echo "fake gh: unexpected flag $1" >&2; exit 91 ;;
    *) endpoint=$1; shift ;;
  esac
done
path=${endpoint%%\?*}
case "$path" in
  repos/*/actions/runs) file="$FAKE_GH_DIR/runs.json" ;;
  repos/*/actions/runs/*/jobs) id=${path%/jobs}; id=${id##*/}; file="$FAKE_GH_DIR/jobs-$id.json" ;;
  *) echo "fake gh: unexpected endpoint $path" >&2; exit 92 ;;
esac
[ -f "$file" ] || { echo "fake gh: no fixture for $path" >&2; exit 93; }
printf '%s\n' "$endpoint" >> "$FAKE_GH_LOG"
cat "$file"
`

const runsPage = `{"total_count": 2, "workflow_runs": [
  {"id": 101, "path": ".github/workflows/ci.yml", "head_branch": "main"},
  {"id": 102, "path": ".github/workflows/release.yml", "head_branch": "release/v0.6"}
]}`

// Run 101: two completed jobs on two labels, and one still running, which has no
// duration and must be left out.
const jobsRun101 = `{"total_count": 3, "jobs": [
  {"id": 1, "status": "completed", "conclusion": "success", "labels": ["billet-2vcpu"],
   "created_at": "2026-03-02T09:00:00Z", "started_at": "2026-03-02T09:00:40Z", "completed_at": "2026-03-02T09:04:45Z"},
  {"id": 2, "status": "completed", "conclusion": "failure", "labels": ["billet-4vcpu", "self-hosted"],
   "created_at": "2026-03-02T09:00:05Z", "started_at": "2026-03-02T09:01:00Z", "completed_at": "2026-03-02T09:11:00Z"},
  {"id": 3, "status": "in_progress", "conclusion": null, "labels": ["billet-2vcpu"],
   "created_at": "2026-03-02T09:00:10Z", "started_at": "2026-03-02T09:00:50Z", "completed_at": null}
]}`

// Run 102: one job on a hosted label the replay's fleet would not declare.
const jobsRun102 = `{"total_count": 1, "jobs": [
  {"id": 4, "status": "completed", "conclusion": "success", "labels": ["ubuntu-latest"],
   "created_at": "2026-03-02T10:00:00Z", "started_at": "2026-03-02T10:00:10Z", "completed_at": "2026-03-02T10:02:10Z"}
]}`

// exportHarness is one run's fake CLI, fixtures and log, on a PATH holding only
// the fake `gh`, jq and the coreutils the script uses, so the machine's real gh
// can never be reached.
type exportHarness struct {
	bin, fixtures, log string
}

func newExportHarness(t *testing.T) exportHarness {
	t.Helper()

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq on PATH; the exporter shapes GitHub's answers with it")
	}

	root := t.TempDir()
	h := exportHarness{
		bin:      filepath.Join(root, "bin"),
		fixtures: filepath.Join(root, "fixtures"),
		log:      filepath.Join(root, "gh.log"),
	}

	for _, dir := range []string{h.bin, h.fixtures} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	for _, tool := range []string{"bash", "jq", "mktemp", "rm", "wc", "cat"} {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("this test needs %s on PATH: %v", tool, err)
		}

		if err := os.Symlink(resolved, filepath.Join(h.bin, tool)); err != nil {
			t.Fatalf("symlink %s: %v", tool, err)
		}
	}

	if err := os.WriteFile(filepath.Join(h.bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	for name, body := range map[string]string{
		"runs.json":     runsPage,
		"jobs-101.json": jobsRun101,
		"jobs-102.json": jobsRun102,
	} {
		if err := os.WriteFile(filepath.Join(h.fixtures, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	return h
}

// run executes the exporter with the fake on PATH and returns its stdout,
// stderr and exit error.
func (h exportHarness) run(t *testing.T, env []string, args ...string) (string, string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), filepath.Join(h.bin, "bash"), append([]string{exportScript}, args...)...)
	cmd.Env = append([]string{
		"PATH=" + h.bin,
		"HOME=" + t.TempDir(),
		"FAKE_GH_DIR=" + h.fixtures,
		"FAKE_GH_LOG=" + h.log,
	}, env...)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

// The exporter writes a trace the replay reads: completed jobs only, GitHub's
// stamps as arrivals, whole-second durations, the wire's spelling of success and
// the workflow reference the scale-set message would carry.
func TestTheExporterWritesATraceTheReplayReads(t *testing.T) {
	h := newExportHarness(t)

	stdout, stderr, err := h.run(t, nil, "acme/web", "--since", "2026-03-01")
	if err != nil {
		t.Fatalf("the exporter failed: %v\nstderr: %s", err, stderr)
	}

	trace, err := replay.ReadTrace(strings.NewReader(stdout))
	if err != nil {
		t.Fatalf("the replay cannot read what the exporter wrote: %v\n%s", err, stdout)
	}

	if len(trace.Arrivals) != 3 {
		t.Fatalf("exported %d jobs, want the 3 completed ones (the in-progress job has no duration):\n%s",
			len(trace.Arrivals), stdout)
	}

	first := trace.Arrivals[0]

	if first.Tier != "billet-2vcpu" || first.Owner != "acme" || first.Repository != "web" ||
		first.RunID != 101 || first.Result != replay.ResultSucceeded {
		t.Errorf("the first job was exported as %+v", first)
	}

	if first.WorkflowRef != "acme/web/.github/workflows/ci.yml@refs/heads/main" {
		t.Errorf("workflow ref %q is not spelled as the scale-set wire spells one", first.WorkflowRef)
	}

	if got := time.Duration(first.Duration); got != 4*time.Minute+5*time.Second {
		t.Errorf("duration %s, want 4m5s from started_at to completed_at", got)
	}

	if !first.At.Equal(time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("arrival %s, want the job's created_at", first.At)
	}

	// A FAILURE IS KEPT VERBATIM. Only success is respelled, because it is the one
	// word billet recognises; anything else has to reach the report as GitHub said it.
	if second := trace.Arrivals[1]; second.Result != "failure" || second.Tier != "billet-4vcpu" {
		t.Errorf("the failed job was exported as %+v", second)
	}

	if third := trace.Arrivals[2]; third.Tier != "ubuntu-latest" || third.RunID != 102 ||
		third.WorkflowRef != "acme/web/.github/workflows/release.yml@refs/heads/release/v0.6" {
		t.Errorf("the second run's job was exported as %+v", third)
	}

	if !strings.Contains(stderr, "exported 3 jobs from 2 completed runs") {
		t.Errorf("the summary on stderr does not count what was written: %q", stderr)
	}

	// AND THE DATE REACHED GITHUB, encoded, on the runs request.
	log, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(log), "created=%3E%3D2026-03-01") {
		t.Errorf("the runs request did not carry the --since date: %s", log)
	}
}

// --label keeps only the jobs that asked for that label and names it as their
// tier, so a repository whose workflows run on several labels can be replayed
// one tier at a time.
func TestTheExporterFiltersByLabel(t *testing.T) {
	h := newExportHarness(t)

	stdout, stderr, err := h.run(t, nil, "acme/web", "--since", "2026-03-01", "--label", "self-hosted")
	if err != nil {
		t.Fatalf("the exporter failed: %v\nstderr: %s", err, stderr)
	}

	trace, err := replay.ReadTrace(strings.NewReader(stdout))
	if err != nil {
		t.Fatalf("unreadable trace: %v\n%s", err, stdout)
	}

	if len(trace.Arrivals) != 1 || trace.Arrivals[0].Tier != "self-hosted" || trace.Arrivals[0].RunID != 101 {
		t.Fatalf("--label self-hosted exported %+v, want only run 101's second job under that tier",
			trace.Arrivals)
	}
}

// A page GitHub refuses fails the run. A trace missing a page is a shorter
// workload that replays clean, which is the wrong kind of wrong.
func TestAFailedPageFailsTheExport(t *testing.T) {
	h := newExportHarness(t)

	_, stderr, err := h.run(t, []string{"FAKE_GH_FAIL=1"}, "acme/web", "--since", "2026-03-01")
	if err == nil {
		t.Fatal("the exporter exited 0 with every gh call failing")
	}

	if !strings.Contains(stderr, "Bad credentials") {
		t.Errorf("gh's refusal did not reach stderr: %q", stderr)
	}
}

// The arguments are refused before GitHub is asked.
func TestTheExporterRefusesBadArguments(t *testing.T) {
	h := newExportHarness(t)

	for name, args := range map[string][]string{
		"no repository": {"--since", "2026-03-01"},
		"no date":       {"acme/web"},
		"bad date":      {"acme/web", "--since", "yesterday"},
		"bare repo":     {"web", "--since", "2026-03-01"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := h.run(t, nil, args...); err == nil {
				t.Fatalf("%v was accepted", args)
			}

			if _, err := os.Stat(h.log); err == nil {
				t.Fatalf("gh was called despite %s", name)
			}
		})
	}
}
