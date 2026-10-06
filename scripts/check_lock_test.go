package scripts_test

import (
	"bufio"
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
		`echo held > "$1"
i=0
while [ ! -e "$2" ] && [ "$i" -lt 600 ]; do sleep 0.05; i=$((i + 1)); done
echo done >> "$1"`, "first", trace, release)
	if err := first.Start(); err != nil {
		t.Fatalf("start the first run: %v", err)
	}

	// Only a test that failed before waiting below reaps the first run here, by
	// which time the test's context is cancelled and the result means nothing.
	t.Cleanup(func() {
		if first.ProcessState == nil {
			//nolint:errcheck // reaping only; the run's verdict is asserted in the body
			first.Wait()
		}
	})

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

	lock := filepath.Join(t.TempDir(), "check.lock")

	out, err := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock,
		"sh", "-c", "sleep 10 > /dev/null 2>&1 & exit 0").CombinedOutput()
	if err != nil {
		t.Fatalf("a run that leaves a child behind: %v (%s)", err, out)
	}

	next := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock, "true")

	out, err = next.CombinedOutput()
	if err != nil {
		t.Fatalf("the next run: %v (%s)", err, out)
	}

	if strings.Contains(string(out), "waiting") {
		t.Errorf("the next run waited for a lock only a leftover child held: %q", out)
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
