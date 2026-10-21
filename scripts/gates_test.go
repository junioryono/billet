package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE GATE SCRIPTS ARE EXECUTED IN A TREE OF THEIR OWN. Each test copies the
// scripts it runs, the lock script and the pinned versions into a temporary
// checkout and puts stand-ins where the scripts would find the real work: the
// steps of the full gate, the verify, `go`, `nice`, golangci-lint. A script that
// stopped reading an override could then reach only a stand-in, never the real
// gate, which run from inside `go test` would run this suite inside itself; and
// the environment is sealed (gateEnv), so no startup file or inherited variable
// can put the real tools or the shared lock back in reach.

// gateFixture is a checkout in a new temporary directory; see gateFixtureAt.
func gateFixture(t *testing.T, scripts []string, pins bool, executables map[string]string) string {
	t.Helper()

	return gateFixtureAt(t, t.TempDir(), scripts, pins, executables)
}

// gateFixtureAt makes root a checkout holding copies of env.sh and the named gate
// scripts, the lock script and .github/tool-versions.env (unless pins is false),
// plus the given executables, each at its path under the checkout.
func gateFixtureAt(t *testing.T, root string, scripts []string, pins bool, executables map[string]string) string {
	t.Helper()

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

// gateEnv is a sealed environment for a gate script: the test's own, less every
// variable the gate scripts or the shell read (so a script's defaults are what a
// case does not set, and no BASH_ENV, exported function or shell option runs
// first), with a private HOME, XDG_CACHE_HOME and TMPDIR, bin first on PATH when
// it is not empty, and extra, which replaces a variable of the same name.
func gateEnv(t *testing.T, bin string, extra ...string) []string {
	t.Helper()

	private := t.TempDir()
	env := []string{
		"HOME=" + filepath.Join(private, "home"),
		"XDG_CACHE_HOME=" + filepath.Join(private, "cache"),
		"TMPDIR=" + private,
	}

	for _, kv := range scriptEnv() {
		name, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "BASH_FUNC_") || setIn(env, name) {
			continue
		}

		switch name {
		case "NICE", "TEST_PARALLEL", "GO", "GOEXPERIMENT", "GOOS", "GOARCH", "CI", "CHECK_LOCK", "CHECK_STEPS",
			"VERIFY_CHANGED", "COVERPROFILE", "BILLET_NICED", "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "CDPATH",
			"GLOBIGNORE", "PWD", "OLDPWD":
			continue
		case "PATH":
			if bin != "" {
				kv = "PATH=" + bin + string(os.PathListSeparator) + value
			}
		}

		env = append(env, kv)
	}

	var out []string

	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); !setIn(extra, name) {
			out = append(out, kv)
		}
	}

	return append(out, extra...)
}

// gateCommand runs the gate script at rel under root, from root, in a sealed
// environment with root/bin first on PATH.
func gateCommand(t *testing.T, root, rel string, extra ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), filepath.Join(root, rel))
	cmd.Dir = root
	cmd.Env = gateEnv(t, filepath.Join(root, "bin"), extra...)
	inGroup(cmd)

	return cmd
}

// recordArgv is shell that prints its arguments each in brackets, so one argument
// holding a space reads differently from two.
const recordArgv = `for a; do printf ' [%s]' "$a"; done; echo`

// argv is the line recordArgv prints for args.
func argv(args ...string) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteString(" [" + a + "]")
	}

	return b.String()
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

	out, err := gateCommand(t, root, "scripts/gates/check.sh", "XDG_CACHE_HOME="+xdg).CombinedOutput()
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
		cmd.Dir = root
		cmd.Env = gateEnv(t, "", "CHECK_LOCK="+lock, "CI=true")
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

		out, err := gateCommand(t, root, "scripts/gates/verify.sh", tc.env...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: gates/verify.sh: %v (%s)", name, err, out)
		}

		if !strings.Contains(string(out), tc.want+"\n") {
			t.Errorf("%s: the verify ran as %q, want %q", name, out, tc.want)
		}
	}
}

// raceSuite runs test.sh in a fixture whose `go` and `nice` are stand-ins, and
// answers the line go printed: whether it ran niced, and its arguments.
func raceSuite(t *testing.T, env ...string) string {
	t.Helper()

	root := gateFixture(t, []string{"test.sh"}, true, map[string]string{
		"bin/go":   "#!/bin/sh\nprintf 'niced=%s' \"${BILLET_NICED-}\"\n" + recordArgv + "\n",
		"bin/nice": niceStandIn,
	})

	out, err := gateCommand(t, root, "scripts/gates/test.sh", env...).CombinedOutput()
	if err != nil {
		t.Fatalf("gates/test.sh: %v (%s)", err, out)
	}

	return strings.TrimSpace(string(out))
}

