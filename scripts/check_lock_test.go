package scripts_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireLockTool skips where the machine has no lock tool the script can use,
// except under CI, where a skip would report success for a script nothing
// exercised. It asks the tool the script would choose to take a lock the way the
// script does, because an installed flock too old for -E makes the script fall
// back to running unlocked, which is right for the script and would fail the
// contention test.
func requireLockTool(t *testing.T) {
	t.Helper()

	probe := filepath.Join(t.TempDir(), "probe.lock")

	var err error

	switch {
	case lookPath("lockf"):
		err = exec.CommandContext(t.Context(), "lockf", "-k", "-t", "0", probe, "true").Run()
	case lookPath("flock"):
		err = exec.CommandContext(t.Context(), "flock", "-n", "-E", "75", probe, "true").Run()
	default:
		err = errors.New("neither lockf nor flock is installed")
	}

	if err == nil {
		return
	}

	if os.Getenv("CI") != "" {
		t.Fatalf("no usable lock tool under CI (%v), so with-check-lock.sh would go untested", err)
	}

	t.Skipf("no usable lock tool: %v", err)
}

func lookPath(tool string) bool {
	_, err := exec.LookPath(tool)

	return err == nil
}

// THE COMMAND'S EXIT STATUS IS THE SCRIPT'S, so `make check` failing under the
// lock still fails, and a command that failed is not run again unlocked.
func TestTheCheckLockReturnsTheCommandsStatus(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh",
		filepath.Join(dir, "check.lock"), "sh", "-c", `echo ran >> "$1"; exit 3`, "failing", runs)
	cmd.Env = scriptEnv()

	err := cmd.Run()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("a command exiting 3 under the lock: %v, want exit status 3", err)
	}

	got, err := os.ReadFile(runs)
	if err != nil {
		t.Fatalf("read the command's runs: %v", err)
	}

	if string(got) != "ran\n" {
		t.Errorf("the failing command ran %q, want once", got)
	}
}

// A RUN INSIDE A RUN HOLDING THE SAME LOCK RUNS UNDER IT. A gate that runs
// another gate would otherwise wait for an ancestor that cannot finish until it
// does. Bounded on its own, because the failure is a wait that never ends.
func TestANestedRunRunsUnderItsAncestorsLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	lock := filepath.Join(t.TempDir(), "check.lock")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "./with-check-lock.sh", lock,
		"./with-check-lock.sh", lock, "sh", "-c", "echo inner; exit 7")
	cmd.Env = scriptEnv()
	inGroup(cmd)

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("a run inside a run holding its lock: %v (%s), want the inner command's exit status 7", err, out)
	}

	if !strings.Contains(string(out), "inner") || !strings.Contains(string(out), "running under it") {
		t.Errorf("the nested run did not run its command under its ancestor's lock: %q", out)
	}
}

// A TAKE THAT FAILS AFTER THE PROBE SUCCEEDED STILL RUNS THE COMMAND, unlocked
// and saying so, with the command's own status. The lock tool here answers the
// probe and refuses the take, as one whose file vanished in between would.
func TestAFailedTakeAfterTheProbeStillRunsTheCommand(t *testing.T) {
	t.Parallel()

	// The probe is the only call with -t; every other take is refused before
	// any command runs, as lockf refuses a file it cannot open.
	runOnceUnlocked(t, "", map[string]string{
		"lockf": "#!/bin/sh\ncase \" $* \" in *\" -t \"*) exit 0 ;; esac\necho \"lockf: cannot open\" >&2\nexit 71\n",
	})
}

// NO PRIVATE DIRECTORY FOR THE MARKER IS A COURTESY LOST: without one a failed
// take could not be told from a failed command, so the command runs unlocked,
// once, and says so.
func TestARunWithNoPrivateDirectoryRunsUnlockedOnce(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	runOnceUnlocked(t, filepath.Join(t.TempDir(), "absent", "tmp"), nil)
}

// A MARKER THAT CANNOT BE REMOVED RUNS THE COMMAND ONCE, UNLOCKED: under the
// lock the command runs only after its marker is gone, so a removal that
// failed leaves the marker, the take reads as failed, and the fallback is the
// command's one run. Without that order a failing command would run twice.
func TestAMarkerThatCannotBeRemovedRunsTheCommandOnce(t *testing.T) {
	t.Parallel()

	// This lockf takes nothing and runs its command (after -k and the path);
	// this rm removes nothing.
	runOnceUnlocked(t, "", map[string]string{
		"lockf": "#!/bin/sh\ncase \" $* \" in *\" -t \"*) exit 0 ;; esac\nshift 2\nexec \"$@\"\n",
		"rm":    "#!/bin/sh\nexit 1\n",
	})
}

