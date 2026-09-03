package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE GATE DECIDES FROM A STATUS, NEVER FROM TEXT, and that is the defect it was
// written for.
//
// The build used to run moby's checker as
//
//	(cd "$WORK" && ./check-config.sh cfg | sed ... | grep -Ei missing | head -30) \
//	  || echo "NOTHING MISSING"
//
// Measured in ubuntu:24.04 against the committed config with CONFIG_VETH=y removed:
// the checker exits 1, that pipeline prints fifteen "missing" lines AND THEN
// "NOTHING MISSING", and the script carries on and exits 0. Both halves are wrong
// in the same way — a passing kernel legitimately reports a dozen OPTIONAL features
// as missing, so the text says nothing either.
//
// TWO STAND-IN CHECKERS, IN OPPOSITE DIRECTIONS. One that complains loudly and
// exits 0 must PASS; one that says nothing and exits 1 must FAIL. A gate reading
// output gets both backwards.
func TestTheKernelGateBelievesTheCheckersStatusRatherThanItsOutput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "noisy but passing",
			body: "#!/bin/sh\necho '- CONFIG_VETH: missing'\necho '- zfs command: missing'\nexit 0\n",
		},
		{
			name:    "silent but failing",
			body:    "#!/bin/sh\nexit 1\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := runKernelGate(t, goodKernelConfig(t), stubChecker(t, tc.body))

			if tc.wantErr && err == nil {
				t.Fatalf("the gate passed a kernel its checker rejected:\n%s", out)
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("the gate failed a kernel its checker accepted (%v):\n%s", err, out)
			}
		})
	}
}

// A CHECKER THAT IS NOT THERE IS NOT A PASS. The release workflow never supplied
// one, so the build printed "skipped" and published anyway — for every kernel this
// project has ever released.
func TestTheKernelGateFailsWhenTheCheckerIsAbsent(t *testing.T) {
	t.Parallel()

	out, err := runKernelGate(t, goodKernelConfig(t),
		[]string{"CHECK_CONFIG_SH=" + filepath.Join(t.TempDir(), "not-here.sh")})
	if err == nil {
		t.Fatalf("a kernel was passed with no checker present:\n%s", out)
	}

	if !strings.Contains(out, "must not be published") {
		t.Errorf("the refusal does not say what is at stake:\n%s", out)
	}
}

// AND ONE THAT IS NOT THE AUDITED COPY IS NOT A PASS EITHER. The point of vendoring
// it is that what runs is the revision somebody read.
func TestTheKernelGateFailsWhenTheCheckerIsNotThePinnedOne(t *testing.T) {
	t.Parallel()

	out, err := runKernelGate(t, goodKernelConfig(t),
		[]string{"CHECK_CONFIG_SHA256=" + strings.Repeat("0", 64)})
	if err == nil {
		t.Fatalf("a checker that does not match its pin was run anyway:\n%s", out)
	}

	if !strings.Contains(out, "unreviewed edit") {
		t.Errorf("the refusal does not say why the digest matters:\n%s", out)
	}
}

// EVERY REQUIRED OPTION MUST BE BUILT IN, and the failure has to name which one.
// moby's checker accepts a module here; a microVM has no initramfs, so a module is
// the same as absent and the job fails somewhere in the middle instead.
func TestTheKernelGateRefusesAConfigMissingARequiredBuiltIn(t *testing.T) {
	t.Parallel()

	config := writeKernelConfig(t, strings.ReplaceAll(readGoodKernelConfig(t),
		"CONFIG_VETH=y\n", ""))

	out, err := runKernelGate(t, config, passingChecker(t))
	if err == nil {
		t.Fatalf("a kernel with no veth support was passed:\n%s", out)
	}

	if !strings.Contains(out, "CONFIG_VETH") {
		t.Errorf("the refusal does not name the option:\n%s", out)
	}
}

