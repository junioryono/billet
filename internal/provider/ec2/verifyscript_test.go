package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/guestassets"
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

// variableLine is a component's variable line as the job's environment carries it:
// line with suffix after it, nothing when it is dropped, and another value when it
// is wrong: a directory the fixture makes, so only the value's comparison can
// refuse it.
func variableLine(line, suffix, drop, wrong string) string {
	name, _, _ := strings.Cut(line, "=")

	switch name {
	case drop:
		return ""
	case wrong:
		return name + "=/usr\n"
	default:
		return line + suffix + "\n"
	}
}

// writeFile writes a fixture file with a mode.
func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeExecutableFile writes a fake the verifier executes, through
// forkSafeWriteFile: these probes run in parallel, and a fork that inherits an
// open write descriptor makes the exec fail with ETXTBSY or fall back down PATH.
func writeExecutableFile(t *testing.T, path, body string) {
	t.Helper()

	if err := forkSafeWriteFile(path, []byte(body)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// execEnvForTest is what a test substitutes for execEnvPath: the real
// guestassets helper, run by dash, the image's /bin/sh, so no test writes an
// executable.
func execEnvForTest(t *testing.T) string {
	t.Helper()

	path, err := filepath.Abs(filepath.Join("..", "..", "guestassets", "exec-env.sh"))
	if err != nil {
		t.Fatalf("locate exec-env.sh: %v", err)
	}

	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Fatalf("no dash on PATH; the image runs billet-exec-env under dash: %v", err)
	}

	return dash + " " + path
}

// verifyProbe is one run of the emitted verification script against a fake
// machine.
type verifyProbe struct {
	// arch is the image's architecture; empty is x64.
	arch string
	// dockerDriver is what the fake daemon reports; empty means overlay2.
	dockerDriver string
	// dockerRoot is where the fake daemon says its data lives; empty means
	// /var/lib/docker.
	dockerRoot string
	// brokenToolchain, when set to one of the architecture's toolchains, makes
	// exactly that command exit non-zero; missingToolchain leaves it off the
	// runner's PATH. One at a time, because the point is which step gets reported.
	brokenToolchain  string
	missingToolchain string
	// components is the image's components record; empty is a record of none.
	components string
	// noComponentsRecord leaves the record out of the image entirely.
	noComponentsRecord bool
	// missingComponentPath is one path of a recorded component the image lacks,
	// and unreadableComponentDir one of its directories the runner cannot read.
	missingComponentPath   string
	unreadableComponentDir string
	// noCodeQLBundle leaves a recorded CodeQL's toolcache directory with no bundle
	// in it that runs, and failingExecutable is a recorded executable that exists
	// and fails when run.
	noCodeQLBundle    bool
	failingExecutable string
	// closedExecutable is a recorded executable the runner may not run.
	closedExecutable string
	// recordTail follows the record's line in the file.
	recordTail string
	// strayComponent is a component the image carries and does not record, and
	// strayVariable a variable of one the job's environment names.
	strayComponent string
	strayVariable  string
	// noAbsenceEnv leaves the job's environment unreadable where the absence check
	// reads it.
	noAbsenceEnv bool
	// dropVariable is a recorded component's variable the job's environment leaves
	// out, and wrongVariable one it points somewhere else; overriddenVariable one a
	// later line of the file sets again, and unterminatedVariable one that is the
	// file's last line with no newline, which the runner's environment drops.
	dropVariable         string
	wrongVariable        string
	overriddenVariable   string
	unterminatedVariable string
	// climbingVariable is a versioned variable that starts with its prefix and
	// climbs back out of it, to /opt/ndk, which the fixture makes an NDK, so only
	// the climb can refuse it.
	climbingVariable string
	// noNDK leaves out the NDK a recorded Android SDK's variables name, and
	// closedNDK leaves it where the runner cannot enter it.
	noNDK     bool
	closedNDK bool
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
	// entryPoint replaces the entry point the image carries; empty is the one
	// the build writes.
	entryPoint string
	// entryPointMode is the entry point's mode; zero is the build's 0755.
	entryPointMode os.FileMode
	// execEnv replaces the helper the image carries; empty is guestassets' own.
	execEnv string
}

// legacyEntryPoint is the launch an image built before contract 3 carries: the
// helper may be present, and the registration is still on env's command line.
const legacyEntryPoint = "#!/bin/sh\nset -eu\n" +
	"setpriv --reuid=runner --regid=runner --init-groups \\\n" +
	"  env -i PATH=/usr/bin:/bin HOME=/home/runner \\\n" +
	"  ACTIONS_RUNNER_INPUT_JITCONFIG=\"$ACTIONS_RUNNER_INPUT_JITCONFIG\" \\\n" +
	"  /opt/actions-runner/billet-runner-service\n"

// run emits the script, rewrites its absolute paths onto a fake machine, executes
// it, and returns what billet's own parser makes of the console.
//
// EXECUTED, NOT PATTERN-MATCHED. Every substring an assertion about the TEXT
// could look for survives edits that make the check toothless — dropping the `-e`
// from jq, rendering a value without comparing it, deleting an arm's exit. This
// package learned that on the image-store gate and the rule is the same here.
func (p verifyProbe) run(t *testing.T) (report map[string]string, stdout string) {
	t.Helper()

	arch := p.arch
	if arch == "" {
		arch = "x64"
	}

	script, schema, err := verifyScript(arch, AMIContract, verifyProbeNonce)
	if err != nil {
		t.Fatalf("verifyScript: %v", err)
	}

	root := t.TempDir()

	// THE TOOLCACHE THE REPORT LISTS, one entry, so an inventory that reads the
	// wrong directory comes back empty.
	tcDir := filepath.Join(root, "hostedtoolcache")
	if err := os.MkdirAll(filepath.Join(tcDir, "node", "22.11.0"), 0o755); err != nil {
		t.Fatalf("make the toolcache: %v", err)
	}

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

	// ONE FAKE MACHINE, NAMED ONCE, so the root-side PATH and the runner-side PATH
	// are the same directory and a fake that is wrong is wrong for both.
	bin := p.fakeBin(t, arch)

	// THE IMAGE'S OWN PATH, THROUGH THE MECHANISM THE SCRIPT ALREADY READS. Under
	// `env -i` the runner gets the fixed PATH privilegeDrop states, and on a real
	// image the toolchains are on the one GitHub's /etc/environment names; here they
	// are scripts in this test's fake bin, reached through /etc/billet-image-env.
	//
	// THE FAKE BIN ALONE, with links to the two system commands the runner side
	// runs: a host that has a toolchain of its own (a Mac has /usr/bin/swift)
	// would otherwise answer for one a case left off the image.
	for _, name := range []string{"sh", "printenv"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("no %s on PATH: %v", name, err)
		}

		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
	}

	// test IS THE SYSTEM'S, BUT THE RUNNER MAY NOT READ, RUN OR WRITE WHAT
	// closed-to-runner LISTS: refused only when the call arrived through the
	// privilege drop, so a check that skipped billet_as_runner passes it, and only
	// for those predicates, as a real mode is, so -d or -L of a closed directory
	// still answers and a check that asked only those passes it too.
	// BY ABSOLUTE PATH, because the runner's PATH is the fake bin alone.
	realTest, err := exec.LookPath("test")
	if err != nil {
		t.Fatalf("no test on PATH: %v", err)
	}

	realGrep, err := exec.LookPath("grep")
	if err != nil {
		t.Fatalf("no grep on PATH: %v", err)
	}

	closed := filepath.Join(root, "closed-to-runner")
	writeExecutableFile(t, filepath.Join(bin, "test"), "#!/bin/sh\n"+
		"if [ -n \"${"+droppedMarker+":-}\" ] && [ -f '"+closed+"' ]; then\n"+
		"  case \"$1\" in -r|-x|-w) '"+realGrep+"' -qxF -- \"$2\" '"+closed+"' && exit 1 ;; esac\n"+
		"fi\n"+
		"exec '"+realTest+"' \"$@\"\n")

	// THE JOB'S ENVIRONMENT CARRIES WHAT EACH RECORDED COMPONENT'S INSTALLER SET,
	// but the variable a case drops or points elsewhere.
	env := "PATH=" + bin + "\n"

	for _, item := range strings.Split(p.components, ",") {
		name, _, _ := strings.Cut(item, ":")

		for _, line := range componentVariables[name] {
			env += variableLine(line, "", p.dropVariable, p.wrongVariable)
		}

		for _, line := range componentVariablePrefixes[name] {
			env += variableLine(line, "27.3.13750724", p.dropVariable, p.wrongVariable)
		}
	}

	if p.overriddenVariable != "" {
		env += p.overriddenVariable + "=/usr\n"
	}

	if p.climbingVariable != "" {
		env += p.climbingVariable + "=/usr/local/lib/android/sdk/ndk/../../../../../../opt/ndk\n"
	}

	if p.unterminatedVariable != "" {
		var kept []string

		last := ""

		for _, line := range strings.SplitAfter(env, "\n") {
			if strings.HasPrefix(line, p.unterminatedVariable+"=") {
				last = strings.TrimSuffix(line, "\n")
				continue
			}

			kept = append(kept, line)
		}

		env = strings.Join(kept, "") + last
	}

	envFile := filepath.Join(root, "billet-image-env")
	writeFile(t, envFile, env, 0o644)

	// THE SAME FILE UNDER billet_root, where the absence check reads it: the
	// substitution below moves the check's imageEnvFile to envFile as well.
	croot := filepath.Join(root, "c")
	if err := os.MkdirAll(filepath.Dir(croot+envFile), 0o755); err != nil {
		t.Fatal(err)
	}

	stray := ""
	if p.strayVariable != "" {
		stray = p.strayVariable + "=/somewhere\n"
	}

	if !p.noAbsenceEnv {
		writeFile(t, croot+envFile, env+stray, 0o644)
	}

	script = substitute(t, script, "du -sk "+toolcacheDir, "du -sk "+tcDir)
	script = substitute(t, script, "ls "+toolcacheDir+"/", "ls "+tcDir+"/")
	script = substitute(t, script, imageEnvFile, envFile)

	// THE COMPONENTS RECORD, AND WHAT EACH RECORDED COMPONENT LEAVES, under a root
	// of its own: the script's runner-side tests are rewritten onto it, and the
	// fixture builds the paths of every recorded component but the one a case
	// breaks.
	record := filepath.Join(root, "billet-image-components")
	if !p.noComponentsRecord {
		writeFile(t, record, p.components+"\n"+p.recordTail, 0o644)
	}

	script = substitute(t, script, componentsFile, record)

	for _, item := range strings.Split(p.components, ",") {
		name, _, _ := strings.Cut(item, ":")

		for _, entry := range componentPaths[name] {
			kind, path, _ := strings.Cut(entry, " ")
			if path == p.missingComponentPath {
				continue
			}

			if kind == "d" {
				if err := os.MkdirAll(croot+path, 0o755); err != nil {
					t.Fatalf("make %s: %v", path, err)
				}

				// CLOSED TO THE RUNNER ALONE, so only a check made as the runner
				// refuses it.
				if path == p.unreadableComponentDir {
					writeFile(t, filepath.Join(root, "closed-to-runner"), croot+path+"\n", 0o644)
				}

				continue
			}

			if err := os.MkdirAll(filepath.Dir(croot+path), 0o755); err != nil {
				t.Fatalf("make %s: %v", path, err)
			}

			// ONLY THE RUNNER GETS AN ANSWER, as the toolchains' stand-ins give it, so
			// a check that skipped the privilege drop fails the passing case.
			body := "#!/bin/sh\n[ -n \"${" + droppedMarker + ":-}\" ] || exit 1\nexit 0\n"
			if path == p.failingExecutable {
				body = "#!/bin/sh\nexit 1\n"
			}

			writeExecutableFile(t, croot+path, body)

			// CLOSED TO THE RUNNER ALONE, as an executable only root may run is.
			if path == p.closedExecutable {
				writeFile(t, filepath.Join(root, "closed-to-runner"), croot+path+"\n", 0o644)
			}
		}
	}

	// THE NDK THE VERSIONED VARIABLES NAME, unless a case removes it.
	if strings.Contains(p.components, "android-sdk:") && !p.noNDK {
		ndk := filepath.Join(croot, "usr", "local", "lib", "android", "sdk", "ndk", "27.3.13750724")
		if err := os.MkdirAll(ndk, 0o755); err != nil {
			t.Fatal(err)
		}

		writeFile(t, filepath.Join(ndk, "source.properties"), "Pkg.Revision = 27.3.13750724\n", 0o644)

		if p.climbingVariable != "" {
			elsewhere := filepath.Join(croot, "opt", "ndk")
			if err := os.MkdirAll(elsewhere, 0o755); err != nil {
				t.Fatal(err)
			}

			writeFile(t, filepath.Join(elsewhere, "source.properties"), "Pkg.Revision = 1.0\n", 0o644)
		}

		// CLOSED TO THE RUNNER, as a root-owned 0700 directory is to a job, and
		// to the runner alone, so only a check made as the runner refuses it.
		if p.closedNDK {
			writeFile(t, filepath.Join(root, "closed-to-runner"), filepath.Join(ndk, "source.properties")+"\n", 0o644)
		}
	}

	if strings.Contains(p.components, "codeql:") && !p.noCodeQLBundle {
		bundle := filepath.Join(croot, "opt", "hostedtoolcache", "CodeQL", "2.23.1", "x64", "codeql")
		if err := os.MkdirAll(bundle, 0o755); err != nil {
			t.Fatal(err)
		}

		writeExecutableFile(t, filepath.Join(bundle, "codeql"), "#!/bin/sh\n[ -n \"${"+droppedMarker+":-}\" ] || exit 1\n"+
			"[ \"$1\" = version ]\n")
	}

	if p.strayComponent != "" {
		_, stray, _ := strings.Cut(componentPaths[p.strayComponent][0], " ")
		if err := os.MkdirAll(filepath.Dir(croot+stray), 0o755); err != nil {
			t.Fatal(err)
		}

		writeFile(t, croot+stray, "#!/bin/sh\nexit 0\n", 0o755)
	}

	script = substitute(t, script, "billet_root=''", "billet_root='"+croot+"'")

	// THE HELPER AS THE IMAGE CARRIES IT, an executable file whose bytes the
	// verifier compares, so it is guestassets' own unless a case tampers with it.
	helper := p.execEnv
	if helper == "" {
		helper = guestassets.ExecEnvScript
	}
	helperFile := filepath.Join(root, "billet-exec-env")
	if err := forkSafeWriteFile(helperFile, []byte(helper)); err != nil {
		t.Fatalf("write the helper: %v", err)
	}
	script = substitute(t, script, execEnvPath, helperFile)

	// THE BUILD'S OWN BYTES, out of the layer rather than the function the
	// verifier compares against, so the two are proved to agree.
	entry := p.entryPoint
	if entry == "" {
		entry = entryPoint(t, mustScript(t))
	}
	mode := p.entryPointMode
	if mode == 0 {
		mode = 0o755
	}
	entryFile := filepath.Join(root, "billet-runner")
	writeFile(t, entryFile, entry, mode)
	script = substitute(t, script, entryPointPath, entryFile)
	script = substitute(t, script, "/opt/actions-runner", runnerDir)
	script = substitute(t, script, "/dev/console", console)

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

// fakeBin is the machine the script thinks it is running on.
//
// THE COMMANDS, NOT THE FILESYSTEM. docker, jq and realpath speak about paths a
// test cannot create — /var/lib/docker, /etc/docker/daemon.json — so they answer
// from a fixture instead, exactly as the in-build image-store test does. sleep
// and poweroff are stubbed because the dwell is twenty-four rounds of twenty
// seconds and this is a unit test.
func (p verifyProbe) fakeBin(t *testing.T, arch string) string {
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
		// A REAL DIGEST, NOT A FAKE ONE: Ubuntu's coreutils carries sha256sum and
		// a Mac's base system carries only shasum, which prints the same line.
		"sha256sum": "#!/bin/sh\nexec shasum -a 256 \"$@\"\n",
		"jq":        "#!/bin/sh\nexit " + jqStatus + "\n",
		"sleep":     "#!/bin/sh\nexit 0\n",
		"poweroff":  "#!/bin/sh\nexit 0\n",
	} {
		writeExecutableFile(t, filepath.Join(dir, name), body)
	}

	// THE TOOLCHAINS, each answering only through the privilege drop: one found as
	// root and not as the runner would otherwise pass a check that asked the wrong
	// account. Each is present unless this probe breaks exactly one of them.
	for _, tc := range toolchainsFor(arch) {
		if p.missingToolchain == tc.name {
			continue
		}

		status := "0"
		if p.brokenToolchain == tc.name {
			status = "1"
		}

		writeExecutableFile(t, filepath.Join(dir, tc.name), "#!/bin/sh\n"+
			"[ -n \"${"+droppedMarker+":-}\" ] || exit 1\n"+
			"exit "+status+"\n")
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

	for _, arch := range []string{"x64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			t.Parallel()

			components := "firefox:140.0.4"
			if arch == "x64" {
				components += ",codeql:2.23.1,android-sdk:12.0"
			}

			report, stdout := verifyProbe{arch: arch, components: components}.run(t)

			if got := report[reportVerdictKey]; got != "ok" {
				t.Fatalf("a complete image reported %s=%q at step %q, so every build would fail\n"+
					"--- it printed ---\n%s", reportVerdictKey, got, report[reportStepKey], stdout)
			}

			if got := report[reportStepKey]; got != "done" {
				t.Errorf("the passing report stopped at step %q, want done", got)
			}

			// AND THE FACTS ARE IN IT. A verdict with an empty report is a check nobody
			// can act on, and every one of these is a value the operator asked for by
			// hand.
			for _, key := range []string{
				"root_free_kib", "root_total_kib", "docker_driver", "docker_root", "runner",
				"toolcache_kib", "tc_node",
			} {
				if report[key] == "" {
					t.Errorf("the report carries no %s: %v", key, report)
				}
			}

			if got := report["docker_driver"]; got != "overlay2" {
				t.Errorf("docker_driver=%q, want overlay2", got)
			}

			if got := report["runner"]; got != "2.328.0" {
				t.Errorf("runner=%q, want 2.328.0: the version is read through the privilege "+
					"drop a job takes, so an empty one means that path did not run", got)
			}

			if got := report[buildComponentsKey]; got != components {
				t.Errorf("%s=%q, want the record the image carries", buildComponentsKey, got)
			}
		})
	}
}