// runOnceUnlocked runs a command that records its runs and exits 5 under the
// script, with TMPDIR at tmp (a directory of the test's own when empty) and
// fakes standing in front of PATH for the named tools (the script prefers lockf,
// so a fake lockf stands in on Linux too), and requires that the command ran
// exactly once, with its own status, and that the run said it went unlocked.
func runOnceUnlocked(t *testing.T, tmp string, fakes map[string]string) {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	runs := filepath.Join(dir, "runs")

	if tmp == "" {
		tmp = dir
	}

	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatalf("make the fake tools' directory: %v", err)
	}

	for name, script := range fakes {
		if err := forkSafeWriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake %s: %v", name, err)
		}
	}

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh", filepath.Join(dir, "check.lock"),
		"sh", "-c", `echo ran >> "$1"; exit 5`, "command", runs)
	cmd.Env = scriptEnv("PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp)

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 5 {
		t.Fatalf("a command whose lock could not be taken: %v (%s), want its own exit status 5", err, out)
	}

	got, err := os.ReadFile(runs)
	if err != nil {
		t.Fatalf("the command never ran: %v (%s)", err, out)
	}

	if string(got) != "ran\n" {
		t.Errorf("the command ran %q, want once", got)
	}

	if !strings.Contains(string(out), "not serialised with others") {
		t.Errorf("the run did not say it went unlocked: %q", out)
	}
}

// A RUN INSIDE ANOTHER LOCK INSIDE ITS OWN STILL RUNS UNDER ITS OWN: a holds A,
// b inside it holds B, and c inside b asks for A again. Every lock an ancestor
// holds is handed down, not only the nearest.
func TestARunNestedThroughAnotherLockRunsUnderItsOwn(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.lock"), filepath.Join(dir, "b.lock")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "./with-check-lock.sh", a,
		"./with-check-lock.sh", b,
		"./with-check-lock.sh", a, "sh", "-c", "echo innermost; exit 7")
	cmd.Env = scriptEnv()
	inGroup(cmd)

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("a run nested through another lock: %v (%s), want the innermost command's exit status 7", err, out)
	}

	if !strings.Contains(string(out), "innermost") {
		t.Errorf("the innermost command did not run: %q", out)
	}
}

// A LOCK NAMED BY A RUN THAT IS NOT AN ANCESTOR IS NOT HELD FOR THIS ONE: a
// process a gate left behind, still carrying what that gate handed it, waits
// like anyone else once another run holds the lock. Here the entry names the
// holder itself, which this run is not inside.
func TestALockHeldOutsideThisRunsAncestryIsWaitedFor(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	lock := filepath.Join(dir, "check.lock")
	trace := filepath.Join(dir, "trace")
	release := filepath.Join(dir, "release")
	seen := filepath.Join(dir, "seen")

	holder := exec.CommandContext(holderContext(t), "./with-check-lock.sh", lock, "sh", "-c",
		holdUntilReleased+`echo done >> "$1"`, "holder", trace, release)
	holder.Env = scriptEnv()
	inGroup(holder)

	if err := holder.Start(); err != nil {
		t.Fatalf("start the holder: %v", err)
	}

	releaseAndReap(t, holder, release)
	waitForLockFile(t, trace, "the holder never took the lock")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	recorder, takes := recordTakes(t)

	stale := exec.CommandContext(ctx, "./with-check-lock.sh", lock, "sh", "-c", `cat "$1" > "$2"`, "stale", trace, seen)
	stale.Env = scriptEnv(append(recorder, fmt.Sprintf("DEV_GATE_LOCK_HELD=%d:%s", holder.Process.Pid, lock))...)
	inGroup(stale)

	stderr, err := stale.StderrPipe()
	if err != nil {
		t.Fatalf("the run's stderr: %v", err)
	}

	if err := stale.Start(); err != nil {
		t.Fatalf("start the run: %v", err)
	}

	// Reaped on every path out, a failure below included; the group kill above
	// ends it first.
	t.Cleanup(func() {
		cancel()
		//nolint:errcheck // reaping only; the verdict is the test's
		stale.Wait()
	})

	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil {
		t.Fatalf("the run said nothing before it ended: %v", err)
	}

	if !strings.Contains(line, "waiting for it to finish") {
		t.Errorf("a run outside the holder's ancestry said %q, want that it is waiting", line)
	}

	waitForLockFile(t, takes, "the run said it was waiting and never asked for the lock")

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release the holder: %v", err)
	}

	if err := stale.Wait(); err != nil {
		t.Fatalf("the run, once the holder let go: %v", err)
	}

	// AND ITS COMMAND STARTED ONLY ONCE THE HOLDER HAD FINISHED, the holder
	// having been released only after this run was waiting in the lock tool.
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("read what the run saw: %v", err)
	}

	if string(got) != "held\ndone\n" {
		t.Errorf("the run's command saw %q, want the holder's whole trace: it did not wait", got)
	}
}