// A MODULE IS THE SAME AS ABSENT, WHEREVER IT IS. This is how billet stays stricter
// than moby's checker for every flag moby grades without keeping a second copy of
// moby's list: one rule refusing =m anywhere covers all of them.
func TestTheKernelGateRefusesAModuleWhereABuiltInIsRequired(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, from, to string }{
		{"a required option", "CONFIG_OVERLAY_FS=y", "CONFIG_OVERLAY_FS=m"},
		{"an option billet does not list", "CONFIG_CGROUP_PERF=y", "CONFIG_CGROUP_PERF=m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := readGoodKernelConfig(t)

			if !strings.Contains(body, tc.from+"\n") {
				t.Fatalf("the committed config no longer has %s, so this case tests nothing",
					tc.from)
			}

			config := writeKernelConfig(t, strings.ReplaceAll(body, tc.from+"\n", tc.to+"\n"))

			out, err := runKernelGate(t, config, passingChecker(t))
			if err == nil {
				t.Fatalf("a kernel building %s as a module was passed:\n%s", tc.to, out)
			}

			if !strings.Contains(out, tc.to) {
				t.Errorf("the refusal does not name the module:\n%s", out)
			}
		})
	}
}

// AND A CONFIG FULL OF MODULES STILL EXPLAINS ITSELF, which is the same pipefail
// trap one branch over from where it was first fixed.
//
// MEASURED in ubuntu:24.04: `grep -E '^CONFIG_...=m$' <3.2MB file> | head -20` under
// `set -euo pipefail` exits **141** — so the refusal's explanation, and its
// deliberate `exit 1`, were never reached. The verdict stayed a refusal, which is
// why it survived a reading: the artifact is still rejected and the operator is
// simply not told why.
func TestAConfigFullOfModulesStillSaysWhyItWasRefused(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	b.WriteString(readGoodKernelConfig(t))

	// FAR PAST THE CAP AND PAST A PIPE BUFFER: twenty-one lines fit in 64KB, so the
	// writer finishes before the reader leaves and there is no SIGPIPE to see.
	for i := range 200000 {
		fmt.Fprintf(&b, "CONFIG_BILLET_PROBE%d=m\n", i)
	}

	out, err := runKernelGate(t, writeKernelConfig(t, b.String()), passingChecker(t))
	if err == nil {
		t.Fatalf("a config building 200000 modules was accepted:\n%s", lastLines(out, 5))
	}

	if !strings.Contains(out, "no initramfs") {
		t.Errorf("the refusal never reached its explanation, so an operator is told a "+
			"kernel is refused and not why:\n%s", lastLines(out, 8))
	}
}

// A READ IT COULD NOT PERFORM IS NOT "NO MODULES".
//
// grep's status is three-valued — 0 found, 1 not found, anything ABOVE 1 could not
// look — and folding the third into "not found" is the could-not-tell/no collapse
// this repository has removed from the credential paths and the toolcache gates. It
// matters here in one direction only: the required-builtins loop reads a failure as
// "the option is absent" and refuses, while the module check read one as "there are
// no modules" and published. moby's checker does not restore that verdict, because
// it accepts `=m` for every flag it grades.
//
// A grep SHIM RATHER THAN A CONTRIVED CONFIG, because status 2 is what grep returns
// when it cannot read the file, and a config that provokes it is not something a
// build produces.
func TestAConfigItCouldNotSearchIsNotAConfigWithoutModules(t *testing.T) {
	t.Parallel()

	shims := t.TempDir()

	tool, err := exec.LookPath("grep")
	if err != nil {
		t.Skip("no grep on PATH to shim")
	}

	// FAILS ONLY THE MODULE QUERY. Everything else is forwarded, so the gate reaches
	// the module check with its other answers intact — which is what makes this about
	// that one read rather than about a broken PATH.
	shim := "#!/bin/sh\nfor a in \"$@\"; do\n" +
		"  case $a in *'=m$') echo 'grep: cannot read' >&2; exit 2 ;; esac\n" +
		"done\nexec " + tool + " \"$@\"\n"

	if err := os.WriteFile(filepath.Join(shims, "grep"), []byte(shim), 0o700); err != nil {
		t.Fatalf("write the grep shim: %v", err)
	}

	out, err := runKernelGate(t, goodKernelConfig(t), append(passingChecker(t),
		"PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH")))
	if err == nil {
		t.Fatalf("a config the gate could not search for modules was accepted:\n%s", out)
	}

	if !strings.Contains(out, "could not read") {
		t.Errorf("the refusal does not say the read failed, so it reads as a verdict "+
			"about the kernel:\n%s", lastLines(out, 6))
	}
}

