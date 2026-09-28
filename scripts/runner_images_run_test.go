package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeTargetDriver stands in for a booted target. It logs every call, one line
// per call with the argv joined by `|`, and does the file work inside $ROOT.
// It EXECUTES only what the runner runs as a step (argv starting with env, or
// runuser and then env), with target paths moved under $ROOT; anything else,
// the prepares included, is logged and never run on the machine running the
// test.
const fakeTargetDriver = `#!/bin/sh
set -eu
log=$BILLET_TEST_ROOT/driver.log
( IFS='|'; printf '%s\n' "$*" ) >>"$log"
case "$1" in
exec)
	shift
	if [ "$1" = runuser ]; then shift 4; fi
	[ "$1" = env ] || exit 0
	set -- $(printf '%s\n' "$@" | sed "s#^/var/tmp/billet-ri/#$BILLET_TEST_ROOT/var/tmp/billet-ri/#")
	"$@"
	;;
copy-in)
	mkdir -p "$(dirname "$BILLET_TEST_ROOT$3")"
	cp -R "$2" "$BILLET_TEST_ROOT$3"
	;;
copy-out) cp -R "$BILLET_TEST_ROOT$2" "$3" ;;
reboot) ;;
*) exit 64 ;;
esac
`

type runnerFixture struct {
	dir, root, out string
	plan           []planStep
	differences    string
}