// ONE RELATIVE PATH IN TWO DIRECTORIES IS TWO LOCKS: a run holding check.lock
// in one directory does not hold check.lock in another, so a run there takes
// its own.
func TestARelativeLockPathIsItsOwnDirectorysLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	first, second := t.TempDir(), t.TempDir()

	script, err := filepath.Abs("with-check-lock.sh")
	if err != nil {
		t.Fatalf("locate the script: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, script, "check.lock",
		"sh", "-c", `cd "$1" && exec "$2" check.lock true`, "outer", second, script)
	cmd.Dir = first
	cmd.Env = scriptEnv()
	inGroup(cmd)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a run inside a run, each with check.lock in its own directory: %v (%s)", err, out)
	}

	if strings.Contains(string(out), "running under it") {
		t.Errorf("the inner run took the outer directory's check.lock for its own: %q", out)
	}

	for _, dir := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(dir, "check.lock")); err != nil {
			t.Errorf("no lock was taken in %s: %v", dir, err)
		}
	}
}

// A PROCESS TREE THAT CANNOT BE READ IS NOT AN ANSWER: a nested run whose
// ancestry ps cannot report goes ahead unlocked and says so, rather than read
// "could not tell" as "not an ancestor" and wait forever on the run it is
// inside. Bounded on its own for that reason.
func TestANestedRunThatCannotReadItsAncestryRunsUnlocked(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	lock := filepath.Join(dir, "check.lock")

	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatalf("make the fake tool's directory: %v", err)
	}

	if err := forkSafeWriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write the fake ps: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "./with-check-lock.sh", lock,
		"./with-check-lock.sh", lock, "sh", "-c", "echo inner; exit 7")
	cmd.Env = scriptEnv("PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	inGroup(cmd)

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("a nested run with no readable ancestry: %v (%s), want the inner command's exit status 7", err, out)
	}

	if !strings.Contains(string(out), "could not tell") {
		t.Errorf("the nested run did not say it could not tell: %q", out)
	}
}

// AN INHERITED ERREXIT NEITHER STOPS THE SCRIPT NOR IS TAKEN FROM THE COMMAND.
// bash takes errexit from an exported SHELLOPTS, so a status the script reads
// as an answer would end it (measured on macOS's /bin/sh, 2026-10-06: exit 1,
// the command never run), and any option the script sets rewrites the
// SHELLOPTS the command inherits: a `set +e` took the command's errexit away,
// and a `set -u` gave it nounset. The command under the lock must behave
// exactly as it does run directly, through an unset expansion and a failure.
// dash ignores SHELLOPTS, so on Linux both runs simply agree.
func TestAnInheritedErrexitReachesTheCommandUnchanged(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	command := []string{"sh", "-c",
		`echo ran >> "$1"; unset optional; echo "optional=$optional" >> "$1"; false; echo "carried on" >> "$1"`, "command"}

	run := func(name string, argv ...string) (int, string) {
		runs := filepath.Join(dir, name)

		cmd := exec.CommandContext(t.Context(), argv[0], append(argv[1:], runs)...)
		cmd.Env = scriptEnv("SHELLOPTS=errexit")

		out, err := cmd.CombinedOutput()

		code := 0
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("%s: %v (%s)", name, err, out)
		}

		got, err := os.ReadFile(runs)
		if err != nil {
			t.Fatalf("%s: the command never ran: %v (%s)", name, err, out)
		}

		return code, string(got)
	}

	directCode, direct := run("direct", command...)
	lockedCode, locked := run("locked", append([]string{"./with-check-lock.sh", filepath.Join(dir, "check.lock")}, command...)...)

	if lockedCode != directCode || locked != direct {
		t.Errorf("under SHELLOPTS=errexit the locked command exited %d having written %q; run directly it exited %d having written %q",
			lockedCode, locked, directCode, direct)
	}
}

