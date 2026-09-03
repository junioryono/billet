package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// verifyProbeNonce is a well-formed nonce for a script under test.
const verifyProbeNonce = "0123456789abcdef0123456789abcdef"

// droppedMarker is what the fake setpriv sets so a fake command can tell whether
// it was reached through the privilege drop.
const droppedMarker = "BILLET_TEST_DROPPED"

// substitute rewrites an absolute path in a generated script, and refuses when
// the anchor is not there.
//
// AN EDIT THAT DID NOT APPLY LOOKS EXACTLY LIKE ONE THAT DID. A ReplaceAll that
// matches nothing leaves the script pointing at /opt and /etc on the machine
// running the test — which either fails for the wrong reason or, worse, passes
// against whatever happens to be installed. This package's own history has three
// of those.
func substitute(t *testing.T, script, old, replacement string) string {
	t.Helper()

	if !strings.Contains(script, old) {
		t.Fatalf("the script contains no %q, so this substitution would silently do nothing "+
			"and the check would run against the real filesystem", old)
	}

	return strings.ReplaceAll(script, old, replacement)
}

// declaredGlobals is every command the declaration says a global package
// provides, in the order the gate checks them.
func declaredGlobals(ts runnerimages.Toolset) []string {
	var out []string

	for _, e := range ts.Pipx {
		if e.Cmd != "" {
			out = append(out, e.Cmd)
		}
	}

	for _, e := range ts.NodeModules {
		if e.Command != "" {
			out = append(out, e.Command)
		}
	}

	return out
}

// toolchainBins are the absolute paths the gate executes for tools that are NOT
// toolcache entries — they go on PATH rather than under <tool>/<version>/<arch>,
// so the fixture tree cannot supply them and the fake bin has to.
//
// A LIST RATHER THAN A PREFIX SUBSTITUTION. Rewriting every "/usr/bin/" in the
// script would also rewrite the PATH the privilege drop builds, which is a
// different thing that happens to share a spelling. And because substitute() is
// fatal when its target is absent, adding a tool to the gate without adding it
// here fails this file rather than silently going unexercised — which is the same
// iterate-the-expected-set rule the gate itself is built on.
var toolchainBins = []string{
	"/usr/local/bin/cmake",
	"/usr/bin/pwsh",
	"/usr/bin/dotnet",
	"/usr/bin/clang",
	"/usr/bin/clang++",
	"/usr/local/bin/node",
	"/usr/local/bin/npm",
	"/usr/local/bin/ruby",
	"/usr/local/bin/gem",
}

// verifyProbe is one run of the emitted verification script against a fake
// machine.
type verifyProbe struct {
	// fixture is the toolcache tree the image is pretending to carry.
	fixture toolcacheFixture
	// dockerDriver is what the fake daemon reports; empty means overlay2.
	dockerDriver string
	// dockerRoot is where the fake daemon says its data lives; empty means
	// /var/lib/docker.
	dockerRoot string
	// brokenToolchain, when set to one of toolchainBins, makes exactly that command
	// exit non-zero. One at a time, because the point is which step gets reported.
	brokenToolchain string
	// brokenGlobal does the same for one declared pipx or node_modules command.
	brokenGlobal string
	// brokenAndroid removes one of the two android paths the gate stats, which is
	// what an image built without the licence acceptance actually looks like.
	brokenAndroid string
	// brokenPSModule makes the pwsh stub report exactly that module unavailable.
	brokenPSModule string
	// brokenRunner makes Runner.Listener exit non-zero, which is what a missing
	// libicu looks like from here.
	brokenRunner bool
	// badDaemonJSON makes the fake jq refuse, which is what a daemon.json the
	// snapshot did not carry — or one selecting the containerd store — looks like.
	// It is separate from dockerDriver on purpose: the file and the running daemon
	// answer different questions, and a check that had only one of them would pass
	// an image whose daemon happens to be right and whose file is not.
	badDaemonJSON bool
	// dockerDeniedToRunner makes the fake daemon serve root and refuse the runner
	// account, which is what a `usermod -aG docker runner` that did not take looks
	// like: every root-side check perfect, every workflow dead on "permission
	// denied while trying to connect to the Docker daemon socket".
	//
	// `docker ps` IS THE DISCRIMINATOR. The fake refuses only that, which no
	// root-side check makes, so this case can fail for one reason alone.
	dockerDeniedToRunner bool
	// freeKiB is what the fake df reports free on the root; zero takes a figure a
	// real build measured.
	freeKiB int
	// unwritableConsole points /dev/console at a directory, which is what a
	// machine billet cannot write a console on looks like.
	unwritableConsole bool
}

