package scripts_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	// EXECUTED BY ITS PATH, as the workflow runs it, so a script committed without its
	// executable bit fails here too.
	cmd := exec.CommandContext(t.Context(), abs, args...)
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
		"a comparison that could not be made": {func(t *testing.T, f matrixFakes, file string, body []byte) {
			t.Helper()
			f.command(t, "cmp", "exit 2\n")
			f.fixture(t, file, contentsResponse(t, body))
		}, 2, "could not compare it"},
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
acceptance-run)
	# What lock it was handed, and whether that descriptor is open in it.
	lock=${BILLET_ACCEPTANCE_LOCK_FD:-}
	if [ -n "$lock" ] && [ -e "/dev/fd/$lock" ]; then echo "$lock open" >"$FAKE/lock"; else echo "${lock:-none} closed" >"$FAKE/lock"; fi
	echo "the fake run is running"
	echo "the fake run, on stderr" >&2
	# The process group it runs in, against its supervisor's.
	ps -o pgid= -p "$$" | tr -d ' ' >"$FAKE/pgid"
	if [ -f "$FAKE/run-late" ]; then
		sleep 0.5
		echo "the fake run, still writing"
	fi
	# With $FAKE/run-waits, a run that waits to be stopped: it says when its TERM trap is
	# installed, records each TERM it gets, and ends once it has had one, or after ten
	# seconds as a guard; a second TERM would show as a second line.
	if [ -f "$FAKE/run-waits" ]; then
		trap 'echo term >>"$FAKE/terms"' TERM
		echo "$$" >"$FAKE/ready"
		for _ in $(seq 100); do
			[ -s "$FAKE/terms" ] && break
			sleep 0.1
		done
		sleep 0.5
	fi
	;;
init-iam)
	echo '{"Version":"2012-10-17"}'
	;;
matrix-teardown-policy)
	echo '{"Version":"2012-10-17","Statement":[{"Sid":"BilletMatrixDecommissionState"}]}'
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

func billetLog(t *testing.T, f matrixFakes) string {
	t.Helper()
	log, err := os.ReadFile(filepath.Join(f.dir, "billet.log"))
	if err != nil {
		t.Fatalf("billet never ran: %v", err)
	}

	return string(log)
}

// PREPARE STANDS THE DEPLOYMENT UP AND RENDERS ITS POLICY, naming a guest role for
// iam:PassRole only when one is given, and refuses to go on when the base config gives
// the matrix no tier, whose run would judge every cell not run.
func TestTheMatrixHostPreparesADeploymentWithItsTiersAndPolicy(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "tiers", "ec2 x64 accept-x64\n")
	dir := hostRunDir(t, f)
	out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare", dir, "/base.yaml", "111122223333",
		"arn:aws:kms:us-west-2:111122223333:key/k")
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
	if got, err := os.ReadFile(filepath.Join(dir, "teardown.json")); err != nil ||
		!strings.Contains(string(got), "BilletMatrixDecommissionState") {
		t.Errorf("teardown.json = %q, %v", got, err)
	}
	log := billetLog(t, f)
	if !strings.Contains(log, "acceptance up --config /base.yaml --workspace "+dir+"/ws --account 111122223333") ||
		!strings.Contains(log, "init iam --config "+dir+"/ws/billet.yaml --kms-key-arn arn:aws:kms:us-west-2:111122223333:key/k\n") ||
		!strings.Contains(log, "acceptance matrix teardown-policy --workspace "+dir+"/ws\n") {
		t.Errorf("billet was run as:\n%s", log)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "tiers", "ec2 x64 accept-x64\n")
	dir = hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare", dir, "/b", "1", "k", "arn:aws:iam::1:role/guest"); code != 0 ||
		!strings.Contains(billetLog(t, f), "--kms-key-arn k --role-arn arn:aws:iam::1:role/guest") {
		t.Errorf("a guest role: exit %d\n%s\n%s", code, out, billetLog(t, f))
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "tiers", "ec2 x64 accept-x64\n")
	f.fixture(t, "matrix-teardown-policy.exit", "1")
	dir = hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare", dir, "/b", "1", "k"); code == 0 ||
		!strings.Contains(out, "teardown's policy could not be rendered") {
		t.Errorf("a teardown policy that could not be rendered: exit %d\n%s", code, out)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	dir = hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare", dir, "/b", "1", "k"); code == 0 ||
		!strings.Contains(out, "no tier to dispatch to") {
		t.Errorf("a base config with no matrix tier: exit %d\n%s", code, out)
	}
}