// A LOCK PATH WITH A NEWLINE CANNOT BE HANDED DOWN, so the run goes ahead
// unlocked rather than hold a lock a nested run could not recognise.
func TestALockPathWithANewlineRunsUnlocked(t *testing.T) {
	t.Parallel()

	// One file name with a newline in it, not two path elements.
	name := "check\nlock"

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh",
		filepath.Join(t.TempDir(), name), "sh", "-c", "exit 8")
	cmd.Env = scriptEnv()

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 8 {
		t.Fatalf("a lock path with a newline: %v (%s), want the command's exit status 8", err, out)
	}

	if !strings.Contains(string(out), "holds a newline") {
		t.Errorf("the run did not say why it went unlocked: %q", out)
	}
}

// SOMETHING OTHER THAN A FILE WHERE THE LOCK GOES IS NOT OPENED: a FIFO's open
// waits for a writer, so the probe would hang and the command never run. The
// run goes ahead unlocked and says so. Bounded on its own, because the failure
// is that wait.
func TestALockPathThatIsAFIFORunsUnlocked(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	lock := filepath.Join(t.TempDir(), "check.lock")
	if err := syscall.Mkfifo(lock, 0o600); err != nil {
		t.Fatalf("make a FIFO where the lock goes: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "./with-check-lock.sh", lock, "sh", "-c", "exit 9")
	cmd.Env = scriptEnv()
	inGroup(cmd)

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 9 {
		t.Fatalf("a FIFO where the lock goes: %v (%s), want the command's exit status 9", err, out)
	}

	if !strings.Contains(string(out), "not a regular file") {
		t.Errorf("the run did not say why it went unlocked: %q", out)
	}
}

// A COMMAND ENDED BY A SIGNAL REPORTS THE SHELL'S 128+N under the lock, as it
// does run directly: macOS lockf reports any signalled child as 70, so the
// command runs as the child of a shell that exits with its status.
func TestACommandEndedByASignalReportsItsOwnStatus(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh", filepath.Join(t.TempDir(), "check.lock"),
		"sh", "-c", `kill -TERM $$`)
	cmd.Env = scriptEnv()

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("a command ended by SIGTERM: %v (%s), want exit status %d", err, out, 128+int(syscall.SIGTERM))
	}
}

// THE COMMAND IS THE PROGRAM IT NAMES, NOT A SHELL BUILTIN OF THAT NAME: under
// the lock it runs from a shell, and `test` there would be the builtin while
// the unlocked fallback runs the program on PATH.
func TestALockedCommandIsTheProgramOnPath(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	runs := filepath.Join(dir, "runs")

	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatalf("make the program's directory: %v", err)
	}

	if err := forkSafeWriteFile(filepath.Join(bin, "test"), []byte("#!/bin/sh\necho ran >> \"$TEST_LOCK_RUNS\"\nexit 7\n"), 0o700); err != nil {
		t.Fatalf("write the program: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh", filepath.Join(dir, "check.lock"), "test", "x")
	cmd.Env = scriptEnv("PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_LOCK_RUNS="+runs)

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("`test x` under the lock: %v (%s), want the program's exit status 7", err, out)
	}

	if got, err := os.ReadFile(runs); err != nil || string(got) != "ran\n" {
		t.Errorf("the program on PATH ran %q (%v), want once", got, err)
	}
}

// A DIAGNOSTIC NAMING A PATH PRINTS THE PATH: echo in dash, and in macOS's
// /bin/sh, reads a backslash sequence as an escape, and \c ends the output
// there.
func TestADiagnosticPrintsAPathWithABackslashWhole(t *testing.T) {
	t.Parallel()

	// One file name holding a backslash, not two path elements.
	name := `cut\cshort`
	blocker := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("write a file to stand where a directory must go: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh", filepath.Join(blocker, "sub", "check.lock"), "true")
	cmd.Env = scriptEnv()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a run beside a lock that cannot be made: %v (%s)", err, out)
	}

	if !strings.Contains(string(out), filepath.Join(blocker, "sub")+", so this run is not serialised") {
		t.Errorf("the diagnostic did not carry the path whole: %q", out)
	}
}

// scriptEnv is this process's environment, without the variable naming a lock
// an enclosing gate holds (this may run inside `make check`, and a test's run of
// the script must not take that lock for its ancestor's), plus extra, which
// replaces a variable of the same name.
func scriptEnv(extra ...string) []string {
	var env []string

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "DEV_GATE_LOCK_HELD" || setIn(extra, name) {
			continue
		}

		env = append(env, kv)
	}

	return append(env, extra...)
}

// setIn reports whether one of kvs assigns name.
func setIn(kvs []string, name string) bool {
	for _, kv := range kvs {
		if n, _, _ := strings.Cut(kv, "="); n == name {
			return true
		}
	}

	return false
}

