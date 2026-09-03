package codebuild

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/runnerrelease"
)

// buildspecDoc is the shape billet emits, parsed back with a real YAML parser.
type buildspecDoc struct {
	Version any `yaml:"version"`
	Phases  map[string]struct {
		Commands []string `yaml:"commands"`
	} `yaml:"phases"`
}

// parseBuildspec parses what billet generated, which is the first half of the
// contract: a buildspec that fails to parse is a build that starts, registers
// nothing, and reports whatever CodeBuild reports.
func parseBuildspec(t *testing.T, body string) buildspecDoc {
	t.Helper()

	var doc buildspecDoc
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the generated buildspec is not valid YAML: %v\n%s", err, body)
	}

	if doc.Version == nil {
		t.Fatalf("the buildspec declares no version, which CodeBuild refuses:\n%s", body)
	}

	if len(doc.Phases) == 0 {
		t.Fatalf("the buildspec declares no phases:\n%s", body)
	}

	return doc
}

// THE GENERATED BUILDSPEC IS EXECUTED, NOT PATTERN-MATCHED.
//
// This is the ec2 boot-script lesson applied to a YAML document whose commands are
// shell. Its first version carried the registration in a quoted heredoc inside
// `$( )`, which reads safer than a plain assignment and is not: a single quote in
// the value confused the shell scanning for the closing paren and /bin/sh died with
// "unexpected EOF" on a later line. Compute that starts and registers nothing
// reports success, so nothing but running it can catch that.
//
// THE RUNNER COMMAND IS REPLACED BY ONE THAT PRINTS WHAT IT WAS GIVEN, so the
// assertion is about the argv the runner would have received rather than about a
// substring of the document.
func TestTheGeneratedBuildspecRunsAndHandsTheRunnerItsArgv(t *testing.T) {
	for name, tc := range map[string]struct {
		env     config.CodeBuildEnvironment
		command []string
		want    string
	}{
		"linux container": {
			env:     config.CodeBuildLinuxContainer,
			command: []string{"./run.sh"},
			want:    "./run.sh",
		},
		"arm container": {
			env:     config.CodeBuildARMContainer,
			command: []string{"./run.sh", "--once"},
			want:    "./run.sh|--once",
		},
		"macos": {
			env:     config.CodeBuildMacARM,
			command: []string{"./actions-runner/run.sh"},
			want:    "./actions-runner/run.sh",
		},
		// A SPACE INSIDE ONE ARGUMENT is where naive quoting breaks, and the
		// delimiter is what makes the break visible: without it, three arguments
		// render identically to two.
		"argument with spaces": {
			env:     config.CodeBuildLinuxContainer,
			command: []string{"./run.sh", "--label", "a b"},
			want:    "./run.sh|--label|a b",
		},
		// And YAML metacharacters, which is what makes yamlScalar necessary: a plain
		// scalar starting with `-`, or containing `: `, `#` or `{`, parses as
		// something else entirely or fails to parse at all.
		"yaml metacharacters": {
			env:     config.CodeBuildLinuxContainer,
			command: []string{"./run.sh", "--x", "a: b", "#c", "{d}"},
			want:    "./run.sh|--x|a: b|#c|{d}",
		},
		// A glob and a variable reference must reach the runner UNEXPANDED: a tier
		// command is argv, not a shell expression, and a backend that let the shell
		// interpret it would hand the runner whatever happened to be on disk.
		"no expansion": {
			env:     config.CodeBuildLinuxContainer,
			command: []string{"./run.sh", "*", "$HOME", "`id`"},
			want:    "./run.sh|*|$HOME|`id`",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := providerFor(t, tc.env)

			body, err := p.Buildspec(Spec{Name: "billet-abc", Command: tc.command})
			if err != nil {
				t.Fatalf("Buildspec: %v", err)
			}

			doc := parseBuildspec(t, body)

			got := runBuildspec(t, doc)
			if got != tc.want {
				t.Errorf("the runner would have been invoked as %q, want %q\n%s", got, tc.want, body)
			}
		})
	}
}

// providerFor builds a provider for one environment, with a fleet where the
// environment needs one so config validation is satisfied.
func providerFor(t *testing.T, env config.CodeBuildEnvironment) *Provider {
	t.Helper()

	cfg := testConfig()
	cfg.EnvironmentType = env

	if !env.Container() {
		cfg.PrivilegedMode = false
	}

	if env.ReservedOnly() {
		cfg.FleetARN = "arn:aws:codebuild:us-west-2:000000000000:fleet/f"
	}

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

// runBuildspec executes the buildspec's phases under /bin/sh and returns the argv
// the runner would have been given.
//
// THE FETCH IS SHORT-CIRCUITED BY A PREINSTALLED RUNNER, deliberately. This test is
// about whether the document parses and whether the commands are well-formed shell —
// not about downloading a 200MB tarball from GitHub. The fetch path's own shape is
// checked separately below, and the early-exit branch it takes here is itself one of
// the branches worth exercising: a curated image that already has a runner must not
// pay a download per job.
func runBuildspec(t *testing.T, doc buildspecDoc) string {
	t.Helper()

	// THE STAND-IN IS CREATED ONCE FOR THE WHOLE PACKAGE, and that is a measured
	// decision rather than tidiness: on macOS, writing a NEW executable and running
	// it costs several seconds while the platform inspects it, and doing that per
	// subtest took this one test from milliseconds to 26 seconds. A slow test is a
	// test people stop running.
	//
	// THE OBSERVATION STAYS PER-TEST. Only the stand-in is shared; each run writes
	// its argv to its own file, named by an environment variable, so two subtests
	// cannot read each other's answer.
	runnerHome := sharedRunnerHome(t)

	dir := t.TempDir()
	argvOut := filepath.Join(dir, "argv")

	var body strings.Builder

	// `set -e` so a failing command ends the script, which is how CodeBuild treats a
	// phase. NOT relied on for the buildspec's own gates — see fetchRunnerScript —
	// but correct for the harness.
	body.WriteString("set -e\n")

	// BILLET_RUNNER_IMAGE_DIR is overridden AFTER the buildspec's own assignment, so
	// the script takes its "a runner is already in this image" branch and never
	// reaches the network. The real value is an absolute path outside a temp dir.
	//
	// THE IMAGE PATH RATHER THAN THE DOWNLOAD PATH, and pointing at the wrong one is
	// not a silent mistake: it sends the suite to github.com to fetch a real runner
	// per case, which is how this test went from a second to three minutes when the
	// download directory moved under $HOME. A unit suite that reaches the network is
	// one that fails on somebody else's outage.
	phases := []string{buildspecPhase, "build"}

	for _, phase := range phases {
		for _, cmd := range doc.Phases[phase].Commands {
			body.WriteString(cmd + "\n")

			if strings.Contains(cmd, "BILLET_RUNNER_IMAGE_DIR=") {
				body.WriteString("BILLET_RUNNER_IMAGE_DIR=" +
					shellSingleQuote(runnerHome) + "\n")
			}
		}
	}

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", body.String())
	cmd.Env = append(os.Environ(), "BILLET_ARGV_OUT="+argvOut)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated buildspec failed to run: %v\n--- script ---\n%s\n--- output ---\n%s",
			err, body.String(), out)
	}

	argv, err := os.ReadFile(argvOut)
	if err != nil {
		t.Fatalf("the runner was never invoked (%v); output was:\n%s\nscript:\n%s",
			err, out, body.String())
	}

	// THE RUNNER'S OWN ENVIRONMENT, read back from the process billet started. This is
	// the half a text assertion cannot reach: `export` present, the assignment ahead of
	// the command, and in the same shell.
	rootOK, err := os.ReadFile(argvOut + ".rootok")
	if err != nil {
		t.Fatalf("the stand-in recorded no environment (%v); script:\n%s", err, body.String())
	}

	if string(rootOK) != "1" {
		t.Errorf("the runner was invoked with RUNNER_ALLOW_RUNASROOT=%q, want \"1\"; on a "+
			"container environment GitHub's run.sh exits before registering and CodeBuild "+
			"still reports the build succeeded\n%s", rootOK, body.String())
	}

	return string(argv)
}

func shellSingleQuote(v string) string { return "'" + v + "'" }