// RUN EXITS WITH THE RUN'S STATUS AND RECORDS IT, and is live only while it runs; JUDGE
// EXITS WITH THE JUDGE'S, for exactly the runs and repository it is named, keeping the
// evidence beside the verdicts, and judges nothing for a run with no recorded exit.
func TestTheMatrixHostRunsAndJudges(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"0", "3"} {
		f := newMatrixFakes(t)
		f.command(t, "billet", fakeBillet)
		f.fixture(t, "acceptance-run.exit", status)
		dir := hostRunDir(t, f)
		if out, code := f.run(t, "acceptance/matrix-host.sh", []string{"MATRIX_POLL=0.1"}, "run", dir, "27"); strconv.Itoa(code) != status {
			t.Fatalf("a run that ended %s: exit %d\n%s", status, code, out)
		}
		if log := billetLog(t, f); !strings.Contains(log, "acceptance run --workspace "+dir+"/ws --jobs 27 --wait 210m --no-teardown\n") {
			t.Fatalf("billet was run as:\n%s", log)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "run.exit")); err != nil || string(got) != status+"\n" {
			t.Errorf("run.exit = %q, %v; want %s", got, err, status)
		}
		// THE RUN IS HANDED THE LOCK, BY NAME, so it can keep it from its services.
		if got, err := os.ReadFile(filepath.Join(f.dir, "lock")); err != nil || !strings.HasSuffix(string(got), " open\n") ||
			strings.HasPrefix(string(got), "none") {
			t.Errorf("the run was handed %q (%v), want an open descriptor named in BILLET_ACCEPTANCE_LOCK_FD", got, err)
		}
	}

	f := newMatrixFakes(t)
	f.command(t, "billet", fakeBillet)
	dir := hostRunDir(t, f)
	if err := os.WriteFile(filepath.Join(dir, "run.log"), []byte("the run, still writing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "judge", dir, "x64", dir+"/runs", "billet-testbed/m"); code != 1 ||
		!strings.Contains(out, "no recorded exit") {
		t.Fatalf("a judge of a run with no recorded exit: exit %d\n%s", code, out)
	}
	// THE LOG IS KEPT ALL THE SAME: it is what explains a run with no exit.
	if got, err := os.ReadFile(filepath.Join(dir, "out", "run.log")); err != nil || string(got) != "the run, still writing\n" {
		t.Fatalf("a judge that refused left out/run.log %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "billet.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("billet was asked to judge a run that had not ended (%v)", err)
	}

	for _, status := range []string{"0", "1"} {
		f := newMatrixFakes(t)
		f.command(t, "billet", fakeBillet)
		f.fixture(t, "matrix-judge.exit", status)
		dir := hostRunDir(t, f)
		if err := os.MkdirAll(filepath.Join(dir, "ws"), 0o755); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"ws/evidence.json": `{"version":1}`, "run.exit": "0\n"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		out, code := f.run(t, "acceptance/matrix-host.sh", nil, "judge", dir, "x64", dir+"/runs", "billet-testbed/m")
		if strconv.Itoa(code) != status || !strings.Contains(out, "judged") {
			t.Fatalf("a judge that ended %s: exit %d\n%s", status, code, out)
		}
		if log := billetLog(t, f); !strings.Contains(log, "acceptance matrix judge --workspace "+dir+"/ws --archs x64 --runs "+
			dir+"/runs --repository billet-testbed/m --out "+dir+"/out/matrix.json") {
			t.Fatalf("billet was run as:\n%s", log)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "out", "evidence.json")); err != nil || string(got) != `{"version":1}` {
			t.Errorf("the evidence was not kept beside the verdicts: %q, %v", got, err)
		}
	}
}

// holdRunLock holds a run directory's lock as a live run's supervisor would, until
// the test ends.
func holdRunLock(t *testing.T, dir string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("release the lock: %v", err)
		}
	})
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("take the lock: %v", err)
	}
}