// runRunner runs run-runner-images.sh over the fixture and returns its combined
// output, the driver's log lines, and the exit error.
func runRunner(t *testing.T, fx runnerFixture) (string, []string, error) {
	t.Helper()

	plan, err := json.Marshal(fx.plan)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"plan.json": string(plan), "differences.tsv": fx.differences} {
		if err := os.WriteFile(filepath.Join(fx.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	driver := filepath.Join(fx.dir, "driver")
	if err := forkSafeWriteFile(driver, []byte(fakeTargetDriver), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "run-runner-images.sh")
	cmd.Env = append(os.Environ(),
		"BILLET_RI_TARGET="+driver, "BILLET_RI_IMAGE_VERSION=20260927.1", "BILLET_RI_OUT="+fx.out,
		"BILLET_RI_DIR="+fx.dir, "BILLET_RI_NO_PAUSE=1", "BILLET_TEST_ROOT="+fx.root,
		"BILLET_TEST_LOG="+filepath.Join(fx.root, "steps.log"))
	output, runErr := cmd.CombinedOutput()
	raw, err := os.ReadFile(filepath.Join(fx.root, "driver.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var calls []string
	if text := strings.TrimSpace(string(raw)); text != "" {
		calls = strings.Split(text, "\n")
	}

	return string(output), calls, runErr
}

// newRunnerFixture is a vendored tree of three recording scripts, a failing one
// and a file to copy, with an empty difference list.
func newRunnerFixture(t *testing.T) runnerFixture {
	t.Helper()

	fx := runnerFixture{dir: t.TempDir(), root: t.TempDir(), out: t.TempDir()}
	build := filepath.Join(fx.dir, "upstream", "images", "ubuntu", "scripts", "build")
	if err := os.MkdirAll(build, 0o700); err != nil {
		t.Fatal(err)
	}
	record := "#!/bin/sh\necho \"$(basename \"$0\") HELPER_SCRIPTS=${HELPER_SCRIPTS:-} " +
		"IMAGE_VERSION=${IMAGE_VERSION:-}\" >>\"$BILLET_TEST_LOG\"\n"
	for name, body := range map[string]string{
		"first.sh": record, "second.sh": record, "third.sh": record,
		"fails.sh": "#!/bin/sh\nexit 3\n", "asset.txt": "asset\n",
	} {
		if err := forkSafeWriteFile(filepath.Join(build, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"HELPER_SCRIPTS": "/imagegeneration/helpers", "IMAGE_VERSION": "@image_version@"}
	fx.plan = []planStep{
		{ID: "file:/imagegeneration/asset.txt", Kind: "file",
			Sources: []string{"images/ubuntu/scripts/build/asset.txt"}, Destination: "/imagegeneration/asset.txt"},
		{ID: "first.sh", Kind: "shell", Execute: "root", Env: env, Script: "images/ubuntu/scripts/build/first.sh"},
		{ID: "inline:reboot", Kind: "shell", Execute: "root", Reboots: true, Inline: []string{"sudo reboot"}},
		{ID: "second.sh", Kind: "shell", Execute: "user", Env: env, Script: "images/ubuntu/scripts/build/second.sh"},
		{ID: "third.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/third.sh"},
		{ID: "download:/report.md", Kind: "file", Download: true, Sources: []string{"/report.md"},
			Destination: "report.md"},
	}
	if err := os.WriteFile(filepath.Join(fx.root, "report.md"), []byte("report\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return fx
}

func readSteps(t *testing.T, fx runnerFixture) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(fx.root, "steps.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return string(raw)
}

// THE PLAN RUNS IN ITS ORDER WITH ITS ENVIRONMENT: files copied in, each script
// run through the driver with the template's variables and the build's image
// version, the reboot left to the driver, a user step run as runner, and a
// download copied out.
func TestTheRunnerRunsThePlanInOrder(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	output, calls, err := runRunner(t, fx)
	if err != nil {
		t.Fatalf("run-runner-images.sh: %v\n%s", err, output)
	}
	// A SCRIPT RUNS UNDER ITS STEP NUMBER, the name it was staged as: first.sh is
	// step 2, second.sh step 4 and third.sh step 5.
	want := "2 HELPER_SCRIPTS=/imagegeneration/helpers IMAGE_VERSION=20260927.1\n" +
		"4 HELPER_SCRIPTS=/imagegeneration/helpers IMAGE_VERSION=20260927.1\n" +
		"5 HELPER_SCRIPTS= IMAGE_VERSION=\n"
	if got := readSteps(t, fx); got != want {
		t.Fatalf("the steps ran as\n%s\nwant\n%s", got, want)
	}
	joined := strings.Join(calls, "\n")
	for _, expected := range []string{
		"copy-in|" + filepath.Join(fx.dir, "upstream", "images", "ubuntu", "scripts", "build", "asset.txt") +
			"|/imagegeneration/asset.txt",
		"reboot",
		"exec|runuser|-u|runner|--|env|HELPER_SCRIPTS=/imagegeneration/helpers|IMAGE_VERSION=20260927.1|" +
			"/var/tmp/billet-ri/4",
		"copy-out|/report.md|" + fx.out,
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("the driver was never asked %q; it was asked:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "sudo reboot") {
		t.Error("the template's own reboot command was run instead of the driver's reboot")
	}
	if _, err := os.Stat(filepath.Join(fx.root, "imagegeneration", "asset.txt")); err != nil {
		t.Errorf("the file step did not reach the target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.out, "report.md")); err != nil {
		t.Errorf("the download did not reach BILLET_RI_OUT: %v", err)
	}
}

// A STEP THAT FAILS STOPS THE BUILD, named with its status, and nothing after it
// runs.
func TestAFailingStepStopsTheBuildByName(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	fx.plan[3] = planStep{ID: "fails.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/fails.sh"}
	output, _, err := runRunner(t, fx)
	if err == nil || !strings.Contains(output, "step 4 fails.sh failed with status 3") {
		t.Fatalf("a failing step answered %v:\n%s", err, output)
	}
	if steps := readSteps(t, fx); strings.Contains(steps, "5 ") {
		t.Fatalf("a step after the failure ran:\n%s", steps)
	}
}

// A SKIPPED STEP IS SAID AND NOT RUN, and a prepare runs in the target just
// before its step.
func TestDifferencesSkipAndPrepare(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	fx.differences = "# a comment\nskip\tsecond.sh\tnot for billet\n" +
		"prepare:waagent-conf\tthird.sh\tthird assumes Azure\n"
	output, calls, err := runRunner(t, fx)
	if err != nil {
		t.Fatalf("run-runner-images.sh: %v\n%s", err, output)
	}
	if !strings.Contains(output, "skipped: not for billet") {
		t.Errorf("the skip was not said:\n%s", output)
	}
	if strings.Contains(readSteps(t, fx), "4 ") {
		t.Error("a skipped step ran")
	}
	prepare, third := -1, -1
	for i, call := range calls {
		if strings.Contains(call, "/etc/waagent.conf") {
			prepare = i
		}
		if strings.HasSuffix(call, "/var/tmp/billet-ri/5") && strings.HasPrefix(call, "exec|") {
			third = i
		}
	}
	if prepare < 0 || third < 0 || prepare > third {
		t.Fatalf("the prepare ran at call %d and its step at %d, want the prepare first:\n%s",
			prepare, third, strings.Join(calls, "\n"))
	}
}

// A DIFFERENCE THAT NAMES NOTHING STOPS THE BUILD BEFORE ANYTHING RUNS: a step the
// plan no longer has, a prepare the runner does not implement, an unknown action.
func TestADifferenceThatNamesNothingStopsTheBuild(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ differences, clause string }{
		"a step gone":        {"skip\tgone.sh\twas removed upstream\n", "which the plan does not have"},
		"an unknown prepare": {"prepare:nothing\tfirst.sh\tno such prepare\n", "does not implement"},
		"an unknown action":  {"rewrite\tfirst.sh\tno such action\n", "an action nobody implements"},
		"no reason":          {"skip\tfirst.sh\n", "without a step or a reason"},
	} {
		fx := newRunnerFixture(t)
		fx.differences = tc.differences
		output, calls, err := runRunner(t, fx)
		if err == nil || !strings.Contains(output, tc.clause) {
			t.Errorf("%s: answered %v, want a refusal saying %q:\n%s", name, err, tc.clause, output)
		}
		if len(calls) != 0 {
			t.Errorf("%s: the target was touched before the refusal: %v", name, calls)
		}
	}
}

// THE REAL DIFFERENCE LIST NAMES REAL STEPS and prepares the runner implements,
// and it skips Azure's deprovision wherever the plan has one: the runner reads it
// whole and refuses before touching the target, so a driver that refuses its
// first call proves the list itself was accepted.
func TestTheRealDifferenceListIsAccepted(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	driver := filepath.Join(dir, "driver")
	if err := forkSafeWriteFile(driver, []byte("#!/bin/sh\necho \"$*\" >>\""+root+"/calls\"\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "run-runner-images.sh")
	cmd.Env = append(os.Environ(), "BILLET_RI_TARGET="+driver, "BILLET_RI_IMAGE_VERSION=1",
		"BILLET_RI_OUT="+t.TempDir())
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a driver that refuses everything let the build finish:\n%s", output)
	}
	calls, readErr := os.ReadFile(filepath.Join(root, "calls"))
	if readErr != nil || !strings.HasPrefix(string(calls), "exec mkdir -p /var/tmp/billet-ri") {
		t.Fatalf("the real difference list was refused before the target was reached (%v):\n%s",
			readErr, output)
	}

	differences, err := os.ReadFile(filepath.Join(runnerImagesDir, "differences.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	skipped := map[string]bool{}
	for _, line := range strings.Split(string(differences), "\n") {
		if fields := strings.Split(line, "\t"); len(fields) == 3 && fields[0] == "skip" {
			skipped[fields[1]] = true
		}
	}
	deprovision := regexp.MustCompile(`waagent\s+-force\s+-deprovision`)
	for _, step := range readTemplatePlan(t, vendoredTemplate(t)) {
		if deprovision.MatchString(strings.Join(step.Inline, "\n")) && !skipped[step.ID] {
			t.Errorf("step %s deprovisions the VM for Azure and is not skipped", step.ID)
		}
	}
}