// THE RACE SUITE TAKES THE LOCAL CAPS BY DEFAULT, AND CI'S RUN TAKES NONE: at
// most TEST_PARALLEL packages at once (4 unless set) under `nice -n 10` locally,
// go test's own -p and no nice under CI or with both set empty, over the whole
// tree with atomic coverage into the named profile.
func TestTheRaceSuiteTakesTheLocalCapsAndCIsNone(t *testing.T) {
	t.Parallel()

	suite := []string{"-race", "-count=1", "-timeout", "35m", "-covermode=atomic", "-coverprofile=c.out", "./..."}

	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"local, by default":    {want: "niced=yes" + argv(append([]string{"test", "-p", "4"}, suite...)...)},
		"local, three at once": {env: []string{"TEST_PARALLEL=3"}, want: "niced=yes" + argv(append([]string{"test", "-p", "3"}, suite...)...)},
		"under CI":             {env: []string{"CI=true", "TEST_PARALLEL=3"}, want: "niced=" + argv(append([]string{"test"}, suite...)...)},
		"local, both lifted":   {env: []string{"NICE=", "TEST_PARALLEL="}, want: "niced=" + argv(append([]string{"test"}, suite...)...)},
	} {
		if got := raceSuite(t, append(tc.env, "COVERPROFILE=c.out")...); got != tc.want {
			t.Errorf("%s: the suite would run as %q, want %q", name, got, tc.want)
		}
	}
}

// THE LOCAL RACE SUITE IS CI'S: the arguments test.sh hands go under CI are the
// test job's own go test, but for what changes only the output (-json, which the
// job pipes through jq, and the profile's name) and -vet=off (the gates vet in a
// step of their own; the local suite vetting again only adds a check).
func TestTheLocalRaceSuiteRunsCIsFlags(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join("..", ".github", "workflows", "ci.yml"))), &workflow); err != nil {
		t.Fatal(err)
	}

	var ci []string

	for _, step := range workflow.Jobs["test"].Steps {
		for line := range strings.SplitSeq(strings.ReplaceAll(step.Run, "\\\n", " "), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "go test -race "); ok {
				command, _, _ := strings.Cut(rest, " |")
				ci = append([]string{"test", "-race"}, strings.Fields(command)...)
			}
		}
	}

	if ci == nil {
		t.Fatal("the test job runs no `go test -race`")
	}

	local := strings.Split(strings.TrimPrefix(raceSuite(t, "CI=true"), "niced= ["), "] [")
	local[len(local)-1] = strings.TrimSuffix(local[len(local)-1], "]")

	normal := func(args []string) []string {
		var out []string

		for i := 0; i < len(args); i++ {
			a := args[i]
			if slices.Contains([]string{"-timeout", "-run", "-skip", "-tags", "-p", "-parallel", "-coverpkg"}, a) && i+1 < len(args) {
				a += "=" + args[i+1]
				i++
			}

			if a == "-json" || a == "-vet=off" || strings.HasPrefix(a, "-coverprofile=") {
				continue
			}

			out = append(out, a)
		}

		slices.Sort(out)

		return out
	}

	if !slices.Equal(normal(ci), normal(local)) {
		t.Errorf("test.sh hands go %v and CI's test job runs %v", normal(local), normal(ci))
	}
}

// THE TOOLS ARE INSTALLED AT THE PINS, each read from .github/tool-versions.env,
// and trivy under GOEXPERIMENT=jsonv2, which Go 1.26 needs to build it.
func TestToolsInstallsEveryToolAtItsPin(t *testing.T) {
	t.Parallel()

	v := toolVersions(t)
	root := gateFixture(t, []string{"tools.sh"}, true, map[string]string{
		"bin/go": "#!/bin/sh\nprintf 'GOEXPERIMENT=%s' \"${GOEXPERIMENT-}\"\n" + recordArgv + "\n",
	})

	out, err := gateCommand(t, root, "scripts/gates/tools.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("gates/tools.sh: %v (%s)", err, out)
	}

	want := strings.Join([]string{
		"GOEXPERIMENT=" + argv("install", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"+v["GOLANGCI_VERSION"]),
		"GOEXPERIMENT=" + argv("install", "github.com/goreleaser/goreleaser/v2@"+v["GORELEASER_VERSION"]),
		"GOEXPERIMENT=" + argv("install", "github.com/sqlc-dev/sqlc/cmd/sqlc@"+v["SQLC_VERSION"]),
		"GOEXPERIMENT=" + argv("install", "github.com/terraform-linters/tflint@"+v["TFLINT_VERSION"]),
		"GOEXPERIMENT=jsonv2" + argv("install", "github.com/aquasecurity/trivy/cmd/trivy@"+v["TRIVY_VERSION"]),
	}, "\n") + "\n"
	if string(out) != want {
		t.Errorf("gates/tools.sh ran\n%s\nwant\n%s", out, want)
	}
}

// A CHECKOUT WITH NO PIN FILE IS REFUSED, not run with whatever tools are on PATH.
func TestAGateRefusesACheckoutWithNoPinFile(t *testing.T) {
	t.Parallel()

	root := gateFixture(t, []string{"test.sh"}, false, map[string]string{
		"bin/go": "#!/bin/sh\necho the suite ran\n",
	})

	out, err := gateCommand(t, root, "scripts/gates/test.sh").CombinedOutput()

	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), ".github/tool-versions.env is missing") ||
		strings.Contains(string(out), "the suite ran") {
		t.Errorf("a gate in a checkout with no pin file = %v, %q; want exit 2 naming the file, and nothing run", err, out)
	}
}