// standInRunner prints its own argv, ONE ARGUMENT PER FIELD.
//
// THE DELIMITER IS THE ASSERTION. `"$*"` joins with a space, which collapses
// `--label 'a b'` (two arguments) and `--label a b` (three) into one string — so a
// test asserting the joined form passes even when the quoting that keeps them apart
// has broken, which is the exact property shellQuote exists to provide. A separator
// no argument contains makes the boundaries observable.
//
// `$0` is what gives the command's own name, and it needs no cooperation from an
// option parser — the rule the firecracker stand-in records.
//
// IT ALSO RECORDS RUNNER_ALLOW_RUNASROOT FROM ITS OWN ENVIRONMENT, which is the only
// way to prove billet EXPORTED it rather than merely writing the characters somewhere
// in the build phase. A text assertion passes with the `export` removed, with the
// assignment moved after the runner command, and against the string appearing in an
// unrelated command — three ways to emit a buildspec that leaves the real run.sh
// exiting on its root guard while the build reports SUCCEEDED.
const standInRunner = "#!/bin/sh\n" +
	"out=\"$0\"\n" +
	"for a in \"$@\"; do out=\"$out|$a\"; done\n" +
	"printf '%s' \"$out\" > \"$BILLET_ARGV_OUT\"\n" +
	"printf '%s' \"$RUNNER_ALLOW_RUNASROOT\" > \"$BILLET_ARGV_OUT.rootok\"\n"

var (
	sharedRunnerOnce sync.Once
	sharedRunnerDir  string
	sharedRunnerErr  error
)

// TestMain owns the stand-in runner's lifetime, because the directory is PACKAGE
// scoped and nothing narrower can hold it.
//
// A t.TempDir belongs to the test that asked for it and is removed when that test
// ends, which would pull the runner out from under every later case — and so would a
// t.Cleanup registered by whichever test happened to be first. The directory is shared
// deliberately: macOS inspects each newly written executable, and writing the stand-in
// per case took this suite from 1.2s to 26s.
func TestMain(m *testing.M) {
	code := m.Run()

	if sharedRunnerDir != "" {
		_ = os.RemoveAll(sharedRunnerDir)
	}

	os.Exit(code)
}

// sharedRunnerHome is a directory holding the stand-in runner at both paths a tier
// command might name, created once per package run.
func sharedRunnerHome(t *testing.T) string {
	t.Helper()

	sharedRunnerOnce.Do(func() { sharedRunnerDir, sharedRunnerErr = buildSharedRunner() })

	if sharedRunnerErr != nil {
		t.Fatalf("create the stand-in runner: %v", sharedRunnerErr)
	}

	return sharedRunnerDir
}

// buildSharedRunner writes the stand-in at both paths a tier command might name.
//
// IT TAKES NO *testing.T on purpose: it is package-scoped work whose result outlives
// every individual test, so there is no test whose failure or cleanup it belongs to.
// TestMain removes what it created.
func buildSharedRunner() (string, error) {
	dir, err := os.MkdirTemp(os.TempDir(), "billet-codebuild-runner")
	if err != nil {
		return "", err
	}

	for _, path := range []string{
		filepath.Join(dir, "run.sh"),
		filepath.Join(dir, "actions-runner", "run.sh"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return dir, err
		}

		if err := os.WriteFile(path, []byte(standInRunner), 0o700); err != nil {
			return dir, err
		}
	}

	return dir, nil
}

// THE RUNNER IS ALLOWED TO RUN AS ROOT, AND WITHOUT THAT THIS BACKEND RUNS NOTHING.
//
// MEASURED ON A REAL JOB, which is the only place it could have been. A CodeBuild
// container runs as root, and GitHub's `run.sh` carries a guard that exits immediately
// when root invokes it without RUNNER_ALLOW_RUNASROOT — the build downloaded the runner,
// VERIFIED its checksum, printed `Must not run interactively with sudo` / `Exiting
// runner...`, and CodeBuild then reported the build SUCCEEDED because the script exited
// zero. Compute that starts, registers nothing and reports success is the exact failure
// the ec2 boot script's comment warns about, one backend over.
//
// THE EXECUTED-BUILDSPEC SUITE COULD NOT SEE IT and this test does not pretend to: the
// harness substitutes a STAND-IN runner, and a stand-in has no root guard. What is
// asserted here is that billet EMITS the variable, in the phase that runs the command,
// which is the half billet owns. The other half belongs to a binary billet does not own
// and is pinned by the acceptance run in docs/aws-acceptance.md.
func TestTheBuildspecAllowsTheRunnerToRunAsRoot(t *testing.T) {
	for name, env := range map[string]config.CodeBuildEnvironment{
		"linux container": config.CodeBuildLinuxContainer,
		"arm container":   config.CodeBuildARMContainer,
		"macos":           config.CodeBuildMacARM,
	} {
		t.Run(name, func(t *testing.T) {
			p := providerFor(t, env)

			spec, err := p.Buildspec(Spec{Name: "billet-abc", Command: []string{"./run.sh"}})
			if err != nil {
				t.Fatalf("Buildspec: %v", err)
			}

			if !strings.Contains(spec, "RUNNER_ALLOW_RUNASROOT=1") {
				t.Errorf("the buildspec does not allow the runner to start as root; on a "+
					"container environment GitHub's run.sh exits before registering and the "+
					"build still reports success:\n%s", spec)
			}

			// IN THE BUILD PHASE, BESIDE THE COMMAND IT GUARDS — and the reason is NOT
			// that a variable cannot cross phases, which is what this comment used to
			// say and which billet's own production code contradicts: pre_build exports
			// BILLET_RUNNER_DIR and build reads it, and a real job confirms that works.
			// A comment whose stated reason is untrue is worse than no comment, and this
			// one sat a few lines from the code that disproves it.
			//
			// The real reason is that the export and the exec are then in one shell
			// LOCALLY — they are now literally one `&&` list — rather than resting on a
			// cross-phase behaviour billet has observed once and does not own.
			build, _, found := strings.Cut(spec, "  build:")
			if !found {
				t.Fatalf("the buildspec has no build phase:\n%s", spec)
			}

			if strings.Contains(build, "RUNNER_ALLOW_RUNASROOT") {
				t.Error("the variable is exported before the build phase, where it will not " +
					"survive into the shell that execs the runner")
			}
		})
	}
}

// THE FETCH PATH IS SHELL TOO, so it is parsed by a shell rather than read.
//
// `sh -n` is a syntax check that runs nothing, which is the right instrument here:
// the commands download a tarball and verify a checksum, so executing them would
// make the unit suite depend on GitHub — but a construct that does not parse is
// exactly the failure this whole approach exists to catch, and it is checkable
// without running anything.
func TestTheRunnerFetchIsWellFormedShell(t *testing.T) {
	for name, env := range map[string]config.CodeBuildEnvironment{
		"linux": config.CodeBuildLinuxContainer,
		"arm":   config.CodeBuildARMContainer,
		"macos": config.CodeBuildMacARM,
	} {
		t.Run(name, func(t *testing.T) {
			p := providerFor(t, env)

			body, err := p.Buildspec(Spec{Name: "billet-abc", Command: []string{"./run.sh"}})
			if err != nil {
				t.Fatalf("Buildspec: %v", err)
			}

			doc := parseBuildspec(t, body)

			script := "set -e\n" + strings.Join(doc.Phases[buildspecPhase].Commands, "\n") + "\n"

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-n")
			cmd.Stdin = strings.NewReader(script)

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("the fetch commands are not valid shell: %v\n%s\n--- script ---\n%s",
					err, out, script)
			}
		})
	}
}