// inGroup starts cmd in a process group of its own and has its cancellation
// kill the whole group, so a lock tool, a shell or a sub-make it started cannot
// outlive the test.
func inGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A killed group's pipes close with it; this bounds a straggler holding one.
	cmd.WaitDelay = 2 * time.Second
}

// A SECOND RUN WAITS FOR THE FIRST, says so in one line of its own, and starts
// only once the first has finished. The first run holds the lock until the test
// releases it, and the test releases it only after the second has said it is
// waiting, so no amount of load can let the second find the lock free: the
// timeouts bound a failure and decide nothing.
func TestASecondCheckWaitsForTheFirst(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	lock := filepath.Join(dir, "check.lock")
	trace := filepath.Join(dir, "trace")
	release := filepath.Join(dir, "release")
	seen := filepath.Join(dir, "seen")

	first := exec.CommandContext(holderContext(t), "./with-check-lock.sh", lock, "sh", "-c",
		holdUntilReleased+`echo done >> "$1"`, "first", trace, release)
	first.Env = scriptEnv()
	inGroup(first)
	if err := first.Start(); err != nil {
		t.Fatalf("start the first run: %v", err)
	}

	releaseAndReap(t, first, release)

	waitForLockFile(t, trace, "the first run never started its command")

	recorder, takes := recordTakes(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	second := exec.CommandContext(ctx, "./with-check-lock.sh", lock,
		"sh", "-c", `cat "$1" > "$2"`, "second", trace, seen)
	second.Env = scriptEnv(recorder...)
	inGroup(second)

	stderr, err := second.StderrPipe()
	if err != nil {
		t.Fatalf("the second run's stderr: %v", err)
	}

	if err := second.Start(); err != nil {
		t.Fatalf("start the second run: %v", err)
	}

	// Reaped on every path out, a failure below included: the group kill ends
	// it, and with it the lock tool that would otherwise start its command once
	// the holder is released.
	t.Cleanup(func() {
		cancel()
		//nolint:errcheck // reaping only; the verdict is the test's
		second.Wait()
	})

	// Buffered past anything the script prints, so the reader never blocks
	// on a test that stopped listening.
	lines := make(chan string, 64)

	go func() {
		defer close(lines)

		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	var said []string

	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatal("the second run ended without saying it was waiting")
		}

		said = append(said, line)
	case <-time.After(30 * time.Second):
		t.Fatal("the second run said nothing in 30s")
	}

	// THE SECOND RUN IS WAITING IN THE LOCK TOOL before the first is released,
	// so its command cannot have started on its own: the message alone is
	// printed before the take.
	waitForLockFile(t, takes, "the second run said it was waiting and never asked for the lock")

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release the first run: %v", err)
	}

	for line := range lines {
		said = append(said, line)
	}

	if err := second.Wait(); err != nil {
		t.Fatalf("the second run: %v (%q)", err, said)
	}

	if err := first.Wait(); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("read what the second run saw: %v", err)
	}

	if string(got) != "held\ndone\n" {
		t.Errorf("the second run's command saw %q, want the first run's whole trace: the two overlapped", got)
	}

	// ONE LINE, the script's own: the probe's lock tool says "already locked" in
	// its own words, and a waiting run should not print it.
	if len(said) != 1 || !strings.Contains(said[0], "waiting for it to finish") {
		t.Errorf("the second run's stderr was %q, want the one line saying it is waiting", said)
	}
}

// waitForLockFile polls until path exists, failing with msg after ten seconds.
func waitForLockFile(t *testing.T, path, msg string) {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(path); err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal(msg)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// recordTakes puts a stand-in for the lock tool the script will choose in front
// of PATH: it appends a line to the returned file for every blocking take (any
// call that is not the probe's -t 0 or -n) and then runs the real tool, so a
// test can wait until a run is waiting in the lock rather than until it has
// said it will. It returns the run's environment entries; the stand-in reads
// both paths from them, so no path is ever quoted into its source.
func recordTakes(t *testing.T) ([]string, string) {
	t.Helper()

	tool := "lockf"
	if !lookPath(tool) {
		tool = "flock"
	}

	toolPath, err := exec.LookPath(tool)
	if err != nil {
		t.Fatalf("find %s: %v", tool, err)
	}

	dir := t.TempDir()
	takes := filepath.Join(dir, "takes")
	shim := "#!/bin/sh\ncase \" $* \" in *\" -t 0 \"*|*\" -n \"*) ;; *) echo take >> \"$TEST_LOCK_TAKES\" ;; esac\n" +
		"exec \"$TEST_LOCK_TOOL\" \"$@\"\n"

	if err := forkSafeWriteFile(filepath.Join(dir, tool), []byte(shim), 0o700); err != nil {
		t.Fatalf("write the %s stand-in: %v", tool, err)
	}

	return []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TEST_LOCK_TAKES=" + takes,
		"TEST_LOCK_TOOL=" + toolPath,
	}, takes
}