// THE VENDORED CHECKER IS THE ONE THAT WAS AUDITED. The pin holds two facts on one
// line for the reason internal/runnerimages gives: the commit is provenance and the
// digest is integrity, and a refresh that updates one of them either names a file
// nobody has or claims a revision the bytes did not come from.
func TestTheVendoredKernelCheckerMatchesItsPin(t *testing.T) {
	t.Parallel()

	pin, err := os.ReadFile(filepath.Join("kernel", "check-config.pin"))
	if err != nil {
		t.Fatalf("read the pin: %v", err)
	}

	fields := strings.Fields(strings.SplitN(strings.TrimSpace(string(pin)), "\n", 2)[0])
	if len(fields) != 2 {
		t.Fatalf("the pin's first line is %q; it is '<upstream commit> <sha256>'", pin)
	}

	if len(fields[0]) != 40 {
		t.Errorf("%q is not a git commit, and it is what says where the copy came from",
			fields[0])
	}

	body, err := os.ReadFile(filepath.Join("kernel", "check-config.sh"))
	if err != nil {
		t.Fatalf("read the vendored checker: %v", err)
	}

	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != fields[1] {
		t.Errorf("the vendored checker hashes to %s and the pin names %s; refresh the copy "+
			"and its pin together, or restore the file", got, fields[1])
	}
}

// THE CONFIG IN THE REPOSITORY PASSES ITS OWN GATE. This is the cheap half of the
// release check, running at every `make check` instead of once a week: the base
// config is what a version bump starts from, and an option dropped from it would
// otherwise be found by a kernel build an hour into a Monday morning.
func TestTheCommittedKernelConfigSatisfiesEveryRequiredBuiltIn(t *testing.T) {
	t.Parallel()

	out, err := runKernelGate(t, goodKernelConfig(t), passingChecker(t))
	if err != nil {
		t.Fatalf("the committed kernel config fails billet's own rules (%v):\n%s", err, out)
	}
}

// AND IT PASSES MOBY'S CHECKER FOR REAL, on the platform that can run it.
//
// LINUX ONLY, and not out of caution: the vendored script reads /proc/mounts, calls
// GNU `stat -f -c` and `sed -rne`, and dies on macOS inside a command substitution
// under `set -e`. The builder and CI are Linux. A test that cannot be trusted on a
// platform should say so rather than assert something else there.
func TestTheCommittedKernelConfigPassesTheVendoredChecker(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("moby's check-config.sh needs /proc, GNU stat and GNU sed")
	}

	out, err := runKernelGate(t, goodKernelConfig(t), nil)
	if err != nil {
		t.Fatalf("the committed kernel config does not pass moby's checker (%v):\n%s", err, out)
	}

	if !strings.Contains(out, "KERNEL CONFIG OK") {
		t.Errorf("the gate passed without saying so:\n%s", out)
	}
}