// A TARBALL THAT DOES NOT MATCH THE PINNED CHECKSUM IS NEVER INSTALLED, AND THE
// GATE DOES NOT DEPEND ON `set -e`.
//
// THE FETCH IS EXECUTED HERE, and until this test nothing executed it at all — which is
// why the previous version of that path could not have failed anything. It goes through
// the shared helper, which it did not when it was written: assembling the script by hand
// is what let its image-directory override land before the assignment it was overriding,
// so it reached the fetch branch only by accident. The steps were separated
// by `;`, so — MEASURED under dash in ubuntu:24.04, not reasoned about — a failing
// `sha256sum -c -` printed `FAILED`, execution carried on into `tar -xzf`, the
// tarball extracted, and the command exited ZERO. The `test -x` gate behind it then
// passed, because a tarball that extracted has a run.sh in it. A mirror serving a
// runner billet had not pinned would have been installed and run, with the
// verification visible in the log and doing nothing.
//
// THE HARNESS DELIBERATELY DOES NOT `set -e`. That is the property under test: the
// suite's other executions add it (correctly, since CodeBuild fails a phase on a
// failing command), and adding it here would hide exactly the defect this exists to
// catch — which is what the whole-buildspec harness does, and why it saw nothing.
//
// NO NETWORK. A stand-in `curl` earlier on PATH writes a REAL gzipped tar carrying a
// run.sh, so `tar` succeeds and the only thing wrong with it is its checksum. That
// models a compromised mirror rather than an outage, and an outage is already caught
// by the trailing `test -x`.
func TestAnUnverifiedRunnerTarballIsNeverInstalled(t *testing.T) {
	home := canonicalTempDir(t)
	fakeBin := filepath.Join(home, "bin")

	if err := os.MkdirAll(fakeBin, 0o750); err != nil {
		t.Fatalf("create the stand-in bin: %v", err)
	}

	// The payload is a valid .tar.gz containing an executable run.sh — the archive a
	// mirror would serve. Built with the real tar so nothing here depends on this
	// test's idea of the format.
	payload := filepath.Join(home, "payload")

	if err := os.MkdirAll(payload, 0o750); err != nil {
		t.Fatalf("create the payload dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(payload, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write the payload runner: %v", err)
	}

	tarball := filepath.Join(home, "payload.tar.gz")

	pack := exec.CommandContext(t.Context(), "tar", "-czf", tarball, "-C", payload, "run.sh")
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("pack the payload: %v\n%s", err, out)
	}

	curl := "#!/bin/sh\n" +
		"# Stand-in curl: ignore every flag, write the payload to the -o target.\n" +
		"out=\"\"\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\n" +
		"cp " + shellSingleQuote(tarball) + " \"$out\"\n"

	if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(curl), 0o700); err != nil {
		t.Fatalf("write the stand-in curl: %v", err)
	}

	// THROUGH THE SHARED HELPER, which places the override AFTER billet's own
	// assignment. Assembled by hand here, the generated assignment reset it and this
	// test reached the fetch branch only because this machine happens to have nothing
	// executable at /opt/billet/actions-runner.
	outStr, runErr := runFetch(t, fetchScript(t, home), home, fakeBin)
	out := []byte(outStr)

	// The refusal has to be the SCRIPT's, not a missing tool's: a machine with no
	// sha256sum and no shasum takes the "refusing to install an unverified runner"
	// branch, which is also a non-zero exit and would satisfy a bare error assertion
	// while proving nothing about the comparison.
	if strings.Contains(outStr, "no sha256 tool") {
		t.Skip("this machine has neither sha256sum nor shasum, so the comparison never ran")
	}

	if runErr == nil {
		t.Errorf("the fetch installed a tarball that does not match the pinned checksum "+
			"and exited zero:\n%s", out)
	}

	if installed := filepath.Join(home, ".billet", "actions-runner", "run.sh"); !gone(t, installed) {
		t.Errorf("an unverified runner was left at %s", installed)
	}

	// And the comparison genuinely ran, so the failure above is the checksum's and
	// not some earlier step's.
	if !strings.Contains(outStr, "FAILED") {
		t.Errorf("no checksum comparison reported a mismatch; the script failed somewhere "+
			"else and this test would pass against a missing gate:\n%s", out)
	}
}

// fetchScript renders the pre_build commands with the image branch pointed at a path
// that does not exist, so an execution takes the FETCH branch.
//
// THE WHOLE-BUILDSPEC HARNESS TAKES THE OTHER BRANCH — it substitutes a stand-in
// runner at BILLET_RUNNER_IMAGE_DIR — which is correct for what it asserts and is why
// the fetch went unexecuted long enough to ship a checksum gate that did not gate.
// Every test that reaches the fetch goes through here.
func fetchScript(t *testing.T, home string) string {
	t.Helper()

	return fetchScriptVerifying(t, home, "")
}

// fetchScriptVerifying is fetchScript with the pinned checksum replaced, so a test can
// make the fetch SUCCEED against a tarball it built itself.
//
// THE OVERRIDE IS WRITTEN AFTER BILLET'S OWN ASSIGNMENT, exactly as the whole-buildspec
// harness overrides BILLET_RUNNER_IMAGE_DIR — the script is left alone and the
// environment it reads is what changes. Without this seam the only executable fetch is
// a FAILING one, and "what does a successful fetch leave behind" is a question no test
// could ask.
func fetchScriptVerifying(t *testing.T, home, sum string) string {
	t.Helper()

	p := providerFor(t, config.CodeBuildLinuxContainer)

	body, err := p.Buildspec(Spec{Name: "billet-abc", Command: []string{"./run.sh"}})
	if err != nil {
		t.Fatalf("Buildspec: %v", err)
	}

	doc := parseBuildspec(t, body)

	var b strings.Builder

	// EVERY OVERRIDE GOES AFTER THE ASSIGNMENT IT OVERRIDES, and putting the image
	// directory BEFORE was a defect that made three tests pass for a reason none of
	// them names. The generated `BILLET_RUNNER_IMAGE_DIR='/opt/billet/actions-runner'`
	// simply reset it, so the fetch branch was reached only because this machine has no
	// executable at that real absolute path — on a host that does, every one of those
	// tests would run the IMAGE branch and fail with something unrelated.
	overrides := map[string]string{
		"BILLET_RUNNER_IMAGE_DIR=": filepath.Join(home, "absent"),
	}

	if sum != "" {
		overrides["BILLET_RUNNER_SHA256="] = sum
	}

	seen := map[string]int{}

	for _, cmd := range doc.Phases[buildspecPhase].Commands {
		b.WriteString(cmd + "\n")

		for prefix, value := range overrides {
			// COUNTED OVER THE WHOLE COMMAND, not once per command that starts with
			// the prefix: `X='good'; X='bad'` is one command carrying two assignments,
			// the effective value is the last, and a per-command count reads it as one.
			if n := strings.Count(cmd, prefix); n > 0 {
				seen[prefix] += n

				b.WriteString(prefix + shellSingleQuote(value) + "\n")
			}
		}
	}

	// EXACTLY ONE EACH, because a seam that overrides "at least one" lies in the
	// direction that matters: a production buildspec emitting a value twice — the
	// second one wrong — would use the wrong one and fail every real build, while this
	// helper overrode both and every test here passed.
	for prefix := range overrides {
		if seen[prefix] != 1 {
			t.Fatalf("the buildspec makes %d %s assignments, want exactly 1; more than one "+
				"means the effective value is the last, and this seam would hide that",
				seen[prefix], strings.TrimSuffix(prefix, "="))
		}
	}

	return b.String()
}

// runFetch executes the fetch branch WITHOUT `set -e`, with a stand-in bin first on
// PATH, and returns its combined output and error.
//
// THE ABSENT `set -e` IS THE POINT. CodeBuild fails a phase on a failing command, so
// the other harnesses add it and are right to — but every gate here has to hold on its
// own, and a harness that supplies the very thing the gate must not depend on is a
// harness that cannot see the defect. That is not hypothetical: it is exactly how the
// `;`-chained checksum survived.
func runFetch(t *testing.T, script, home, fakeBin string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "/bin/sh")
	cmd.Stdin = strings.NewReader(script)
	// THE WORKING DIRECTORY IS THE TEST'S OWN, so a RELATIVE home resolves under a
	// directory the test controls rather than under the package source.
	cmd.Dir = filepath.Dir(fakeBin)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// standInRecorders replaces every external command the fetch can reach with one that
// records itself and then FAILS.
//
// EVERY DESTRUCTIVE COMMAND THE SCRIPT INVOKES, which is a narrower claim than "the
// PATH is closed" and is the one that is true: the real PATH is still behind this
// directory, so `sha256sum` and `shasum` remain reachable — deliberately, since they
// only read. The first version stubbed `rm`, `mkdir` and `curl` and claimed to cover
// everything; `tar` was not stubbed, and `tar` is the one that writes into the
// directory. The failure needs the guard to regress AND a correctly-named tarball to
// be present, which is contrived; it is closed anyway because this test's whole
// justification is that it cannot damage the machine, and "cannot" has to mean cannot.
//
// AND EACH ONE EXITS NON-ZERO after recording, so the script stops at the first
// destructive call rather than proceeding to the next. The assertion callers make is
// on the RECORD — a non-zero exit from a recorder is not evidence the guard worked.
func standInRecorders(t *testing.T, dir, log string) {
	t.Helper()

	for _, name := range []string{"rm", "mkdir", "curl", "tar"} {
		standInBin(t, dir, name,
			"echo "+name+" >> "+shellSingleQuote(log)+"\nexit 1\n")
	}
}

