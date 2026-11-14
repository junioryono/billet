package scripts_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
matrix-pool-consoles)
	ws=""
	while [ "$#" -gt 0 ]; do [ "$1" = --workspace ] && ws=$2; shift; done
	[ -f "$FAKE/consoles-fail" ] && exit 1
	[ -d "$ws" ] && echo "POOL-SENTINEL" >"$ws/pool-consoles.log"
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
matrix-pool-init-args)
	cat "$FAKE/init-args" 2>/dev/null
	;;
matrix-pool-host-iam)
	echo '{"Version":"2012-10-17","Statement":[{"Sid":"BilletCacheSelfAttach"}]}'
	;;
acceptance-down)
	# A teardown that succeeds removes the workspace, as the real one does.
	ws=""
	while [ "$#" -gt 0 ]; do [ "$1" = --workspace ] && ws=$2; shift; done
	if [ "$(cat "$FAKE/$key.exit" 2>/dev/null || echo 0)" = 0 ]; then rm -rf "$ws"; fi
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

// A POOL'S PRELUDE NAMES THIS RUN'S ARTIFACTS BY THEIR SHA256, ITS WIRE, THE POOL'S
// init_args AND THE HOST'S HALT, ABOVE THE TEMPLATE'S BODY WITHOUT ITS COMMENTS; one past
// what fits beside the join in EC2's user data is refused.
func TestTheMatrixHostRendersAPoolsPrelude(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "sha256sum")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "init-args", "--site=aws-us-west-2a\n--cache-listen=10.240.0.1:7718\n")
	dir := hostRunDir(t, f)
	for file, body := range map[string]string{
		"collection.tar.gz":   "a collection",
		"billet-arm64":        "arm64's billet",
		"prelude-template.sh": "#!/bin/bash\n# a comment\nset -Eeuo pipefail\n\t# an indented comment\n\t\techo prepared\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prelude", dir, "/pool.yaml", "10.77.0.10:7717",
		"runs-bucket", "us-west-2", "42-1-pool", "240")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "prelude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	billetBody, err := os.ReadFile(filepath.Join(f.bin, "billet"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export ARTIFACT_BUCKET=runs-bucket ARTIFACT_REGION=us-west-2\n",
		fmt.Sprintf("export BILLET_KEY=runs/42-1-pool/billet BILLET_SHA256=%x\n", sha256.Sum256(billetBody)),
		fmt.Sprintf("export BILLET_KEY_ARM64=runs/42-1-pool/billet-arm64 BILLET_SHA256_ARM64=%x\n",
			sha256.Sum256([]byte("arm64's billet"))),
		fmt.Sprintf("export COLLECTION_KEY=runs/42-1-pool/collection.tar.gz COLLECTION_SHA256=%x\n",
			sha256.Sum256([]byte("a collection"))),
		"export PRELUDE_JOIN_SERVER=10.77.0.10:7717\n",
		"export PRELUDE_INIT_ARGS=--site=aws-us-west-2a\\ --cache-listen=10.240.0.1:7718\n",
		"export PRELUDE_MAX_LIFETIME_MINUTES=240\n",
		"\nset -Eeuo pipefail\necho prepared\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the prelude has no %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "comment") || strings.Contains(string(got), "#!/bin/bash") {
		t.Errorf("the prelude kept the template's comments:\n%s", got)
	}
	if !strings.Contains(billetLog(t, f), "acceptance matrix pool-init-args --config /pool.yaml\n") {
		t.Errorf("billet was run as:\n%s", billetLog(t, f))
	}

	// No arm64 build beside the host's own: an arm64 host would be handed the wrong one.
	if err := os.Remove(filepath.Join(dir, "billet-arm64")); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prelude", dir, "/pool.yaml", "10.77.0.10:7717",
		"runs-bucket", "us-west-2", "42-1-pool", "240"); code == 0 {
		t.Errorf("a prelude rendered with no arm64 build: exit %d\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "billet-arm64"), []byte("arm64's billet"), 0o600); err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("echo 0123456789012345678901234567890123456789\n", 400)
	if err := os.WriteFile(filepath.Join(dir, "prelude-template.sh"), []byte("#!/bin/bash\n"+big), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prelude", dir, "/pool.yaml", "10.77.0.10:7717",
		"runs-bucket", "us-west-2", "42-1-pool", "240"); code == 0 || !strings.Contains(out, "past what fits") {
		t.Errorf("a prelude past EC2's user data: exit %d\n%s", code, out)
	}
}

// THE COLLECTION'S OWN PRELUDE FITS, rendered as the lane renders it with init_args and a
// bucket and run as long as the lane's: a prelude that grew past the limit would fail the
// pool lane's preparation, which only a dispatch from main runs.
func TestTheCollectionsPreludeFitsTheLane(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "sha256sum")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "init-args", "--site=aws-us-west-2a\n--ebs-s3-region=us-west-2\n"+
		"--ebs-s3-availability-zone=us-west-2a\n--ebs-s3-bucket=acceptance-cache-123456789012\n"+
		"--ebs-s3-prefix=billet-cache\n"+
		"--ebs-s3-kms-key-id=arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000\n"+
		"--cache-listen=10.240.0.1:7718\n--cache-guest-endpoint=http://10.240.0.1:7718\n")
	dir := hostRunDir(t, f)
	template, err := os.ReadFile("../ansible_collections/junioryono/billet/examples/pool-host-prelude/prelude.sh")
	if err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string][]byte{"collection.tar.gz": []byte("a collection"),
		"billet-arm64": []byte("arm64's billet"), "prelude-template.sh": template} {
		if err := os.WriteFile(filepath.Join(dir, file), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prelude", dir, "/pool.yaml", "10.77.0.10:7717",
		"acceptance-runs-123456789012", "us-west-2", "42888123208-1-pool", "390")
	if code != 0 {
		t.Fatalf("the collection's prelude does not fit the lane: exit %d\n%s", code, out)
	}
	t.Log(strings.TrimSpace(out))
	// WHAT THE RENDER REMOVED, the comments and the leading tabs, left a script bash reads.
	if out, err := exec.CommandContext(t.Context(), "bash", "-n", filepath.Join(dir, "prelude.sh")).CombinedOutput(); err != nil {
		t.Errorf("the rendered prelude does not parse: %v\n%s", err, out)
	}
}

