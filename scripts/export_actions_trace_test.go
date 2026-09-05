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
// ran must be left out, the tier must be the label the rule names and never
// the first one the workflow happened to list, GitHub's conclusion must arrive
// spelled as the wire spells it. So it runs here against a fake `gh` that
// serves fixture pages, and what it emits is read back by the reader the replay
// uses.

const exportScript = "export-actions-trace.sh"

// fakeGH stands in for the CLI. It answers `api` calls from $FAKE_GH_DIR: the
// runs from runs.json and each run's jobs from jobs-<id>.json, each fixture the
// concatenated pages `gh api --paginate` writes, and it refuses a call without
// --paginate, because a call without it is a first page presented as the whole
// history. It refuses any other flag, because `gh api` has no --arg and takes
// --jq's filter as the next word, so a script that shaped its answer inside gh
// would be tested against a jq the real CLI never runs. FAKE_GH_FAIL makes
// every call fail, the way an expired token would; FAKE_GH_FAIL_RUN makes one
// run's jobs page fail, the way a later page can.
const fakeGH = `#!/bin/bash
set -euo pipefail
if [ -n "${FAKE_GH_FAIL:-}" ]; then
  echo "gh: HTTP 401: Bad credentials" >&2
  exit 1
fi
[ "$1" = "api" ] || { echo "fake gh: unexpected subcommand $1" >&2; exit 90; }
shift
endpoint=""
paginate=""
while [ $# -gt 0 ]; do
  case "$1" in
    --paginate) paginate=yes; shift ;;
    -*) echo "fake gh: unexpected flag $1" >&2; exit 91 ;;
    *) endpoint=$1; shift ;;
  esac
done
[ -n "$paginate" ] || { echo "fake gh: a history request without --paginate is one page" >&2; exit 94; }
path=${endpoint%%\?*}
case "$path" in
  repos/*/actions/runs) file="$FAKE_GH_DIR/runs.json" ;;
  repos/*/actions/runs/*/jobs)
    id=${path%/jobs}; id=${id##*/}
    if [ "$id" = "${FAKE_GH_FAIL_RUN:-}" ]; then echo "gh: HTTP 502: Bad gateway" >&2; exit 1; fi
    file="$FAKE_GH_DIR/jobs-$id.json" ;;
  *) echo "fake gh: unexpected endpoint $path" >&2; exit 92 ;;
esac
[ -f "$file" ] || { echo "fake gh: no fixture for $path" >&2; exit 93; }
printf '%s\n' "$endpoint" >> "$FAKE_GH_LOG"
cat "$file"
`

// runsPages is what --paginate writes for a two-page listing: two objects, one
// after the other, with no array around them.
const runsPages = `{"total_count": 2, "workflow_runs": [
  {"id": 101, "path": ".github/workflows/ci.yml", "head_branch": "main"}
]}
{"total_count": 2, "workflow_runs": [
  {"id": 102, "path": ".github/workflows/release.yml", "head_branch": "release/v0.6"}
]}`

// Run 101: the tier label is listed LAST, after the generic labels a self-hosted
// job carries, on two completed jobs; a third is still running and has no
// duration; a fourth started and completed within one second of GitHub's clock.
const jobsRun101 = `{"total_count": 4, "jobs": [
  {"id": 1, "status": "completed", "conclusion": "success", "labels": ["self-hosted", "linux", "billet-2vcpu"],
   "created_at": "2026-03-02T09:00:00Z", "started_at": "2026-03-02T09:00:40Z", "completed_at": "2026-03-02T09:04:45Z"},
  {"id": 2, "status": "completed", "conclusion": "failure", "labels": ["self-hosted", "linux", "billet-4vcpu"],
   "created_at": "2026-03-02T09:00:05Z", "started_at": "2026-03-02T09:01:00Z", "completed_at": "2026-03-02T09:11:00Z"},
  {"id": 3, "status": "in_progress", "conclusion": null, "labels": ["self-hosted", "linux", "billet-2vcpu"],
   "created_at": "2026-03-02T09:00:10Z", "started_at": "2026-03-02T09:00:50Z", "completed_at": null},
  {"id": 6, "status": "completed", "conclusion": "success", "labels": ["billet-2vcpu"],
   "created_at": "2026-03-02T09:00:20Z", "started_at": "2026-03-02T09:00:59Z", "completed_at": "2026-03-02T09:00:59Z"}
]}`