// A STOP IS A REQUEST THE RUN'S SUPERVISOR HONOURS ONCE: it signals the run one TERM,
// records how the run ended, and says so once; a stop requested before the run starts
// means it never starts; and a second run is never started beside one that holds the
// lock.
func TestTheMatrixHostStopIsHonouredByTheRunOnce(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "python3")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "run-waits", "")
	dir := hostRunDir(t, f)
	abs, err := filepath.Abs("acceptance/matrix-host.sh")
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	run := exec.CommandContext(t.Context(), abs, "run", dir, "27")
	run.Env = []string{"PATH=" + f.path, "HOME=" + t.TempDir(), "FAKE=" + f.dir, "MATRIX_POLL=0.1"}
	run.Stderr = &stderr
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	// READY, by the fake run's own word that its trap is installed.
	ready := false
	for range 200 {
		if b, err := os.ReadFile(filepath.Join(f.dir, "ready")); err == nil && len(b) > 0 {
			ready = true

			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("the run never said it was ready")
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "run", dir, "27"); code != 1 ||
		!strings.Contains(out, "not starting a second") {
		t.Fatalf("a second run beside a live one: exit %d\n%s", code, out)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "stop", dir); code != 0 || !strings.Contains(out, "a stop is requested") {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	if err := run.Wait(); err != nil {
		t.Fatalf("the run: %v\n%s", err, stderr.String())
	}
	if n := strings.Count(stderr.String(), "stopping the run early"); n != 1 {
		t.Fatalf("the supervisor signalled %d times, want once:\n%s", n, stderr.String())
	}
	if b, err := os.ReadFile(filepath.Join(f.dir, "terms")); err != nil || string(b) != "term\n" {
		t.Fatalf("the run got %q TERMs (%v), want one", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "run.exit")); err != nil || string(b) != "0\n" {
		t.Fatalf("run.exit = %q, %v", b, err)
	}

	// A SIGTERM OF THE SUPERVISOR'S OWN is the same request: the run gets one TERM
	// through the same gate, and how it ended is still recorded.
	f = newMatrixFakes(t, "python3")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "run-waits", "")
	dir = hostRunDir(t, f)
	stderr.Reset()
	run = exec.CommandContext(t.Context(), abs, "run", dir, "27")
	run.Env = []string{"PATH=" + f.path, "HOME=" + t.TempDir(), "FAKE=" + f.dir, "MATRIX_POLL=0.1"}
	run.Stderr = &stderr
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	ready = false
	for range 200 {
		if b, err := os.ReadFile(filepath.Join(f.dir, "ready")); err == nil && len(b) > 0 {
			ready = true

			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("the run never said it was ready")
	}
	if err := run.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := run.Wait(); err != nil {
		t.Fatalf("the run after its supervisor's SIGTERM: %v\n%s", err, stderr.String())
	}
	if n := strings.Count(stderr.String(), "stopping the run early"); n != 1 {
		t.Fatalf("the supervisor signalled %d times on its own SIGTERM, want once:\n%s", n, stderr.String())
	}
	if b, err := os.ReadFile(filepath.Join(f.dir, "terms")); err != nil || string(b) != "term\n" {
		t.Fatalf("the run got %q TERMs (%v) on its supervisor's SIGTERM, want one", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "run.exit")); err != nil || string(b) != "0\n" {
		t.Fatalf("run.exit after its supervisor's SIGTERM = %q, %v", b, err)
	}

	// Requested before it started.
	f = newMatrixFakes(t, "python3")
	f.command(t, "billet", fakeBillet)
	dir = hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "stop", dir); code != 0 {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "run", dir, "27"); code != 130 ||
		!strings.Contains(out, "does not start") {
		t.Fatalf("a run stopped before it started: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "billet.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("billet was started after a stop was requested (%v)", err)
	}
}

// DOWN IS IDEMPOTENT AND NEVER RUNS BESIDE A LIVE RUN: with no workspace there is
// nothing to take down; with the run's lock held past the wait it is said and nothing
// is taken down; with the lock free, as a reboot leaves it whatever files remain, its
// status is down's.
func TestTheMatrixHostDownWaitsForTheRunAndIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "python3")
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
	env := []string{"MATRIX_DOWN_WAIT=1", "MATRIX_POLL=0.1"}
	locked := t.TempDir()
	if err := os.Symlink(filepath.Join(f.bin, "billet"), filepath.Join(locked, "billet")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(locked, "ws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "ws", "acceptance.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	holdRunLock(t, locked)
	if out, code := f.run(t, "acceptance/matrix-host.sh", env, "down", locked); code != 1 || !strings.Contains(out, "still live") {
		t.Fatalf("down beside a live run: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(locked, "stop-requested")); err != nil {
		t.Fatalf("down did not ask the run to stop: %v", err)
	}
	if log, err := os.ReadFile(filepath.Join(f.dir, "billet.log")); err == nil && strings.Contains(string(log), "acceptance down") {
		t.Fatalf("down ran beside a live run:\n%s", log)
	}
	// STOPPED ANSWERS WHETHER A RUN LIVES, and takes nothing down either way: the
	// teardown's grant goes on the role only after it says none does.
	if out, code := f.run(t, "acceptance/matrix-host.sh", env, "stopped", locked); code != 1 || !strings.Contains(out, "still live") {
		t.Fatalf("stopped beside a live run: exit %d\n%s", code, out)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", env, "stopped", dir); code != 0 || !strings.Contains(out, "no run lives") {
		t.Fatalf("stopped with no live run: exit %d\n%s", code, out)
	}
	if log := billetLog(t, f); !strings.Contains(log, "acceptance stopped --workspace "+dir+"/ws\n") {
		t.Fatalf("stopped never asked the services' own locks:\n%s", log)
	}
	// A SERVICE THAT OUTLIVED THE RUN, a node draining past its stop's grace, is live.
	f.fixture(t, "acceptance-stopped.exit", "1")
	if out, code := f.run(t, "acceptance/matrix-host.sh", env, "stopped", dir); code != 1 || !strings.Contains(out, "may still live") {
		t.Fatalf("stopped beside a service that outlived the run: exit %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(f.dir, "acceptance-stopped.exit")); err != nil {
		t.Fatal(err)
	}
	if log, err := os.ReadFile(filepath.Join(f.dir, "billet.log")); err == nil && strings.Contains(string(log), "acceptance down") {
		t.Fatalf("stopped took the deployment down:\n%s", log)
	}
	// THE SERVICES' LOGS OUTLIVE THE WORKSPACE, on the host and not in what is uploaded.
	if err := os.WriteFile(filepath.Join(dir, "ws", "node.log"), []byte("the node's own account\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fixture(t, "acceptance-down.exit", "4")
	if out, code := f.run(t, "acceptance/matrix-host.sh", env, "down", dir); code != 4 {
		t.Fatalf("a down that failed: exit %d\n%s", code, out)
	}
	if kept, err := os.ReadFile(filepath.Join(dir, "logs", "node.log")); err != nil || string(kept) != "the node's own account\n" {
		t.Errorf("down did not keep the node's log in the run directory: %q, %v", kept, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "node.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the node's log is among what the workflow uploads: %v", err)
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
	*"--command-id cmd-1"*) ;;
	*) echo "asked about another command: $*" >&2; exit 1 ;;
	esac
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
			env := []string{"SSM_POLL=0", "SSM_GRACE=0"}
			out, code := f.run(t, "acceptance/ssm-run.sh", env, "run", "i-1", "1", "echo 'hi there'")
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

// SEND PRINTS THE COMMAND'S ID AND RETURNS; WAIT, GIVEN IT, ANSWERS AS RUN DOES, so a
// long command can be awaited in a later step.
func TestTheSSMHelperSendsInOneStepAndWaitsInAnother(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "aws", fakeSSM)
	f.fixture(t, "status", "Failed\n")
	f.fixture(t, "code", "7\n")
	env := []string{"SSM_POLL=0", "SSM_GRACE=0"}
	out, code := f.run(t, "acceptance/ssm-run.sh", env, "send", "i-1", "60", "true")
	if code != 0 || strings.TrimSpace(out) != "cmd-1" {
		t.Fatalf("send: exit %d, printed %q", code, out)
	}
	if out, code := f.run(t, "acceptance/ssm-run.sh", env, "wait", "i-1", "cmd-1", "60"); code != 7 {
		t.Fatalf("wait: exit %d, want the command's 7:\n%s", code, out)
	}
	if out, code := f.run(t, "acceptance/ssm-run.sh", env, "wait", "i-1"); code != 2 || !strings.Contains(out, "Usage") {
		t.Fatalf("a wait with no command id: exit %d\n%s", code, out)
	}
}

// fakeDispatchGH dispatches by appending "<workflow> <label> <tag>" to $FAKE/sent, and
// lists a workflow's runs as GitHub would, as JSON of each run's id (its line in
// $FAKE/sent) and title (the matrix's run-name), through the dispatcher's own --jq
// filter: the fake does not judge which run is the one asked for. A run is completed
// once $FAKE/done-<id> exists, or $FAKE/done-all does; $FAKE/unlisted makes every
// listing empty.
const fakeDispatchGH = `case "$1 $2" in
"workflow run")
	label="" tag=""
	for a in "$@"; do case "$a" in runs_on=*) label=${a#runs_on=} ;; tag=*) tag=${a#tag=} ;; esac; done
	echo "$3 $label $tag" >>"$FAKE/sent"
	;;
"run list")
	[ -f "$FAKE/unlisted" ] && exit 0
	wf="" filter="" prev=""
	for a in "$@"; do
		[ "$prev" = --workflow ] && wf=$a
		[ "$prev" = --jq ] && filter=$a
		prev=$a
	done
	n=0
	while read -r w label tag; do
		n=$((n + 1))
		[ "$w" = "$wf" ] || continue
		jq -cn --argjson id "$n" --arg t "${w%.yml} $label $tag" '{databaseId: $id, displayTitle: $t}'
	done <"$FAKE/sent" | jq -s . | jq -r "$filter"
	;;
"run view")
	if [ -f "$FAKE/done-all" ] || [ -f "$FAKE/done-$3" ]; then
		case "$*" in *conclusion*) echo success ;; *) echo completed ;; esac
	else
		echo in_progress
	fi
	;;
esac
`

func dispatchSent(t *testing.T, f matrixFakes) []string {
	t.Helper()
	sent, err := os.ReadFile(filepath.Join(f.dir, "sent"))
	if err != nil {
		return nil
	}

	return strings.Split(strings.TrimSpace(string(sent)), "\n")
}

var dispatchEnv = []string{"MATRIX_SETTLE=0", "MATRIX_POLL=0", "MATRIX_LIST_WAIT=1", "MATRIX_TIMEOUT=3"}

// THE COLD PHASE SENDS EVERY LABEL'S COLD AND FAILED-WRITER RUNS, never a two-writer one, records
// each run it started, and returns only once EVERY label's cold run has finished: with
// one architecture's still running, it fails rather than let the warm phase start.
func TestTheMatrixDispatchersColdPhaseWaitsForEveryArchitecture(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	runs := filepath.Join(t.TempDir(), "runs")
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "cold", "billet-testbed/m", "t1", runs,
		"accept-x64", "accept-arm")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want := []string{
		"matrix-cold.yml accept-x64 t1", "matrix-writer-fails.yml accept-x64 t1",
		"matrix-cold.yml accept-arm t1", "matrix-writer-fails.yml accept-arm t1",
	}
	if got := dispatchSent(t, f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "1\n2\n3\n4\n" {
		t.Fatalf("the runs file is %q, %v; want every run started, by id", got, err)
	}

	// The x64 cold run (id 1) finishes; the arm one (id 3) does not.
	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-1", "")
	runs = filepath.Join(t.TempDir(), "runs")
	out, code = f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "cold", "billet-testbed/m", "t2", runs,
		"accept-x64", "accept-arm")
	if code != 1 || !strings.Contains(out, "run 3 did not complete") {
		t.Fatalf("an arm cold run still running: exit %d\n%s", code, out)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "unlisted", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "cold", "billet-testbed/m", "t3",
		filepath.Join(t.TempDir(), "runs"), "accept-x64"); code != 1 || !strings.Contains(out, "lists no run of matrix-cold.yml") {
		t.Fatalf("a run GitHub never listed: exit %d\n%s", code, out)
	}
}

// THE WARM PHASE SENDS EACH LABEL'S WARM RUN WITH THE COLD RUNS' TAG, and waits for
// every run the runs file names, the cold phase's failed-writer runs included.
func TestTheMatrixDispatchersWarmPhaseWaitsForEveryRun(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	runs := filepath.Join(t.TempDir(), "runs")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "cold", "billet-testbed/m", "t1", runs,
		"accept-x64"); code != 0 {
		t.Fatalf("cold: exit %d\n%s", code, out)
	}
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "warm", "billet-testbed/m", "t1", runs, "accept-x64")
	if code != 0 || !strings.Contains(out, "every matrix run for tag t1 has completed") {
		t.Fatalf("warm: exit %d\n%s", code, out)
	}
	if got := dispatchSent(t, f); len(got) != 3 || got[2] != "matrix-warm.yml accept-x64 t1" {
		t.Fatalf("sent %q", got)
	}

	// A failed-writer run from the cold phase (id 2) still running fails the warm phase.
	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	runs = filepath.Join(t.TempDir(), "runs")
	f.fixture(t, "done-1", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "cold", "billet-testbed/m", "t2", runs,
		"accept-x64"); code != 0 {
		t.Fatalf("cold: exit %d\n%s", code, out)
	}
	f.fixture(t, "done-3", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "warm", "billet-testbed/m", "t2", runs,
		"accept-x64"); code != 1 || !strings.Contains(out, "run 2 did not complete") {
		t.Fatalf("a cold-phase run still running: exit %d\n%s", code, out)
	}
}

// THE TWO-WRITER RUNS GO ONE ARCHITECTURE AT A TIME: each label's run is sent only once
// the one before it has finished, so two architectures' writer a never hold every slot,
// and one still running sends no other and fails the phase.
func TestTheMatrixDispatchersWritersPhaseRunsOneArchitectureAtATime(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	runs := filepath.Join(t.TempDir(), "runs")
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "writers", "billet-testbed/m", "t1", runs,
		"accept-x64", "accept-arm")
	if code != 0 || !strings.Contains(out, "the two-writer runs are done") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want := []string{"matrix-writers.yml accept-x64 t1", "matrix-writers.yml accept-arm t1"}
	if got := dispatchSent(t, f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "1\n2\n" {
		t.Fatalf("the runs file is %q, %v", got, err)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	out, code = f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "writers", "billet-testbed/m", "t2",
		filepath.Join(t.TempDir(), "runs"), "accept-x64", "accept-arm")
	if code != 1 || !strings.Contains(out, "run 1 did not complete") {
		t.Fatalf("an x64 writers run still running: exit %d\n%s", code, out)
	}
	if got := dispatchSent(t, f); len(got) != 1 {
		t.Fatalf("the arm writers run was sent beside a running x64 one: %q", got)
	}
}

// THE IMAGE STORE'S PAIR RUNS ALONE: its cold run for every label, then, only once each
// has finished, its warm one, and nothing else is sent; a cold run still running sends no
// warm one.
func TestTheMatrixDispatchersImagePhaseRunsThePairAlone(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	runs := filepath.Join(t.TempDir(), "runs")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "images", "billet-testbed/m", "t1", runs,
		"accept-x64", "accept-arm"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want := []string{
		"matrix-images-cold.yml accept-x64 t1", "matrix-images-cold.yml accept-arm t1",
		"matrix-images-warm.yml accept-x64 t1", "matrix-images-warm.yml accept-arm t1",
	}
	if got := dispatchSent(t, f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "1\n2\n3\n4\n" {
		t.Fatalf("the runs file is %q, %v", got, err)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-1", "")
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "images", "billet-testbed/m", "t2",
		filepath.Join(t.TempDir(), "runs"), "accept-x64", "accept-arm")
	if code != 1 || !strings.Contains(out, "run 2 did not complete") {
		t.Fatalf("an arm cold run still running: exit %d\n%s", code, out)
	}
	for _, line := range dispatchSent(t, f) {
		if strings.HasPrefix(line, "matrix-images-warm") {
			t.Fatalf("a warm run was sent before every cold one finished: %q", dispatchSent(t, f))
		}
	}
}

// THE RUN ASKED FOR IS THE ONE WHOSE TITLE IS EXACTLY THIS ONE'S: a run of the same
// workflow with a title that only begins the same way is not it.
func TestTheMatrixDispatcherFindsTheRunByItsExactTitle(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	f.fixture(t, "sent", "matrix-images-cold.yml accept-x64 t10\nmatrix-images-cold.yml accept-x64-old t1\n")
	runs := filepath.Join(t.TempDir(), "runs")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "images", "billet-testbed/m", "t1", runs,
		"accept-x64"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "3\n4\n" {
		t.Fatalf("the runs file is %q, %v; want this run's own ids, 3 and 4", got, err)
	}
}

// A RUN THAT COULD NOT BE RECORDED FAILS THE DISPATCH, naming the run, because the judge
// would read its cells as never run.
func TestTheMatrixDispatcherFailsOnARunItCouldNotRecord(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	// A DIRECTORY WHERE THE FILE SHOULD BE, which refuses the append whoever runs it,
	// root included, where a read-only directory would not.
	runs := filepath.Join(t.TempDir(), "runs")
	if err := os.Mkdir(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "images", "billet-testbed/m", "t1",
		runs, "accept-x64")
	if code != 1 || !strings.Contains(out, "dispatched run 1 of matrix-images-cold.yml for accept-x64, and could not record it") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// THE RUN WRITES TO ITS LOG, NEVER TO THE SUPERVISOR'S OUTPUT: with every reader of the
// supervisor's output gone, as SSM leaves a command it timed out, the run goes on
// writing, ends as itself, and its exit is still recorded.
func TestTheMatrixRunOutlivesItsSupervisorsOutput(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "python3")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "run-late", "")
	// A run that fails, so the supervisor has its own line to say with nobody reading.
	f.fixture(t, "acceptance-run.exit", "5")
	dir := hostRunDir(t, f)
	abs, err := filepath.Abs("acceptance/matrix-host.sh")
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	run := exec.CommandContext(t.Context(), abs, "run", dir, "27")
	run.Env = []string{"PATH=" + f.path, "HOME=" + t.TempDir(), "FAKE=" + f.dir, "MATRIX_POLL=0.1"}
	run.Stdout, run.Stderr = w, w
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	// NOBODY READS: both ends close in this process, so the supervisor's next write
	// to its output fails as SSM's closed pipe would.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	err = run.Wait()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 5 {
		t.Fatalf("the supervisor with no reader answered %v, want the run's own 5", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "run.exit")); err != nil || string(b) != "5\n" {
		t.Fatalf("run.exit = %q, %v; want the run's own 5", b, err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "run.log"))
	for _, want := range []string{"the fake run is running", "the fake run, on stderr", "the fake run, still writing",
		"matrix: the run ended with status 5"} {
		if err != nil || !strings.Contains(string(log), want) {
			t.Fatalf("run.log = %q, %v; want %q in it", log, err, want)
		}
	}
}