// A PROCESS THE COMMAND LEAVES BEHIND DOES NOT KEEP THE LOCK. util-linux flock
// passes the locked descriptor to the command and so to anything it starts,
// unless told -o; a daemon a test left running would then hold every later gate.
// The next run must find the lock free while the leftover still sleeps.
func TestALeftoverChildDoesNotKeepTheLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	lock := filepath.Join(dir, "check.lock")
	up := filepath.Join(dir, "up")
	release := filepath.Join(dir, "release")
	finished := filepath.Join(dir, "finished")

	// The child writes its pid, stays up until released and then says it
	// finished; past its watchdog it exits without saying so. It is its own
	// sh, so $$ is its pid.
	out, err := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock, "sh", "-c",
		`sh -c '`+leftoverChild+`' child "$1" "$2" "$3" > /dev/null 2>&1 &
exit 0`, "leaver", up, release, finished).CombinedOutput()
	if err != nil {
		t.Fatalf("a run that leaves a child behind: %v (%s)", err, out)
	}

	// Registered after t.TempDir, so it runs first: the child is released and
	// its finish is awaited before the directory holding the signal goes.
	t.Cleanup(func() { releaseLeftover(t, release, finished) })

	waitForLockFile(t, up, "the leftover child never started")

	pid, err := readPID(up)
	if err != nil {
		t.Fatalf("the leftover child's pid: %v", err)
	}

	// Bounded on its own: a lock the child kept would make this wait forever.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	out, err = exec.CommandContext(ctx, "./with-check-lock.sh", lock, "true").CombinedOutput()
	if err != nil {
		t.Fatalf("the next run, with the leftover child still alive: %v (%s)", err, out)
	}

	// THE CHILD WAS ALIVE FOR THE WHOLE RUN, or this proves nothing: a child
	// whose watchdog had already fired would have let go of anything it held.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the leftover child (pid %d) was gone when the next run finished, so this proves nothing: %v", pid, err)
	}

	if strings.Contains(string(out), "waiting") {
		t.Errorf("the next run waited for a lock only a leftover child held: %q", out)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release the leftover child: %v", err)
	}

	waitForLockFile(t, finished, "the leftover child did not finish when released")
}

// leftoverChild is sh that writes its pid to $1 (through a rename, so $1 never
// exists empty), waits until $2 exists and then writes $3. Past its watchdog it
// exits 9 without writing $3.
const leftoverChild = `echo $$ > "$1.tmp" && mv "$1.tmp" "$1"
i=0
while [ ! -e "$2" ]; do
	i=$((i + 1))
	if [ "$i" -gt 1200 ]; then exit 9; fi
	sleep 0.05
done
echo finished > "$3"`

// releaseLeftover releases a leftover child and waits up to ten seconds for it
// to say it finished, so its directory is not removed under it.
func releaseLeftover(t *testing.T, release, finished string) {
	t.Helper()

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Errorf("release the leftover child: %v", err)

		return
	}

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(finished); err == nil {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Errorf("the leftover child did not finish within 10s of its release")
}

// readPID reads the pid a child wrote to path.
func readPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("%q is not a pid: %w", raw, err)
	}

	return pid, nil
}

// holdUntilReleased is shell that writes "held" to $1, then waits until $2
// exists. It never gives up and succeeds: past its watchdog it exits 9, so a
// test whose release never came fails rather than seeing the lock come free.
const holdUntilReleased = `echo held > "$1"
i=0
while [ ! -e "$2" ]; do
	i=$((i + 1))
	if [ "$i" -gt 1200 ]; then exit 9; fi
	sleep 0.05
done
`

// holderContext is the context a lock holder runs under: not the test's, which
// is cancelled before cleanup runs and would kill the lock tool and orphan the
// holder's shell, but one cleanup cancels only after it has released the holder
// and given it ten seconds to finish.
func holderContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	t.Cleanup(cancel)

	return ctx
}

