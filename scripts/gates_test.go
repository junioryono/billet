package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// THE GATE SCRIPTS ARE EXECUTED IN A TREE OF THEIR OWN. Each test copies the
// scripts it runs, the lock script and the pinned versions into a temporary
// checkout and puts stand-ins where the scripts would find the real work: the
// steps of the full gate, the verify, `go`, `nice`. A script that stopped
// reading an override could then reach only a stand-in, never the real gate,
// which run from inside `go test` would run this suite inside itself.

// gateFixture is a checkout holding copies of the named gate scripts, the lock
// script and .github/tool-versions.env (unless pins is false), plus the given
// executables, each at its path under the checkout.
func gateFixture(t *testing.T, scripts []string, pins bool, executables map[string]string) string {
	t.Helper()

	root := t.TempDir()

	copyInto := func(from, to string) {
		body, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}

		writeExecutable(t, to, string(body))
	}

	for _, script := range append([]string{"env.sh"}, scripts...) {
		copyInto(filepath.Join("gates", script), filepath.Join(root, "scripts", "gates", script))
	}

	copyInto("with-check-lock.sh", filepath.Join(root, "scripts", "with-check-lock.sh"))

	if pins {
		copyInto(filepath.Join("..", ".github", "tool-versions.env"), filepath.Join(root, ".github", "tool-versions.env"))
	}

	for rel, body := range executables {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		writeExecutable(t, path, body)
	}

	return root
}

// gateEnv is the test's environment with the gates' own variables removed, so
// the script's defaults are what a case does not set, plus extra, with bin first
// on PATH when it is not empty.
func gateEnv(bin string, extra ...string) []string {
	var env []string

	for _, kv := range scriptEnv() {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case "NICE", "TEST_PARALLEL", "GO", "GOEXPERIMENT", "CI", "CHECK_LOCK", "CHECK_STEPS", "VERIFY_CHANGED", "COVERPROFILE":
			continue
		case "PATH":
			if bin != "" {
				kv = "PATH=" + bin + string(os.PathListSeparator) + value
			}
		}

		if !setIn(extra, name) {
			env = append(env, kv)
		}
	}

	return append(env, extra...)
}

// niceStandIn is a `nice` that insists on the gates' `-n 10`, marks the
// environment of what it runs, and runs it.
const niceStandIn = `#!/bin/sh
[ "$1" = -n ] && [ "$2" = 10 ] || { echo "nice was called as: $*" >&2; exit 97; }
shift 2
BILLET_NICED=yes exec "$@"
`

// lockHeld reports whether another process holds path's lock now, asked the way
// with-check-lock.sh's own probe asks it: a take that may not wait, whose status
// 75 means held.
func lockHeld(t *testing.T, path string) bool {
	t.Helper()

	var cmd *exec.Cmd

	switch {
	case lookPath("lockf"):
		cmd = exec.CommandContext(t.Context(), "lockf", "-k", "-t", "0", path, "true")
	case lookPath("flock"):
		cmd = exec.CommandContext(t.Context(), "flock", "-n", "-E", "75", path, "true")
	default:
		t.Fatal("no lock tool to probe with")
	}

	err := cmd.Run()
	if err == nil {
		return false
	}

	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 75 {
		return true
	}

	t.Fatalf("probe %s: %v", path, err)

	return false
}

// THE FULL GATE RUNS ITS STEPS UNDER THE SHARED LOCK, handing the lock script the
// path whole: its default steps (check-steps.sh beside it) run, and the lock is
// taken under an XDG_CACHE_HOME holding spaces.
func TestTheFullGateTakesTheSharedLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	root := gateFixture(t, []string{"check.sh"}, true, map[string]string{
		"scripts/gates/check-steps.sh": "#!/bin/sh\necho the steps ran\n",
	})
	xdg := filepath.Join(t.TempDir(), "a cache")

	cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", "gates", "check.sh"))
	cmd.Env = gateEnv("", "XDG_CACHE_HOME="+xdg)
	inGroup(cmd)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gates/check.sh: %v (%s)", err, out)
	}

	if !strings.Contains(string(out), "the steps ran") {
		t.Errorf("the gate did not run its steps: %q", out)
	}

	if _, err := os.Stat(filepath.Join(xdg, "dev-gate.lock")); err != nil {
		t.Errorf("gates/check.sh did not take the shared lock under an XDG_CACHE_HOME with spaces: %v (%s)", err, out)
	}
}