// A POOL'S DEPLOYMENT IS STOOD UP ON ITS FLEET WIRE WITH THE RUN'S PRELUDE, AND BOTH ITS
// POLICIES ARE RENDERED FOR THE RUN'S DEPLOYMENT: the control plane's pool, passing the
// pool hosts' role, and the hosts'.
func TestTheMatrixHostPreparesAPoolsDeployment(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "tiers", "firecracker/aws-metal x64 accept-pool-metal\n")
	dir := hostRunDir(t, f)
	out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare-pool", dir, "/pool.yaml", "111122223333",
		"10.77.0.10:7717", "10.77.0.10:7718", "arn:aws:iam::111122223333:role/b1t22-accept-pool-host")
	if code != 0 || !strings.Contains(out, "pool's deployment dep-1 is up") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for file, want := range map[string]string{"iam.json": "2012-10-17", "pool-host.json": "BilletCacheSelfAttach",
		"teardown.json": "BilletMatrixDecommissionState"} {
		if got, err := os.ReadFile(filepath.Join(dir, file)); err != nil || !strings.Contains(string(got), want) {
			t.Errorf("%s = %q, %v", file, got, err)
		}
	}
	log := billetLog(t, f)
	for _, want := range []string{
		"acceptance up --config /pool.yaml --workspace " + dir + "/ws --account 111122223333 --label-prefix accept-pool " +
			"--fleet-listen 10.77.0.10:7717 --fleet-bootstrap 10.77.0.10:7718 --boot-prelude " + dir + "/prelude.sh\n",
		"init iam --pool --config " + dir + "/ws/billet.yaml --deployment dep-1 --role-arn " +
			"arn:aws:iam::111122223333:role/b1t22-accept-pool-host\n",
		"acceptance matrix pool-host-iam --workspace " + dir + "/ws\n",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("billet was not run as %q:\n%s", want, log)
		}
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "billet", fakeBillet)
	f.fixture(t, "tiers", "firecracker/aws-metal x64 accept-pool-metal\n")
	f.fixture(t, "matrix-pool-host-iam.exit", "1")
	dir = hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "prepare-pool", dir, "/pool.yaml", "1",
		"10.77.0.10:7717", "10.77.0.10:7718", "r"); code == 0 || !strings.Contains(out, "pool hosts' policy could not be rendered") {
		t.Errorf("a pool host policy that could not be rendered: exit %d\n%s", code, out)
	}
}

