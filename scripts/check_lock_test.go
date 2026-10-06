package scripts_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// lock still fails.
func TestTheCheckLockReturnsTheCommandsStatus(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	err := exec.CommandContext(t.Context(), "./with-check-lock.sh",
		filepath.Join(t.TempDir(), "check.lock"), "sh", "-c", "exit 3").Run()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("a command exiting 3 under the lock: %v, want exit status 3", err)
	}
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
	if err := first.Start(); err != nil {
		t.Fatalf("start the first run: %v", err)
	}

	releaseAndReap(t, first, release)

	waitForLockFile(t, trace, "the first run never started its command")

	second := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock,
		"sh", "-c", `cat "$1" > "$2"`, "second", trace, seen)

	stderr, err := second.StderrPipe()
	if err != nil {
		t.Fatalf("the second run's stderr: %v", err)
	}

	if err := second.Start(); err != nil {
		t.Fatalf("start the second run: %v", err)
	}

	lines := make(chan string)

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
// cancel, it runs before it.
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
			//nolint:errcheck // a holder that outlived its release is killed; the error above is the verdict
			cmd.Process.Kill()
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

// makeEnv is the environment for a nested make: this may run inside a `make
// check` holding the lock, and an inherited CHECK_LOCK, or one carried in
// MAKEFLAGS from `make check CHECK_LOCK=...`, would make the nested make wait for
// the lock its own enclosing gate holds.
func makeEnv(extra ...string) []string {
	var env []string

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "CHECK_LOCK", "MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES", "MAKELEVEL":
			continue
		}

		env = append(env, kv)
	}

	return append(env, extra...)
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
	// grandchildren, holding the output pipe open past the deadline.
	cmd.WaitDelay = 2 * time.Second

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
// and ran it unlocked. With MAKE=echo the recipe runs the real script around an
// echo instead of the gate.
func TestARealCheckWithOverridesTakesTheLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	lock := filepath.Join(t.TempDir(), "tmp", "override.lock")

	// CHECK_LOCK LAST, its path holding a "t": on GNU Make 3.81 a command line of
	// only assignments puts the last of them first in MAKEFLAGS, with no flags.
	// Run from the root rather than with -C, which adds its own flag (w) to
	// MAKEFLAGS and would hide the shape under test.
	cmd := exec.CommandContext(t.Context(), "make",
		"NICE=nice -n 10", "TEST_PARALLEL=2", "MAKE="+printFlags, "CHECK_LOCK="+lock, "check")
	cmd.Dir = ".."
	cmd.Env = makeEnv()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make check with overrides: %v (%s)", err, out)
	}

	if _, err := os.Stat(lock); err != nil {
		t.Errorf("make check with overrides on the command line did not take the lock: %v (%s)", err, out)
	}

	// AND THE OVERRIDES REACH THE STEPS: the line the sub-make is reached
	// through is not recursive to make, and an override still travels to it
	// in the MAKEFLAGS make exports to every recipe.
	if !strings.Contains(string(out), "TEST_PARALLEL=2") {
		t.Errorf("the sub-make did not receive the command line's overrides: %q", out)
	}
}

// printFlags stands in for MAKE: it prints the MAKEFLAGS it was handed, so a
// test sees what the sub-make would have received. Make expands $$ to $ when
// it reads the command line's value.
const printFlags = `sh -c 'printf "%s\n" "$$MAKEFLAGS"' sh`

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