// canonicalTempDir is t.TempDir() with every symlink resolved.
//
// ON macOS t.TempDir() SITS UNDER `/var`, WHICH IS A SYMLINK TO `/private/var`. The
// fetch script refuses a HOME that is not its own canonical path, so a raw t.TempDir()
// is refused for THAT reason on this platform — which silently masked the case a test
// was actually about, and would have made these tests pass on Linux and fail on a Mac
// for a reason unrelated to any of their names.
func canonicalTempDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalise the temp dir: %v", err)
	}

	return dir
}

// payloadTarball builds a real .tar.gz carrying an executable run.sh and returns its
// path and sha256 — the archive a mirror would serve, so a fetch can SUCCEED offline.
func payloadTarball(t *testing.T, dir string) (string, string) {
	t.Helper()

	payload := filepath.Join(dir, "payload")
	if err := os.MkdirAll(payload, 0o750); err != nil {
		t.Fatalf("create the payload dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(payload, "run.sh"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write the payload runner: %v", err)
	}

	tarball := filepath.Join(dir, "payload.tar.gz")

	pack := exec.CommandContext(t.Context(), "tar", "-czf", tarball, "-C", payload, "run.sh")
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("pack the payload: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatalf("read the payload: %v", err)
	}

	return tarball, fmt.Sprintf("%x", sha256.Sum256(raw))
}

// gone reports whether a path is absent, distinguishing that from being unable to look.
//
// A NON-NIL ERROR FROM Lstat IS NOT "IT WAS REMOVED": a permission error, an I/O error
// and a path that is genuinely gone all return one, and folding them together is the
// could-not-tell/no collapse this repository keeps taking out of its credential paths.
func gone(t *testing.T, path string) bool {
	t.Helper()

	// LSTAT, NOT STAT: a dangling symlink at the path is something that is THERE, and
	// Stat reports it as absent because it follows the link to nothing.
	_, err := os.Lstat(path)
	if err == nil {
		return false
	}

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("could not tell whether %s is gone: %v", path, err)
	}

	return true
}

// standInBin writes an executable of the given name that runs the given shell body.
func standInBin(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create the stand-in bin: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write the stand-in %s: %v", name, err)
	}
}

// AN UNSET $HOME IS REFUSED, RATHER THAN RESOLVED INTO A ROOT-LEVEL DIRECTORY.
//
// `"$HOME/.billet/actions-runner"` with HOME empty is `/.billet/actions-runner`. A
// root Linux container CREATES that happily, so the mistake ships and works, and the
// first environment to notice is a non-root one — where it surfaces as a permission
// error naming a path that appears in no configuration and no document. That is the
// same shape as the defect that cost the macOS acceptance run, one variable over.
func TestTheRunnerFetchRefusesAnUnsetHOME(t *testing.T) {
	home := canonicalTempDir(t)
	fakeBin := filepath.Join(home, "bin")
	reached := filepath.Join(home, "reached")

	// EVERY DESTRUCTIVE TOOL IS A RECORDING STAND-IN, for two reasons and the first is
	// the test's own safety. With HOME absent, the path this script would delete is
	// `/.billet/actions-runner` — so a version of this test that let the real `rm` run
	// would, the moment somebody weakened the guard, DELETE THAT PATH on the host, as
	// root in CI. A test whose failure mode is damaging the machine is worse than no
	// test.
	//
	// The second is that it makes the assertion exact. A stand-in that records its own
	// invocation turns "the script exited non-zero" — which a later checksum failure
	// satisfies just as well — into "the guard exited BEFORE anything was touched",
	// which is the actual property. Without this the guard could keep its message and
	// lose its `exit 1` and this test would still pass.
	standInRecorders(t, fakeBin, reached)

	script := fetchScript(t, home)

	cmd := exec.CommandContext(t.Context(), "/bin/sh")
	cmd.Stdin = strings.NewReader(script)
	// HOME is REMOVED rather than set empty, which is the harder of the two: a shell
	// expands an unset and an empty variable identically, so this covers both.
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "HOME=") })

	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Errorf("the fetch ran with no HOME and exited zero:\n%s", out)
	}

	if !strings.Contains(string(out), "HOME is unset") {
		t.Errorf("the refusal does not say what is wrong, so an operator meets a bare "+
			"filesystem error about a path nothing configured:\n%s", out)
	}

	if !gone(t, reached) {
		got, _ := os.ReadFile(reached) //nolint:errcheck // gone() proved it is readable
		t.Errorf("the guard did not exit: the script went on to run %s",
			strings.Join(strings.Fields(string(got)), ", "))
	}
}

