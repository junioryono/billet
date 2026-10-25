package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	# The final check for a leftover update-grub stand-in: absent (1) unless a
	# test stages another answer.
	if [ "$1" = sh ] && [ "$2" = -c ]; then
		case "$3" in *billet-update-grub-stand-in*) exit "${BILLET_TEST_STANDIN_STATUS:-1}" ;; esac
	fi
	if [ "$1" = runuser ]; then shift 4; fi
	[ "$1" = /var/tmp/billet-ri/with-environment ] || exit 0
	set -- $(printf '%s\n' "$@" | sed "s#^/var/tmp/billet-ri/#$BILLET_TEST_ROOT/var/tmp/billet-ri/#")
	BILLET_RI_ENVIRONMENT=$BILLET_TEST_ROOT/etc/environment \
		BILLET_RI_ENVIRONMENT_READER=$BILLET_TEST_ROOT/var/tmp/billet-ri/pam-environment.awk "$@"
	;;
copy-in)
	if [ -z "$2" ] && [ "${BILLET_TEST_PERMISSIVE_COPY:-}" = 1 ]; then exit 0; fi
	mkdir -p "$(dirname "$BILLET_TEST_ROOT$3")"
	cp -R "$2" "$BILLET_TEST_ROOT$3"
	;;
copy-out) cp -R "$BILLET_TEST_ROOT$2" "$3" ;;
reboot) printf 'next=%s\n' "${BILLET_RI_NEXT_STEP:-}" >>"$log" ;;
*) exit 64 ;;
esac
`

type runnerFixture struct {
	dir, root, out string
	plan           []planStep
	// armPlan is plan-arm64.json, for an arm64 target; plan when it is nil.
	armPlan     []planStep
	differences string
	// env is added to the runner's environment, for what the fake target reads.
	env []string
}

// runRunner runs run-runner-images.sh over the fixture and returns its combined
// output, the driver's log lines, and the exit error.
func runRunner(t *testing.T, fx runnerFixture) (string, []string, error) {
	t.Helper()

	plan, err := json.Marshal(fx.plan)
	if err != nil {
		t.Fatal(err)
	}
	armPlan := plan
	if fx.armPlan != nil {
		if armPlan, err = json.Marshal(fx.armPlan); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"plan.json": string(plan), "plan-arm64.json": string(armPlan),
		"differences.tsv": fx.differences} {
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
		"BILLET_RI_DIR="+fx.dir, "BILLET_RI_SCOPE=guest-x64", "BILLET_RI_NO_PAUSE=1", "BILLET_TEST_ROOT="+fx.root,
		"BILLET_TEST_LOG="+filepath.Join(fx.root, "steps.log"))
	cmd.Env = append(cmd.Env, fx.env...)
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
	// EACH SCRIPT LOGS ITS STEP NUMBER, the name it was staged as, and writes its own
	// name to names.log, so a wrong script staged at the right step shows.
	record := func(name string) string {
		return "#!/bin/sh\necho \"$(basename \"$0\") HELPER_SCRIPTS=${HELPER_SCRIPTS:-} " +
			"IMAGE_VERSION=${IMAGE_VERSION:-}\" >>\"$BILLET_TEST_LOG\"\n" +
			"echo " + name + " >>\"$BILLET_TEST_ROOT/names.log\"\n"
	}
	for name, body := range map[string]string{
		"first.sh": record("first.sh"), "second.sh": record("second.sh"), "third.sh": record("third.sh"),
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
// THE GRUB STAND-IN NEVER SHIPS: one still in the image after the last step (its
// step no longer called update-grub) fails the build, and so does an image the
// runner could not ask. An absent one is the ordinary build.
func TestALeftoverUpdateGrubStandInFailsTheBuild(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status, clause string
	}{
		"left in the image": {"0", "the update-grub stand-in is still in the image"},
		"could not ask":     {"2", "could not check the image for the update-grub stand-in"},
	} {
		fx := newRunnerFixture(t)
		fx.env = []string{"BILLET_TEST_STANDIN_STATUS=" + tc.status}

		output, calls, err := runRunner(t, fx)
		if err == nil || !strings.Contains(output, tc.clause) {
			t.Errorf("%s: answered %v, want a failure saying %q:\n%s", name, err, tc.clause, output)
		}

		asked := false
		for _, call := range calls {
			if strings.HasPrefix(call, "exec|sh|-c|") && strings.Contains(call, "billet-update-grub-stand-in") {
				asked = true
			}
		}
		if !asked {
			t.Errorf("%s: the runner never asked the image about the stand-in: %v", name, calls)
		}
	}

	fx := newRunnerFixture(t)
	if output, _, err := runRunner(t, fx); err != nil {
		t.Fatalf("an image with no stand-in failed: %v\n%s", err, output)
	}
}

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
	fx.differences = "# a comment\nskip\tsecond.sh\t*\tnot for billet\n" +
		"prepare:waagent-conf\tthird.sh\tguest-x64,ami-x64\tthird assumes Azure\n" +
		"skip\tthird.sh\tguest-arm64\tanother target's difference\n"
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
		"a step gone":                {"skip\tgone.sh\t*\twas removed upstream\n", "which plan.json does not have"},
		"a step gone for this build": {"skip\tgone.sh\tguest-x64\twas removed upstream\n", "which plan.json does not have"},
		"an unknown prepare":         {"prepare:nothing\tfirst.sh\t*\tno such prepare\n", "does not implement"},
		"an unknown action":          {"rewrite\tfirst.sh\t*\tno such action\n", "an action nobody implements"},
		"no targets":                 {"skip\tfirst.sh\ta reason\n", "want four tab-separated fields"},
		"no reason":                  {"skip\tfirst.sh\t*\n", "want four tab-separated fields"},
		"a fifth field":              {"skip\tfirst.sh\t*\ta reason\tmore\n", "want four tab-separated fields"},
		"an empty field":             {"skip\t\t*\tfirst.sh\n", "want four tab-separated fields"},
		"an unknown target":          {"skip\tfirst.sh\tguest-riscv\ta reason\n", "guest-riscv is not a target"},
		"another target misspelt":    {"skip\tgone.sh\tami-x46\ta reason\n", "ami-x46 is not a target"},
		"a carriage return":          {"skip\tfirst.sh\t*\ta reason\r\n", "carriage return"},
		"said twice":                 {"skip\tfirst.sh\t*\tone\nskip\tfirst.sh\tami-x64\ttwo\n", "listed twice"},
		"skipped and prepared": {"skip\tthird.sh\t*\tnot run\nprepare:waagent-conf\tthird.sh\t*\tprepared\n",
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

	for _, sc := range runnerImagesScopes {
		dir, root := t.TempDir(), t.TempDir()
		driver := filepath.Join(dir, "driver")
		if err := forkSafeWriteFile(driver, []byte("#!/bin/sh\necho \"$*\" >>\""+root+"/calls\"\nexit 9\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "./run-runner-images.sh")
		cmd.Env = append(os.Environ(), "BILLET_RI_TARGET="+driver, "BILLET_RI_IMAGE_VERSION=1",
			"BILLET_RI_OUT="+t.TempDir(), "BILLET_RI_SCOPE="+sc.name)
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("%s: a driver that refuses everything let the build finish:\n%s", sc.name, output)
		}
		calls, readErr := os.ReadFile(filepath.Join(root, "calls"))
		if readErr != nil || !strings.HasPrefix(string(calls), "exec mkdir -p /var/tmp/billet-ri") {
			t.Fatalf("%s: the real difference list was refused before the target was reached (%v):\n%s",
				sc.name, readErr, output)
		}

		_, skipped := scopedDifferences(t, sc.name)
		deprovision := regexp.MustCompile(`waagent\s+-force\s+-deprovision`)
		for _, step := range readTemplatePlan(t, vendoredTemplateNamed(t, sc.template)) {
			if deprovision.MatchString(strings.Join(step.Inline, "\n")) && skipped[step.ID] == "" {
				t.Errorf("%s: step %s deprovisions the VM for Azure and is not skipped", sc.name, step.ID)
			}
		}
	}
}

// runnerImagesScopes are the targets differences.tsv names, each with the template
// its plan comes from.
var runnerImagesScopes = []struct{ name, template string }{
	{"guest-x64", "build.ubuntu-24_04.pkr.hcl"},
	{"guest-arm64", "build.ubuntu-24_04-arm64.pkr.hcl"},
	{"ami-x64", "build.ubuntu-24_04.pkr.hcl"},
	{"ami-arm64", "build.ubuntu-24_04-arm64.pkr.hcl"},
}

// scopedDifferences reads the real difference list as the runner does for scope:
// the prepares by name and the steps they are on, and each skipped step's reason.
func scopedDifferences(t *testing.T, scope string) (map[string]map[string]bool, map[string]string) {
	t.Helper()

	differences, err := os.ReadFile(filepath.Join(runnerImagesDir, "differences.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	preparedOn := map[string]map[string]bool{}
	skipped := map[string]string{}
	for _, line := range strings.Split(string(differences), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		applies := fields[2] == "*"
		for _, target := range strings.Split(fields[2], ",") {
			applies = applies || target == scope
		}
		if !applies {
			continue
		}
		switch {
		case fields[0] == "skip":
			skipped[fields[1]] = fields[3]
		case strings.HasPrefix(fields[0], "prepare:"):
			name := strings.TrimPrefix(fields[0], "prepare:")
			if preparedOn[name] == nil {
				preparedOn[name] = map[string]bool{}
			}
			preparedOn[name][fields[1]] = true
		}
	}

	return preparedOn, skipped
}

// THE SCOPE IS REQUIRED AND CHECKED before the target is touched: a build that
// named none, or one the runner does not know, would otherwise take a default plan
// and difference set written for another image.
func TestTheRunnerRefusesAMissingOrUnknownScope(t *testing.T) {
	t.Parallel()

	for _, scope := range []string{"", "guest-x86_64", "ami", "*"} {
		fx := newRunnerFixture(t)
		fx.env = []string{"BILLET_RI_SCOPE=" + scope}
		output, calls, err := runRunner(t, fx)
		if err == nil || !strings.Contains(output, "BILLET_RI_SCOPE") {
			t.Errorf("scope %q: answered %v, want a refusal naming BILLET_RI_SCOPE:\n%s", scope, err, output)
		}
		if len(calls) != 0 {
			t.Errorf("scope %q: the target was touched before the refusal: %v", scope, calls)
		}
	}
}

// namesRan is the fixture scripts' own names in the order they ran, each script
// writing its name rather than the number it was staged under, so a wrong script
// staged at the right step shows.
func namesRan(t *testing.T, fx runnerFixture) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(fx.root, "names.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return strings.Fields(string(raw))
}

// EACH TARGET RUNS ITS OWN PLAN WITH ITS OWN TOOLSET, and a difference applies to a
// target wherever the target sits in its list: first, in the middle or last.
func TestEachTargetRunsItsOwnPlanToolsetAndDifferences(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		scope, toolset string
		ran            []string
		prepared       bool
	}{
		{"guest-x64", "toolset-2404.json", []string{"third.sh"}, false},
		{"ami-x64", "toolset-2404.json", []string{"first.sh", "third.sh"}, true},
		{"guest-arm64", "toolset-2404-arm64.json", []string{"first.sh", "third.sh"}, true},
		{"ami-arm64", "toolset-2404-arm64.json", []string{"second.sh", "third.sh"}, false},
	} {
		fx := newRunnerFixture(t)
		x64 := []planStep{
			{ID: "file:/imagegeneration/installers/toolset.json", Kind: "file",
				Sources:     []string{"images/ubuntu/toolsets/toolset-2404.json"},
				Destination: "/imagegeneration/installers/toolset.json"},
			{ID: "first.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/first.sh"},
			{ID: "second.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/second.sh"},
			{ID: "third.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/third.sh"},
		}
		arm64 := []planStep{
			{ID: "file:/imagegeneration/installers/toolset.json", Kind: "file",
				Sources:     []string{"images/ubuntu/toolsets/toolset-2404-arm64.json"},
				Destination: "/imagegeneration/installers/toolset.json"},
			{ID: "second.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/second.sh"},
			{ID: "first.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/first.sh"},
			{ID: "third.sh", Kind: "shell", Execute: "root", Script: "images/ubuntu/scripts/build/third.sh"},
		}
		fx.plan = x64
		fx.armPlan = arm64
		fx.differences = "skip\tfirst.sh\tguest-x64,ami-arm64\tfirst in its list\n" +
			"prepare:waagent-conf\tfirst.sh\tguest-arm64,ami-x64\tlast in its list\n" +
			"skip\tsecond.sh\tami-x64,guest-arm64,guest-x64\tin the middle of its list\n"
		fx.env = []string{"BILLET_RI_SCOPE=" + tc.scope}
		output, calls, err := runRunner(t, fx)
		if err != nil {
			t.Fatalf("%s: run-runner-images.sh: %v\n%s", tc.scope, err, output)
		}
		if got := namesRan(t, fx); !slices.Equal(got, tc.ran) {
			t.Errorf("%s: ran %v, want %v", tc.scope, got, tc.ran)
		}
		joined := strings.Join(calls, "\n")
		copied := false
		for _, call := range calls {
			if strings.HasPrefix(call, "copy-in|") && strings.HasSuffix(call, "|/imagegeneration/installers/toolset.json") {
				copied = strings.HasSuffix(strings.Split(call, "|")[1], "/internal/runnerimages/"+tc.toolset)
			}
		}
		if !copied {
			t.Errorf("%s: the toolset copied in was not %s:\n%s", tc.scope, tc.toolset, joined)
		}
		if got := strings.Contains(joined, "/etc/waagent.conf"); got != tc.prepared {
			t.Errorf("%s: the prepare ran %v, want %v:\n%s", tc.scope, got, tc.prepared, joined)
		}
	}
}

// THE TOOLSET IS THE PINNED ONE BEFORE THE TARGET IS TOUCHED: a declaration that
// does not hash to the line of pinned.txt for its architecture, or a pin with no
// line for it, stops every target, not only the guest build that checks it itself.
func TestATargetRefusesAToolsetThatIsNotThePinnedOne(t *testing.T) {
	t.Parallel()

	vendored, err := os.ReadFile(filepath.Join("..", "internal", "runnerimages", "toolset-2404-arm64.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin, err := os.ReadFile(filepath.Join("..", "internal", "runnerimages", "pinned.txt"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(pin), "\n")
	for name, tc := range map[string]struct{ scope, toolset, pin, clause string }{
		"a tampered arm64 declaration": {"ami-arm64", string(vendored) + " ", string(pin), "hashes to"},
		"the x64 declaration on arm64": {"guest-arm64", "", string(pin), "hashes to"},
		"no arm64 line":                {"ami-arm64", string(vendored), first + "\n", "names no digest"},
	} {
		fx := newRunnerFixture(t)
		env := []string{"BILLET_RI_SCOPE=" + tc.scope}
		if tc.toolset != "" {
			path := filepath.Join(t.TempDir(), "toolset.json")
			if err := os.WriteFile(path, []byte(tc.toolset), 0o600); err != nil {
				t.Fatal(err)
			}
			env = append(env, "BILLET_RI_TOOLSET="+path)
		} else {
			env = append(env, "BILLET_RI_TOOLSET="+filepath.Join("..", "internal", "runnerimages", "toolset-2404.json"))
		}
		pinPath := filepath.Join(t.TempDir(), "pinned.txt")
		if err := os.WriteFile(pinPath, []byte(tc.pin), 0o600); err != nil {
			t.Fatal(err)
		}
		env = append(env, "BILLET_RI_TOOLSET_PIN="+pinPath)
		fx.env = env
		fx.armPlan = fx.plan
		output, calls, err := runRunner(t, fx)
		if err == nil || !strings.Contains(output, tc.clause) {
			t.Errorf("%s: answered %v, want a refusal saying %q:\n%s", name, err, tc.clause, output)
		}
		if len(calls) != 0 {
			t.Errorf("%s: the target was touched before the refusal: %v", name, calls)
		}
	}
}

// A PLAN FILE WITH NO SOURCE STOPS THE BUILD AT ITS STEP, even under a driver that
// would accept a copy with no source: the toolset of another architecture is not
// copied in as an empty path.
func TestAPlanFileWithNoSourceStopsTheBuild(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	fx.plan = append([]planStep{{ID: "file:/imagegeneration/installers/toolset.json", Kind: "file",
		Sources:     []string{"images/ubuntu/toolsets/toolset-2404-arm64.json"},
		Destination: "/imagegeneration/installers/toolset.json"}}, fx.plan...)
	fx.env = []string{"BILLET_TEST_PERMISSIVE_COPY=1"}
	output, calls, err := runRunner(t, fx)
	if err == nil || !strings.Contains(output, "step 1 file:/imagegeneration/installers/toolset.json: "+
		"images/ubuntu/toolsets/toolset-2404-arm64.json has no source here") {
		t.Fatalf("answered %v, want the step's refusal:\n%s", err, output)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "copy-in||") || strings.HasSuffix(call, "|/imagegeneration/installers/toolset.json") {
			t.Fatalf("the copy ran anyway: %s", call)
		}
	}
	if got := namesRan(t, fx); len(got) != 0 {
		t.Fatalf("steps ran after the refusal: %v", got)
	}
}

// A DIFFERENCE FOR ANOTHER TARGET IS NOT APPLIED, even when it names this plan's
// step, and a difference for another target whose step this plan lacks does not
// stop this build.
func TestADifferenceAppliesOnlyToItsTargets(t *testing.T) {
	t.Parallel()

	fx := newRunnerFixture(t)
	fx.differences = "skip\tsecond.sh\tguest-arm64,ami-arm64\tnot on x64\n" +
		"skip\tonly-on-arm64.sh\tguest-arm64\tthis plan has no such step\n"
	output, _, err := runRunner(t, fx)
	if err != nil {
		t.Fatalf("run-runner-images.sh: %v\n%s", err, output)
	}
	if got, want := namesRan(t, fx), []string{"first.sh", "second.sh", "third.sh"}; strings.Contains(output, "not on x64") ||
		!slices.Equal(got, want) {
		t.Fatalf("another target's skip was applied: ran %v, want %v\n%s", got, want, output)
	}
}

// A RESUMED BUILD STARTS AT ITS STEP: nothing before it runs again, the steps from
// it on run in order, and a prepare on the resume step itself runs. A reboot hands
// the driver the step after it, wherever the reboot is, which is where a driver
// whose reboot ends the run resumes it.
func TestABuildResumesAtItsStepAndARebootTellsTheDriverWhere(t *testing.T) {
	t.Parallel()

	for _, at := range []int{2, 4} {
		fx := newRunnerFixture(t)
		reboot := fx.plan[2]
		rest := append(append([]planStep{}, fx.plan[:2]...), fx.plan[3:]...)
		fx.plan = append(append(append([]planStep{}, rest[:at]...), reboot), rest[at:]...)
		_, calls, err := runRunner(t, fx)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("next=%d", at+2); !slices.Contains(calls, want) {
			t.Fatalf("the reboot at step %d did not tell the driver %s:\n%s", at+1, want, strings.Join(calls, "\n"))
		}
	}

	fx := newRunnerFixture(t)
	fx.differences = "prepare:waagent-conf\tfirst.sh\t*\tbefore the resume point\n" +
		"prepare:docker-daemon\tsecond.sh\t*\ton the resume step\n"
	fx.env = []string{"BILLET_RI_FROM=4"}
	output, calls, err := runRunner(t, fx)
	if err != nil {
		t.Fatalf("run-runner-images.sh: %v\n%s", err, output)
	}
	if got, want := namesRan(t, fx), []string{"second.sh", "third.sh"}; !slices.Equal(got, want) {
		t.Fatalf("resumed at 4, ran %v, want %v", got, want)
	}
	joined := strings.Join(calls, "\n")
	for _, call := range calls {
		if strings.Contains(call, "/etc/waagent.conf") || strings.HasPrefix(call, "reboot") ||
			strings.HasSuffix(call, "|/imagegeneration/asset.txt") {
			t.Fatalf("a step before the resume point acted: %s\n%s", call, joined)
		}
	}
	if !strings.Contains(joined, "/etc/docker") {
		t.Fatalf("the prepare on the resume step itself did not run:\n%s", joined)
	}
	if strings.Count(output, "ran before the reboot this build resumed from") != 3 {
		t.Errorf("the three steps before the resume point were not each said:\n%s", output)
	}

	fx = newRunnerFixture(t)
	fx.env = []string{"BILLET_RI_FROM=6"}
	if output, _, err := runRunner(t, fx); err != nil || !strings.Contains(output, "=== [6/6] download:/report.md ===") {
		t.Fatalf("resuming at the last step answered %v:\n%s", err, output)
	}

	for _, from := range []string{"0", "07", "x", "-1", "7"} {
		fx := newRunnerFixture(t)
		fx.env = []string{"BILLET_RI_FROM=" + from}
		output, calls, err := runRunner(t, fx)
		if err == nil || !strings.Contains(output, "BILLET_RI_FROM") {
			t.Errorf("BILLET_RI_FROM=%s: answered %v, want a refusal naming it:\n%s", from, err, output)
		}
		if len(calls) != 0 {
			t.Errorf("BILLET_RI_FROM=%s: the target was touched before the refusal: %v", from, calls)
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
	script := "SCRIPT_DIR=" + here + "\n" + guestImageFunction(t, "write_image_env") +
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
		guestImageFunction(t, "runner_images_machine") + "\n" +
		guestImageFunction(t, "recover_runner_images") + "\n" +
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
		"list": "elsewhere container systemd-nspawn - - -\n" +
			"  old-survivor  container systemd-nspawn ubuntu 24.04 -\n",
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
		script := "SCRIPT_DIR=" + bin + "\n" + guestImageFunction(t, "recover_runner_images") + "\n" +
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