// A GATE NOTHING CALLS IS DECORATION, which is exactly what the old check-config
// block was.
//
// ASSERTED AS EXACT LINES, AFTER THREE ROUNDS OF SOMETHING CLEVERER. A substring
// search was satisfied by a comment; a prefix check by `echo <gate>`; a prefix check
// with an operator scan by `<gate> … || true`; and each fix opened another hole — a
// line continuation, `set -euo pipefail; exit 0`, `if ! <gate>; then :; fi`, a
// heredoc, `function name { }`. Every one was a line-based guess at shell control
// flow, and the next hole was always one construct away.
//
// SO THE CONTRACT IS THE LINE ITSELF. Both call sites are single, deliberate lines
// that nothing else needs to share, and comparing them exactly needs no inference:
// an appended `|| true` is a different line, a continuation is a different line, an
// indent is a different line, and a move inside a function changes the indent. What
// it costs is that a legitimate reformat fails this test, which the message says
// plainly — a two-line contract is worth restating by hand.
//
// WHAT IT PROVES IS THE PRESENCE OF AN EXACT PHYSICAL LINE, and nothing about
// whether that line is reached — said plainly, because a rule whose stated reason is
// untrue is worse than no rule. The same bytes inside a heredoc, or on the right of
// `: || \`, are still that line and still pass.
//
// WHAT IT REJECTS is the concrete family the review rounds actually produced: a
// comment naming the gate, `echo <gate>`, `<gate> || true`, `<gate> || echo …`, a
// trailing `&`, a line continuation, a `<gate>.disabled`, an added
// `continue-on-error`, and an indented copy — which is what the invocation looks like
// when it is moved into a function as this repository writes them. Nothing here
// claims a shell could not be made to defeat it; only a parser could answer that,
// and the accidents this exists for are all in that list.
//
// STRUCTURAL, AND DELIBERATELY SO. Running the real build needs root, debootstrap
// and an hour; what can be checked cheaply is that the calls exist, exactly, in the
// only form that lets the gate's status end the build.
func TestTheKernelBuildAndTheWorkflowBothRunTheGate(t *testing.T) {
	t.Parallel()

	// ASKED OF THE CODE, NOT THE PROSE. The comments in this script quote the line
	// the gate replaced and name both files by path, so a comment-blind assertion
	// passes against a script whose actual invocations have been deleted. WHOLE
	// comment lines only: `s/#.*$//` reads `${version#go}` as a comment.
	build := withoutShellComments(readFile(t, "build-guest-kernel.sh"))

	const buildCall = `"$REPO_ROOT/scripts/check-guest-kernel-config.sh" "$OUT/vmlinux-billet.config"`

	if !hasExactLine(build, buildCall) {
		t.Errorf("scripts/build-guest-kernel.sh does not run the gate as exactly\n\n"+
			"    %s\n\nso a hand-run build can publish a kernel nothing checked. If the "+
			"line was reformatted rather than removed, update this test to the new one.",
			buildCall)
	}

	// THE SWALLOWED STATUS MUST NOT COME BACK. It read as a diagnostic and was the
	// bug: under pipefail the checker's failure becomes the pipeline's, and `|| echo`
	// prints a reassuring sentence on the failure path.
	if strings.Contains(build, `|| echo "NOTHING MISSING"`) {
		t.Error("the build still ends its check with `|| echo \"NOTHING MISSING\"`, which " +
			"reports a failing checker as a pass")
	}

	// THE LIST IS READ AND ACTED ON, not merely mentioned. One file with two readers
	// is the whole reason it is a file; a build that kept its own copy would drift
	// silently, which is the two-pins problem this arrangement removes.
	for _, want := range []string{
		`done <"$REQUIRED_BUILTINS"`,
		`./scripts/config --enable "$opt"`,
	} {
		if !strings.Contains(build, want) {
			t.Errorf("the build does not %s, so the list it enables and the list the gate "+
				"requires can drift into a check that passes for options nothing sets", want)
		}
	}

	// PARSED RATHER THAN GREPPED, for two reasons: a step's POSITION is the property
	// here, and a workflow that does not parse is one github rejects at dispatch —
	// which nothing else in the suite would notice.
	steps := guestImageBuildSteps(t)

	const workflowCall = "scripts/check-guest-kernel-config.sh out/vmlinux-billet.config"

	checkAt, rootfsAt, cacheAt := -1, -1, -1

	for i, step := range steps {
		if hasExactLine(withoutShellComments(step.Run), workflowCall) {
			checkAt = i
		}

		if step.Name == "Build the root filesystem" {
			rootfsAt = i
		}

		if step.Name == "Kernel cache" {
			cacheAt = i
		}
	}

	// THE CACHE HIT IS THE PATH THAT MATTERS. build-guest-kernel.sh does not run when
	// the kernel comes from the cache, and actions/cache saves in its post step even
	// after a failed one -- so the workflow needs a gate of its own, with no `if:`
	// that could skip it on exactly that path.
	if checkAt < 0 {
		t.Fatalf("no workflow step runs the gate as exactly\n\n    %s\n\nso a cached "+
			"kernel is published unchecked", workflowCall)
	}

	if steps[checkAt].If != "" {
		t.Errorf("the gate step is conditional on %q; the run it must not be skipped on is "+
			"the one where the kernel came from the cache", steps[checkAt].If)
	}

	if steps[checkAt].ContinueOnError {
		t.Error("the gate step is continue-on-error, so it runs, fails, and the job " +
			"publishes anyway")
	}

	// AND THE STEP IS NOTHING BUT THE GATE. It is a dedicated step, so its whole
	// block can be compared: anything else in it — an `exit 0`, a `set -e; exit 0` on
	// one line, a continuation — is a way for the gate not to run while the step
	// succeeds and the job goes on to publish. Preparation belongs in its own step.
	if body := substantiveLines(steps[checkAt].Run); len(body) != 1 || body[0] != workflowCall {
		t.Errorf("the gate step runs %v; it is a dedicated step and must run the gate and "+
			"nothing else, or something before it can leave and the kernel goes "+
			"unchecked", body)
	}

	if rootfsAt < 0 || checkAt > rootfsAt {
		t.Error("the kernel is checked after the hour-long root filesystem build")
	}

	// AND THE BUILT CONFIG IS WHAT THE GATE READS, so the cache has to carry it: on a
	// hit nothing regenerates one, and `make olddefconfig` means the committed base
	// is where the build STARTS rather than what it produced.
	if cacheAt < 0 {
		t.Fatal("the workflow no longer caches the kernel")
	}

	if !strings.Contains(steps[cacheAt].With.Path, "out/vmlinux-billet.config") {
		t.Errorf("the kernel cache carries %q; without the built config a cache hit has "+
			"nothing for the gate to read", steps[cacheAt].With.Path)
	}
}