// run emits the script, rewrites its absolute paths onto a fake machine, executes
// it, and returns what billet's own parser makes of the console.
//
// EXECUTED, NOT PATTERN-MATCHED. Every substring an assertion about the TEXT
// could look for survives edits that make the check toothless — dropping the `-e`
// from jq, rendering a value without comparing it, deleting an arm's exit. This
// package learned that on the image-store gate and the rule is the same here.
func (p verifyProbe) run(t *testing.T) (report map[string]string, stdout string) {
	t.Helper()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	script, schema, err := verifyScript("x64", AMIContract, verifyProbeNonce)
	if err != nil {
		t.Fatalf("verifyScript: %v", err)
	}

	root := t.TempDir()
	tcDir, envFile := p.fixture.build(t, ts)

	runnerDir := filepath.Join(root, "actions-runner")
	if err := os.MkdirAll(filepath.Join(runnerDir, "bin"), 0o755); err != nil {
		t.Fatalf("make the runner: %v", err)
	}

	listener := "#!/bin/sh\necho 2.328.0\n"
	if p.brokenRunner {
		listener = "#!/bin/sh\nexit 1\n"
	}

	writeFile(t, filepath.Join(runnerDir, "bin", "Runner.Listener"), listener, 0o755)

	console := filepath.Join(root, "console")
	if p.unwritableConsole {
		if err := os.MkdirAll(console, 0o755); err != nil {
			t.Fatalf("make the console: %v", err)
		}
	}

	// ONE FAKE MACHINE, NAMED ONCE. Calling fakeBin twice built two directories with
	// identical contents, so the root-side PATH and the runner-side PATH pointed at
	// different copies — which works, and would go on working if one of them were
	// wrong, because nothing could tell them apart.
	bin := p.fakeBin(t)

	// THE IMAGE'S OWN PATH, THROUGH THE MECHANISM THE SCRIPT ALREADY READS. Under
	// `env -i` the runner gets the fixed PATH privilegeDrop states, and on a real
	// image `docker` is on it; here the daemon is a script in this test's fake bin.
	// /etc/billet-image-env is exactly how an image adds to a job's environment, so
	// the fixture uses it rather than a fake that reimplements `env`.
	appendFile(t, envFile, "PATH="+bin+":/usr/bin:/bin\n")

	script = substitute(t, script, toolcacheDir, tcDir)
	script = substitute(t, script, imageEnvFile, envFile)
	script = substitute(t, script, "/opt/actions-runner", runnerDir)
	script = substitute(t, script, "/dev/console", console)

	for _, path := range toolchainBins {
		script = substitute(t, script, path+" ", filepath.Join(bin, filepath.Base(path))+" ")
	}

	// THE ANDROID SDK IS A TREE, NOT A COMMAND. The gate stats sdkmanager and the
	// ndk directory, so the fixture builds those two paths rather than a stub on
	// PATH — and brokenAndroid omits one, which is what an image built without the
	// licence acceptance looks like from the gate's side.
	sdk := filepath.Join(root, "android-sdk")

	// THE PARENTS ARE MADE FIRST. writeFile does not create them, and a fixture
	// that fails to build reads as the gate refusing a correct image.
	for _, d := range []string{
		filepath.Join(sdk, "ndk", "27.0.12077973"),
		filepath.Join(sdk, "cmdline-tools", "latest", "bin"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("make the android fixture: %v", err)
		}
	}

	if p.brokenAndroid != androidSDKRoot+"/ndk" {
		writeFile(t, filepath.Join(sdk, "ndk", "27.0.12077973", ".keep"), "", 0o644)
	}

	if p.brokenAndroid != androidSDKRoot {
		writeFile(t, filepath.Join(sdk, "cmdline-tools", "latest", "bin", "sdkmanager"),
			"#!/bin/sh\nexit 0\n", 0o755)
	}

	script = substitute(t, script, androidSDKRoot, sdk)

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))

	// THE EXIT STATUS IS DELIBERATELY NOT THE ASSERTION, and it is kept for the
	// diagnostic rather than discarded. The script's whole design is that a failed
	// check still REPORTS — the verdict travels on the console, not in a status
	// nobody can read from outside the machine — so what a test asserts on is what
	// it said. But when there is no report at all, the status is most of what
	// explains why.
	out, runErr := cmd.CombinedOutput()

	stdout = string(out)

	body := stdout

	if !p.unwritableConsole {
		raw, err := os.ReadFile(console)
		if err != nil {
			t.Fatalf("the script wrote no console at all: %v (it exited %v)\n"+
				"--- it printed ---\n%s", err, runErr, stdout)
		}

		body = string(raw)
	}

	// PARSED BY THE PRODUCTION PARSER, so this is a round trip rather than two
	// independent readings of one format: an emitter and a parser that drift apart
	// fail here rather than on a paid builder.
	report, ok := parseReport(body, verifyProbeNonce, schema)
	if !ok {
		t.Fatalf("the script printed no report billet can read (it exited %v)\n"+
			"--- console ---\n%s\n--- stdout ---\n%s", runErr, body, stdout)
	}

	return report, stdout
}