// AND IT REFUSES A HOME IT CANNOT VOUCH FOR — one that is relative, that resolves to
// the root, that cannot be entered, or whose `.billet` is a symlink.
//
// A SYMLINKED HOME IS NOT ON THAT LIST ANY MORE and deliberately so: billet resolves it
// and works from the result, which is what TestASymlinkedHomeIsResolvedRatherThanRefused
// covers. Refusing it was a build that never starts a runner on an image whose only sin
// is an ordinary layout choice.
//
// NEITHER IS COVERED BY "NOT EMPTY", which is what the first version of this guard
// checked and called proof. A relative HOME resolves against whatever CodeBuild's
// working directory happens to be; a symlinked `.billet` — plantable by an earlier
// build, since a reserved host is measurably not scrubbed between them — redirects the
// recursive delete to whatever it points at. Both end in billet running `rm -rf` over
// a directory somebody else chose.
//
// THE STAND-INS ARE WHAT MAKE THIS SAFE TO ASSERT AT ALL: the symlink case points at a
// directory holding a canary, and if the guard ever stops refusing, the recorder says
// so without the canary having been touched.
func TestTheRunnerFetchRefusesAHomeItCannotVouchFor(t *testing.T) {
	for name, tc := range map[string]struct {
		home func(t *testing.T, root string) string
		want string
		// canary, when set, is a path under root that a successful delete would have
		// destroyed. Empty means this case has nothing a delete could reach.
		canary string
	}{
		// A HOME THAT CANNOT BE ENTERED. billet cannot resolve what it cannot reach,
		// and "could not tell" is not "safe to delete under".
		"unenterable": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				return filepath.Join(root, "does-not-exist")
			},
			want: "cannot be entered",
		},
		// A RELATIVE HOME THAT EXISTS, which is the only version of this case that says
		// anything. The first one used a path that did NOT exist, so `cd` failed and the
		// refusal came from the resolution — the test passed while the rule it named was
		// absent, and a relative home naming a real directory was accepted and resolved
		// against whatever working directory the build happened to have.
		"relative": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				if err := os.MkdirAll(filepath.Join(root, "rel", "home"), 0o750); err != nil {
					t.Fatalf("create the relative home: %v", err)
				}

				return "rel/home"
			},
			want: "not an absolute path",
		},
		// A NEWLINE IN THE NAME, in the two positions that behave differently. MEASURED
		// in ubuntu:24.04: a TRAILING one is stripped by command substitution, so
		// `cd "$HOME" && pwd -P` yields a DIFFERENT pathname that carries no newline,
		// passes every later check, and is what would be recursively deleted — which is
		// why the guard has to look at $HOME and not only at what it resolved to.
		"a trailing newline": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				// NOT filepath.Join: the name deliberately contains a byte gocritic reads as a
				// path separator, and concatenation is what expresses "this exact name".
				h := root + "/home\n"
				if err := os.MkdirAll(h, 0o750); err != nil {
					t.Fatalf("create the home: %v", err)
				}

				return h
			},
			want: "contains a newline",
		},
		"a newline in the middle": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				h := root + "/ho\nme"
				if err := os.MkdirAll(h, 0o750); err != nil {
					t.Fatalf("create the home: %v", err)
				}

				return h
			},
			want: "contains a newline",
		},
		// A NEWLINE THE SYMLINK BRINGS IN, which is the only case the check AFTER
		// resolution earns its place for: $HOME itself is a clean name here, so the
		// guard in front of the resolution sees nothing wrong, and the newline appears
		// only in what `pwd -P` returns. Without this case that second check is
		// redundant with the first and mutating it away survives.
		"a newline via the symlink target": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				target := root + "/tar\nget"
				if err := os.MkdirAll(target, 0o750); err != nil {
					t.Fatalf("create the redirect target: %v", err)
				}

				link := filepath.Join(root, "clean-home")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("plant the symlink: %v", err)
				}

				return link
			},
			want: "contains a newline",
		},
		// A SYMLINK TARGET WHOSE NAME ENDS IN A NEWLINE, which is the case that got past
		// BOTH newline checks and is why `pwd -P` now runs behind a sentinel.
		//
		// MEASURED in ubuntu:24.04: $HOME is a clean name so the first guard sees
		// nothing, and `$( )` strips the pathname's own trailing newline along with
		// `pwd`'s, so the second guard sees nothing either — leaving BILLET_HOME as a
		// DIFFERENT directory that exists. The canary sits under that stripped name,
		// because it is the directory a successful delete would have reached.
		"a symlink target ending in a newline": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				// Both names: the real target, and the one stripping the newline yields.
				for _, d := range []string{root + "/target\n", root + "/target"} {
					if err := os.MkdirAll(d, 0o750); err != nil {
						t.Fatalf("create %q: %v", d, err)
					}
				}

				if err := os.MkdirAll(root+"/target/.billet/actions-runner", 0o750); err != nil {
					t.Fatalf("create the canary's directory: %v", err)
				}

				if err := os.WriteFile(root+"/target/.billet/actions-runner/canary",
					[]byte("x"), 0o600); err != nil {
					t.Fatalf("write the canary: %v", err)
				}

				link := filepath.Join(root, "clean-name")
				if err := os.Symlink(root+"/target\n", link); err != nil {
					t.Fatalf("plant the symlink: %v", err)
				}

				return link
			},
			want:   "contains a newline",
			canary: "target/.billet/actions-runner/canary",
		},
		// AND A BACKSLASH, because GNU coreutils ESCAPES such a path in a checksum line
		// and rejects an unescaped one — MEASURED: `sha256sum -c` answers "no properly
		// formatted checksum lines found". So it is the verification that breaks, and
		// refusing here names HOME rather than leaving a checksum error naming nothing
		// an operator configured. (The rationale used to be `echo` rewriting the byte;
		// that stopped being true when the line moved to `printf`.)
		"a backslash": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				h := root + "/ho\\me"
				if err := os.MkdirAll(h, 0o750); err != nil {
					t.Fatalf("create the home: %v", err)
				}

				return h
			},
			want: "contains a backslash",
		},
		// THE ROOT DIRECTORY IS THE ONE ALIAS RESOLUTION DOES NOT CATCH: `pwd -P` in
		// `/` answers `/`, so it has to be named.
		"the root directory": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				return "/"
			},
			want: "the root directory",
		},
		// AND ITS ALIASES, which is the whole reason resolution replaced lexical rules:
		// `//` and `/.` are spellings of the root that no pattern list catches.
		"the root by another spelling": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				return "//"
			},
			want: "the root directory",
		},
		"the root as /.": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				return "/."
			},
			want: "the root directory",
		},
		// THE ONE SYMLINK POSITION RESOLUTION DOES NOT COVER, because it is a path
		// UNDER a home that has already been resolved. THE CANARY IS INSIDE THE
		// DIRECTORY THE DELETE WOULD REACH — `<target>/actions-runner` — because a
		// canary anywhere else survives a successful `rm -rf` and proves nothing.
		"symlinked .billet": {
			home: func(t *testing.T, root string) string {
				t.Helper()

				target := filepath.Join(root, "elsewhere")
				if err := os.MkdirAll(filepath.Join(target, "actions-runner"), 0o750); err != nil {
					t.Fatalf("create the redirect target: %v", err)
				}

				if err := os.WriteFile(filepath.Join(target, "actions-runner", "canary"),
					[]byte("x"), 0o600); err != nil {
					t.Fatalf("write the canary: %v", err)
				}

				h := filepath.Join(root, "home")
				if err := os.MkdirAll(h, 0o750); err != nil {
					t.Fatalf("create the home: %v", err)
				}

				if err := os.Symlink(target, filepath.Join(h, ".billet")); err != nil {
					t.Fatalf("plant the symlink: %v", err)
				}

				return h
			},
			want:   "is a symlink",
			canary: filepath.Join("elsewhere", "actions-runner", "canary"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := canonicalTempDir(t)
			fakeBin := filepath.Join(root, "bin")
			reached := filepath.Join(root, "reached")

			standInRecorders(t, fakeBin, reached)

			out, err := runFetch(t, fetchScript(t, root), tc.home(t, root), fakeBin)

			if err == nil {
				t.Errorf("the fetch accepted a HOME it cannot vouch for and exited zero:\n%s", out)
			}

			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not name the problem (%q):\n%s", tc.want, out)
			}

			if !gone(t, reached) {
				got, _ := os.ReadFile(reached) //nolint:errcheck // gone() proved it is readable
				t.Errorf("the guard did not exit: the script went on to run %s",
					strings.Join(strings.Fields(string(got)), ", "))
			}

			if tc.canary != "" {
				if _, err := os.Stat(filepath.Join(root, tc.canary)); err != nil {
					t.Errorf("the redirect target was disturbed, so the guard did not "+
						"stop the recursive delete: %v", err)
				}
			}
		})
	}
}

// THE NEWLINE GUARD STILL REFUSES ON A MACHINE WITH NO `wc`.
//
// THE FIRST VERSION OF THAT GUARD ASKED `wc -l` AND WAS WRONG IN THIS EXACT WAY.
// MEASURED under /bin/sh on macOS and dash on ubuntu:24.04: with `wc` absent the
// command substitution is empty, `[ "" -ne 0 ]` is an ERROR rather than a comparison,
// the `if` reads that non-zero status as false, and the guard SILENTLY DOES NOT REFUSE
// — could-not-tell collapsing into no, in front of a recursive delete.
//
// The check is shell builtins now, so this test is what stops it going back. The PATH is
// closed to everything but the stand-ins, which is the only way to ask what happens when
// a tool the guard might lean on is not there.
func TestTheNewlineGuardDoesNotDependOnAnExternalTool(t *testing.T) {
	root := canonicalTempDir(t)
	fakeBin := filepath.Join(root, "bin")
	reached := filepath.Join(root, "reached")

	// A home with a newline in it, which must be refused.
	home := root + "/ho\nme"
	if err := os.MkdirAll(home, 0o750); err != nil {
		t.Fatalf("create the home: %v", err)
	}

	// EVERY destructive tool records and fails; NOTHING else is on PATH — no `wc`, no
	// `sha256sum`, no `shasum`. If the guard needs an external command to decide, it
	// cannot decide here, and the recorders say whether it went ahead anyway.
	standInRecorders(t, fakeBin, reached)

	cmd := exec.CommandContext(t.Context(), "/bin/sh")
	cmd.Stdin = strings.NewReader(fetchScript(t, home))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fakeBin)

	raw, err := cmd.CombinedOutput()
	out := string(raw)

	if err == nil {
		t.Errorf("a HOME containing a newline was accepted on a machine with no wc:\n%s", out)
	}

	if !strings.Contains(out, "contains a newline") {
		t.Errorf("the refusal is not the newline guard's, so something else stopped the "+
			"script and this proves nothing:\n%s", out)
	}

	if !gone(t, reached) {
		got, _ := os.ReadFile(reached) //nolint:errcheck // gone() proved it is readable
		t.Errorf("the guard did not stop the script: it went on to run %s",
			strings.Join(strings.Fields(string(got)), ", "))
	}
}