// AN IMAGE THAT RECORDS NO COMPONENTS PASSES AND SAYS SO: none accepted is an
// answer, and the report carries it as an empty record rather than a missing one.
func TestTheVerifierReportsAnEmptyComponentsRecord(t *testing.T) {
	t.Parallel()

	report, stdout := verifyProbe{}.run(t)

	if got := report[reportVerdictKey]; got != "ok" {
		t.Fatalf("an image completed with nothing reported %s=%q at %q\n%s",
			reportVerdictKey, got, report[reportStepKey], stdout)
	}

	got, ok := report[buildComponentsKey]
	if !ok || got != "" {
		t.Errorf("%s=%q (present %v), want an empty record", buildComponentsKey, got, ok)
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
		// wantSays is what the refusal must say, where a case is about one reason
		// of the several its step refuses for.
		wantSays string
	}{
		{
			// A TOOLCHAIN A WORKFLOW CALLS BY NAME. GitHub's tests proved it on the
			// builder as root; only a machine booted from the image, asked as the
			// runner under `env -i`, says a job finds it.
			name:     "a toolchain does not run",
			probe:    verifyProbe{brokenToolchain: "rustc"},
			wantStep: "toolchain rustc",
		},
		{
			name:     "a toolchain is not on the runner's PATH",
			probe:    verifyProbe{missingToolchain: "swift"},
			wantStep: "toolchain swift",
		},
		{
			// HASKELL IS x64's ALONE, and an x64 image without it is short of
			// GitHub's.
			name:     "an x64 image has no ghc",
			probe:    verifyProbe{missingToolchain: "ghc"},
			wantStep: "toolchain ghc",
		},
		{
			name:     "an arm64 toolchain does not run",
			probe:    verifyProbe{arch: "arm64", brokenToolchain: "dotnet"},
			wantStep: "toolchain dotnet",
		},
		{
			// NO RECORD IS NOT "NONE": it is an image no contract-4 build finished.
			name:     "the image carries no components record",
			probe:    verifyProbe{noComponentsRecord: true},
			wantStep: "components",
		},
		{
			name: "a recorded component is missing a driver",
			probe: verifyProbe{
				components:           "firefox:140.0.4",
				missingComponentPath: "/usr/bin/geckodriver",
			},
			wantStep: "components",
		},
		{
			// A DIRECTORY IS NOT A COMPONENT: CodeQL's only path is the toolcache
			// directory, which an empty entry satisfies.
			name:     "a recorded CodeQL has no bundle that runs",
			probe:    verifyProbe{components: "codeql:2.23.1", noCodeQLBundle: true},
			wantStep: "components",
		},
		{
			name: "a recorded component's directory is closed to the runner",
			probe: verifyProbe{
				components:             "firefox:140.0.4",
				unreadableComponentDir: "/usr/local/share/gecko_driver",
			},
			wantStep: "components",
			wantSays: "the image records firefox:140.0.4 and does not carry it for the runner",
		},
		{
			name: "a recorded component's executable is closed to the runner",
			probe: verifyProbe{
				components:       "firefox:140.0.4",
				closedExecutable: "/usr/bin/geckodriver",
			},
			wantStep: "components",
			wantSays: "the image records firefox:140.0.4 and does not carry it for the runner",
		},
		{
			name: "a recorded component's executable does not run",
			probe: verifyProbe{
				components:        "firefox:140.0.4",
				failingExecutable: "/usr/bin/geckodriver",
			},
			wantStep: "components",
		},
		{
			// A BASE IMAGE THAT ALREADY HELD ONE: the record of none is a claim the
			// image does not meet.
			name:     "the image carries a component it does not record",
			probe:    verifyProbe{strayComponent: "google-chrome"},
			wantStep: "components",
		},
		{
			// AND ONE IT ONCE CARRIED: the binaries are gone and the job is still
			// told where Chrome is.
			name:     "the job's environment names a component the image does not record",
			probe:    verifyProbe{strayVariable: "CHROME_BIN"},
			wantStep: "components",
		},
		{
			// A COMPONENT A JOB CANNOT FIND THROUGH ITS VARIABLE IS NOT ONE IT HAS.
			name:     "the job's environment does not name a recorded component's variable",
			probe:    verifyProbe{components: "firefox:140.0.4", dropVariable: "GECKOWEBDRIVER"},
			wantStep: "components",
			wantSays: "the image records firefox:140.0.4 and does not carry it for the runner",
		},
		{
			name:     "a recorded component's variable points somewhere else",
			probe:    verifyProbe{components: "firefox:140.0.4", wrongVariable: "GECKOWEBDRIVER"},
			wantStep: "components",
			wantSays: "the image records firefox:140.0.4 and does not carry it for the runner",
		},
		{
			// THE VALUE A JOB RECEIVES, not a line of the file: a later line wins,
			// and a last line without its newline is never read.
			name:     "a later line sets a recorded component's variable again",
			probe:    verifyProbe{components: "firefox:140.0.4", overriddenVariable: "GECKOWEBDRIVER"},
			wantStep: "components",
			wantSays: "the image records firefox:140.0.4 and does not carry it for the runner",
		},
		{
			name:     "a recorded component's variable is a last line the runner never reads",
			probe:    verifyProbe{components: "firefox:140.0.4", unterminatedVariable: "GECKOWEBDRIVER"},
			wantStep: "components",
			wantSays: "the image records firefox:140.0.4 and does not carry it for the runner",
		},
		{
			name:     "a later line sets a recorded component's versioned variable again",
			probe:    verifyProbe{components: "android-sdk:12.0", overriddenVariable: "ANDROID_NDK_HOME"},
			wantStep: "components",
			wantSays: "the image records android-sdk:12.0 and does not carry it for the runner",
		},
		{
			name:     "a recorded component's versioned variable climbs out of its prefix",
			probe:    verifyProbe{components: "android-sdk:12.0", climbingVariable: "ANDROID_NDK_HOME"},
			wantStep: "components",
			wantSays: "the image records android-sdk:12.0 and does not carry it for the runner",
		},
		{
			name:     "a recorded component's versioned variable points somewhere else",
			probe:    verifyProbe{components: "android-sdk:12.0", wrongVariable: "ANDROID_NDK_LATEST_HOME"},
			wantStep: "components",
			wantSays: "the image records android-sdk:12.0 and does not carry it for the runner",
		},
		{
			// UNDER THE PREFIX IS NOT ENOUGH: the NDK it names must be there.
			name:     "a recorded component's versioned variable names nothing",
			probe:    verifyProbe{components: "android-sdk:12.0", noNDK: true},
			wantStep: "components",
			wantSays: "the image records android-sdk:12.0 and does not carry it for the runner",
		},
		{
			// A DIRECTORY THE RUNNER CANNOT ENTER IS NOT AN NDK A JOB CAN USE.
			name:     "a recorded component's NDK is closed to the runner",
			probe:    verifyProbe{components: "android-sdk:12.0", closedNDK: true},
			wantStep: "components",
			wantSays: "the image records android-sdk:12.0 and does not carry it for the runner",
		},
		{
			// AND ONE LEFT BEHIND BY AN SDK THAT IS GONE.
			name:     "the job's environment names an NDK the image does not record",
			probe:    verifyProbe{strayVariable: "ANDROID_NDK"},
			wantStep: "components",
			wantSays: "the image carries android-sdk, which it does not record",
		},
		{
			// AN UNREADABLE ENVIRONMENT IS NOT ONE THAT NAMES NOTHING.
			name:     "the absence check cannot read the job's environment",
			probe:    verifyProbe{noAbsenceEnv: true},
			wantStep: "components",
			wantSays: "the job environment cannot be read",
		},
		{
			// A SECOND LINE IS A REPORT LINE OF THE IMAGE'S CHOOSING.
			name:     "the record runs to a second line",
			probe:    verifyProbe{components: "firefox:140.0.4", recordTail: "runner=secret-token-123\n"},
			wantStep: "components",
		},
		{
			name:     "the record is not a list",
			probe:    verifyProbe{components: "firefox:140.0.4 secret"},
			wantStep: "components",
		},
		{
			name:     "the record names a component billet does not install",
			probe:    verifyProbe{components: "netscape:4.0"},
			wantStep: "components",
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
			// root can see.
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
			// THE HELPER IS ON THE IMAGE AND THE ENTRY POINT DOES NOT USE IT. Every
			// billet_as_runner call passes, because the verifier brings its own
			// launch; only the installed file says what a job does.
			name:     "the entry point launches the runner the old way",
			probe:    verifyProbe{entryPoint: legacyEntryPoint},
			wantStep: "entrypoint",
		},
		{
			// NO REGISTRATION ON A COMMAND LINE, AND NO HELPER EITHER: the runner
			// would start without its environment at all.
			name:     "the entry point does not launch through the helper",
			probe:    verifyProbe{entryPoint: "#!/bin/sh\nexec /opt/actions-runner/billet-runner-service\n"},
			wantStep: "entrypoint",
		},
		{
			// THE LAUNCH LINE IS THERE AND THE PRIVILEGE DROP AROUND IT IS NOT
			// WHOLE: setpriv refuses a primary GID without a supplementary-group
			// option, so every job would fail before the runner started.
			name: "the entry point's privilege drop is damaged",
			probe: verifyProbe{
				entryPoint: strings.Replace(entryPointScript(), " --init-groups", "", 1),
			},
			wantStep: "entrypoint",
		},
		{
			// AND THE STREAM IT HANDS THE HELPER IS EMPTY: the runner would start
			// with no registration and exit, on a machine that looks healthy.
			name: "the entry point hands the helper no environment",
			probe: verifyProbe{
				entryPoint: strings.Replace(entryPointScript(), "${billet_env}end\n", "end\n", 1),
			},
			wantStep: "entrypoint",
		},
		{
			name:     "the entry point is not executable",
			probe:    verifyProbe{entryPointMode: 0o644},
			wantStep: "entrypoint",
		},
		{
			// THE HELPER STILL LAUNCHES EVERY COMMAND, so each billet_as_runner
			// passes; only its bytes say it put a value in an argument list.
			name: "the helper is not this billet's",
			probe: verifyProbe{
				execEnv: strings.Replace(guestassets.ExecEnvScript, "exec 3<&-\n",
					"exec 3<&-\n/bin/echo \"$billet_exec_env_all\" >/dev/null\n", 1),
			},
			wantStep: "entrypoint",
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

			// REFUSED FOR ITS OWN REASON, where a case names one: a step refuses for
			// many, and the case is about one of them.
			if tc.wantSays != "" && !strings.Contains(stdout, tc.wantSays) {
				t.Errorf("the refusal does not say %q\n--- it printed ---\n%s", tc.wantSays, stdout)
			}
		})
	}
}