// appendFile adds a line to a fixture file that already exists.
func appendFile(t *testing.T, path, line string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// fakeBin is the machine the script thinks it is running on.
//
// THE COMMANDS, NOT THE FILESYSTEM. docker, jq and realpath speak about paths a
// test cannot create — /var/lib/docker, /etc/docker/daemon.json — so they answer
// from a fixture instead, exactly as the in-build image-store test does. sleep
// and poweroff are stubbed because the dwell is twenty-four rounds of twenty
// seconds and this is a unit test.
func (p verifyProbe) fakeBin(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make the fake bin: %v", err)
	}

	driver := p.dockerDriver
	if driver == "" {
		driver = "overlay2"
	}

	dockerRoot := p.dockerRoot
	if dockerRoot == "" {
		dockerRoot = "/var/lib/docker"
	}

	jqStatus := "0"
	if p.badDaemonJSON {
		jqStatus = "1"
	}

	psStatus := "0"
	if p.dockerDeniedToRunner {
		psStatus = "1"
	}

	// THE FIGURES A REAL BUILD MEASURED, read from a fresh boot of the AMI it
	// produced: 7GiB used of 28GiB usable, 20GiB free. Using the HOST's df instead
	// — which is what this did first — makes the free-space assertion a statement
	// about the machine running the test.
	free := p.freeKiB
	if free == 0 {
		free = 20 * 1024 * 1024
	}

	for name, body := range map[string]string{
		// setpriv IS NOT ON macOS AT ALL, and the point of the fake is not to drop
		// privileges: it is that the script goes through the same invocation the
		// image's entry point does, so a change to privilegeDrop that the entry
		// point would choke on chokes here too.
		//
		// AND IT LEAVES A MARK, which is what makes "as the runner" observable at
		// all. Without it a fake that refuses a command refuses it whoever asked,
		// so replacing `billet_as_runner docker ps` with a bare `docker ps` keeps
		// every case green — the mechanism proven and nothing proving it is used.
		//
		// THE MARK IS AN ASSIGNMENT INSERTED AFTER `env -i`, because that is the one
		// place a variable survives: the real env then wipes the environment and
		// builds a new one from what follows it, so anything exported before is
		// gone. The fake knows the shape it is handed — privilegeDrop's flags, then
		// `env`, then `-i` — so this is two shifts rather than argument surgery.
		"setpriv": "#!/bin/sh\n" +
			"while [ $# -gt 0 ]; do\n" +
			"  case \"$1\" in --*) shift ;; *) break ;; esac\n" +
			"done\n" +
			"env=$1; shift\n" +
			"flag=$1; shift\n" +
			"exec \"$env\" \"$flag\" " + droppedMarker + "=1 \"$@\"\n",
		// THE DAEMON ANSWERS DIFFERENTLY TO ROOT AND TO THE RUNNER, which is the
		// whole point of the dockerDeniedToRunner case: `ps` is refused only when
		// the call arrived through the privilege drop.
		"docker": "#!/bin/sh\n" +
			"case \"$*\" in\n" +
			"  *ServerVersion*) echo 29.7.2 ;;\n" +
			"  *.Driver*) echo " + driver + " ;;\n" +
			"  *DockerRootDir*) echo " + dockerRoot + " ;;\n" +
			"  ps*) [ -z \"${" + droppedMarker + ":-}\" ] && exit 0\n" +
			"        exit " + psStatus + " ;;\n" +
			"  *) exit 0 ;;\n" +
			"esac\n",
		// A HEADER AND ONE RECORD, in the POSIX format the script asks for with -P.
		"df": "#!/bin/sh\n" +
			"echo 'Filesystem 1024-blocks Used Available Capacity Mounted on'\n" +
			"echo '/dev/root 29360128 " + strconv.Itoa(29360128-free) + " " +
			strconv.Itoa(free) + " 30% /'\n",
		// -m RESOLVES A PATH WHOSE TAIL DOES NOT EXIST, which is the flag the script
		// passes and the reason it can ask about a cache root that is not mounted
		// yet. The fixture paths are already canonical, so echoing is faithful.
		"realpath": "#!/bin/sh\n" +
			"while [ $# -gt 1 ]; do shift; done\n" +
			"printf '%s\\n' \"$1\"\n",
		"jq":       "#!/bin/sh\nexit " + jqStatus + "\n",
		"sleep":    "#!/bin/sh\nexit 0\n",
		"poweroff": "#!/bin/sh\nexit 0\n",
	} {
		writeFile(t, filepath.Join(dir, name), body, 0o755)
	}

	// THE TOOLCHAINS THAT ARE NOT TOOLCACHE ENTRIES. Each is present unless this
	// probe was asked to break exactly one of them, which is how a test proves the
	// gate reports the RIGHT step rather than merely reporting some failure.
	for _, path := range toolchainBins {
		status := "0"
		if p.brokenToolchain == path {
			status = "1"
		}

		body := "#!/bin/sh\nexit " + status + "\n"

		// pwsh IS ASKED ABOUT ONE MODULE AT A TIME, so its stub needs to answer
		// differently per module rather than uniformly. It does not parse
		// PowerShell: it looks for the name in its arguments, which is the only
		// distinction the fixture has to make.
		if filepath.Base(path) == "pwsh" && p.brokenPSModule != "" {
			body = "#!/bin/sh\ncase \"$*\" in\n  *" + p.brokenPSModule +
				"*) exit 1 ;;\nesac\nexit " + status + "\n"
		}

		writeFile(t, filepath.Join(dir, filepath.Base(path)), body, 0o755)
	}

	// AND THE GLOBALS, READ FROM THE DECLARATION RATHER THAN LISTED HERE. The gate
	// iterates every pipx and node_modules entry the toolset names, so a fixture
	// with a hand-written list would go stale the next time upstream adds one and
	// the failure would look like a broken gate rather than a stale fixture. These
	// are found through PATH by the emitted billet_need, so no substitution is
	// needed — only that a command of the name exists.
	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("load the declaration: %v", err)
	}

	for _, cmd := range declaredGlobals(ts) {
		// ABSENT, NOT FAILING. The gate asks `command -v`, which reports whether the
		// command EXISTS; a stub that exits non-zero is still found, so writing one
		// would leave this case green against a gate that checks nothing. The real
		// failure is a package manager that installed into a prefix nothing puts on
		// PATH, and absence is exactly what that looks like.
		if p.brokenGlobal == cmd {
			continue
		}

		writeFile(t, filepath.Join(dir, cmd), "#!/bin/sh\nexit 0\n", 0o755)
	}

	return dir
}