// workflowStep is the half of a workflow step these assertions need.
type workflowStep struct {
	Name string `yaml:"name"`
	If   string `yaml:"if"`
	Run  string `yaml:"run"`

	// ContinueOnError IS PARSED BECAUSE ONE LINE OF IT UNDOES THE WHOLE GATE. A step
	// carrying `continue-on-error: true` runs, fails, and the job goes on to build
	// the root filesystem and publish — which is the swallowed verdict this file is
	// about, expressed in yaml instead of shell.
	ContinueOnError bool `yaml:"continue-on-error"`

	With struct {
		Path string `yaml:"path"`
	} `yaml:"with"`
}

func guestImageBuildSteps(t *testing.T) []workflowStep {
	t.Helper()

	var workflow struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}

	if err := yaml.Unmarshal([]byte(readWorkflow(t)), &workflow); err != nil {
		t.Fatalf("the guest image workflow is not valid yaml: %v", err)
	}

	build, ok := workflow.Jobs["build"]
	if !ok {
		t.Fatal("the guest image workflow has no build job")
	}

	if len(build.Steps) == 0 {
		t.Fatal("the build job has no steps")
	}

	return build.Steps
}

// hasExactLine IS ITSELF A GATE, so the shapes it must reject have their own test.
//
// It replaced a line-based matcher that three review rounds walked through in turn —
// a comment, `echo <gate>`, `<gate> || true`, a line continuation, `if ! <gate>;
// then :; fi`, an indented copy inside a function nothing calls. Equality rejects
// all of them by construction, which is the argument for it; this exists because
// each earlier version ALSO looked obviously sufficient, and the one change that
// reopened the function-body bypass was a trim added to make it "more robust".
func TestHasExactLineRejectsEveryShapeThatDoesNotRunTheCommand(t *testing.T) {
	t.Parallel()

	const want = `"$GATE" "$CFG"`

	for _, tc := range []struct {
		name   string
		script string
		found  bool
	}{
		{"the line itself", "set -e\n" + want + "\necho done", true},
		{"the only line", want, true},

		{"a comment naming it", "# " + want, false},
		{"echoed", "echo " + want, false},
		{"its status discarded", want + " || true", false},
		{"a reassuring echo", want + ` || echo "NOTHING MISSING"`, false},
		{"backgrounded", want + " &", false},
		// INDENTATION IS THE ONE THAT CAME BACK. A trim makes this pass, and an
		// indented copy is what a gate moved inside an uncalled function looks like.
		{"indented", "never_called() {\n  " + want + "\n}", false},
		{"continued onto another line", `"$GATE" \\` + "\n  " + `"$CFG"`, false},
		{"a different command with the same prefix", `"$GATE".disabled "$CFG"`, false},
		{"absent", "set -e\necho done", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := hasExactLine(tc.script, want); got != tc.found {
				t.Errorf("hasExactLine(%q) = %v, want %v", tc.script, got, tc.found)
			}
		})
	}
}

// hasExactLine reports whether some line of script IS want, byte for byte.
//
// NO INFERENCE AT ALL, which is the point. Every cleverer version of this accepted
// something that does not run the gate, or rejected something that does; an exact
// line cannot be `echo`ed, appended to with `|| true`, continued onto another line,
// or moved inside a function without ceasing to be that line.
//
// INDENTATION IS PART OF IT, and that is not fussiness: trimming it accepts the
// invocation moved into a function body AS THIS REPOSITORY WRITES ONE, which is the
// construct that makes a command unreachable while still looking like a call. Shell
// does not require that indentation and an unindented body would pass; what this
// rejects is the shape a real edit produces, which is the shape the mutation runs
// produced. Both call sites are at column zero — a workflow `run:` block arrives with
// its own indentation already stripped by yaml — so the comparison is equality.
func hasExactLine(script, want string) bool {
	for line := range strings.SplitSeq(script, "\n") {
		if line == want {
			return true
		}
	}

	return false
}