// THE GATES HOLD THE LOCK FOR AS LONG AS THEIR WORK RUNS, not around a moment of
// it: while the full gate's steps or the verify are still running, the lock is
// held, and once they end it is free. The work holds until the test releases it,
// so the probe cannot land after it by chance.
func TestTheGatesHoldTheLockWhileTheyRun(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	for gate, work := range map[string]string{
		"check.sh":  "scripts/gates/check-steps.sh",
		"verify.sh": "scripts/verify-changed.sh",
	} {
		root := gateFixture(t, []string{gate}, true, map[string]string{work: "#!/bin/sh\n" + holdUntilReleased})
		dir := t.TempDir()
		lock := filepath.Join(dir, "gate.lock")
		held := filepath.Join(dir, "held")
		release := filepath.Join(dir, "release")

		cmd := exec.CommandContext(holderContext(t), filepath.Join(root, "scripts", "gates", gate), held, release)
		cmd.Env = gateEnv("", "CHECK_LOCK="+lock, "CI=true")
		inGroup(cmd)

		if err := cmd.Start(); err != nil {
			t.Fatalf("%s: start: %v", gate, err)
		}

		releaseAndReap(t, cmd, release)
		waitForLockFile(t, held, gate+": the work under the lock never started")

		if !lockHeld(t, lock) {
			t.Errorf("%s: the lock was free while the work it guards ran", gate)
		}

		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := cmd.Wait(); err != nil {
			t.Fatalf("%s: %v", gate, err)
		}

		if lockHeld(t, lock) {
			t.Errorf("%s: the lock was still held after the gate ended", gate)
		}
	}
}

// THE VERIFY RUNS UNDER `nice -n 10` LOCALLY BY DEFAULT, and under nothing when
// CI runs it or NICE is set empty. Its default verify-changed.sh is a stand-in
// that says whether it was niced, and `nice` is a stand-in first on PATH.
func TestTheVerifyRunsUnderNiceLocallyAndNotUnderCI(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"local, by default": {want: "verified niced=yes"},
		"under CI":          {env: []string{"CI=true"}, want: "verified niced="},
		"local, lifted":     {env: []string{"NICE="}, want: "verified niced="},
	} {
		root := gateFixture(t, []string{"verify.sh"}, true, map[string]string{
			"scripts/verify-changed.sh": "#!/bin/sh\necho \"verified niced=${BILLET_NICED-}\"\n",
			"bin/nice":                  niceStandIn,
		})

		cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", "gates", "verify.sh"))
		cmd.Env = gateEnv(filepath.Join(root, "bin"), append(tc.env, "CHECK_LOCK="+filepath.Join(root, "v.lock"))...)
		inGroup(cmd)

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: gates/verify.sh: %v (%s)", name, err, out)
		}

		if !strings.Contains(string(out), tc.want+"\n") {
			t.Errorf("%s: the verify ran as %q, want %q", name, out, tc.want)
		}
	}
}

// THE RACE SUITE TAKES THE LOCAL CAPS BY DEFAULT, AND CI'S RUN TAKES NONE: at
// most TEST_PARALLEL packages at once (4 unless set) under `nice -n 10` locally,
// go test's own -p and no nice under CI or with both set empty, over the whole
// tree with atomic coverage into the named profile. `go` and `nice` are
// stand-ins first on PATH that print the line they were given.
func TestTheRaceSuiteTakesTheLocalCapsAndCIsNone(t *testing.T) {
	t.Parallel()

	const suite = "-race -count=1 -timeout 35m -covermode=atomic -coverprofile=c.out ./..."

	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"local, by default":    {want: "niced=yes test -p 4 " + suite},
		"local, three at once": {env: []string{"TEST_PARALLEL=3"}, want: "niced=yes test -p 3 " + suite},
		"under CI":             {env: []string{"CI=true", "TEST_PARALLEL=3"}, want: "niced= test " + suite},
		"local, both lifted":   {env: []string{"NICE=", "TEST_PARALLEL="}, want: "niced= test " + suite},
	} {
		root := gateFixture(t, []string{"test.sh"}, true, map[string]string{
			"bin/go":   "#!/bin/sh\necho \"niced=${BILLET_NICED-} $*\"\n",
			"bin/nice": niceStandIn,
		})

		cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", "gates", "test.sh"))
		cmd.Env = gateEnv(filepath.Join(root, "bin"), append(tc.env, "COVERPROFILE=c.out")...)

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: gates/test.sh: %v (%s)", name, err, out)
		}

		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("%s: the suite would run as %q, want %q", name, got, tc.want)
		}
	}
}

// THE TOOLS ARE INSTALLED AT THE PINS, each read from .github/tool-versions.env,
// and trivy under GOEXPERIMENT=jsonv2, which Go 1.26 needs to build it.
func TestToolsInstallsEveryToolAtItsPin(t *testing.T) {
	t.Parallel()

	v := toolVersions(t)
	root := gateFixture(t, []string{"tools.sh"}, true, map[string]string{
		"bin/go": "#!/bin/sh\necho \"GOEXPERIMENT=${GOEXPERIMENT-} $*\"\n",
	})

	cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", "gates", "tools.sh"))
	cmd.Env = gateEnv(filepath.Join(root, "bin"))

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gates/tools.sh: %v (%s)", err, out)
	}

	for _, want := range []string{
		"GOEXPERIMENT= install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@" + v["GOLANGCI_VERSION"] + "\n",
		"GOEXPERIMENT= install github.com/goreleaser/goreleaser/v2@" + v["GORELEASER_VERSION"] + "\n",
		"GOEXPERIMENT= install github.com/sqlc-dev/sqlc/cmd/sqlc@" + v["SQLC_VERSION"] + "\n",
		"GOEXPERIMENT= install github.com/terraform-linters/tflint@" + v["TFLINT_VERSION"] + "\n",
		"GOEXPERIMENT=jsonv2 install github.com/aquasecurity/trivy/cmd/trivy@" + v["TRIVY_VERSION"] + "\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("gates/tools.sh did not run %q:\n%s", want, out)
		}
	}
}