// THE SCRIPT IS RUN, AND IT REPORTS WHAT IT FOUND.
//
// This is the whole change in one test: the emitted shell executes on a machine
// that is not the builder, asserts the contract, and prints a block billet's own
// parser reads back. Everything else in this file is one way for that to be wrong.
func TestTheVerifierScriptRunsAndReports(t *testing.T) {
	t.Parallel()

	report, stdout := verifyProbe{}.run(t)

	if got := report[reportVerdictKey]; got != "ok" {
		t.Fatalf("a complete image reported %s=%q at step %q, so every build would fail\n"+
			"--- it printed ---\n%s", reportVerdictKey, got, report[reportStepKey], stdout)
	}

	if got := report[reportStepKey]; got != "done" {
		t.Errorf("the passing report stopped at step %q, want done", got)
	}

	// AND THE FACTS ARE IN IT. A verdict with an empty report is a check nobody can
	// act on, and every one of these is a value the operator asked for by hand.
	for _, key := range []string{
		"root_free_kib", "root_total_kib", "docker_driver", "docker_root", "runner",
		"toolcache_kib", "tc_node", "tc_java_temurin_hotspot_jdk",
	} {
		if report[key] == "" {
			t.Errorf("the report carries no %s: %v", key, report)
		}
	}

	if got := report["docker_driver"]; got != "overlay2" {
		t.Errorf("docker_driver=%q, want overlay2", got)
	}

	if got := report["runner"]; got != "2.328.0" {
		t.Errorf("runner=%q, want 2.328.0: the version is read through the privilege drop a "+
			"job takes, so an empty one means that path did not run", got)
	}
}