// substantiveLines is what a workflow run block actually executes, once comments and
// blank lines are removed.
//
// `set -euo pipefail` IS DROPPED because every block here begins with it and a test
// that counted it would be asserting a number rather than a property. A line that
// merely STARTS with it is kept, so `set -euo pipefail; exit 0` is visible.
func substantiveLines(run string) []string {
	var out []string

	for line := range strings.SplitSeq(withoutShellComments(run), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "set -euo pipefail" || trimmed == "set -eo pipefail" {
			continue
		}

		out = append(out, trimmed)
	}

	return out
}

// withoutShellComments drops whole comment lines, so an assertion about what a
// script RUNS is not satisfied by a comment describing what it used to run.
func withoutShellComments(script string) string {
	var kept []string

	for line := range strings.SplitSeq(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		kept = append(kept, line)
	}

	return strings.Join(kept, "\n")
}

// runKernelGate drives the real gate script and returns everything it printed.
//
// The environment is the seam: CHECK_CONFIG_SH and CHECK_CONFIG_SHA256 name the
// checker AND the digest it must have, so a stand-in exercises the digest step
// rather than skipping it. Passing nil uses the vendored copy and its pin.
func runKernelGate(t *testing.T, config string, env []string) (string, error) {
	t.Helper()

	gate, err := filepath.Abs("check-guest-kernel-config.sh")
	if err != nil {
		t.Fatalf("absolute gate path: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), gate, config)
	cmd.Env = append(os.Environ(), env...)

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// stubChecker writes a stand-in checker and returns the environment naming it and
// its digest.
func stubChecker(t *testing.T, body string) []string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "check-config.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write the stand-in checker: %v", err)
	}

	sum := sha256.Sum256([]byte(body))

	return []string{
		"CHECK_CONFIG_SH=" + path,
		"CHECK_CONFIG_SHA256=" + hex.EncodeToString(sum[:]),
	}
}

// passingChecker is a stand-in that grades nothing, for the cases about billet's
// own rules. Those run before the checker and must be reachable on any platform.
func passingChecker(t *testing.T) []string {
	t.Helper()

	return stubChecker(t, "#!/bin/sh\nexit 0\n")
}

func goodKernelConfig(t *testing.T) string {
	t.Helper()

	path, err := filepath.Abs("guest-kernel.config")
	if err != nil {
		t.Fatalf("absolute config path: %v", err)
	}

	return path
}

func readGoodKernelConfig(t *testing.T) string {
	t.Helper()

	return readFile(t, "guest-kernel.config")
}

func writeKernelConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kernel.config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config fixture: %v", err)
	}

	return path
}

func readFile(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(body)
}

func readWorkflow(t *testing.T) string {
	t.Helper()

	return readFile(t, filepath.Join("..", ".github", "workflows", "guest-image.yml"))
}

// A PASSING CHECKER THAT SAYS A LOT IS STILL A PASS, and this is the trap the gate
// exists about reappearing inside the gate's own diagnostic.
//
// MEASURED in ubuntu:24.04 under `set -euo pipefail`: `grep -Fi missing <2.9MB
// file> | head -30` exits **141** — head leaves after thirty lines, grep takes
// SIGPIPE, pipefail reports the writer's status, and set -e ends the script. The
// summary is capped at thirty, so a checker producing more than thirty "missing"
// lines is exactly the case; a real one produces about a dozen, which is why two
// noisy lines could never have found this.
func TestALoudPassingCheckerDoesNotFailTheGateThroughItsOwnSummary(t *testing.T) {
	t.Parallel()

	// FAR PAST THE CAP AND FAR PAST A PIPE BUFFER. Thirty-one lines fits in the 64KB
	// pipe buffer, so grep finishes writing before head leaves and never sees
	// SIGPIPE — the mutation survives and the test proves nothing.
	body := "#!/bin/sh\nawk 'BEGIN { for (i = 0; i < 200000; i++) print \"- CONFIG_X\" i \": missing\" }'\nexit 0\n"

	out, err := runKernelGate(t, goodKernelConfig(t), stubChecker(t, body))
	if err != nil {
		t.Fatalf("a passing checker failed the gate through the gate's own summary (%v); "+
			"the last thirty lines follow:\n%s", err, lastLines(out, 5))
	}

	if !strings.Contains(out, "30 shown") {
		t.Errorf("the summary does not say it was truncated:\n%s", lastLines(out, 5))
	}
}