// EACH COMPONENT THE VERIFIER RECOGNISES IS PROVED BY WHAT check-guest-component.sh
// SAYS IT LEAVES, the list the build's `present` asked: a path one names and the
// other does not is a component proved on the builder and not on the image, or
// the reverse.
func TestTheVerifiersComponentPathsAreTheComponentChecks(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "check-guest-component.sh"))
	if err != nil {
		t.Fatalf("read check-guest-component.sh: %v", err)
	}

	_, body, ok := strings.Cut(string(raw), "\npaths() {\n")
	if !ok {
		t.Fatal("check-guest-component.sh defines no paths()")
	}

	body, _, _ = strings.Cut(body, "\n}\n")

	arm := regexp.MustCompile(`^\t([a-z][a-z-]*)\)`)
	echo := regexp.MustCompile(`echo "([xd] /[^"]+)"`)
	want := map[string][]string{}
	current := ""

	for _, line := range strings.Split(body, "\n") {
		if m := arm.FindStringSubmatch(line); m != nil {
			current = m[1]
		}

		for _, m := range echo.FindAllStringSubmatch(line, -1) {
			want[current] = append(want[current], m[1])
		}
	}

	if len(want) != len(componentPaths) {
		t.Errorf("check-guest-component.sh lists %d components and the verifier %d", len(want), len(componentPaths))
	}

	for name, paths := range want {
		if !slices.Equal(componentPaths[name], paths) {
			t.Errorf("%s: the verifier checks %q and check-guest-component.sh lists %q",
				name, componentPaths[name], paths)
		}
	}
}

