package scripts_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The acceptance matrix's shell (#554) is executed here against a fake gh and a fake
// billet placed first on PATH, in both directions, because each script's verdict is its
// exit status and a CI job reads nothing else.

// matrixFakes is a directory holding fake commands, and the PATH that finds them first.
type matrixFakes struct {
	bin, dir, path string
}

func newMatrixFakes(t *testing.T, tools ...string) matrixFakes {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("this test runs the script, which needs %s on PATH", tool)
		}
	}
	root := t.TempDir()
	f := matrixFakes{bin: filepath.Join(root, "bin"), dir: filepath.Join(root, "fixtures")}
	for _, d := range []string{f.bin, f.dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.path = f.bin + string(os.PathListSeparator) + os.Getenv("PATH")

	return f
}

func (f matrixFakes) command(t *testing.T, name, body string) {
	t.Helper()
	if err := forkSafeWriteFile(filepath.Join(f.bin, name), []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f matrixFakes) fixture(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f matrixFakes) run(t *testing.T, script string, env []string, args ...string) (string, int) {
	t.Helper()
	abs, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{abs}, args...)...)
	cmd.Env = append([]string{"PATH=" + f.path, "HOME=" + t.TempDir(), "FAKE=" + f.dir}, env...)
	out, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("run %s: %v\n%s", script, err, out)
	}

	return string(out), 0
}

// fakeContentsGH answers `gh api -i repos/<repo>/contents/.github/workflows/<file>` from
// $FAKE/<file>: a response as gh -i prints it, and $FAKE/<file>.exit for gh's status.
const fakeContentsGH = `name=$(basename "${3%%\?*}")
if [ -f "$FAKE/$name" ]; then cat "$FAKE/$name"; fi
exit "$(cat "$FAKE/$name.exit" 2>/dev/null || echo 0)"
`

func contentsResponse(t *testing.T, body []byte) string {
	t.Helper()
	enc, err := json.Marshal(map[string]string{"content": base64.StdEncoding.EncodeToString(body)})
	if err != nil {
		t.Fatal(err)
	}

	return "HTTP/2.0 200 OK\nContent-Type: application/json\n\n" + string(enc) + "\n"
}

// THE COPY CHECK ANSWERS THREE WAYS: every file the same is 0, one that differs or is
// missing is 1, and an answer GitHub did not give, or gave in a form that will not
// decode, is 2, never a match and never a mismatch.
func TestTheMatrixWorkflowCopyCheckAnswersThreeWays(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("acceptance/matrix/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no matrix workflows: %v", err)
	}
	for name, c := range map[string]struct {
		edit func(t *testing.T, f matrixFakes, file string, body []byte)
		want int
		says string
	}{
		"every file the same": {func(t *testing.T, f matrixFakes, file string, body []byte) {
			t.Helper()
			f.fixture(t, file, contentsResponse(t, body))
		}, 0, "matches this checkout"},
		"one differs": {func(t *testing.T, f matrixFakes, file string, body []byte) {
			t.Helper()
			if file == "matrix-warm.yml" {
				body = append(body, '#')
			}
			f.fixture(t, file, contentsResponse(t, body))
		}, 1, "matrix-warm.yml: billet-testbed/m's copy differs"},
		"one is missing": {func(t *testing.T, f matrixFakes, file string, body []byte) {
			t.Helper()
			if file == "matrix-cold.yml" {
				f.fixture(t, file, "HTTP/2.0 404 Not Found\n\n{}\n")
				f.fixture(t, file+".exit", "1")

				return
			}
			f.fixture(t, file, contentsResponse(t, body))
		}, 1, "matrix-cold.yml: not in billet-testbed/m"},
		"GitHub failed": {func(t *testing.T, f matrixFakes, file string, body []byte) {
			t.Helper()
			if file == "matrix-cold.yml" {
				f.fixture(t, file, "HTTP/2.0 502 Bad Gateway\n\n{}\n")
				f.fixture(t, file+".exit", "1")

				return
			}
			f.fixture(t, file, contentsResponse(t, body))
		}, 2, "could not read it"},
		"gh never connected": {func(t *testing.T, f matrixFakes, file string, _ []byte) {
			t.Helper()
			f.fixture(t, file+".exit", "1")
		}, 2, "HTTP none"},
		"content that will not decode": {func(t *testing.T, f matrixFakes, file string, body []byte) {
			t.Helper()
			f.fixture(t, file, "HTTP/2.0 200 OK\n\n{\"content\":\"!!!not base64\"}\n")
		}, 2, "would not decode"},
		"an answer with no content": {func(t *testing.T, f matrixFakes, file string, _ []byte) {
			t.Helper()
			f.fixture(t, file, "HTTP/2.0 200 OK\n\n{\"message\":\"a directory\"}\n")
		}, 2, "has no content"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newMatrixFakes(t, "jq", "base64", "cmp")
			f.command(t, "gh", fakeContentsGH)
			for _, path := range files {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				c.edit(t, f, filepath.Base(path), body)
			}
			out, code := f.run(t, "acceptance/matrix-workflows-match.sh", nil, "billet-testbed/m")
			if code != c.want || !strings.Contains(out, c.says) {
				t.Fatalf("exit %d, want %d with %q:\n%s", code, c.want, c.says, out)
			}
		})
	}
}