// THE FILE THAT RAN IS THE FILE THAT WAS HASHED — the same path, not merely a
// different one from the source.
//
// Hashing a path and executing that path a moment later are two lookups of a name,
// and whatever replaces it in between is what actually runs while the digest vouches
// for something else. The gate copies the checker into its own private directory,
// hashes the copy and runs the copy, so the two questions are about one file.
//
// BOTH PATHS ARE OBSERVED, AND THE WEAKER TEST IS WHY. Asserting only that $0 is not
// the source path passes for a gate that hashes copy A and runs an unverified copy B
// — which reintroduces exactly the property the test claims. So a sha256sum shim on
// PATH records what was hashed, the stand-in records where it ran from, and the two
// must be the same file.
//
// ASSERTED BY WHAT HAPPENED, NOT BY A RACE. A test that swapped the file mid-flight
// would prove nothing on the runs where it lost.
func TestTheCheckerThatRunsIsTheFileThatWasHashed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "check-config.sh")
	ranFromAt := filepath.Join(dir, "ran-from")
	hashedAt := filepath.Join(dir, "hashed")

	// The stand-in records where it was executed from. `sh script` sets $0 to the
	// script it was handed, which needs no cooperation from anything else.
	body := "#!/bin/sh\nprintf '%s' \"$0\" > " + shellQuote(ranFromAt) + "\nexit 0\n"

	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write the stand-in checker: %v", err)
	}

	sum := sha256.Sum256([]byte(body))

	// AND THE SHIM RECORDS WHAT WAS HASHED, forwarding to the real tool so the gate
	// still gets a true digest and this case is about the PATH rather than about a
	// checksum that stopped matching.
	//
	// THE LAST ARGUMENT, because the gate spells the call two ways — `sha256sum FILE`
	// where coreutils is present and `shasum -a 256 FILE` where it is not — and the
	// file is last in both. Recording $1 works on one platform and records "-a" on
	// the other.
	//
	// AND THE REAL TOOL IS RESOLVED RATHER THAN ASSUMED: /usr/bin/shasum is a macOS
	// path and does not exist on the Linux this runs on in CI.
	shims := t.TempDir()

	for _, name := range []string{"sha256sum", "shasum"} {
		tool, lookErr := exec.LookPath(name)
		if lookErr != nil {
			continue
		}

		shim := "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\n" +
			"printf '%s' \"$last\" > " + shellQuote(hashedAt) +
			"\nexec " + shellQuote(tool) + " \"$@\"\n"

		if err := os.WriteFile(filepath.Join(shims, name), []byte(shim), 0o700); err != nil {
			t.Fatalf("write the %s shim: %v", name, err)
		}
	}

	if entries, readErr := os.ReadDir(shims); readErr != nil || len(entries) == 0 {
		t.Skip("no sha256sum or shasum on PATH to shim")
	}

	out, err := runKernelGate(t, goodKernelConfig(t), []string{
		"CHECK_CONFIG_SH=" + path,
		"CHECK_CONFIG_SHA256=" + hex.EncodeToString(sum[:]),
		"PATH=" + shims + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	if err != nil {
		t.Fatalf("the gate failed a passing stand-in (%v):\n%s", err, out)
	}

	ranFrom := readRecorded(t, ranFromAt, "the stand-in never ran", out)
	hashed := readRecorded(t, hashedAt, "nothing was hashed", out)

	if ranFrom != hashed {
		t.Errorf("the gate hashed %s and executed %s; whatever replaced the hashed path "+
			"in between would be vouched for without ever running", hashed, ranFrom)
	}

	// AND IT IS NOT THE SOURCE, which is what makes the equality above mean the gate
	// took a copy rather than doing both against the operator's own path.
	if ranFrom == path {
		t.Errorf("the gate hashed and executed %s directly rather than a copy of it, so "+
			"the two operations are still two lookups of a name", path)
	}
}

// readRecorded reads a marker a stand-in wrote, and says which one is missing.
func readRecorded(t *testing.T, path, missing, out string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s, so nothing was exercised (%v):\n%s", missing, err, out)
	}

	return string(body)
}

// lastLines keeps a failure message readable when the subject prints a great deal.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}