// Run 102, over two job pages: a hosted job no billet rule names, and a job
// that asked for two billet labels, which no prefix rule can name once.
const jobsRun102 = `{"total_count": 2, "jobs": [
  {"id": 4, "status": "completed", "conclusion": "success", "labels": ["ubuntu-latest"],
   "created_at": "2026-03-02T10:00:00Z", "started_at": "2026-03-02T10:00:10Z", "completed_at": "2026-03-02T10:02:10Z"}
]}
{"total_count": 2, "jobs": [
  {"id": 5, "status": "completed", "conclusion": "success", "labels": ["billet-2vcpu", "billet-4vcpu"],
   "created_at": "2026-03-02T10:00:01Z", "started_at": "2026-03-02T10:00:20Z", "completed_at": "2026-03-02T10:03:20Z"}
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

	for _, tool := range []string{"bash", "jq", "mktemp", "rm", "cat"} {
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
		"runs.json":     runsPages,
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

// The exporter writes a trace the replay reads: completed jobs only, the tier
// the prefix rule names once, GitHub's stamps as arrivals, whole-second
// durations, the wire's spelling of success and the workflow reference the
// scale-set message would carry, across every page of runs and jobs.
func TestTheExporterWritesATraceTheReplayReads(t *testing.T) {
	h := newExportHarness(t)

	stdout, stderr, err := h.run(t, nil, "acme/web", "--since", "2026-03-01", "--prefix", "billet-")
	if err != nil {
		t.Fatalf("the exporter failed: %v\nstderr: %s", err, stderr)
	}

	trace, err := replay.ReadTrace(strings.NewReader(stdout))
	if err != nil {
		t.Fatalf("the replay cannot read what the exporter wrote: %v\n%s", err, stdout)
	}

	// Three jobs and only three: the running job has no duration, the hosted
	// job carries no billet label, and the job with two billet labels is named
	// by neither rule once.
	if len(trace.Arrivals) != 3 {
		t.Fatalf("exported %d jobs, want 3:\n%s", len(trace.Arrivals), stdout)
	}

	first := trace.Arrivals[0]

	// THE TIER IS THE LABEL THE RULE NAMED, listed last in the fixture behind
	// `self-hosted` and `linux`; a first-label rule would have exported
	// `self-hosted` twice.
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

	// A JOB GITHUB'S CLOCK CANNOT SEPARATE FROM ITS OWN START IS ONE SECOND, not
	// zero, which the replay would refuse.
	if third := trace.Arrivals[2]; time.Duration(third.Duration) != time.Second {
		t.Errorf("the same-second job was exported with duration %s, want 1s", time.Duration(third.Duration))
	}

	if !strings.Contains(stderr, "exported 3 jobs from 2 completed runs") ||
		!strings.Contains(stderr, "left out 2 completed jobs") {
		t.Errorf("the summary on stderr does not count what was written and left out: %q", stderr)
	}

	// BOTH RUNS WERE ASKED FOR, so the second page of runs reached the loop, and
	// the date reached GitHub encoded on the runs request.
	log, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"created=%3E%3D2026-03-01", "runs/101/jobs", "runs/102/jobs"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("gh was never asked %q: %s", want, log)
		}
	}
}

// --label exports the jobs that asked for that exact label and names it as their
// tier, whatever else they asked for.
func TestTheExporterFiltersByLabel(t *testing.T) {
	h := newExportHarness(t)

	stdout, stderr, err := h.run(t, nil, "acme/web", "--since", "2026-03-01", "--label", "billet-2vcpu")
	if err != nil {
		t.Fatalf("the exporter failed: %v\nstderr: %s", err, stderr)
	}

	trace, err := replay.ReadTrace(strings.NewReader(stdout))
	if err != nil {
		t.Fatalf("unreadable trace: %v\n%s", err, stdout)
	}

	// Run 101's first and same-second jobs, and run 102's two-label job, which
	// an exact label names once where a prefix cannot.
	if len(trace.Arrivals) != 3 || trace.Arrivals[0].RunID != 101 || trace.Arrivals[2].RunID != 102 {
		t.Fatalf("--label billet-2vcpu exported %+v, want run 101's two jobs and run 102's two-label job",
			trace.Arrivals)
	}

	for _, a := range trace.Arrivals {
		if a.Tier != "billet-2vcpu" {
			t.Errorf("job in run %d exported under tier %q, want the label asked for", a.RunID, a.Tier)
		}
	}
}

// A page GitHub refuses fails the run, whichever page it is. A trace missing a
// page is a shorter workload that replays clean, which is the wrong kind of
// wrong; and a failure on the LAST run's jobs, after every earlier run was
// written, must still be the exit status.
func TestAFailedPageFailsTheExport(t *testing.T) {
	h := newExportHarness(t)

	for name, env := range map[string][]string{
		"every call refused":  {"FAKE_GH_FAIL=1"},
		"the last run's jobs": {"FAKE_GH_FAIL_RUN=102"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := h.run(t, env, "acme/web", "--since", "2026-03-01", "--prefix", "billet-")
			if err == nil {
				t.Fatal("the exporter exited 0 with a gh call failing")
			}

			if !strings.Contains(stderr, "gh: HTTP") {
				t.Errorf("gh's refusal did not reach stderr: %q", stderr)
			}

			// AND NOTHING REACHED STDOUT. With `> trace.jsonl` a partial trace is
			// a complete-looking smaller workload, which is the wrong kind of wrong.
			if stdout != "" {
				t.Errorf("a failed export still wrote a trace to stdout:\n%s", stdout)
			}
		})
	}
}

// A job GitHub says completed before it started is not a short job, it is data
// nobody can replay honestly, and it fails the export with nothing on stdout.
func TestAJobThatCompletedBeforeItStartedFailsTheExport(t *testing.T) {
	h := newExportHarness(t)

	corrupt := `{"total_count": 1, "jobs": [
  {"id": 9, "status": "completed", "conclusion": "success", "labels": ["billet-2vcpu"],
   "created_at": "2026-03-02T10:00:00Z", "started_at": "2026-03-02T10:05:00Z", "completed_at": "2026-03-02T10:04:00Z"}
]}`

	if err := os.WriteFile(filepath.Join(h.fixtures, "jobs-102.json"), []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := h.run(t, nil, "acme/web", "--since", "2026-03-01", "--prefix", "billet-")
	if err == nil {
		t.Fatal("the exporter exited 0 on a job that completed before it started")
	}

	if !strings.Contains(stderr, "completed before it started") {
		t.Errorf("the refusal did not say what was wrong: %q", stderr)
	}

	if stdout != "" {
		t.Errorf("a failed export still wrote a trace to stdout:\n%s", stdout)
	}
}

// The arguments are refused before GitHub is asked.
func TestTheExporterRefusesBadArguments(t *testing.T) {
	h := newExportHarness(t)

	for name, args := range map[string][]string{
		"no repository":       {"--since", "2026-03-01", "--prefix", "billet-"},
		"no date":             {"acme/web", "--prefix", "billet-"},
		"bad date":            {"acme/web", "--since", "yesterday", "--prefix", "billet-"},
		"bare repo":           {"web", "--since", "2026-03-01", "--prefix", "billet-"},
		"no label rule":       {"acme/web", "--since", "2026-03-01"},
		"two label rules":     {"acme/web", "--since", "2026-03-01", "--label", "a", "--prefix", "b"},
		"an unknown flag":     {"acme/web", "--since", "2026-03-01", "--prefix", "billet-", "--all"},
		"a rule with no word": {"acme/web", "--since", "2026-03-01", "--prefix"},
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