// EVERY WAY THE IMAGE CAN BE WRONG PRODUCES A FAILING VERDICT THAT NAMES THE STEP.
//
// THE STEP IS THE POINT, not merely the verdict. billet terminates the verifier as
// soon as it has read a block, so the console is gone by the time an operator
// looks; a report that says only "fail" sends them to buy another builder to find
// out what failed. And the label is a token billet itself wrote into the script,
// which is what makes quoting it back safe.
func TestTheVerifierRefusesEveryBrokenImage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		probe    verifyProbe
		wantStep string
	}{
		{
			// A TOOLCHAIN THAT IS NOT A TOOLCACHE ENTRY. cmake, pwsh, dotnet and
			// clang go on PATH, so the toolcache loop cannot see them and a separate
			// check has to — and it has to name ITSELF, or an operator learns only
			// that something in a twenty-check block failed.
			name:     "a declared toolchain does not run",
			probe:    verifyProbe{brokenToolchain: "/usr/local/bin/cmake"},
			wantStep: "cmake",
		},
		{
			// clang SPECIFICALLY, because it is the one whose presence depends on a
			// symlink the build makes rather than on a package apt installed: the
			// versioned packages are co-installable and none of them owns the bare
			// name. An image can carry clang-18 and have no `clang`.
			name:     "the default clang symlink is missing",
			probe:    verifyProbe{brokenToolchain: "/usr/bin/clang"},
			wantStep: "clang",
		},
		{
			// A GLOBAL THE DECLARATION NAMES. These are the checks that catch a
			// package manager which reported success and installed into a prefix
			// nothing puts on PATH -- npm's default is a toolcache directory, which
			// is exactly how an image shipped with every node module installed and
			// none of their commands runnable.
			name:     "a declared node module provides no command",
			probe:    verifyProbe{brokenGlobal: "tsc"},
			wantStep: "npm tsc",
		},
		{
			name:     "a declared pipx package provides no command",
			probe:    verifyProbe{brokenGlobal: "yamllint"},
			wantStep: "pipx yamllint",
		},
		{
			// THE BUG THIS WHOLE CHANGE IS ABOUT. The in-build gate asserted this
			// against a daemon apt had already started, read the answer from before
			// daemon.json existed, and failed every build on a correct image.
			name:     "the image store is the containerd one",
			probe:    verifyProbe{dockerDriver: "overlayfs"},
			wantStep: "docker",
		},
		{
			// A PATH BOUNDARY, NOT A PREFIX. The cache attaches its filesystem at
			// /var/lib/docker, so a data root beside it publishes with no images in
			// it and nothing errors.
			name:     "the docker data root is somewhere else",
			probe:    verifyProbe{dockerRoot: "/var/lib/docker-elsewhere"},
			wantStep: "docker",
		},
		{
			// THE FILE, NOT THE DAEMON. It is what the snapshot preserved and
			// therefore what an instance launched from the image starts with, so a
			// daemon that is right today and a file that is wrong is an image whose
			// NEXT boot loses the cache — which is precisely the nine-day silent
			// failure the contract exists for.
			name:     "the daemon configuration the image carries is wrong",
			probe:    verifyProbe{badDaemonJSON: true},
			wantStep: "docker",
		},
		{
			// THE DAEMON IS UP AND THE JOB CANNOT REACH IT. Every value the other
			// docker cases assert is correct here; what is missing is the runner
			// account's membership of the docker group, which no check running as
			// root can see — and which the in-build gate structurally cannot ask.
			//
			// THE FAKE SERVES ROOT PERFECTLY, so this case can only fail if the
			// assertion went through the privilege drop. Asking as root would find a
			// working daemon and the verdict would be ok.
			name:     "the daemon serves root and refuses the runner",
			probe:    verifyProbe{dockerDeniedToRunner: true},
			wantStep: "docker",
		},
		{
			// A NUMBER IS NOT AN ANSWER. df returning something is what the first
			// version of this check proved; an image whose filesystem was never
			// grown onto its volume reports a perfectly good number and fails every
			// job on ENOSPC.
			name:     "the filesystem was never grown onto the volume",
			probe:    verifyProbe{freeKiB: 900 * 1024},
			wantStep: "disk",
		},
		{
			name:     "the runner does not execute",
			probe:    verifyProbe{brokenRunner: true},
			wantStep: "runner",
		},
		{
			// A DECLARED LINE THE IMAGE DOES NOT CARRY. The build gate would have
			// caught this too; what only the artifact can answer is whether the
			// entry is reachable as the runner account under `env -i`, and this
			// exercises the same path.
			name:     "a declared node line is missing",
			probe:    verifyProbe{fixture: toolcacheFixture{omitNode: firstNodeGlob(t)}},
			wantStep: "node " + firstNodeGlob(t),
		},
		{
			name: "a per-version JAVA_HOME names nothing",
			probe: verifyProbe{
				fixture: toolcacheFixture{versionedJDKPath: "/usr/lib/jvm/nothing"},
			},
			wantStep: "JAVA_HOME_" + firstJavaVersion(t) + "_X64",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report, stdout := tc.probe.run(t)

			if got := report[reportVerdictKey]; got != "fail" {
				t.Fatalf("a broken image reported %s=%q; it would have been stamped with a "+
					"contract it does not meet\n--- it printed ---\n%s",
					reportVerdictKey, got, stdout)
			}

			if got := report[reportStepKey]; got != tc.wantStep {
				t.Errorf("the failure is reported at step %q, want %q; an operator reads this "+
					"instead of a console that no longer exists", got, tc.wantStep)
			}
		})
	}
}