// A VERIFIED TARBALL THAT CARRIES NO RUNNER STILL FAILS THE BUILD.
//
// THIS IS THE ONE TEST THE FINAL `test -x` GATE HAS, and without it that gate could be
// replaced by `true` with nothing failing anywhere: every fixture that gets far enough
// to reach it contains an executable `run.sh`, and every fixture that does not stops
// earlier. A gate no test can distinguish from `true` is a gate somebody deletes.
//
// The archive here has the RIGHT checksum and the wrong contents, which is a published
// release whose layout changed, a mirror serving a different artifact, or simply the
// next actions/runner tarball that stops putting run.sh at the top level. Verified is
// not the same as usable, and the build must not proceed to exec something that is not
// there.
func TestAVerifiedTarballWithNoRunnerFailsTheBuild(t *testing.T) {
	home := canonicalTempDir(t)
	fakeBin := filepath.Join(home, "bin")

	// A real, correctly checksummed .tar.gz that simply has no run.sh in it.
	payload := filepath.Join(home, "payload")
	if err := os.MkdirAll(payload, 0o750); err != nil {
		t.Fatalf("create the payload dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(payload, "README"), []byte("not a runner\n"), 0o600); err != nil {
		t.Fatalf("write the payload: %v", err)
	}

	tarball := filepath.Join(home, "payload.tar.gz")

	pack := exec.CommandContext(t.Context(), "tar", "-czf", tarball, "-C", payload, "README")
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("pack the payload: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatalf("read the payload: %v", err)
	}

	sum := fmt.Sprintf("%x", sha256.Sum256(raw))

	standInBin(t, fakeBin, "curl",
		"out=\"\"\n"+
			"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\n"+
			"cp "+shellSingleQuote(tarball)+" \"$out\"\n")

	out, err := runFetch(t, fetchScriptVerifying(t, home, sum), home, fakeBin)

	if err == nil {
		t.Errorf("a tarball that verified but carries no runner was accepted, so the build "+
			"phase would exec something that is not there:\n%s", out)
	}

	// AND IT GOT PAST THE CHECKSUM, or this passes for the wrong reason — a mismatch
	// would fail earlier and prove nothing about the gate.
	if strings.Contains(out, "FAILED") {
		t.Fatalf("the checksum rejected the archive, so the executable gate never ran:\n%s", out)
	}

	// The extraction really did happen, which is what leaves the gate as the only thing
	// that can refuse.
	if gone(t, filepath.Join(home, ".billet", "actions-runner", "README")) {
		t.Errorf("the archive was never extracted, so nothing reached the gate:\n%s", out)
	}
}

// AND A MACHINE WITH NO SHA256 TOOL REFUSES RATHER THAN INSTALLING UNVERIFIED.
//
// THE THIRD BRANCH OF THE CHECKSUM STEP, and until now nothing executed it: every other
// fetch test runs on a machine that has one, so deleting its `exit 1` changed no test.
// The branch exists because the two platforms disagree — a Linux image has `sha256sum`,
// a macOS build has `shasum -a 256` and no `sha256sum` at all — and a gate that silently
// does nothing on one platform is the failure CLAUDE.md records for `command -v` under a
// privilege drop.
//
// THE PATH IS CLOSED for this one, which is the only way to ask the question: the
// stand-in directory is the WHOLE PATH, so neither tool can be found.
func TestNoChecksumToolRefusesRatherThanInstalling(t *testing.T) {
	home := canonicalTempDir(t)
	fakeBin := filepath.Join(home, "bin")

	tarball, sum := payloadTarball(t, home)

	standInBin(t, fakeBin, "curl",
		"out=\"\"\n"+
			"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\n"+
			"cp "+shellSingleQuote(tarball)+" \"$out\"\n")

	// Everything the script needs EXCEPT a checksum tool. `command -v` searches PATH,
	// so a closed PATH is what makes both lookups fail.
	for _, name := range []string{"rm", "mkdir", "tar", "wc", "printf", "cp"} {
		if found, err := exec.LookPath(name); err == nil {
			standInBin(t, fakeBin, name, "exec "+shellSingleQuote(found)+" \"$@\"\n")
		}
	}

	script := fetchScriptVerifying(t, home, sum)

	cmd := exec.CommandContext(t.Context(), "/bin/sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fakeBin)

	raw, err := cmd.CombinedOutput()
	out := string(raw)

	if err == nil {
		t.Errorf("a machine with no sha256 tool installed the runner anyway:\n%s", out)
	}

	if !strings.Contains(out, "refusing to install an unverified runner") {
		t.Errorf("the refusal does not say the runner was never verified:\n%s", out)
	}

	if !gone(t, filepath.Join(home, ".billet", "actions-runner", "run.sh")) {
		t.Error("an unverified runner was installed on a machine that could not check it")
	}
}

// A SUCCESSFUL FETCH LEAVES ONLY WHAT THIS BUILD VERIFIED.
//
// THIS IS WHAT THE `rm -rf` IS FOR, and it is worth stating because every OTHER
// property one might reach for it is redundant: with the gate `&&`-joined to the
// install, a fetch that fails already fails the command, so the leftover is never
// reached. Mutating the `rm -rf` away survives all of those tests.
//
// What it does buy is this: a CodeBuild reserved instance is measurably not scrubbed
// between builds, so the previous build's runner directory is still here, and untarring
// over it REPLACES the files the tarball carries and leaves every other file exactly
// where it was. The directory the runner then executes out of would be part verified
// tarball and part whatever the last build left — on a host billet's own docs describe
// as shared between builds. Emptying it first makes the whole directory something this
// build downloaded and checksummed.
func TestASuccessfulFetchLeavesOnlyWhatItVerified(t *testing.T) {
	home := canonicalTempDir(t)
	fakeBin := filepath.Join(home, "bin")

	// A tarball this test builds and checksums itself, so the fetch can SUCCEED with no
	// network — the only executable fetch until now was a failing one.
	payload := filepath.Join(home, "payload")
	if err := os.MkdirAll(payload, 0o750); err != nil {
		t.Fatalf("create the payload dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(payload, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write the payload runner: %v", err)
	}

	tarball := filepath.Join(home, "payload.tar.gz")

	pack := exec.CommandContext(t.Context(), "tar", "-czf", tarball, "-C", payload, "run.sh")
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("pack the payload: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatalf("read the payload: %v", err)
	}

	sum := fmt.Sprintf("%x", sha256.Sum256(raw))

	// The previous build's leavings: a file the new tarball does not carry, so nothing
	// but an explicit removal can take it away.
	stale := filepath.Join(home, ".billet", "actions-runner")
	if err := os.MkdirAll(filepath.Join(stale, "bin"), 0o750); err != nil {
		t.Fatalf("stage the stale runner: %v", err)
	}

	leftover := filepath.Join(stale, "bin", "left-by-the-previous-build")
	if err := os.WriteFile(leftover, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}

	standInBin(t, fakeBin, "curl",
		"out=\"\"\n"+
			"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\n"+
			"cp "+shellSingleQuote(tarball)+" \"$out\"\n")

	out, err := runFetch(t, fetchScriptVerifying(t, home, sum), home, fakeBin)
	if err != nil {
		t.Fatalf("the fetch failed, so this test proves nothing about what it leaves: %v\n%s",
			err, out)
	}

	// It really did install — otherwise the assertion below passes because nothing ran.
	if _, err := os.Stat(filepath.Join(stale, "run.sh")); err != nil {
		t.Fatalf("the fetch reported success and installed no runner: %v\n%s", err, out)
	}

	if !gone(t, leftover) {
		t.Error("a file from the previous build survived into the directory this build's " +
			"runner executes out of; on a reserved instance, which is not scrubbed between " +
			"builds, that directory is then part verified tarball and part whatever was there")
	}
}

// THE ROOT GUARD IS RUN DIRECTLY, AGAINST SPELLINGS THIS PLATFORM WOULD NEVER PRODUCE.
//
// REACHING IT THROUGH `pwd -P` CAN ONLY EVER TEST THE LOCAL PLATFORM'S HALF, and that
// is the whole problem the pattern exists for. MEASURED: `cd // && pwd -P` answers `/`
// under dash on ubuntu:24.04 and `//` under /bin/sh on macOS, because POSIX leaves a
// pathname beginning with exactly two slashes implementation-defined. So on Linux the
// OLD `= "/"` comparison passes every case a script-level test can construct — the
// regression is invisible on the platform CI runs, which is a test that agrees with the
// bug exactly where it would ship.
//
// Executing the production fragment with BILLET_HOME set directly is what removes the
// platform from the question.
func TestTheRootHomeGuardRefusesEverySpellingOfTheRoot(t *testing.T) {
	for _, tc := range []struct {
		home   string
		refuse bool
	}{
		{home: "/", refuse: true},
		// The one a `= "/"` comparison lets through wherever the shell preserves it.
		{home: "//", refuse: true},
		{home: "////", refuse: true},
		// And the directories a real build actually has, which must NOT be refused —
		// a guard that rejects correct input is the failure ADR-005 names.
		{home: "/root", refuse: false},
		{home: "/Users/cbuser", refuse: false},
		{home: "/home/runner", refuse: false},
	} {
		t.Run(strings.ReplaceAll(tc.home, "/", "_"), func(t *testing.T) {
			script := "BILLET_HOME=" + shellSingleQuote(tc.home) + "\n" + refuseRootHome + "\n"

			cmd := exec.CommandContext(t.Context(), "/bin/sh")
			cmd.Stdin = strings.NewReader(script)

			out, err := cmd.CombinedOutput()

			switch {
			case tc.refuse && err == nil:
				t.Errorf("BILLET_HOME=%q was accepted; billet would recursively delete "+
					"under the root directory:\n%s", tc.home, out)
			case tc.refuse && !strings.Contains(string(out), "the root directory"):
				t.Errorf("BILLET_HOME=%q was refused without saying why:\n%s", tc.home, out)
			case !tc.refuse && err != nil:
				t.Errorf("BILLET_HOME=%q was refused, so a real build user's home would "+
					"never start a runner: %v\n%s", tc.home, err, out)
			}
		})
	}
}

// A HOME WHOSE NAME ENDS IN THE SENTINEL'S OWN CHARACTER IS STILL ACCEPTED.
//
// THE ACCEPTANCE DIRECTION OF THE SENTINEL, which nothing else covers. `pwd -P` runs
// behind `printf x` and the result is trimmed with `${BILLET_HOME%x}`, so the obvious
// worry is that a directory named `…/max` loses its own last byte and billet installs
// into `…/ma`. It does not — `pwd`'s newline always sits between the pathname and the
// sentinel, so the two trims take the sentinel and that separator and stop — but "it
// does not" is worth an executed test rather than a reading, because the failure would
// be silent: a runner installed one directory over, and a `test -x` that passes because
// the tarball put a run.sh there too.
func TestAHomeEndingInTheSentinelCharacterIsAccepted(t *testing.T) {
	root := canonicalTempDir(t)
	fakeBin := filepath.Join(root, "bin")

	home := filepath.Join(root, "max")
	if err := os.MkdirAll(home, 0o750); err != nil {
		t.Fatalf("create the home: %v", err)
	}

	tarball, sum := payloadTarball(t, root)

	standInBin(t, fakeBin, "curl",
		"out=\"\"\n"+
			"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\n"+
			"cp "+shellSingleQuote(tarball)+" \"$out\"\n")

	out, err := runFetch(t, fetchScriptVerifying(t, home, sum), home, fakeBin)
	if err != nil {
		t.Fatalf("a home ending in %q was refused: %v\n%s", "x", err, out)
	}

	// IN `…/max`, not in `…/ma`.
	if _, err := os.Stat(filepath.Join(home, ".billet", "actions-runner", "run.sh")); err != nil {
		t.Errorf("the runner is not under the home billet was given: %v\n%s", err, out)
	}

	if !gone(t, filepath.Join(root, "ma")) {
		t.Error("billet created a directory one character short of the home it was given, " +
			"so the sentinel trim ate the pathname's own last byte")
	}
}

// A SYMLINKED HOME IS ACCEPTED, AND THE RUNNER LANDS IN THE DIRECTORY IT RESOLVES TO.
//
// THIS IS THE CASE THE PREVIOUS GUARD REFUSED, and refusing it was a build that never
// starts a runner on an image whose only sin is an ordinary layout choice. billet now
// resolves HOME and works from the result, so a symlinked home is not a refusal — what
// it is instead is a directory with no links left above `.billet`, which is the
// property the recursive delete actually needs.
func TestASymlinkedHomeIsResolvedRatherThanRefused(t *testing.T) {
	root := canonicalTempDir(t)
	fakeBin := filepath.Join(root, "bin")

	target := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("create the real home: %v", err)
	}

	home := filepath.Join(root, "homelink")
	if err := os.Symlink(target, home); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	// A tarball this test builds and checksums itself, so the fetch SUCCEEDS offline.
	tarball, sum := payloadTarball(t, root)

	standInBin(t, fakeBin, "curl",
		"out=\"\"\n"+
			"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\n"+
			"cp "+shellSingleQuote(tarball)+" \"$out\"\n")

	// THE SYMLINK MOVES MID-SCRIPT, and without that this test cannot tell the two
	// spellings apart at all. A STABLE link makes `$HOME/.billet/...` and
	// `$BILLET_HOME/.billet/...` name the same directory, so the assertion below passes
	// just as well against a regression to the unresolved `$HOME` — which is precisely
	// the redirection the resolution exists to prevent.
	//
	// Repointing it inside the stand-in `rm` — after HOME has been resolved, before
	// anything is written — separates them: the resolved path still names the first
	// target, while `$HOME` now names the second.
	moved := filepath.Join(root, "moved-to")
	if err := os.MkdirAll(moved, 0o750); err != nil {
		t.Fatalf("create the second target: %v", err)
	}

	// `&&`, NOT TWO STATEMENTS: with `ln` on its own line a FAILED repoint is erased by
	// the successful `rm` that follows, the original link stays stable, and both
	// assertions below pass against the very regression this test exists to catch.
	standInBin(t, fakeBin, "rm",
		"ln -sfn "+shellSingleQuote(moved)+" "+shellSingleQuote(home)+" && "+
			"exec /bin/rm \"$@\"\n")

	out, err := runFetch(t, fetchScriptVerifying(t, home, sum), home, fakeBin)
	if err != nil {
		t.Fatalf("a symlinked HOME was refused, so a custom image laid out this way "+
			"could never start a runner: %v\n%s", err, out)
	}

	// IN THE DIRECTORY HOME RESOLVED TO, not wherever the link points now.
	if _, err := os.Stat(filepath.Join(target, ".billet", "actions-runner", "run.sh")); err != nil {
		t.Errorf("the runner is not in the directory HOME resolved to: %v\n%s", err, out)
	}

	// THE FIXTURE ACTUALLY MUTATED, asserted rather than inferred. Without this, a
	// repoint that never happened looks exactly like a repoint the script correctly
	// ignored — the absence of `moved/.billet` proves nothing on its own.
	if dest, err := os.Readlink(home); err != nil || dest != moved {
		t.Fatalf("the stand-in never repointed HOME (now %q, %v), so this test says "+
			"nothing about which spelling the script used", dest, err)
	}

	// And nothing followed the link after it moved, which is the half a stable symlink
	// cannot demonstrate.
	if !gone(t, filepath.Join(moved, ".billet")) {
		t.Error("the script wrote through HOME after it was repointed, so it is using the " +
			"unresolved spelling and a moving symlink can still redirect it")
	}
}

// A FAILED CLEANUP FAILS THE BUILD, RATHER THAN BEING ANSWERED FOR BY THE STALE RUNNER.
//
// THE GATE USED TO BE A SEPARATE BUILDSPEC COMMAND, and with no `set -e` that made it
// able to answer FOR a failed install instead of gating it: a `rm -rf` that failed
// while leaving the previous build's runner in place — an unremovable file, a full or
// read-only filesystem — short-circuited the chain, and then `test -x` found the stale
// executable and returned zero. The script exited zero and the build phase got a runner
// this job never downloaded and never verified, which is the exact outcome the `rm -rf`
// was added to prevent, arriving through the gate meant to catch it.
func TestAFailedCleanupIsNotAnsweredForByTheStaleRunner(t *testing.T) {
	home := canonicalTempDir(t)
	fakeBin := filepath.Join(home, "bin")

	stale := filepath.Join(home, ".billet", "actions-runner")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatalf("stage the stale runner: %v", err)
	}

	if err := os.WriteFile(filepath.Join(stale, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write the stale runner: %v", err)
	}

	// An `rm` that refuses and changes nothing, which is what an unremovable file or a
	// read-only filesystem looks like from the script's side. IT RECORDS BEING CALLED,
	// because "the script failed and the stale runner is still there" is also true of
	// an earlier guard refusing — this test would then pass without the cleanup having
	// been attempted at all, which is not what its name claims.
	reached := filepath.Join(home, "rm-reached")
	standInBin(t, fakeBin, "rm",
		"echo rm >> "+shellSingleQuote(reached)+"\n"+
			"echo 'rm: cannot remove: Operation not permitted' >&2\nexit 1\n")

	out, err := runFetch(t, fetchScript(t, home), home, fakeBin)

	if gone(t, reached) {
		t.Fatalf("the script never reached the cleanup, so this test says nothing about "+
			"what a failed cleanup does:\n%s", out)
	}

	if err == nil {
		t.Errorf("the cleanup failed, the previous build's runner is still installed, and "+
			"the script exited zero:\n%s", out)
	}

	// And the stale runner really was there to be found, or the assertion above passes
	// for the wrong reason.
	if _, err := os.Stat(filepath.Join(stale, "run.sh")); err != nil {
		t.Errorf("the stale runner was not present, so nothing could have answered for "+
			"the failed cleanup: %v", err)
	}
}

// A FAILED DOWNLOAD FAILS THE BUILD EVEN WHEN A RUNNER IS ALREADY THERE.
//
// The cleanup SUCCEEDS here, so what is under test is the chain reporting the download.
// TestAFailedCleanupIsNotAnsweredForByTheStaleRunner covers the other half, where the
// cleanup is what fails and the leftover survives to be found.
//
// THE RESERVED-FLEET CASE, and billet MEASURED the premise rather than assuming it: an
// instance is not scrubbed between builds, so an earlier build's runner is still on
// disk. What stops the build is the `&&` chain reporting the download's failure and the
// gate being JOINED to it — the trailing `test -x` never runs here, which is the point:
// as a separate command it would have found the leftover and answered zero.
func TestAFailedDownloadFailsEvenWithAStaleRunnerPresent(t *testing.T) {
	home := canonicalTempDir(t)
	fakeBin := filepath.Join(home, "bin")

	stale := filepath.Join(home, ".billet", "actions-runner")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatalf("stage the stale runner: %v", err)
	}

	if err := os.WriteFile(filepath.Join(stale, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write the stale runner: %v", err)
	}

	// RECORDED, for the same reason as the cleanup test: any earlier refusal also
	// produces a non-zero exit, and this test would pass without a download having been
	// attempted.
	reached := filepath.Join(home, "curl-reached")
	standInBin(t, fakeBin, "curl",
		"echo curl >> "+shellSingleQuote(reached)+"\n"+
			"echo 'curl: (22) 404' >&2\nexit 22\n")

	out, err := runFetch(t, fetchScript(t, home), home, fakeBin)

	if gone(t, reached) {
		t.Fatalf("the script never reached the download, so this test says nothing about "+
			"what a failed one does:\n%s", out)
	}

	// And the cleanup DID run before it, which is what leaves nothing for the gate to
	// find — the stale runner must be gone by now.
	if !gone(t, filepath.Join(stale, "run.sh")) {
		t.Error("the cleanup did not remove the previous build's runner before the " +
			"download was attempted")
	}

	if err == nil {
		t.Errorf("the fetch failed to download and still exited zero, with a stale runner "+
			"left by an earlier build satisfying the test -x gate:\n%s", out)
	}
}

// A LOST BILLET_RUNNER_DIR FAILS THE BUILD RATHER THAN RUNNING SOMETHING ELSE.
//
// THE ARGUMENT THIS REPLACES WAS WRONG, and it is worth keeping because it read as
// airtight. `BILLET_RUNNER_DIR` is exported in pre_build and read in build, which
// depends on the phases sharing a shell; the claim was that losing it still failed
// closed, since a bare `cd ""` exits ZERO (measured) and `./run.sh` would then exit
// 127 from the wrong directory. THE SECOND HALF ONLY HOLDS FOR `./run.sh`. The tier
// command is operator-configured, so it can be an absolute path or any name resolvable
// from wherever the build started — and the phase then runs something that is not this
// job's runner and exits zero, which CodeBuild reports as SUCCEEDED.
//
// The decoy here is exactly that: an executable at the name the tier command uses,
// sitting in the directory the build starts in. It records being run, and the
// assertion is that it never was.
func TestALostRunnerDirectoryFailsRatherThanRunningWhateverIsToHand(t *testing.T) {
	p := providerFor(t, config.CodeBuildLinuxContainer)

	body, err := p.Buildspec(Spec{Name: "billet-abc", Command: []string{"./run.sh"}})
	if err != nil {
		t.Fatalf("Buildspec: %v", err)
	}

	doc := parseBuildspec(t, body)

	dir := t.TempDir()
	ran := filepath.Join(dir, "decoy-ran")

	// The wrong `run.sh` — the one a build that lost its variable would find.
	if err := os.WriteFile(filepath.Join(dir, "run.sh"),
		[]byte("#!/bin/sh\necho decoy > "+shellSingleQuote(ran)+"\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write the decoy: %v", err)
	}

	// ONLY THE BUILD PHASE, with the variable never set — which is precisely the state
	// the phases NOT sharing a shell would produce. No `set -e`, for the usual reason.
	script := strings.Join(doc.Phases["build"].Commands, "\n") + "\n"

	cmd := exec.CommandContext(t.Context(), "/bin/sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Dir = dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "BILLET_RUNNER_DIR=")
	})

	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Errorf("the build phase lost BILLET_RUNNER_DIR and still exited zero, which "+
			"CodeBuild reports as SUCCEEDED:\n%s", out)
	}

	if !gone(t, ran) {
		t.Error("the build phase ran the executable that happened to be in the directory " +
			"it started in, instead of this job's runner")
	}
}