// fakeBillet records each invocation in $FAKE/billet.log and exits with
// $FAKE/<subcommand>.exit, writing what `acceptance up` and `matrix tiers` would.
const fakeBillet = `echo "$*" >>"$FAKE/billet.log"
key="$1-$2"
[ "$1" = acceptance ] && [ "$2" = matrix ] && key="matrix-$3"
case "$key" in
acceptance-up)
	ws=""
	while [ "$#" -gt 0 ]; do [ "$1" = --workspace ] && ws=$2; shift; done
	mkdir -p "$ws"
	echo '{"deployment_id":"dep-1"}' >"$ws/acceptance.json"
	;;
matrix-tiers)
	cat "$FAKE/tiers" 2>/dev/null
	;;
matrix-judge)
	echo "judged"
	;;
init-iam)
	echo '{"Version":"2012-10-17"}'
	;;
esac
exit "$(cat "$FAKE/$key.exit" 2>/dev/null || echo 0)"
`

func hostRunDir(t *testing.T, f matrixFakes) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(f.bin, "billet"), filepath.Join(dir, "billet")); err != nil {
		t.Fatal(err)
	}

	return dir
}

// PREPARE STANDS THE DEPLOYMENT UP AND RENDERS ITS POLICY, and refuses to go on when the
// base config gives the matrix no tier, whose run would judge every cell not run.
func TestTheMatrixHostPreparesADeploymentWithItsTiersAndPolicy(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "tiers", "ec2 x64 accept-x64\n")
	dir := hostRunDir(t, f)
	out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare", dir, "/base.yaml", "111122223333",
		"arn:aws:iam::111122223333:role/r", "arn:aws:kms:us-west-2:111122223333:key/k")
	if code != 0 || !strings.Contains(out, "deployment dep-1 is up") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for file, want := range map[string]string{"tiers": "ec2 x64 accept-x64\n", "deployment": "dep-1\n"} {
		got, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", file, got, err, want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "iam.json")); err != nil || !strings.Contains(string(got), "2012-10-17") {
		t.Errorf("iam.json = %q, %v", got, err)
	}
	log, err := os.ReadFile(filepath.Join(f.dir, "billet.log"))
	if err != nil || !strings.Contains(string(log), "acceptance up --config /base.yaml --workspace "+dir+"/ws --account 111122223333") ||
		!strings.Contains(string(log), "init iam --config "+dir+"/ws/billet.yaml --role-arn arn:aws:iam::111122223333:role/r") {
		t.Errorf("billet was run as:\n%s", log)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	dir = hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare", dir, "/b", "1", "r", "k"); code == 0 ||
		!strings.Contains(out, "no tier to dispatch to") {
		t.Errorf("a base config with no matrix tier: exit %d\n%s", code, out)
	}
}

