package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
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
// It EXECUTES only what the runner runs as a step (argv starting with the
// environment wrapper, or runuser and then it), with target paths moved under
// $ROOT and the target's /etc/environment at $ROOT/etc/environment; anything
// else, the prepares included, is logged and never run on the machine running
// the test.
const fakeTargetDriver = `#!/bin/sh
set -eu
log=$BILLET_TEST_ROOT/driver.log
( IFS='|'; printf '%s\n' "$*" ) >>"$log"
case "$1" in
exec)
	shift
	if [ "$1" = runuser ]; then shift 4; fi
	[ "$1" = /var/tmp/billet-ri/with-environment ] || exit 0
	set -- $(printf '%s\n' "$@" | sed "s#^/var/tmp/billet-ri/#$BILLET_TEST_ROOT/var/tmp/billet-ri/#")
	BILLET_RI_ENVIRONMENT=$BILLET_TEST_ROOT/etc/environment \
		BILLET_RI_ENVIRONMENT_READER=$BILLET_TEST_ROOT/var/tmp/billet-ri/pam-environment.awk "$@"
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
	cmd := exec.CommandContext(t.Context(), "./run-runner-images.sh")
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
	// THE REAL READER, so the fixture runs the one the build ships.
	reader, err := os.ReadFile(filepath.Join(runnerImagesDir, "pam-environment.awk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.dir, "pam-environment.awk"), reader, 0o600); err != nil {
		t.Fatal(err)
	}
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
		"exec|runuser|-u|runner|--|/var/tmp/billet-ri/with-environment|env|" +
			"HELPER_SCRIPTS=/imagegeneration/helpers|IMAGE_VERSION=20260927.1|/var/tmp/billet-ri/4",
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
		"no reason":          {"skip\tfirst.sh\n", "want three tab-separated fields"},
		"a fourth field":     {"skip\tfirst.sh\ta reason\tmore\n", "want three tab-separated fields"},
		"an empty field":     {"skip\t\tfirst.sh\n", "want three tab-separated fields"},
		"a carriage return":  {"skip\tfirst.sh\ta reason\r\n", "carriage return"},
		"said twice":         {"skip\tfirst.sh\tone\nskip\tfirst.sh\ttwo\n", "listed twice"},
		"skipped and prepared": {"skip\tthird.sh\tnot run\nprepare:waagent-conf\tthird.sh\tprepared\n",
			"so one of them would be ignored"},
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
	cmd := exec.CommandContext(t.Context(), "./run-runner-images.sh")
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

// A STEP STARTS IN THE ENVIRONMENT THE STEPS BEFORE IT WROTE, as a Packer step
// does through pam_env: a value written to /etc/environment by one step is set for
// the next, with its quotes removed and nothing expanded, and the step's own
// variables still win.
func TestAStepSeesTheEnvironmentEarlierStepsWrote(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	build := filepath.Join(fx.dir, "upstream", "images", "ubuntu", "scripts", "build")
	writes := "#!/bin/sh\nmkdir -p \"$BILLET_TEST_ROOT/etc\"\n" +
		"printf 'AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\\nXDG_CONFIG_HOME=\"$HOME/.config\"\\n" +
		"HELPER_SCRIPTS=/from/environment\\n' >\"$BILLET_TEST_ROOT/etc/environment\"\n"
	reads := "#!/bin/sh\necho \"$AGENT_TOOLSDIRECTORY $XDG_CONFIG_HOME $HELPER_SCRIPTS\" >>\"$BILLET_TEST_LOG\"\n"
	for name, body := range map[string]string{"writes.sh": writes, "reads.sh": reads} {
		if err := forkSafeWriteFile(filepath.Join(build, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fx.plan = []planStep{
		{ID: "writes.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/writes.sh"},
		{ID: "reads.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/reads.sh",
			Env: map[string]string{"HELPER_SCRIPTS": "/from/the/step"}},
	}
	output, _, err := runRunner(t, fx)
	if err != nil {
		t.Fatalf("run-runner-images.sh: %v\n%s", err, output)
	}
	if got, want := readSteps(t, fx), "/opt/hostedtoolcache $HOME/.config /from/the/step\n"; got != want {
		t.Fatalf("the second step saw %q, want %q", got, want)
	}
}

// A STEP THAT READS STDIN CANNOT EAT THE PLAN: the steps after it still run, a
// failing one still fails the build, and the count of steps reached is checked.
func TestAStepReadingStdinCannotSkipTheRest(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	build := filepath.Join(fx.dir, "upstream", "images", "ubuntu", "scripts", "build")
	if err := forkSafeWriteFile(filepath.Join(build, "drains.sh"), []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fx.plan = []planStep{
		{ID: "drains.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/drains.sh"},
		{ID: "fails.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/fails.sh"},
	}
	output, _, err := runRunner(t, fx)
	if err == nil || !strings.Contains(output, "step 2 fails.sh failed with status 3") {
		t.Fatalf("a step after one reading stdin did not run and fail: %v\n%s", err, output)
	}
}

// THE JOB'S ENVIRONMENT IS GITHUB'S, MADE PLAIN: the agent passes each line of
// /etc/billet-image-env through env -i as it is, so write_image_env removes the
// quotes pam_env would and gives $HOME the runner's home, which GitHub writes
// into PATH for a login shell to expand and nothing here would.
func TestTheImageEnvironmentIsGitHubsMadePlain(t *testing.T) {
	t.Parallel()

	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := "PATH=\"$HOME/.local/bin:/opt/pipx_bin:$HOME/.cargo/bin:/usr/bin\"\n" +
		"ImageOS=ubuntu24\nXDG_CONFIG_HOME=$HOME/.config\nJAVA_HOME_17_X64=/usr/lib/jvm/temurin-17\n" +
		"# a comment\nnot an assignment\n  export SINGLE='/opt/jdk'\n"
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "environment"), []byte(environment), 0o600); err != nil {
		t.Fatal(err)
	}
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := "SCRIPT_DIR=" + here + "\n" + scriptFunction(t, "build-guest-image.sh", "write_image_env") +
		"\nset -euo pipefail\nwrite_image_env \"$1\"\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", rootfs)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write_image_env: %v\n%s", err, output)
	}
	got, err := os.ReadFile(filepath.Join(rootfs, "etc", "billet-image-env"))
	if err != nil {
		t.Fatal(err)
	}
	want := "PATH=/home/runner/.local/bin:/opt/pipx_bin:/home/runner/.cargo/bin:/usr/bin\n" +
		"ImageOS=ubuntu24\nXDG_CONFIG_HOME=/home/runner/.config\nJAVA_HOME_17_X64=/usr/lib/jvm/temurin-17\n" +
		"SINGLE=/opt/jdk\n"
	if string(got) != want {
		t.Fatalf("the image environment is\n%s\nwant\n%s", got, want)
	}

	if err := os.WriteFile(filepath.Join(rootfs, "etc", "environment"), []byte("PATH=/usr/bin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(t.Context(), "bash", "-c", script, "bash", rootfs)
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "no ImageOS") {
		t.Fatalf("an environment without ImageOS answered %v:\n%s", err, output)
	}
}

// A STEP WHOSE ENVIRONMENT CANNOT BE READ DOES NOT RUN WITHOUT IT: the reader
// failing fails the step, rather than running it with none of what earlier steps
// wrote.
func TestAnUnreadableEnvironmentFailsTheStep(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	if err := os.WriteFile(filepath.Join(fx.dir, "pam-environment.awk"), []byte("{ this is not awk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.root, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.root, "etc", "environment"), []byte("A=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.plan = fx.plan[1:2]
	output, _, err := runRunner(t, fx)
	if err == nil || !strings.Contains(output, "could not read") {
		t.Fatalf("a step with an unreadable environment answered %v:\n%s", err, output)
	}
	if steps := readSteps(t, fx); steps != "" {
		t.Fatalf("the step ran without its environment:\n%s", steps)
	}
}

// recoveryFakes stand in for machinectl and the driver for recover_runner_images:
// machinectl lists $FAKE/list and answers each machine's properties from
// $FAKE/<name>.root and $FAKE/<name>.unit (a missing root reads as gone), or
// fails outright while $FAKE/broken names the verb; the driver records who it
// was asked to stop and fails for a name listed in $FAKE/refuse.
var recoveryFakes = map[string]string{
	"machinectl": `#!/bin/sh