// AND THE VARIABLES ARE check-guest-component.sh's, which the build's `present` and
// `absent` asked: both its lists, line for line.
func TestTheVerifiersComponentVariablesAreTheComponentChecks(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "check-guest-component.sh"))
	if err != nil {
		t.Fatalf("read check-guest-component.sh: %v", err)
	}

	for function, verifier := range map[string]map[string][]string{
		"variables": componentVariables,
		"versioned": componentVariablePrefixes,
	} {
		_, body, ok := strings.Cut(string(raw), "\n"+function+"() {\n")
		if !ok {
			t.Fatalf("check-guest-component.sh defines no %s()", function)
		}

		body, _, _ = strings.Cut(body, "\n}\n")

		arm := regexp.MustCompile(`^\t([a-z][a-z |-]*)\)`)
		echo := regexp.MustCompile(`echo "([A-Z_]+=[^"]*)"`)
		want := map[string][]string{}

		var current []string

		for _, line := range strings.Split(body, "\n") {
			if m := arm.FindStringSubmatch(line); len(m) == 2 {
				current = strings.Split(m[1], " | ")
				for _, name := range current {
					if _, ok := want[name]; !ok {
						want[name] = nil
					}
				}
			}

			for _, m := range echo.FindAllStringSubmatch(line, -1) {
				for _, name := range current {
					want[name] = append(want[name], m[1])
				}
			}
		}

		if len(want) != len(verifier) {
			t.Errorf("%s(): check-guest-component.sh lists %d components and the verifier %d",
				function, len(want), len(verifier))
		}

		for name, lines := range want {
			got, ok := verifier[name]
			if !ok || !slices.Equal(got, lines) {
				t.Errorf("%s() %s: the verifier checks %q and check-guest-component.sh lists %q",
					function, name, got, lines)
			}
		}
	}
}