// releaseAndReap releases a holder and reaps it if the test ends before it
// does, so no holder outlives its test. Registered after holderContext's
// cancel, it runs before it. The holder was started inGroup, so a kill reaches
// everything it started.
func releaseAndReap(t *testing.T, cmd *exec.Cmd, release string) {
	t.Helper()

	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}

		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("release the holder: %v", err)
		}

		done := make(chan error, 1)

		go func() { done <- cmd.Wait() }()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("the holder did not finish within 10s of its release")
			// The whole group: the lock tool, and the shell holding the lock under it.
			//nolint:errcheck // a holder that outlived its release is killed; the error above is the verdict
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
}

// AN EMPTY LOCK PATH IS THE SHARED ONE, worked out in the shell so an
// XDG_CACHE_HOME holding spaces stays one path, and a relative one is not used:
// it would give every checkout a lock of its own.
func TestAnEmptyLockPathIsTheSharedOne(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	home := filepath.Join(dir, "home")

	for name, tc := range map[string]struct{ xdg, want string }{
		"absolute with spaces": {filepath.Join(dir, "a cache"), filepath.Join(dir, "a cache", "dev-gate.lock")},
		"relative":             {"relative-cache", filepath.Join(home, ".cache", "dev-gate.lock")},
	} {
		cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh", "", "true")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+tc.xdg)

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v (%s)", name, err, out)
		}

		if _, err := os.Stat(tc.want); err != nil {
			t.Errorf("%s: the shared lock was not taken at %s: %v", name, tc.want, err)
		}
	}
}

// NO PLACE FOR THE SHARED LOCK IS A COURTESY LOST, NOT A GATE: with neither an
// absolute XDG_CACHE_HOME nor HOME, the command still runs, its status is the
// script's, and the run says it is unlocked.
func TestANoHomeCheckStillRuns(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh", "", "sh", "-c", "exit 6")

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "HOME" && name != "XDG_CACHE_HOME" {
			cmd.Env = append(cmd.Env, kv)
		}
	}

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 6 {
		t.Fatalf("a command exiting 6 with no HOME: %v (%s), want exit status 6", err, out)
	}

	if !strings.Contains(string(out), "not serialised with others") {
		t.Errorf("the run did not say it went unlocked: %q", out)
	}
}

// MAKE CHECK HANDS THE SCRIPT THE PATH WHOLE. With MAKE=echo the recipe runs the
// real script around an echo instead of the gate, so this is the Makefile's own
// line, with an XDG_CACHE_HOME holding spaces.
func TestMakeCheckTakesTheSharedLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	xdg := filepath.Join(dir, "a cache")

	cmd := exec.CommandContext(t.Context(), "make", "-C", "..", "MAKE=echo", "check")

	// The Makefile's own default is the subject (see makeEnv).
	cmd.Env = makeEnv("XDG_CACHE_HOME=" + xdg)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make check with MAKE=echo: %v (%s)", err, out)
	}

	if !strings.Contains(string(out), "check-unlocked") {
		t.Errorf("the recipe did not run its steps' target under the script: %q", out)
	}

	if _, err := os.Stat(filepath.Join(xdg, "dev-gate.lock")); err != nil {
		t.Errorf("make check did not take the shared lock under an XDG_CACHE_HOME with spaces: %v (%s)", err, out)
	}
}

// makeEnv is scriptEnv for a nested make: this may run inside a `make check`
// holding the lock, and an inherited CHECK_LOCK, or one carried in MAKEFLAGS
// from `make check CHECK_LOCK=...`, would make the nested make wait for the lock
// its own enclosing gate holds.
func makeEnv(extra ...string) []string {
	var env []string

	for _, kv := range scriptEnv(extra...) {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "CHECK_LOCK", "MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES", "MAKELEVEL":
			if !setIn(extra, name) {
				continue
			}
		}

		env = append(env, kv)
	}

	return env
}

// A DRY RUN TAKES NO LOCK. `make -n check` waits for nothing even while another
// gate holds the lock, and prints the line it would have run.
func TestADryRunCheckTakesNoLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	lock := filepath.Join(dir, "held.lock")
	held := filepath.Join(dir, "held")
	release := filepath.Join(dir, "release")

	holder := exec.CommandContext(holderContext(t), "./with-check-lock.sh", lock, "sh", "-c",
		holdUntilReleased, "holder", held, release)
	holder.Env = scriptEnv()
	inGroup(holder)
	if err := holder.Start(); err != nil {
		t.Fatalf("start the holder: %v", err)
	}

	releaseAndReap(t, holder, release)
	waitForLockFile(t, held, "the holder never took the lock")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "make", "-C", "..", "-n", "check", "CHECK_LOCK="+lock)
	cmd.Env = makeEnv()
	// Killing make alone would leave the script and the lock tool, its
	// grandchildren, waiting for the lock and starting the steps once the holder
	// is released.
	inGroup(cmd)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n check while the lock is held: %v (%s)", err, out)
	}

	if !strings.Contains(string(out), "with-check-lock.sh") {
		t.Errorf("make -n check did not print the line it would run: %q", out)
	}

	if strings.Contains(string(out), "waiting") {
		t.Errorf("make -n check waited for the lock: %q", out)
	}
}