// THE FULL GATE RUNS ITS STEPS, IN ITS ORDER, AND STOPS AT THE FIRST THAT FAILS:
// no-mutants first, because an interrupted mutation run leaves a mutant every
// later step would pass, and the rest as CI runs them one by one. CI never runs
// check-steps.sh, so this is what holds it. Every step is a stand-in that records
// its name and fails when FAIL names it.
func TestTheFullGateRunsItsStepsInOrderAndStopsAtAFailure(t *testing.T) {
	t.Parallel()

	stand := func(name string) string {
		return "#!/bin/sh\n{ printf %s " + name + "; " + recordArgv + "; } >> \"$STEPS\"\n[ \"${FAIL-}\" != " + name + " ]\n"
	}
	executables := map[string]string{
		"scripts/check-no-mutants.py":     "import os\nopen(os.environ['STEPS'], 'a').write('no-mutants\\n')\n",
		"bin/go":                          stand("go"),
		"scripts/check-module-sources.sh": stand("module-sources"),
	}

	for _, step := range []string{"build", "fmt-check", "lint", "lint-custom", "test", "lambda-test"} {
		executables["scripts/gates/"+step+".sh"] = stand(step)
	}

	all := []string{"no-mutants", "build", "go" + argv("vet", "./..."), "fmt-check", "lint", "lint-custom", "test", "lambda-test",
		"module-sources"}

	for _, tc := range []struct {
		fail string
		want []string
	}{
		{"", all},
		{"lint", all[:5]},
	} {
		root := gateFixture(t, []string{"check-steps.sh"}, true, executables)
		steps := filepath.Join(t.TempDir(), "steps")

		out, err := gateCommand(t, root, "scripts/gates/check-steps.sh", "STEPS="+steps, "FAIL="+tc.fail).CombinedOutput()
		if (err != nil) != (tc.fail != "") {
			t.Errorf("FAIL=%q: check-steps.sh = %v (%s)", tc.fail, err, out)
		}

		var got []string

		for line := range strings.SplitSeq(strings.TrimSpace(readFile(t, steps)), "\n") {
			got = append(got, strings.TrimSpace(line))
		}

		if !slices.Equal(got, tc.want) {
			t.Errorf("FAIL=%q: check-steps.sh ran %q, want %q", tc.fail, got, tc.want)
		}
	}
}

// THE LINTER RUNS FOR LINUX AS WELL AS THE HOST, because a linter sees only the
// files it would compile; CI's lint job runs the action rather than lint.sh, so
// this is what holds the second pass. A checkout path holding glob syntax is
// refused before either runs.
func TestTheLintGateLintsForTheHostAndLinux(t *testing.T) {
	t.Parallel()

	lint := map[string]string{"bin/golangci-lint": "#!/bin/sh\nprintf 'GOOS=%s GOARCH=%s' \"${GOOS-}\" \"${GOARCH-}\"\n" +
		recordArgv + "\n"}
	pass := argv("run", "--timeout=15m", "--allow-serial-runners")

	root := gateFixture(t, []string{"lint.sh"}, true, lint)

	out, err := gateCommand(t, root, "scripts/gates/lint.sh", "CI=true").CombinedOutput()
	if err != nil {
		t.Fatalf("gates/lint.sh: %v (%s)", err, out)
	}

	if want := "GOOS= GOARCH=" + pass + "\nGOOS=linux GOARCH=amd64" + pass + "\n"; string(out) != want {
		t.Errorf("gates/lint.sh ran\n%s\nwant\n%s", out, want)
	}

	globbed := filepath.Join(t.TempDir(), "billet[2]")
	gateFixtureAt(t, globbed, []string{"lint.sh"}, true, lint)

	out, err = gateCommand(t, globbed, "scripts/gates/lint.sh", "CI=true").CombinedOutput()

	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 1 || !strings.Contains(string(out), "glob syntax") || strings.Contains(string(out), "GOOS=") {
		t.Errorf("gates/lint.sh under %s = %v, %q; want exit 1 naming the glob, and no lint run", globbed, err, out)
	}
}
