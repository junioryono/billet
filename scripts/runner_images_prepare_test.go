package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// prepareTarget stands in for the target driver inside the shell that runs a
// prepare: it runs an exec on this machine with the target's /imagegeneration
// and /usr/sbin moved under $ROOT, and refuses every other call.
const prepareTarget = `target() {
	[ "$1" = exec ] || { echo "unexpected target call: $*" >&2; return 64; }
	shift
	local arg args=()
	for arg; do
		arg=${arg//\/imagegeneration\//$ROOT/imagegeneration/}
		args+=("${arg//\/usr\/sbin\//$ROOT/usr/sbin/}")
	done
	"${args[@]}"
}
`

// runnerImagesFunction is the shell function NAME as run-runner-images.sh
// defines it.
func runnerImagesFunction(t *testing.T, name string) string {
	t.Helper()

	source := readScriptFile(t, "run-runner-images.sh")
	start := strings.Index(source, "\n"+name+"() {\n")
	if start < 0 {
		t.Fatalf("run-runner-images.sh has no %s function", name)
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s in run-runner-images.sh", name)
	}

	return source[start+1 : start+end+3]
}

// runPrepare runs CALL in bash with the named functions from the runner and the
// stand-in target, against ROOT, and returns the combined output.
func runPrepare(t *testing.T, root, call string, functions ...string) (string, error) {
	t.Helper()

	script := prepareTarget
	for _, name := range functions {
		script += runnerImagesFunction(t, name)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script+call+"\n")
	cmd.Env = append(os.Environ(), "ROOT="+root)
	output, err := cmd.CombinedOutput()

	return string(output), err
}

// stageUpstreamTests copies the vendored Pester files a prepare edits into
// ROOT/imagegeneration/tests, where the template's file step puts them.
func stageUpstreamTests(t *testing.T, root string, files ...string) {
	t.Helper()

	dir := filepath.Join(root, "imagegeneration", "tests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		body, err := os.ReadFile(filepath.Join(runnerImagesDir, "upstream", "images", "ubuntu", "scripts", "tests", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// THE TEST PREPARES SKIP EXACTLY THEIR TESTS IN THE VENDORED SUITE: each named
// test's It line becomes `It "<name>" -Skip {` at its own indentation, its
// conditional -Skip included, and no other line of either file changes. A pin
// bump that renames one of them fails here rather than in the build's last hour.
func TestTheTestPreparesSkipExactlyTheirTests(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stageUpstreamTests(t, root, "System.Tests.ps1", "Apt.Tests.ps1")
	output, err := runPrepare(t, root,
		"prepare_system_tests_virtio && prepare_system_tests_rootflags && prepare_apt_tests_two_mirrors",
		"skip_upstream_test", "prepare_system_tests_virtio", "prepare_system_tests_rootflags",
		"prepare_apt_tests_two_mirrors")
	if err != nil {
		t.Fatalf("the prepares failed against the vendored tests: %v\n%s", err, output)
	}

	for file, names := range map[string][]string{
		"System.Tests.ps1": {
			"All SCSI and NVMe devices have read_ahead_kb set to 128",
			"Kernel command line contains the root filesystem mount options",
			"Root filesystem is mounted with the relaxed durability options",
		},
		"Apt.Tests.ps1": {"Apt sources resolve through the mirror list"},
	} {
		before, err := os.ReadFile(filepath.Join(runnerImagesDir, "upstream", "images", "ubuntu", "scripts", "tests", file))
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(filepath.Join(root, "imagegeneration", "tests", file))
		if err != nil {
			t.Fatal(err)
		}
		was, is := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
		if len(was) != len(is) {
			t.Fatalf("%s went from %d lines to %d", file, len(was), len(is))
		}
		want := map[string]bool{}
		for _, name := range names {
			want[`It "`+name+`" -Skip {`] = true
		}
		for i := range was {
			if was[i] == is[i] {
				continue
			}
			trimmed := strings.TrimLeft(is[i], " \t")
			if !want[trimmed] || is[i][:len(is[i])-len(trimmed)] != was[i][:len(was[i])-len(strings.TrimLeft(was[i], " \t"))] {
				t.Errorf("%s line %d changed from %q to %q, which no prepare asks for", file, i+1, was[i], is[i])

				continue
			}
			delete(want, trimmed)
		}
		for line := range want {
			t.Errorf("%s: no line became %q", file, line)
		}
		if _, err := os.Stat(filepath.Join(root, "imagegeneration", "tests", file+".billet")); !os.IsNotExist(err) {
			t.Errorf("%s: the rewrite left its scratch copy behind (%v)", file, err)
		}
	}
}

// A TEST NAMED NONE OR TWICE IS REFUSED, AND THE FILE IS LEFT AS IT WAS: a name
// that only begins another test's name is not that test.
func TestSkippingATestRefusesAnythingButOneMatch(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"absent":      "Describe \"x\" {\n    It \"another test\" {\n    }\n}\n",
		"a prefix":    "Describe \"x\" {\n    It \"the test, longer\" {\n    }\n}\n",
		"named twice": "Describe \"x\" {\n    It \"the test\" {\n    }\n    It \"the test\" {\n    }\n}\n",
	} {
		root := t.TempDir()
		dir := filepath.Join(root, "imagegeneration", "tests")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "X.Tests.ps1")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := runPrepare(t, root, `skip_upstream_test /imagegeneration/tests/X.Tests.ps1 "the test"`,
			"skip_upstream_test")
		if err == nil || !strings.Contains(output, `tests named "the test", want exactly one`) {
			t.Errorf("%s: answered %v, want a refusal naming the test:\n%s", name, err, output)
		}
		got, readErr := os.ReadFile(file)
		if readErr != nil || string(got) != body {
			t.Errorf("%s: the file changed on a refusal (%v):\n%s", name, readErr, got)
		}
		if _, statErr := os.Stat(file + ".billet"); !os.IsNotExist(statErr) {
			t.Errorf("%s: the refusal left its scratch copy behind (%v)", name, statErr)
		}
	}
}