// RUN SAYS THE WORSE OF ITS TWO ANSWERS: a run that failed is the exit status, and is
// still judged; a run that passed exits with the judge's verdict.
func TestTheMatrixHostRunExitsWithTheRunsOrTheJudgesFailure(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		run, judge string
		want       int
	}{
		"both passed":       {"0", "0", 0},
		"a cell failed":     {"0", "1", 1},
		"the run failed":    {"3", "0", 3},
		"both failed":       {"3", "1", 3},
		"the run timed out": {"1", "1", 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newMatrixFakes(t)
			f.command(t, "billet", fakeBillet)
			f.fixture(t, "acceptance-run.exit", c.run)
			f.fixture(t, "matrix-judge.exit", c.judge)
			dir := hostRunDir(t, f)
			out, code := f.run(t, "acceptance/matrix-host.sh", nil, "run", dir, "27", "x64")
			if code != c.want {
				t.Fatalf("exit %d, want %d:\n%s", code, c.want, out)
			}
			log, err := os.ReadFile(filepath.Join(f.dir, "billet.log"))
			if err != nil || !strings.Contains(string(log), "acceptance matrix judge --workspace "+dir+"/ws --archs x64") ||
				!strings.Contains(string(log), "acceptance run --workspace "+dir+"/ws --jobs 27") {
				t.Fatalf("billet was run as:\n%s", log)
			}
			if !strings.Contains(out, "judged") {
				t.Errorf("the verdicts were not printed:\n%s", out)
			}
		})
	}
}

// DOWN IS IDEMPOTENT: with no workspace there is nothing to take down, and with one its
// status is down's.
func TestTheMatrixHostDownIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t)
	f.command(t, "billet", fakeBillet)
	dir := hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code != 0 || !strings.Contains(out, "nothing to take down") {
		t.Fatalf("down with no workspace: exit %d\n%s", code, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ws", "acceptance.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fixture(t, "acceptance-down.exit", "4")
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code != 4 {
		t.Fatalf("a down that failed: exit %d\n%s", code, out)
	}
}

// fakeSSM answers send-command with a command id and get-command-invocation from
// $FAKE/status, $FAKE/code and $FAKE/stdout, recording the parameters it was sent.
const fakeSSM = `case "$2" in
send-command)
	prev=""
	for a in "$@"; do [ "$prev" = --parameters ] && echo "$a" >"$FAKE/params"; prev=$a; done
	echo cmd-1
	;;
get-command-invocation)
	case "$*" in
	*"--query Status"*) cat "$FAKE/status" ;;
	*StandardOutputContent*) cat "$FAKE/stdout" 2>/dev/null ;;
	*StandardErrorContent*) ;;
	*ResponseCode*) cat "$FAKE/code" ;;
	esac
	;;
esac
`

// THE HOST COMMAND'S STATUS IS THE HELPER'S: a success is 0, a failure its own code, a
// command SSM timed out or never saw end is 124, and one SSM cannot account for is 125,
// never a success.
func TestTheSSMHelperExitsWithTheHostCommandsStatus(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		status, code string
		want         int
	}{
		"a success":                      {"Success", "0", 0},
		"a failure keeps its code":       {"Failed", "3", 3},
		"SSM timed it out":               {"TimedOut", "-1", 124},
		"it was cancelled":               {"Cancelled", "-1", 125},
		"it never ended":                 {"InProgress", "", 124},
		"an exit SSM never ran":          {"Failed", "-1", 125},
		"a status SSM would not give us": {"Success", "", 125},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newMatrixFakes(t, "jq")
			f.command(t, "aws", fakeSSM)
			f.fixture(t, "status", c.status+"\n")
			f.fixture(t, "code", c.code+"\n")
			f.fixture(t, "stdout", "matrix: done\n")
			out, code := f.run(t, "acceptance/ssm-run.sh", []string{"SSM_POLL=0", "SSM_GRACE=0"}, "i-1", "1", "echo 'hi there'")
			if code != c.want {
				t.Fatalf("exit %d, want %d:\n%s", code, c.want, out)
			}
			if c.status != "InProgress" && !strings.Contains(out, "matrix: done") {
				t.Errorf("the host's output was not printed:\n%s", out)
			}
			params, err := os.ReadFile(filepath.Join(f.dir, "params"))
			if err != nil || strings.TrimSpace(string(params)) != `{"commands":["echo 'hi there'"],"executionTimeout":["1"]}` {
				t.Errorf("SSM was sent %s (%v)", params, err)
			}
		})
	}
}