// A POOL'S RUN KEEPS ITS CONTROL PLANE UP FOR THE POOL'S OWN RELEASE, AND A POOL'S JUDGE
// OWES ITS BACKEND.
func TestTheMatrixHostRunsAndJudgesAPool(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t)
	f.command(t, "billet", fakeBillet)
	dir := hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", []string{"MATRIX_POLL=0.1"}, "run", dir, "27", "50m"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if log := billetLog(t, f); !strings.Contains(log, "acceptance run --workspace "+dir+"/ws --jobs 27 --wait 125m "+
		"--pool-released 50m --no-teardown\n") {
		t.Errorf("billet was run as:\n%s", log)
	}

	f = newMatrixFakes(t)
	f.command(t, "billet", fakeBillet)
	dir = hostRunDir(t, f)
	if err := os.MkdirAll(filepath.Join(dir, "ws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.exit"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "judge", dir, "x64", dir+"/runs", "billet-testbed/m",
		"firecracker/aws-metal"); code != 0 || !strings.Contains(billetLog(t, f), "--backends firecracker/aws-metal --out") {
		t.Errorf("a pool's judge: exit %d\n%s\n%s", code, out, billetLog(t, f))
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
			dir+"/runs --repository billet-testbed/m --backends ec2 --out "+dir+"/out/matrix.json") {
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
	f := newMatrixFakes(t, "python3", "timeout")
	f.command(t, "billet", fakeBillet)
	dir := hostRunDir(t, f)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code != 0 || !strings.Contains(out, "nothing to take down") {
		t.Fatalf("down with no workspace: exit %d\n%s", code, out)
	}
	// A WORKSPACE NOTHING RAN IN (no ledger) is nothing to take down; ONE WITH A LEDGER AND
	// NO RECORD is never read as that, by down or by stopped, since its run may have
	// made what nothing here can now name.
	if err := os.MkdirAll(filepath.Join(dir, "ws", "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code != 0 || !strings.Contains(out, "nothing to take down") {
		t.Fatalf("down of a workspace with no ledger and no record: exit %d\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "ws", "server", "billet.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"down", "stopped"} {
		if out, code := f.run(t, "acceptance/matrix-host.sh", nil, phase, dir); code == 0 ||
			!strings.Contains(out, "a person decides") {
			t.Fatalf("%s of a workspace with a ledger and no record: exit %d\n%s", phase, code, out)
		}
	}
	// A LINK IS NEVER READ AS ABSENCE: a server directory linked to where nothing resolves,
	// or a ledger name that is a dangling link, is a workspace whose run may have made
	// something.
	if err := os.RemoveAll(filepath.Join(dir, "ws", "server")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dir, "ws", "server")); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code == 0 ||
		!strings.Contains(out, "could not be read") {
		t.Fatalf("down of a workspace whose server is a dangling link: exit %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(dir, "ws", "server")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ws", "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dir, "ws", "server", "billet.db")); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code == 0 ||
		!strings.Contains(out, "a person decides") {
		t.Fatalf("down of a workspace whose ledger is a dangling link: exit %d\n%s", code, out)
	}
	if err := os.RemoveAll(filepath.Join(dir, "ws", "server")); err != nil {
		t.Fatal(err)
	}
	// A LOOKUP THAT FAILS IS NOT AN ABSENCE: a server directory that cannot be searched
	// hides whether a ledger is in it, which the shell's -e reads as none.
	if os.Geteuid() != 0 {
		server := filepath.Join(dir, "ws", "server")
		if err := os.MkdirAll(server, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(server, "billet.db"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(server, 0o000); err != nil {
			t.Fatal(err)
		}
		out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir)
		if err := os.Chmod(server, 0o755); err != nil {
			t.Fatal(err)
		}
		if code == 0 || !strings.Contains(out, "could not be read") {
			t.Fatalf("down of a workspace whose server directory cannot be searched: exit %d\n%s", code, out)
		}
		if err := os.RemoveAll(server); err != nil {
			t.Fatal(err)
		}
	}
	// A HELPER THAT FAILED SAYS NOTHING: a python3 that exits 1 for the lookup, as an
	// uncaught exception or a failed start would, is never read as an absent ledger, by
	// down or by stopped.
	if err := os.MkdirAll(filepath.Join(dir, "ws", "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ws", "server", "billet.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	broken := t.TempDir()
	if err := forkSafeWriteFile(filepath.Join(broken, "python3"), []byte("#!/usr/bin/env bash\n"+
		"case \"$*\" in *billet.db) exit 1 ;; esac\nexec "+python+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	brokenPath := []string{"PATH=" + broken + string(os.PathListSeparator) + f.path}
	for _, phase := range []string{"down", "stopped"} {
		if out, code := f.run(t, "acceptance/matrix-host.sh", brokenPath, phase, dir); code == 0 ||
			!strings.Contains(out, "could not be read") {
			t.Fatalf("%s with a lookup helper that failed: exit %d\n%s", phase, code, out)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, "ws", "server")); err != nil {
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
	f.fixture(t, "acceptance-down.exit", "4")
	if out, code := f.run(t, "acceptance/matrix-host.sh", env, "down", dir); code != 4 {
		t.Fatalf("a down that failed: exit %d\n%s", code, out)
	}
}

// THE SERVICES' LOGS OUTLIVE THE TEARDOWN, ON THE HOST AND NOWHERE ELSE: a successful
// down removes the workspace they were written in, so they are moved out of it first,
// into a private directory beside the run and never into out/, which is uploaded. A log
// that would go through a symlink is not kept, and the teardown runs and answers anyway.
func TestTheMatrixHostDownKeepsTheServicesLogsOnTheHost(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "python3", "timeout")
	f.command(t, "billet", fakeBillet)
	stage := func() string {
		t.Helper()
		dir := hostRunDir(t, f)
		for name, body := range map[string]string{
			"acceptance.json": "{}", "server.log": "SERVER-SENTINEL\n", "node.log": "NODE-SENTINEL\n",
		} {
			if err := os.MkdirAll(filepath.Join(dir, "ws"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "ws", name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "out", "run.log"), []byte("the run's own log\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		return dir
	}
	noSentinelIn := func(dir string) {
		t.Helper()
		err := filepath.WalkDir(filepath.Join(dir, "out"), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(body), "SENTINEL") {
				t.Errorf("%s, among what the workflow uploads, holds a service's log:\n%s", path, body)
			}

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	dir := stage()
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code != 0 {
		t.Fatalf("down: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ws")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fake teardown left the workspace, so nothing here proves a log outlived it: %v", err)
	}
	for name, want := range map[string]string{"server.log": "SERVER-SENTINEL\n", "node.log": "NODE-SENTINEL\n",
		"pool-consoles.log": "POOL-SENTINEL\n"} {
		kept, err := os.ReadFile(filepath.Join(dir, "logs", name))
		if err != nil || string(kept) != want {
			t.Errorf("down did not keep %s past the teardown: %q, %v", name, kept, err)
		}
	}
	// THE POOL'S CONSOLES ARE ASKED FOR BEFORE THE TEARDOWN, which ends the hosts they
	// are on and removes the workspace they are written in.
	log := billetLog(t, f)
	asked := strings.Index(log, "acceptance matrix pool-consoles --workspace "+dir+"/ws\n")
	down := strings.Index(log, "acceptance down --workspace "+dir+"/ws")
	if asked < 0 || down < 0 || asked > down {
		t.Errorf("down did not ask for the pool's consoles before the teardown:\n%s", log)
	}

	// CONSOLES THAT COULD NOT BE KEPT ARE SAID, AND THE TEARDOWN RUNS AND ANSWERS anyway.
	failing := stage()
	f.fixture(t, "consoles-fail", "")
	out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", failing)
	if code != 0 || !strings.Contains(out, "not every pool host's console was kept") {
		t.Errorf("down with consoles that could not be kept: exit %d\n%s", code, out)
	}
	if !strings.Contains(billetLog(t, f), "acceptance down --workspace "+failing+"/ws") {
		t.Errorf("the teardown did not run after the consoles failed:\n%s", billetLog(t, f))
	}
	if err := os.Remove(filepath.Join(f.dir, "consoles-fail")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "logs")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the kept logs' directory is %v, %v; want 0700", info.Mode().Perm(), err)
	}
	noSentinelIn(dir)

	// A LOGS DIRECTORY THIS TEARDOWN DID NOT CREATE keeps nothing, whether a symlink into
	// out/ or a directory anyone may read, and the teardown still runs and gives down its
	// status.
	for name, plant := range map[string]func(dir string) error{
		"a symlink into out/": func(dir string) error {
			return os.Symlink(filepath.Join(dir, "out"), filepath.Join(dir, "logs"))
		},
		"a directory anyone may read": func(dir string) error {
			return os.Mkdir(filepath.Join(dir, "logs"), 0o755)
		},
		// Private and the caller's, so only the rule that the teardown creates it refuses.
		"a private directory already there": func(dir string) error {
			return os.Mkdir(filepath.Join(dir, "logs"), 0o700)
		},
	} {
		dir = stage()
		if err := plant(dir); err != nil {
			t.Fatal(err)
		}
		if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code != 0 || !strings.Contains(out, "no logs kept") {
			t.Fatalf("down with logs as %s: exit %d\n%s", name, code, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "ws")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("logs as %s stopped the teardown: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "logs", "node.log")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("logs as %s took the node's log: %v", name, err)
		}
		noSentinelIn(dir)
	}

	// A LOG THAT IS NOT A PLAIN FILE is not kept, and the other one still is.
	dir = stage()
	if err := os.Remove(filepath.Join(dir, "ws", "server.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "out", "run.log"), filepath.Join(dir, "ws", "server.log")); err != nil {
		t.Fatal(err)
	}
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "down", dir); code != 0 || !strings.Contains(out, "server.log is not kept") {
		t.Fatalf("down with a symlinked server log: exit %d\n%s", code, out)
	}
	if kept, err := os.ReadFile(filepath.Join(dir, "logs", "node.log")); err != nil || string(kept) != "NODE-SENTINEL\n" {
		t.Errorf("the node's log was not kept beside a server log that was not: %q, %v", kept, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "logs", "server.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a symlinked server log was kept: %v", err)
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

// The block on S3's fakes each append their call to $FAKE/calls. curl serves
// $FAKE/ip-ranges.json for AWS's ranges ($FAKE/fetch.exit fails it), and answers a request
// to S3 with $FAKE/reach-blocked (default 28, no answer in time) while a table is installed
// and $FAKE/reach-open (default 0) while none is; nft installs a table by copying the file
// it is given to $FAKE/table ($FAKE/nft-f.exit refuses it), deletes it ($FAKE/stuck keeps
// it) and lists it; systemd-run exits $FAKE/systemd-run.exit.
const (
	fakeS3Curl = `echo "curl $*" >>"$FAKE/calls"
out="" prev=""
for a in "$@"; do [ "$prev" = -o ] && out=$a; prev=$a; done
case "$*" in
*ip-ranges*)
	[ -f "$FAKE/fetch.exit" ] && exit "$(cat "$FAKE/fetch.exit")"
	cp "$FAKE/ip-ranges.json" "$out"
	;;
*)
	if [ -f "$FAKE/table" ]; then exit "$(cat "$FAKE/reach-blocked" 2>/dev/null || echo 28)"; fi
	exit "$(cat "$FAKE/reach-open" 2>/dev/null || echo 0)"
	;;
esac
`
	fakeNft = `echo "nft $*" >>"$FAKE/calls"
case "$1 $2" in
"-f "*)
	[ -f "$FAKE/nft-f.exit" ] && exit "$(cat "$FAKE/nft-f.exit")"
	cp "$2" "$FAKE/table"
	;;
"delete table")
	[ -f "$FAKE/stuck" ] && exit 1
	[ -f "$FAKE/table" ] || exit 1
	rm "$FAKE/table"
	;;
"list tables")
	[ -f "$FAKE/table" ] && echo "table inet billet_matrix_s3"
	echo "table ip filter"
	;;
esac
`
	fakeSystemdRun = `echo "systemd-run $*" >>"$FAKE/calls"
exit "$(cat "$FAKE/systemd-run.exit" 2>/dev/null || echo 0)"
`
	fakeS3Systemctl = `echo "systemctl $*" >>"$FAKE/calls"
exit 5
`
	// Two of us-west-2's S3 ranges and one of its IPv6 ones, beside ranges of another
	// service, another region, and the same range under the umbrella service.
	ipRangesFixture = `{"prefixes": [
 {"ip_prefix": "52.92.128.0/17", "region": "us-west-2", "service": "S3"},
 {"ip_prefix": "3.5.76.0/22", "region": "us-west-2", "service": "S3"},
 {"ip_prefix": "52.92.128.0/17", "region": "us-west-2", "service": "AMAZON"},
 {"ip_prefix": "52.219.0.0/20", "region": "us-east-1", "service": "S3"},
 {"ip_prefix": "35.80.0.0/12", "region": "us-west-2", "service": "EC2"}],
 "ipv6_prefixes": [
 {"ipv6_prefix": "2600:1fa0:4000::/39", "region": "us-west-2", "service": "S3"},
 {"ipv6_prefix": "2600:1ffa:5000::/39", "region": "us-east-1", "service": "S3"}]}`
)

func s3BlockFakes(t *testing.T) (matrixFakes, string) {
	t.Helper()
	f := newMatrixFakes(t, "python3")
	f.command(t, "curl", fakeS3Curl)
	f.command(t, "nft", fakeNft)
	f.command(t, "systemd-run", fakeSystemdRun)
	f.command(t, "systemctl", fakeS3Systemctl)
	f.fixture(t, "ip-ranges.json", ipRangesFixture)

	return f, t.TempDir()
}

func s3BlockCalls(t *testing.T, f matrixFakes) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// callIndex is the first call that starts with prefix, or -1.
func callIndex(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// THE NODE IS CUT OFF FROM ITS REGION'S S3 AND NOTHING ELSE, by a table whose expiry is
// armed before it exists, and only once a request through it goes unanswered; and S3 is
// given back only once it answers again. Every refusal leaves no table behind.
func TestTheMatrixHostCutsTheNodeOffFromS3AndGivesItBack(t *testing.T) {
	t.Parallel()
	f, dir := s3BlockFakes(t)
	out, code := f.run(t, "acceptance/matrix-host.sh", nil, "block-s3", dir, "us-west-2", "3300")
	if code != 0 || !strings.Contains(out, "out of this host's reach, for at most 3300s") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	table, err := os.ReadFile(filepath.Join(f.dir, "table"))
	if err != nil {
		t.Fatalf("no table installed: %v", err)
	}
	for _, want := range []string{"table inet billet_matrix_s3", "3.5.76.0/22", "52.92.128.0/17",
		"2600:1fa0:4000::/39", "hook output", "ip daddr @v4 drop", "ip6 daddr @v6 drop"} {
		if !strings.Contains(string(table), want) {
			t.Errorf("the table does not say %q:\n%s", want, table)
		}
	}
	for _, never := range []string{"52.219.0.0/20", "35.80.0.0/12", "2600:1ffa:5000::/39"} {
		if strings.Contains(string(table), never) {
			t.Errorf("the table blocks %s, which is not this region's S3:\n%s", never, table)
		}
	}
	calls := s3BlockCalls(t, f)
	armed, installed := callIndex(calls, "systemd-run "), callIndex(calls, "nft -f ")
	switch {
	case armed < 0 || installed < 0 || armed > installed:
		t.Errorf("the expiry was not armed before the table was installed: %q", calls)
	case !strings.Contains(calls[armed], "--on-active=3300s") ||
		!strings.HasSuffix(calls[armed], "nft delete table inet billet_matrix_s3"):
		t.Errorf("the expiry does not remove the table at 3300 s: %q", calls[armed])
	case callIndex(calls[installed:], "curl -s -o /dev/null --max-time 8 https://s3.us-west-2.amazonaws.com/") < 0:
		t.Errorf("the block was not proved by a request to S3 through it: %q", calls)
	}

	for range 2 {
		out, code = f.run(t, "acceptance/matrix-host.sh", nil, "unblock-s3", dir, "us-west-2")
		if code != 0 || !strings.Contains(out, "answers this host again") {
			t.Fatalf("unblock: exit %d:\n%s", code, out)
		}
		if _, err := os.Stat(filepath.Join(f.dir, "table")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the table is still installed: %v", err)
		}
	}

	for name, c := range map[string]struct {
		region, expiry string
		fixtures       []string
		code           int
		says           string
		armed          bool
	}{
		"a region with no S3 range": {region: "eu-north-1", expiry: "3300", code: 1, says: "no S3 range for eu-north-1"},
		"ranges that cannot be fetched": {region: "us-west-2", expiry: "3300", fixtures: []string{"fetch.exit=22"},
			code: 1, says: "could not be fetched"},
		"a block that does not hold": {region: "us-west-2", expiry: "3300", fixtures: []string{"reach-blocked=0"},
			code: 1, says: "did not go unanswered through the block (curl exit 0)", armed: true},
		"S3 that cannot be asked": {region: "us-west-2", expiry: "3300", fixtures: []string{"reach-blocked=6"},
			code: 1, says: "(curl exit 6)", armed: true},
		"an expiry that cannot be armed": {region: "us-west-2", expiry: "3300",
			fixtures: []string{"systemd-run.exit=1"}, code: 1, says: "expiry could not be armed", armed: true},
		"rules nftables refuses": {region: "us-west-2", expiry: "3300", fixtures: []string{"nft-f.exit=1"},
			code: 1, says: "could not be installed", armed: true},
		"an expiry that is not seconds": {region: "us-west-2", expiry: "55m", code: 2, says: "not a number of seconds"},
	} {
		f, dir := s3BlockFakes(t)
		for _, fx := range c.fixtures {
			name, value, _ := strings.Cut(fx, "=")
			f.fixture(t, name, value)
		}
		out, code := f.run(t, "acceptance/matrix-host.sh", nil, "block-s3", dir, c.region, c.expiry)
		if code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: exit %d, want %d saying %q:\n%s", name, code, c.code, c.says, out)
		}
		if _, err := os.Stat(filepath.Join(f.dir, "table")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a table is left installed: %v", name, err)
		}
		if armed := callIndex(s3BlockCalls(t, f), "systemd-run ") >= 0; armed != c.armed {
			t.Errorf("%s: the expiry was asked for: %v, want %v", name, armed, c.armed)
		}
	}

	// A BLOCK THAT WILL NOT COME OFF, and S3 still silent once it has, are both said.
	f, dir = s3BlockFakes(t)
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "block-s3", dir, "us-west-2", "60"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	f.fixture(t, "stuck", "")
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "unblock-s3", dir, "us-west-2"); code != 1 ||
		!strings.Contains(out, "still installed") {
		t.Errorf("a stuck block: exit %d:\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(f.dir, "stuck")); err != nil {
		t.Fatal(err)
	}
	f.fixture(t, "reach-open", "28")
	if out, code := f.run(t, "acceptance/matrix-host.sh", nil, "unblock-s3", dir, "us-west-2"); code != 1 ||
		!strings.Contains(out, "did not answer this host (curl exit 28)") {
		t.Errorf("S3 still silent: exit %d:\n%s", code, out)
	}
}

// fakeStatusBillet answers `billet status` with $FAKE/status.<n> on its nth call, or
// $FAKE/status.last past those, and exits $FAKE/status.exit.
const fakeStatusBillet = `[ "$1" = status ] || exit 9
n=$(( $(cat "$FAKE/status.n" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$FAKE/status.n"
file="$FAKE/status.$n"
[ -f "$file" ] || file="$FAKE/status.last"
cat "$file"
exit "$(cat "$FAKE/status.exit" 2>/dev/null || echo 0)"
`

// THE BREAKER IS WAITED FOR UNTIL NONE IS OPEN, each read kept, and a breaker still open
// at the deadline, or a status that could not be read, is never read as one that closed.
func TestTheMatrixHostWaitsForTheStoreBreakerToLetJobsIn(t *testing.T) {
	t.Parallel()
	const open = "cache     cache-us: ebs-s3 breaker OPEN for 20s (3 timeout failures); its new jobs start cold\n"
	const probation = "cache     cache-us: ebs-s3 breaker on probation for 7s; it answered a probe\n"
	for name, c := range map[string]struct {
		seconds  string
		fixtures map[string]string
		code     int
		says     string
		reads    int
	}{
		"open, then on probation": {seconds: "60", fixtures: map[string]string{"status.1": "admission open\n" + open,
			"status.last": "admission open\n" + probation}, says: "on probation for 7s", reads: 2},
		"no breaker at all": {seconds: "60", fixtures: map[string]string{"status.last": "admission open\n"},
			says: "no store breaker is open", reads: 1},
		"open at the deadline": {seconds: "0", fixtures: map[string]string{"status.last": open}, code: 1,
			says: "still open", reads: 1},
		"a status that fails": {seconds: "60", fixtures: map[string]string{"status.last": probation, "status.exit": "1"},
			code: 2, says: "could not be read", reads: 1},
		"seconds that are not": {seconds: "5m", fixtures: map[string]string{"status.last": probation}, code: 2,
			says: "not a number of seconds"},
	} {
		f := newMatrixFakes(t)
		f.command(t, "billet", fakeStatusBillet)
		for name, body := range c.fixtures {
			f.fixture(t, name, body)
		}
		dir := hostRunDir(t, f)
		out, code := f.run(t, "acceptance/matrix-host.sh", []string{"MATRIX_POLL=0"}, "breaker", dir, c.seconds)
		if code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: exit %d, want %d saying %q:\n%s", name, code, c.code, c.says, out)
		}
		if c.reads == 0 {
			continue
		}
		kept, err := filepath.Glob(filepath.Join(dir, "logs", "status-*.txt"))
		if err != nil || len(kept) == 0 {
			t.Errorf("%s: no read was kept (%v)", name, err)
		}
		n, err := os.ReadFile(filepath.Join(f.dir, "status.n"))
		if err != nil || strings.TrimSpace(string(n)) != strconv.Itoa(c.reads) {
			t.Errorf("%s: status was read %q times (%v), want %d", name, n, err, c.reads)
		}
	}
}

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

// THE STORE-UNREACHABLE PHASE SENDS EVERY LABEL'S RUN AT ONCE, records each, and waits
// for every one: a run still going fails the phase, whichever architecture it is.
func TestTheMatrixDispatchersStoreUnreachablePhaseWaitsForEveryArchitecture(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	runs := filepath.Join(t.TempDir(), "runs")
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "store-unreachable", "billet-testbed/m", "t1",
		runs, "accept-x64", "accept-arm")
	if code != 0 || !strings.Contains(out, "the store-unreachable runs are done") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want := []string{"matrix-store-unreachable.yml accept-x64 t1", "matrix-store-unreachable.yml accept-arm t1"}
	if got := dispatchSent(t, f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "1\n2\n" {
		t.Fatalf("the runs file is %q, %v", got, err)
	}

	for unfinished, done := range map[string]string{"1": "done-2", "2": "done-1"} {
		f := newMatrixFakes(t, "jq")
		f.command(t, "gh", fakeDispatchGH)
		f.fixture(t, done, "")
		out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "store-unreachable", "billet-testbed/m",
			"t2", filepath.Join(t.TempDir(), "runs"), "accept-x64", "accept-arm")
		if code != 1 || !strings.Contains(out, "run "+unfinished+" did not complete") {
			t.Errorf("run %s still running: exit %d\n%s", unfinished, code, out)
		}
		if got := dispatchSent(t, f); len(got) != 2 {
			t.Errorf("run %s still running: sent %q; want both at once", unfinished, got)
		}
	}
}

// A POOL'S ARCHITECTURES SHARE NO HOST, so their two-writer runs are sent together and
// the phase waits for all of them: the arm one is sent while the x64 one still runs, and
// that one unfinished fails the phase.
func TestTheMatrixDispatchersWritersApartPhaseSendsEveryArchitectureAtOnce(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-2", "")
	runs := filepath.Join(t.TempDir(), "runs")
	out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "writers-apart", "billet-testbed/m", "t1", runs,
		"accept-x64", "accept-arm")
	if code != 1 || !strings.Contains(out, "run 1 did not complete") {
		t.Fatalf("an x64 writers run still running: exit %d\n%s", code, out)
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
	f.fixture(t, "done-1", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "writers-apart", "billet-testbed/m", "t2",
		filepath.Join(t.TempDir(), "runs"), "accept-x64", "accept-arm"); code != 1 || !strings.Contains(out, "run 2 did not complete") {
		t.Fatalf("an arm writers run still running: exit %d\n%s", code, out)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	f.fixture(t, "done-all", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "writers-apart", "billet-testbed/m", "t3",
		filepath.Join(t.TempDir(), "runs"), "accept-x64", "accept-arm"); code != 0 || !strings.Contains(out, "the two-writer runs are done") {
		t.Fatalf("both finished: exit %d\n%s", code, out)
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

// A POOL'S IMAGE PAIR IS THREE STEPS: the cold runs are sent and nothing is waited for,
// the wait is a step of its own over every run sent, and only then are the warm runs sent
// and waited for; a cold run still running at the wait's deadline fails it, sending no
// warm one.
func TestTheMatrixDispatchersPoolImageStepsWaitForTheHost(t *testing.T) {
	t.Parallel()
	f := newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	runs := filepath.Join(t.TempDir(), "runs")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "images-cold", "billet-testbed/m", "t1",
		runs, "accept-pool"); code != 0 || !strings.Contains(out, "cold runs are sent") {
		t.Fatalf("images-cold, with nothing finished: exit %d\n%s", code, out)
	}
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "await", "billet-testbed/m", "t1",
		runs, "-"); code != 1 || !strings.Contains(out, "run 1 did not complete") {
		t.Fatalf("await, with the cold run still running: exit %d\n%s", code, out)
	}
	if got := dispatchSent(t, f); len(got) != 1 {
		t.Fatalf("sent %q, want the cold run alone", got)
	}

	f.fixture(t, "done-all", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "await", "billet-testbed/m", "t1",
		runs, "-"); code != 0 {
		t.Fatalf("await, with the cold run done: exit %d\n%s", code, out)
	}
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "images-warm", "billet-testbed/m", "t1",
		runs, "accept-pool"); code != 0 || !strings.Contains(out, "image store's runs are done") {
		t.Fatalf("images-warm: exit %d\n%s", code, out)
	}
	want := []string{"matrix-images-cold.yml accept-pool t1", "matrix-images-warm.yml accept-pool t1"}
	if got := dispatchSent(t, f); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "1\n2\n" {
		t.Fatalf("the runs file is %q, %v", got, err)
	}

	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "await", "billet-testbed/m", "t1",
		filepath.Join(t.TempDir(), "runs"), "-"); code != 1 || !strings.Contains(out, "names no run") {
		t.Fatalf("await with no run sent: exit %d\n%s", code, out)
	}

	// EVERY RUN SENT, not the first or the last: two tiers' cold runs, the first done.
	f = newMatrixFakes(t, "jq")
	f.command(t, "gh", fakeDispatchGH)
	runs = filepath.Join(t.TempDir(), "runs")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "images-cold", "billet-testbed/m", "t1",
		runs, "accept-pool", "accept-pool-2"); code != 0 {
		t.Fatalf("images-cold on two tiers: exit %d\n%s", code, out)
	}
	f.fixture(t, "done-1", "")
	if out, code := f.run(t, "acceptance/matrix-dispatch.sh", dispatchEnv, "await", "billet-testbed/m", "t1",
		runs, "-"); code != 1 || !strings.Contains(out, "run 2 did not complete") {
		t.Fatalf("await, with the second cold run still running: exit %d\n%s", code, out)
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