// THE update-grub STAND-IN ANSWERS configure-environment.sh's ONE CALL AND IS
// GONE, so the image ships no command claiming to update a GRUB it does not
// have; and a real update-grub is refused, never shadowed.
func TestTheGrubStandInAnswersOnceAndLeaves(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sbin := filepath.Join(root, "usr", "sbin")
	if err := os.MkdirAll(sbin, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := runPrepare(t, root, "prepare_grub_absent", "prepare_grub_absent"); err != nil {
		t.Fatalf("prepare_grub_absent: %v\n%s", err, output)
	}
	standIn := filepath.Join(sbin, "update-grub")
	info, err := os.Stat(standIn)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("no executable stand-in after the prepare (%v, %v)", info, err)
	}
	// RUN AS configure-environment.sh RUNS IT, by name through PATH.
	cmd := exec.CommandContext(t.Context(), "bash", "-ec", "update-grub")
	cmd.Env = append(os.Environ(), "PATH="+sbin+":/usr/bin:/bin")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the stand-in failed the call it exists to answer: %v\n%s", err, output)
	}
	if _, err := os.Lstat(standIn); !os.IsNotExist(err) {
		t.Fatalf("the stand-in is still there after its call (%v)", err)
	}

	present := []byte("#!/bin/sh\nexec grub-mkconfig -o /boot/grub/grub.cfg \"$@\"\n")
	if err := os.WriteFile(standIn, present, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runPrepare(t, root, "prepare_grub_absent", "prepare_grub_absent")
	if err == nil || !strings.Contains(output, "has a GRUB the stand-in would hide") {
		t.Fatalf("a present update-grub answered %v, want a refusal:\n%s", err, output)
	}
	if got, err := os.ReadFile(standIn); err != nil || string(got) != string(present) {
		t.Fatalf("the present update-grub was changed (%v):\n%s", err, got)
	}
}

// stepText is what a planned shell step runs: its script's text, or its inline
// commands.
func stepText(t *testing.T, step planStep) string {
	t.Helper()

	if step.Script == "" {
		return strings.Join(step.Inline, "\n")
	}

	return readScriptFile(t, filepath.Join(runnerImagesDir, "upstream", step.Script))
}

// THE REAL DIFFERENCE LIST PUTS EACH OF THESE PREPARES WHERE IT IS NEEDED: the
// update-grub stand-in on every step that calls update-grub, since it answers
// one call and is gone; and each test skip on or before the first step that can
// run its file, an invoke_tests mid-build (filtered or not) or the final suite.
// Deleting an entry, or moving one past the step that needs it, fails here
// rather than in a build.
func TestEachPrepareIsInPlaceBeforeTheStepThatNeedsIt(t *testing.T) {
	t.Parallel()

	differences, err := os.ReadFile(filepath.Join(runnerImagesDir, "differences.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	preparedOn := map[string]map[string]bool{}
	for _, line := range strings.Split(string(differences), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !strings.HasPrefix(fields[0], "prepare:") {
			continue
		}
		name := strings.TrimPrefix(fields[0], "prepare:")
		if preparedOn[name] == nil {
			preparedOn[name] = map[string]bool{}
		}
		preparedOn[name][fields[1]] = true
	}

	plan := readTemplatePlan(t, vendoredTemplate(t))
	texts := make([]string, len(plan))
	for i, step := range plan {
		if step.Kind == "shell" && !step.Reboots {
			texts[i] = stepText(t, step)
		}
	}

	grubCall := regexp.MustCompile(`(?m)^[^#\n]*\bupdate-grub\b`)
	calls := 0
	for i, step := range plan {
		if !grubCall.MatchString(texts[i]) {
			continue
		}
		calls++
		if !preparedOn["grub-absent"][step.ID] {
			t.Errorf("step %s calls update-grub and has no prepare:grub-absent", step.ID)
		}
	}
	if calls == 0 {
		t.Error("no step calls update-grub any more; retire prepare:grub-absent and this check")
	}

	for prepare, file := range map[string]string{
		"apt-tests-two-mirrors":  "Apt",
		"system-tests-virtio":    "System",
		"system-tests-rootflags": "System",
	} {
		runs := regexp.MustCompile(`(?m)^[^#\n]*(invoke_tests\s+"?` + file + `"?(\s|$)|RunAll-Tests\.ps1)`)
		first := -1
		for i := range plan {
			if runs.MatchString(texts[i]) {
				first = i

				break
			}
		}
		if first < 0 {
			t.Errorf("no step runs the %s tests, so prepare:%s skips nothing", file, prepare)

			continue
		}
		placed := false
		for i := 0; i <= first; i++ {
			placed = placed || preparedOn[prepare][plan[i].ID]
		}
		if !placed {
			t.Errorf("prepare:%s is not on or before %s, the first step that runs the %s tests",
				prepare, plan[first].ID, file)
		}
	}
}