// fakeDispatchGH dispatches by appending "<workflow> <label>" to $FAKE/sent, lists one
// run per sent workflow titled as the matrix's run-name, and reports a run completed
// once $FAKE/done-<id> exists, or at once when $FAKE/instant exists. $FAKE/unlisted makes
// the listing empty.
const fakeDispatchGH = `case "$1 $2" in
"workflow run")
	label="" tag=""
	for a in "$@"; do case "$a" in runs_on=*) label=${a#runs_on=} ;; tag=*) tag=${a#tag=} ;; esac; done
	echo "$3 $label $tag" >>"$FAKE/sent"
	;;
"run list")
	[ -f "$FAKE/unlisted" ] && exit 0
	wf="" ; prev=""
	for a in "$@"; do [ "$prev" = --workflow ] && wf=$a; prev=$a; done
	n=0
	while read -r w label tag; do
		n=$((n + 1))
		if [ "$w" = "$wf" ]; then echo "$n"; fi
	done <"$FAKE/sent"
	;;
"run view")
	if [ -f "$FAKE/instant" ] || [ -f "$FAKE/done-$3" ]; then
		case "$*" in *conclusion*) echo success ;; *) echo completed ;; esac
	else
		echo in_progress
	fi
	;;
esac
`

// THE WARM RUNS GO ONLY AFTER THE COLD ONES HAVE FINISHED, for every label, with the
// cold run's tag; and a run GitHub never lists stops the dispatcher rather than leaving it
// waiting on nothing.
func TestTheMatrixDispatcherSendsWarmOnlyAfterCold(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t)
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "instant", "")
	env := []string{"MATRIX_SETTLE=0", "MATRIX_POLL=0", "MATRIX_LIST_WAIT=1", "MATRIX_TIMEOUT=5"}
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", env, "billet-testbed/m", "t1", "accept-x64", "accept-arm")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	sent, err := os.ReadFile(filepath.Join(f.dir, "sent"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(sent)), "\n")
	want := []string{
		"matrix-cold.yml accept-x64 t1", "matrix-writers.yml accept-x64 t1", "matrix-writer-fails.yml accept-x64 t1",
		"matrix-cold.yml accept-arm t1", "matrix-writers.yml accept-arm t1", "matrix-writer-fails.yml accept-arm t1",
		"matrix-warm.yml accept-x64 t1", "matrix-warm.yml accept-arm t1",
	}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("sent\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	// A COLD RUN THAT NEVER FINISHES sends no warm run.
	f = newMatrixFakes(t)
	f.command(t, "gh", fakeDispatchGH)
	out, code = f.run(t, "acceptance/matrix-dispatch.sh", env, "billet-testbed/m", "t2", "accept-x64")
	if code != 1 || !strings.Contains(out, "did not complete within 5s") {
		t.Fatalf("a cold run that never finished: exit %d\n%s", code, out)
	}
	if sent, err := os.ReadFile(filepath.Join(f.dir, "sent")); err != nil || strings.Contains(string(sent), "matrix-warm") {
		t.Fatalf("a warm run was sent before the cold one finished:\n%s", sent)
	}

	f = newMatrixFakes(t)
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "unlisted", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", env, "billet-testbed/m", "t3", "accept-x64"); code != 1 ||
		!strings.Contains(out, "lists no run of matrix-cold.yml") {
		t.Fatalf("a run GitHub never listed: exit %d\n%s", code, out)
	}
}