// A MACHINE WITH NO WRITABLE CONSOLE STILL PRODUCES ITS REPORT.
//
// The report goes to /dev/console because that is the device EC2 captures, and a
// write that fails there would otherwise take the whole verdict with it — the
// script would abort inside its own trap and print nothing anywhere.
//
// WHAT THIS PROVES IS THE LOCAL HALF, AND NOT MORE. It asserts that the fallback
// runs and produces a block billet can parse. Whether cloud-init's stdout reaches
// the console EC2 captures is a property of the image's cloud-init configuration,
// which no unit test can answer and which the live verification run is where it
// belongs — the production comment says as much about that channel, and this test
// deliberately does not claim it.
func TestTheReportSurvivesAnUnwritableConsole(t *testing.T) {
	t.Parallel()

	report, _ := verifyProbe{unwritableConsole: true}.run(t)

	if got := report[reportVerdictKey]; got != "ok" {
		t.Errorf("%s=%q with an unwritable console", reportVerdictKey, got)
	}
}

// THE SCRIPT FITS IN USER DATA, WITH ROOM.
//
// packUserData is the real gate for the builder's script and it is the real gate
// here too. This one is far smaller — it carries the assertions and none of the
// installers — and saying so with a number is what turns a future gate that
// doubles in size into a failing test rather than a launch that AWS refuses.
func TestTheVerifierScriptIsDeliverable(t *testing.T) {
	t.Parallel()

	for _, arch := range []string{"x64", "arm64"} {
		script, _, err := verifyScript(arch, AMIContract, verifyProbeNonce)
		if err != nil {
			t.Fatalf("verifyScript(%s): %v", arch, err)
		}

		packed, err := packUserData(script)
		if err != nil {
			t.Fatalf("packUserData(%s): %v", arch, err)
		}

		// PLAIN, NOT COMPRESSED, WHICH IS WORTH ASSERTING RATHER THAN HOPING. A
		// script under the limit travels as text an operator can read straight out
		// of describe-instance-attribute while diagnosing a verification, and this
		// one has no reason to grow past it.
		if len(packed) != len(script) {
			t.Errorf("the %s verification script is %d bytes and had to be compressed to fit; "+
				"it should be small enough to read", arch, len(script))
		}

		// AND IT KEEPS A RESERVE. Most of this script is one line per declared
		// toolcache version, so it grows when GitHub's declaration does — and the
		// failure at the ceiling is not a compile error, it is a launch AWS refuses
		// with a parameter error naming neither the script nor why. 4 KiB is what
		// the next several declared lines would cost; the measured size when this
		// was written was 9418 bytes for x64.
		// AND WHEN IT FAILS, IT NAMES THE LIKELY CAUSE RATHER THAN THIS FILE. Most
		// of this script is writeToolcacheGate's output, which is SHARED with the
		// in-build gate -- so a check added to the declaration two files away lands
		// here and consumes the reserve, while the failure reads as though
		// verifyscript.go grew. Measured: the toolchains, the globals and the
		// PowerShell and Android checks cost 2006 bytes of it.
		if reserve := 4 << 10; len(script) > maxUserData-reserve {
			t.Errorf("the %s verification script is %d bytes and leaves under %d of EC2's %d "+
				"limit.\n\nMost of this script is writeToolcacheGate's output, which the "+
				"in-build gate shares — so the cause is usually a check added there or a "+
				"section added to the pinned declaration, not a change to verifyscript.go. "+
				"Either shrink what the gate emits (the billet_need and billet_psmod "+
				"pattern is ~20 bytes a check against ~105 written out), or stage this "+
				"script's payload the way the provisioning script's is — noting that it "+
				"must stay UNCOMPRESSED, because an operator reads it out of "+
				"describe-instance-attribute exactly when a verification has failed",
				arch, len(script), reserve, maxUserData)
		}
	}
}

// A NONCE THAT IS NOT ONE IS REFUSED, and an architecture billet does not build
// for is refused before a machine is bought rather than after.
func TestTheVerifierScriptRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		arch  string
		nonce string
	}{
		{name: "an injected nonce", arch: "x64", nonce: "'; poweroff; echo '"},
		{name: "a short nonce", arch: "x64", nonce: "abc"},
		{name: "an empty nonce", arch: "x64", nonce: ""},
		{name: "an architecture with no image", arch: "riscv64", nonce: verifyProbeNonce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := verifyScript(tc.arch, AMIContract, tc.nonce); err == nil {
				t.Errorf("verifyScript accepted arch=%q nonce=%q", tc.arch, tc.nonce)
			}
		})
	}
}