// THE CHECKSUM IN THE BUILDSPEC IS THE PINNED ONE FOR THAT PLATFORM.
//
// Not merely "a checksum is present": the whole point is that an arm64 build
// verifies against the arm64 tarball. A buildspec carrying the x64 checksum on an
// arm64 build fails every download with a mismatch, and one carrying an EMPTY checksum
// is rejected as a malformed digest — both loud, neither installing anything, and
// neither naming the thing that is actually wrong. This pins the right digest to the
// right platform so an operator meets neither.
func TestTheBuildspecCarriesThePinnedChecksumForItsOwnPlatform(t *testing.T) {
	for _, tc := range []struct {
		env      config.CodeBuildEnvironment
		platform string
	}{
		{config.CodeBuildLinuxContainer, "linux-x64"},
		{config.CodeBuildLinuxGPUContainer, "linux-x64"},
		{config.CodeBuildLinuxEC2, "linux-x64"},
		{config.CodeBuildARMContainer, "linux-arm64"},
		{config.CodeBuildARMEC2, "linux-arm64"},
		{config.CodeBuildMacARM, "osx-arm64"},
	} {
		t.Run(string(tc.env), func(t *testing.T) {
			p := providerFor(t, tc.env)

			body, err := p.Buildspec(Spec{Name: "billet-abc", Command: []string{"./run.sh"}})
			if err != nil {
				t.Fatalf("Buildspec: %v", err)
			}

			want, ok := runnerrelease.PinnedSHA256For(tc.platform)
			if !ok {
				t.Fatalf("no checksum is pinned for %s", tc.platform)
			}

			if !strings.Contains(body, want) {
				t.Errorf("the buildspec for %s does not carry the %s checksum:\n%s",
					tc.env, tc.platform, body)
			}

			if !strings.Contains(body, "BILLET_RUNNER_PLATFORM='"+tc.platform+"'") {
				t.Errorf("the buildspec for %s does not name platform %s:\n%s",
					tc.env, tc.platform, body)
			}

			// AND NOT ANOTHER PLATFORM'S. A buildspec that carried both would verify
			// against whichever the shell read last.
			for _, other := range []string{"linux-x64", "linux-arm64", "osx-arm64"} {
				if other == tc.platform {
					continue
				}

				sum, ok := runnerrelease.PinnedSHA256For(other)
				if ok && strings.Contains(body, sum) {
					t.Errorf("the buildspec for %s also carries the %s checksum", tc.env, other)
				}
			}
		})
	}
}