// A SIGNAL TO THE SUPERVISOR'S WHOLE PROCESS GROUP, beside a stop request, reaches the
// run as ONE TERM: the run is in a session of its own, so the group's signal reaches
// only the supervisor, which sends it on once through the gate.
func TestAGroupSignalReachesTheRunOnce(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "python3", "ps")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "run-waits", "")
	dir := hostRunDir(t, f)
	abs, err := filepath.Abs("acceptance/matrix-host.sh")
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	run := exec.CommandContext(t.Context(), abs, "run", dir, "27")
	run.Env = []string{"PATH=" + f.path, "HOME=" + t.TempDir(), "FAKE=" + f.dir, "MATRIX_POLL=0.5"}
	run.Stderr = &stderr
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	for range 200 {
		if b, err := os.ReadFile(filepath.Join(f.dir, "ready")); err == nil && len(b) > 0 {
			ready = true

			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("the run never said it was ready")
	}
	if err := os.WriteFile(filepath.Join(dir, "stop-requested"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-run.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := run.Wait(); err != nil {
		t.Fatalf("the run: %v\n%s", err, stderr.String())
	}
	if n := strings.Count(stderr.String(), "stopping the run early"); n != 1 {
		t.Fatalf("the supervisor signalled %d times, want once:\n%s", n, stderr.String())
	}
	if b, err := os.ReadFile(filepath.Join(f.dir, "terms")); err != nil || string(b) != "term\n" {
		t.Fatalf("the run got %q TERMs (%v), want one: the group's signal reached it beside the supervisor's", b, err)
	}
	// AND IT WAS IN NO GROUP THE SIGNAL WENT TO, which is what keeps it to one: two
	// TERMs to one process can merge, and so cannot be counted at the receiver.
	b, err := os.ReadFile(filepath.Join(f.dir, "pgid"))
	switch pgid := strings.TrimSpace(string(b)); {
	case err != nil || pgid == "":
		t.Fatalf("the fake run could not say its process group (%q, %v)", b, err)
	case pgid == strconv.Itoa(run.Process.Pid):
		t.Fatalf("the run is in its supervisor's process group %s: the group's signal reached it", pgid)
	}
}