// firstNodeGlob is the declaration's first node line, so a fixture naming one
// cannot drift from what the gate checks.
func firstNodeGlob(t *testing.T) string {
	t.Helper()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, e := range ts.Toolcache {
		if e.Name == "node" && len(e.Versions) > 0 {
			return e.Versions[0]
		}
	}

	t.Fatal("the declaration names no node versions")

	return ""
}

// ZERO GATE OPTIONS ARE THE BUILD'S BEHAVIOUR, IN BOTH DIRECTIONS.
//
// The toolcache gate grew two knobs so the artifact check could run each tool
// through the runner's privilege drop and name the line it is checking. Neither
// may touch the in-build script: its compressed size is what decides whether a CA
// bundle fits at all, and every measurement in this package was taken against
// exactly those bytes. (Confirmed once by generating the whole provisioning
// script from this branch and from main and diffing: byte-identical.)
//
// BOTH DIRECTIONS, because a step assignment that never emitted anything and a
// RunPrefix that was ignored would also leave the build alone — and would quietly
// make the verification's two distinguishing properties decoration.
func TestZeroGateOptionsLeaveTheBuildScriptAlone(t *testing.T) {
	t.Parallel()

	build := gateBlock(t, mustScript(t))

	verify, _, err := verifyScript("x64", AMIContract, verifyProbeNonce)
	if err != nil {
		t.Fatalf("verifyScript: %v", err)
	}

	artifact := gateBlock(t, verify)

	// THE INVOCATION LINE IS THE ONE THAT MATTERS. In the build it runs whatever
	// the builder's root shell can see; in the artifact it goes through the same
	// setpriv the image's entry point uses.
	if !strings.Contains(build, "    \"$billet_entry/") {
		t.Errorf("the in-build gate no longer runs a tool unprefixed:\n%s", build)
	}

	if strings.Contains(build, verifyRunAsRunner) {
		t.Errorf("the in-build gate goes through %s; the builder is root with an inherited "+
			"environment and every measurement here was taken without it", verifyRunAsRunner)
	}

	if strings.Contains(build, verifyStepVar) {
		t.Errorf("the in-build gate assigns %s; nothing reads it there and it costs the "+
			"user-data budget the CA bundle competes for", verifyStepVar)
	}

	if !strings.Contains(artifact, "    "+verifyRunAsRunner+" \"$billet_entry/") {
		t.Errorf("the artifact gate does not run its tools as the runner, so it asks the same "+
			"question the build already asked:\n%s", artifact)
	}

	// THE LABEL IS COMPUTED NOW, NOT WRITTEN OUT, so this asks for both halves.
	//
	// It used to assert `billet_step='node ` -- a literal assignment per call site.
	// The repeated shape is an emitted function, so the assignment happens once
	// from its arguments and the call site carries the name and the glob. Either
	// half alone is satisfiable by an accident: a function that assigns from $1 $2
	// proves nothing if no call passes a declared line, and a call passing one
	// proves nothing if the label is never assigned. Together they are the same
	// property the original asserted -- a failure can say WHICH declared line.
	if !strings.Contains(artifact, verifyStepVar+"=\"$1 $2\"") {
		t.Errorf("the artifact gate never assigns %s from the line it was handed, so a "+
			"failure cannot say which one:\n%s", verifyStepVar, artifact)
	}

	if !strings.Contains(artifact, "billet_tc node ") {
		t.Errorf("the artifact gate does not check a declared node line by name, so the "+
			"label above is assigned from nothing:\n%s", artifact)
	}
}

// shellOnlyWords are command words `env` can never execute.
//
// THE RULE IS "IT MUST BE A FILE", AND THIS IS THE PRACTICAL ENCODING OF IT.
// Anything handed to the privilege drop is exec'd by `env`, which resolves
// executables and nothing else — so a shell builtin, a shell keyword or a shell
// function fails there however well it works in an ordinary script.
//
// A DENYLIST RATHER THAN AN ALLOWLIST, because a bare command word is legitimate:
// `docker` is a file on /usr/bin and env finds it on the PATH privilegeDrop
// states. An allowlist of "absolute path, a variable holding one, or sh" would
// reject that correct line, and a rule that fails correct code is one somebody
// deletes.
//
// `echo`, `printf`, `test`, `kill`, `true` and `pwd` are deliberately ABSENT even
// though every shell builds them in: they are also real files on Ubuntu, so env
// executes them and the rule is not broken. What is listed is what has no file
// behind it.
var shellOnlyWords = map[string]bool{
	// POSIX special builtins.
	"break": true, ":": true, "continue": true, ".": true, "eval": true,
	"exec": true, "exit": true, "export": true, "readonly": true, "return": true,
	"set": true, "shift": true, "times": true, "trap": true, "unset": true,
	// Regular builtins with no file behind them on Ubuntu.
	"alias": true, "bg": true, "cd": true, "command": true, "fc": true,
	"fg": true, "getopts": true, "hash": true, "jobs": true, "local": true,
	"read": true, "source": true, "type": true, "ulimit": true, "umask": true,
	"unalias": true, "wait": true,
	// Keywords.
	"case": true, "do": true, "done": true, "elif": true, "else": true,
	"esac": true, "fi": true, "for": true, "function": true, "if": true,
	"in": true, "select": true, "then": true, "until": true, "while": true,
	"!": true, "{": true, "}": true,
}