// TWO TOOLS ONE REPORT KEY IS REFUSED BEFORE ANYTHING LAUNCHES: the parser refuses a
// report naming a key twice, so the collision would otherwise end every verification
// with no report.
func TestTwoToolsSharingAReportKeyAreRefused(t *testing.T) {
	t.Parallel()

	ts := runnerimages.Toolset{Toolcache: []runnerimages.ToolcacheEntry{{Name: "Py-Py"}, {Name: "Py_Py"}}}

	if tools, err := toolReports(ts); err == nil || !strings.Contains(err.Error(), "both become the report key") {
		t.Fatalf("two names that share tc_py_py were answered %v, %v", tools, err)
	}

	if _, err := toolReports(runnerimages.Toolset{Toolcache: []runnerimages.ToolcacheEntry{{Name: "PyPy"}, {Name: "Python"}}}); err != nil {
		t.Fatalf("two names with keys of their own were refused: %v", err)
	}
}

// THE TOOLCHAINS ARE EACH ARCHITECTURE'S OWN: Haskell on x64 alone, because
// GitHub's arm64 image has none, and every other one on both.
func TestTheToolchainsAreEachArchitecturesOwn(t *testing.T) {
	t.Parallel()

	names := func(arch string) []string {
		var out []string
		for _, tc := range toolchainsFor(arch) {
			out = append(out, tc.name)
		}

		return out
	}

	x64, arm := names("x64"), names("arm64")

	if !slices.Contains(x64, "ghc") || slices.Contains(arm, "ghc") {
		t.Errorf("ghc is asked of x64=%v arm64=%v; GitHub installs it on x64 alone",
			slices.Contains(x64, "ghc"), slices.Contains(arm, "ghc"))
	}

	for _, want := range []string{"rustc", "cargo", "swift", "go", "node", "python3", "java", "dotnet", "pwsh"} {
		if !slices.Contains(x64, want) || !slices.Contains(arm, want) {
			t.Errorf("%s is not asked of both architectures (x64 %v, arm64 %v)", want, x64, arm)
		}
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
// packUserData is the real gate, and saying so with a number is what turns a
// script that doubles in size into a failing test rather than a launch that AWS
// refuses.
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

		// AND IT KEEPS A RESERVE. The toolcache inventory is one line per tool the
		// declaration names, so it grows when GitHub's declaration does — and the
		// failure at the ceiling is not a compile error, it is a launch AWS refuses
		// with a parameter error naming neither the script nor why.
		if reserve := 4 << 10; len(script) > maxUserData-reserve {
			t.Errorf("the %s verification script is %d bytes and leaves under %d of EC2's %d "+
				"limit; it must stay UNCOMPRESSED, because an operator reads it out of "+
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

	// THE SITES AT THE TIME OF WRITING, ON x64: the runner version twice, docker
	// ps, the ten toolchains, the component functions' bodies (billet_says once,
	// billet_sets three times, billet_x and billet_d twice each), sdkmanager and
	// CodeQL. Asserting the floor rather than the exact number leaves room for a
	// check to be added without editing a test, while still refusing a script that
	// dropped the drop.
	//
	// THE FLOOR SITS AT THE CURRENT COUNT, not below it. A floor under the real
	// number lets that many sites be deleted with the test still green, which is
	// the vacuous-by-emptiness shape the count exists to prevent. It rose from 16
	// to 28 when contract 4 replaced the toolcache gate with the toolchains and the
	// components, and fell to 23 when the component paths moved into billet_x and
	// billet_d, each asked once per path; lowering it is correct only when a check
	// moves, never because one was removed.
	if seen < 23 {
		t.Errorf("only %d command(s) go through the privilege drop; the artifact gate's whole "+
			"point is that a job reaches these as the runner, so a script emitting fewer than "+
			"twenty-three has stopped asking the question rather than passing it", seen)
	}
}