// A CHECKOUT WITH NO PIN FILE IS REFUSED, not run with whatever tools are on PATH.
func TestAGateRefusesACheckoutWithNoPinFile(t *testing.T) {
	t.Parallel()

	root := gateFixture(t, []string{"test.sh"}, false, map[string]string{
		"bin/go": "#!/bin/sh\necho the suite ran\n",
	})

	cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", "gates", "test.sh"))
	cmd.Env = gateEnv(filepath.Join(root, "bin"))

	out, err := cmd.CombinedOutput()

	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), ".github/tool-versions.env is missing") ||
		strings.Contains(string(out), "the suite ran") {
		t.Errorf("a gate in a checkout with no pin file = %v, %q; want exit 2 naming the file, and nothing run", err, out)
	}
}

// THE FULL GATE'S STEPS ARE THE GATE'S, IN ITS ORDER: no-mutants first, because
// an interrupted mutation run leaves a mutant every later step would pass, and
// the rest as CI runs them one by one. CI never runs check-steps.sh, so this is
// what holds its list.
func TestTheFullGateRunsItsStepsInOrder(t *testing.T) {
	t.Parallel()

	var steps []string

	for line := range strings.SplitSeq(readFile(t, filepath.Join("gates", "check-steps.sh")), "\n") {
		if rest, ok := strings.CutPrefix(line, "step "); ok {
			steps = append(steps, strings.Fields(rest)[0])
		}
	}

	want := []string{"no-mutants", "build", "vet", "fmt-check", "lint", "lint-custom", "test", "lambda-test", "module-sources"}
	if !slices.Equal(steps, want) {
		t.Errorf("check-steps.sh runs %v, want %v", steps, want)
	}
}

// THE LINTER RUNS FOR LINUX AS WELL AS THE HOST, because a linter sees only the
// files it would compile; CI's lint job runs the action rather than lint.sh, so
// this is what holds the second pass.
func TestTheLintGateLintsForLinuxToo(t *testing.T) {
	t.Parallel()

	lint := readFile(t, filepath.Join("gates", "lint.sh"))
	if !regexp.MustCompile(`(?m)^GOOS=linux GOARCH=amd64 .*golangci-lint run `).MatchString(lint) {
		t.Error("lint.sh has no golangci-lint pass for GOOS=linux GOARCH=amd64")
	}

	if !strings.Contains(lint, "*[][*?{}]*)") {
		t.Error("lint.sh no longer refuses a checkout path holding glob syntax")
	}
}

// THE LOCAL RACE SUITE IS CI'S: test.sh and the test job's go test run the same
// flags over the same tree, so a local run is the gate CI runs, caps aside.
func TestTheLocalRaceSuiteRunsCIsFlags(t *testing.T) {
	t.Parallel()

	flags := func(line string) []string {
		var out []string
		for _, f := range strings.Fields(line) {
			if f == "-race" || f == "./..." || strings.HasPrefix(f, "-count=") || strings.HasPrefix(f, "-covermode=") {
				out = append(out, f)
			}
		}

		if i := strings.Index(line, "-timeout "); i >= 0 {
			out = append(out, strings.Fields(line[i:])[1])
		}

		slices.Sort(out)

		return out
	}

	ciLine := regexp.MustCompile(`go test -race -count=1 -timeout \S+ .*\./\.\.\.`).FindString(
		readFile(t, filepath.Join("..", ".github", "workflows", "ci.yml")))
	local := strings.Join(strings.Fields(strings.ReplaceAll(readFile(t, filepath.Join("gates", "test.sh")), "\\\n", " ")), " ")
	localLine := regexp.MustCompile(`test \$\{test_p\[@\]\+"\$\{test_p\[@\]\}"\} -race -count=1 -timeout \S+ .*\./\.\.\.`).FindString(local)

	if ciLine == "" || localLine == "" {
		t.Fatalf("could not find the race suite's line: ci.yml %q, test.sh %q", ciLine, localLine)
	}

	if !slices.Equal(flags(ciLine), flags(localLine)) {
		t.Errorf("test.sh runs %v and CI's test job %v", flags(localLine), flags(ciLine))
	}
}
