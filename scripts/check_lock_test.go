package scripts_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	first := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock, "sh", "-c",
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

	// The child says it is up and stays up until released, so it is alive, and
	// holding whatever it inherited, for the whole of the next run.
	out, err := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock, "sh", "-c",
		`(`+holdUntilReleased+`) > /dev/null 2>&1 &
exit 0`, "leaver", up, release).CombinedOutput()
	if err != nil {
		t.Fatalf("a run that leaves a child behind: %v (%s)", err, out)
	}

	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) }) //nolint:errcheck // releasing a child the test no longer watches

	waitForLockFile(t, up, "the leftover child never started")

	// Bounded on its own: a lock the child kept would make this wait forever.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	out, err = exec.CommandContext(ctx, "./with-check-lock.sh", lock, "true").CombinedOutput()
	if err != nil {
		t.Fatalf("the next run, with the leftover child still alive: %v (%s)", err, out)
	}

	if _, err := os.Stat(release); err == nil {
		t.Fatal("the leftover child was released before the next run finished, so this proves nothing")
	}

	if strings.Contains(string(out), "waiting") {
		t.Errorf("the next run waited for a lock only a leftover child held: %q", out)
	}
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

// releaseAndReap releases a holder and reaps it if the test ends before it
// does, so no holder outlives its test.
func releaseAndReap(t *testing.T, cmd *exec.Cmd, release string) {
	t.Helper()

	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}

		//nolint:errcheck // releasing and reaping only; the run's verdict is asserted in the body
		os.WriteFile(release, nil, 0o600)
		//nolint:errcheck // as above
		cmd.Wait()
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

// MAKE CHECK HANDS THE SCRIPT THE PATH WHOLE. With MAKE=echo the recipe runs the
// real script around an echo instead of the gate, so this is the Makefile's own
// line, with an XDG_CACHE_HOME holding spaces.
func TestMakeCheckTakesTheSharedLock(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	xdg := filepath.Join(dir, "a cache")

	cmd := exec.CommandContext(t.Context(), "make", "-C", "..", "MAKE=echo", "check")

	// The Makefile's own default is the subject, so no inherited CHECK_LOCK.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CHECK_LOCK=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}

	cmd.Env = append(cmd.Env, "XDG_CACHE_HOME="+xdg)

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