// NOTHING RUN AS THE RUNNER MAY BE A SHELL BUILTIN.
//
// The privilege drop is `setpriv … env -i … "$@"`, and `env` execs a FILE. A
// builtin handed to it fails with "No such file or directory" and exit 127 — on
// the image. Not on a Mac: /usr/bin/command EXISTS there (one of Apple's builtin
// stubs), so `env -i … command -v sh` answers /bin/sh and exit 0, while the same
// line on ubuntu:24.04 answers `env: 'command': No such file` and exit 127. Both
// measured.
//
// THAT IS WHY THIS IS A STRUCTURAL TEST AND NOT ANOTHER PROBE CASE. The probe
// executes the real script and would catch it — it did catch exactly this bug on
// Linux CI, in a neighbouring branch — but only on Linux, and the machine this is
// written on is not that. A rule about the emitted text costs no platform.
//
// AND IT ASSERTS A COUNT. A prefix that stopped being emitted at all would
// satisfy every case in the loop by never entering it, which is the
// vacuous-by-emptiness shape this package keeps finding.
func TestNothingRunAsTheRunnerIsAShellBuiltin(t *testing.T) {
	t.Parallel()

	script, _, err := verifyScript("x64", AMIContract, verifyProbeNonce)
	if err != nil {
		t.Fatalf("verifyScript: %v", err)
	}

	const prefix = verifyRunAsRunner + " "

	seen := 0

	for rest := script; ; {
		i := strings.Index(rest, prefix)
		if i < 0 {
			break
		}

		rest = rest[i+len(prefix):]

		// THE FUNCTION'S OWN DEFINITION IS NOT A CALL. It is emitted as
		// `billet_as_runner() {`, which this would otherwise read as a call whose
		// command word is `{`.
		if strings.HasPrefix(rest, "{") {
			continue
		}

		word := rest
		if j := strings.IndexAny(word, " \t\n"); j >= 0 {
			word = word[:j]
		}

		seen++

		if shellOnlyWords[strings.Trim(word, `"'`)] {
			t.Errorf("the verification script runs %q through the privilege drop, and `env` "+
				"cannot exec a shell builtin: it fails with exit 127 on the image and works "+
				"on a Mac, where /usr/bin/%s happens to exist. Wrap it: %s sh -c 'command -v "+
				"\"$0\"' <cmd>", word, word, verifyRunAsRunner)
		}
	}

	// THE SITES AT THE TIME OF WRITING: the runner version twice, docker ps, the
	// toolcache entry, the two JAVA_HOME checks, the toolchains that are not
	// toolcache entries (cmake, pwsh, dotnet, clang, clang++), the default
	// runtimes, the globals through billet_need, and the PowerShell modules
	// through billet_psmod. Asserting the floor rather than the exact number
	// leaves room for a check to be added without editing a test, while still
	// refusing a script that dropped the drop entirely.
	//
	// THE FLOOR SITS AT THE CURRENT COUNT, not below it. A floor under the real
	// number lets that many sites be deleted with the test still green, which is
	// the vacuous-by-emptiness shape the count exists to prevent, arrived at by
	// neglect rather than design. At the exact count it still permits an addition
	// -- seventeen is not fewer than sixteen -- and refuses a removal, which is the
	// asymmetry worth having.
	//
	// IT WENT DOWN ONCE, LEGITIMATELY, AND THAT IS WORTH KNOWING BEFORE SOMEBODY
	// LOWERS IT AGAIN. Compacting the repeated JDK-variable block into one emitted
	// shell function turned five textual drop sites into one that runs five times:
	// the same checks, executed the same number of times, through fewer lines. This
	// counts LINES, so it fell from 21 to 16 while nothing stopped being asked.
	// Lowering it is correct only alongside a compaction of that kind, and never
	// because a check was removed.
	if seen < 16 {
		t.Errorf("only %d command(s) go through the privilege drop; the artifact gate's whole "+
			"point is that a job reaches these as the runner, so a script emitting fewer than "+
			"sixteen has stopped asking the question rather than passing it", seen)
	}
}