// A COMMAND BILLET CANNOT CARRY SAFELY IS REFUSED, not escaped.
//
// A single-quoted assignment is the one construct with no parsing left in it, and an
// escape is a second thing to get right on a path where getting it wrong produces a
// build that starts and registers nothing.
func TestABuildspecRefusesACommandItCannotCarrySafely(t *testing.T) {
	p := providerFor(t, config.CodeBuildLinuxContainer)

	for name, command := range map[string][]string{
		"single quote": {"./run.sh", "--label", "it's"},
		"newline":      {"./run.sh\nrm -rf /"},
		"nul":          {"./run.sh\x00"},
		"empty":        {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.Buildspec(Spec{Name: "billet-abc", Command: command}); err == nil {
				t.Errorf("Buildspec accepted %q", command)
			}
		})
	}
}

// AN ENVIRONMENT WITH NO PINNED RUNNER PLATFORM IS REFUSED.
//
// Defaulting would fetch an x64 runner onto an arm64 build: every file structurally
// correct, and nothing failing until a job execs one. That is the same trap the
// toolcache installers record about architecture spellings, so it fails closed.
func TestAnUnknownEnvironmentHasNoRunnerPlatform(t *testing.T) {
	if _, err := runnerPlatform(config.CodeBuildEnvironment("MOON_CONTAINER")); err == nil {
		t.Error("an unknown environment resolved to a runner platform")
	}

	// Every environment billet accepts must resolve, or a config that validates
	// produces launches that cannot build a buildspec.
	for _, env := range []config.CodeBuildEnvironment{
		config.CodeBuildLinuxContainer, config.CodeBuildARMContainer,
		config.CodeBuildLinuxGPUContainer, config.CodeBuildLinuxEC2,
		config.CodeBuildARMEC2, config.CodeBuildMacARM,
	} {
		platform, err := runnerPlatform(env)
		if err != nil {
			t.Errorf("%s resolves to no runner platform: %v", env, err)

			continue
		}

		if _, ok := runnerrelease.PinnedSHA256For(platform); !ok {
			t.Errorf("%s resolves to platform %s, which billet pins no checksum for", env, platform)
		}
	}
}