if grep -qx "$1" "$FAKE/broken" 2>/dev/null; then echo "Failed to connect to bus" >&2; exit 1; fi
case "$1" in
list) cat "$FAKE/list" 2>/dev/null ;;
show)
	for last; do :; done
	case "$*" in
	*RootDirectory*) [ -f "$FAKE/$last.root" ] || { echo "No machine '$last' known" >&2; exit 1; }
		cat "$FAKE/$last.root" ;;
	*Unit*) cat "$FAKE/$last.unit" ;;
	esac ;;
esac
`,
	"runner-images-nspawn.sh": `#!/bin/sh
echo "stop $BILLET_RI_MACHINE unit=${BILLET_RI_UNIT:-} root=$BILLET_RI_ROOTFS" >>"$FAKE/stops"
! grep -qx "$BILLET_RI_MACHINE" "$FAKE/refuse" 2>/dev/null
`,
}

func runRecovery(t *testing.T, fake string) (string, error) {
	t.Helper()

	for name, body := range recoveryFakes {
		if err := forkSafeWriteFile(filepath.Join(fake, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := "SCRIPT_DIR=" + fake + "\nWORK=/var/tmp/billet-guest\n" +
		scriptFunction(t, "build-guest-image.sh", "runner_images_machine") + "\n" +
		scriptFunction(t, "build-guest-image.sh", "recover_runner_images") + "\n" +
		// CALLED AS THE BUILD CALLS IT, as an if condition, where errexit does not
		// reach inside the function and every failure must be returned.
		"set -euo pipefail\nif ! recover_runner_images /var/tmp/billet-guest/rootfs; then exit 1; fi\n"
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"), "FAKE="+fake)
	output, err := cmd.CombinedOutput()

	return string(output), err
}

// RECOVERY STOPS WHAT HOLDS THIS WORKSPACE, by what it holds: a machine on this
// rootfs is stopped through the unit systemd says holds it, one on another root
// is left alone, and this workspace's own machine is always asked.
func TestRecoveryStopsTheMachinesOnThisWorkspace(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	for name, body := range map[string]string{
		"list": "old-survivor container systemd-nspawn ubuntu 24.04 -\n" +
			"elsewhere container systemd-nspawn - - -\n",
		"old-survivor.root": "/var/tmp/billet-guest/rootfs\n",
		"old-survivor.unit": "machine-old-survivor.scope\n",
		"elsewhere.root":    "/srv/other/rootfs\n",
		"elsewhere.unit":    "elsewhere.service\n",
	} {
		writeFake(t, fake, name, body)
	}
	if output, err := runRecovery(t, fake); err != nil {
		t.Fatalf("recovery: %v\n%s", err, output)
	}
	stops, err := os.ReadFile(filepath.Join(fake, "stops"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(stops)
	if !strings.Contains(got, "stop old-survivor unit=machine-old-survivor.scope root=/var/tmp/billet-guest/rootfs\n") {
		t.Errorf("the machine on this workspace was not stopped through its own unit:\n%s", got)
	}
	if strings.Contains(got, "elsewhere") {
		t.Errorf("recovery stopped a machine that does not hold this workspace:\n%s", got)
	}
	digest := sha256.Sum256([]byte("/var/tmp/billet-guest"))
	if own := "bri-" + hex.EncodeToString(digest[:])[:8]; !strings.Contains(got, "stop "+own+" unit= ") {
		t.Errorf("this workspace's own machine was not asked:\n%s", got)
	}
}

// RECOVERY THAT CANNOT SEE EVERYTHING REFUSES: a listing that fails, a property
// that cannot be read, a machine gone before its root was read (its processes may
// outlive its registration in a unit never learned), an empty holding unit, or a
// stop that cannot be proved each fail it, and the caller then leaves the
// workspace alone.
func TestRecoveryRefusesWhatItCannotSee(t *testing.T) {
	t.Parallel()

	for name, files := range map[string]map[string]string{
		"the listing fails":                           {"broken": "list\n"},
		"a root cannot be read":                       {"list": "m container - - - -\n", "broken": "show\n"},
		"a machine vanished before its root was read": {"list": "m container - - - -\n"},
		"a machine's root reads empty":                {"list": "m container - - - -\n", "m.root": "\n"},
		"the workspace's hash fails":                  {"list": "", "sha256sum": "#!/bin/sh\necho 'abcdef12  -'\nexit 3\n"},
		"the workspace's hash is not a digest":        {"list": "", "sha256sum": "#!/bin/sh\necho 'not-a-digest  -'\n"},
		"the unit holding it reads empty": {"list": "m container - - - -\n", "m.root": "/var/tmp/billet-guest/rootfs\n",
			"m.unit": "\n"},
		"a stop cannot be proved": {"list": "m container - - - -\n", "m.root": "/var/tmp/billet-guest/rootfs\n",
			"m.unit": "m-nspawn.service\n", "refuse": "m\n"},
	} {
		fake := t.TempDir()
		for file, body := range files {
			if strings.HasPrefix(body, "#!") {
				if err := forkSafeWriteFile(filepath.Join(fake, file), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			writeFake(t, fake, file, body)
		}
		if output, err := runRecovery(t, fake); err == nil {
			t.Errorf("%s: recovery succeeded:\n%s", name, output)
		}
	}
}

// A HOST WITH systemd-nspawn AND NO machinectl CANNOT BE ASKED, so recovery
// refuses there; with neither, no machine was ever booted and there is nothing to
// stop.
func TestRecoveryWithoutMachinectl(t *testing.T) {
	t.Parallel()

	for nspawn, refused := range map[bool]bool{true: true, false: false} {
		bin := t.TempDir()
		if nspawn {
			if err := forkSafeWriteFile(filepath.Join(bin, "systemd-nspawn"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		script := "SCRIPT_DIR=" + bin + "\n" + scriptFunction(t, "build-guest-image.sh", "recover_runner_images") + "\n" +
			"set -euo pipefail\nrecover_runner_images /var/tmp/billet-guest/rootfs\n"
		cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin)
		output, err := cmd.CombinedOutput()
		if refused && (err == nil || !strings.Contains(string(output), "machinectl is not")) {
			t.Errorf("nspawn without machinectl answered %v:\n%s", err, output)
		}
		if !refused && err != nil {
			t.Errorf("a host with neither answered %v:\n%s", err, output)
		}
	}
}