// A REAL RUN TAKES THE LOCK whatever is on the command line: reading MAKEFLAGS'
// first word took `make check CHECK_LOCK=/tmp/x NICE="nice -n 10"` for a touch
// and ran it unlocked. With MAKE="make -n" the recipe runs the real script
// around a sub-make that prints its steps instead of running them, so what is
// asserted is the race test line the steps would run: the overrides reach it
// locally, and under CI the Makefile forces both caps empty whatever the
// command line says.
func TestARealCheckWithOverridesTakesTheLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	goTest := regexp.MustCompile(`(?m)^(.*)go test(.*) -race -count=1 -timeout`)

	for name, tc := range map[string]struct{ ci, nice, parallel string }{
		"local": {ci: "", nice: "nice -n 10 ", parallel: " -p 2"},
		"CI":    {ci: "true", nice: "", parallel: ""},
	} {
		lock := filepath.Join(t.TempDir(), "tmp", "override.lock")

		// CHECK_LOCK LAST, its path holding a "t": on GNU Make 3.81 a command line
		// of only assignments puts the last of them first in MAKEFLAGS, with no
		// flags. Run from the root rather than with -C, which adds its own flag
		// (w) to MAKEFLAGS and would hide the shape under test.
		cmd := exec.CommandContext(t.Context(), "make",
			"NICE=nice -n 10", "TEST_PARALLEL=2", "MAKE=make -n", "CHECK_LOCK="+lock, "check")
		cmd.Dir = ".."
		cmd.Env = makeEnv("CI=" + tc.ci)
		inGroup(cmd)

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: make check with overrides: %v (%s)", name, err, out)
		}

		if _, err := os.Stat(lock); err != nil {
			t.Errorf("%s: make check with overrides on the command line did not take the lock: %v (%s)",
				name, err, out)
		}

		// AND THE OVERRIDES REACH THE STEPS: the line the sub-make is reached
		// through is not recursive to make, and an override still travels to it
		// in the MAKEFLAGS make exports to every recipe.
		m := goTest.FindStringSubmatch(string(out))
		if m == nil {
			t.Fatalf("%s: the sub-make printed no race test line: %q", name, out)
		}

		if m[1] != tc.nice || strings.TrimRight(m[2], " ") != tc.parallel {
			t.Errorf("%s: the test step would run as %q, want %q before go test and %q after it",
				name, m[0], tc.nice, tc.parallel)
		}
	}
}

// A LOCK PATH HOLDING SPACES IS ONE PATH: its directories are made and the lock
// is taken there, not at the pieces a word split would give.
func TestTheCheckLockTakesAPathWithSpaces(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	lock := filepath.Join(t.TempDir(), "a cache", "dev gate.lock")

	out, err := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock, "true").CombinedOutput()
	if err != nil {
		t.Fatalf("a lock path with spaces: %v (%s)", err, out)
	}

	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the lock was not taken at the path given: %v (%s)", err, out)
	}
}

// A LOCK THAT CANNOT BE MADE IS A COURTESY LOST, NOT A GATE: the command still
// runs, its status is still the script's, and the run says it is unlocked.
func TestACheckWhoseLockCannotBeMadeStillRuns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("write a file to stand where a directory must go: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh",
		filepath.Join(blocker, "sub", "check.lock"), "sh", "-c", "exit 4")

	out, err := cmd.CombinedOutput()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 4 {
		t.Fatalf("a command exiting 4 beside a lock that cannot be made: %v (%s), want exit status 4", err, out)
	}

	if !strings.Contains(string(out), "not serialised with others") {
		t.Errorf("the run did not say it went unlocked: %q", out)
	}
}

// A CALL WITHOUT A COMMAND IS REFUSED rather than run as a lock with nothing in it.
func TestTheCheckLockRefusesACallWithoutACommand(t *testing.T) {
	t.Parallel()

	err := exec.CommandContext(t.Context(), "./with-check-lock.sh",
		filepath.Join(t.TempDir(), "check.lock")).Run()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("a call with no command: %v, want exit status 2", err)
	}
}
