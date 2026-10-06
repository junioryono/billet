package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireLockTool skips where the machine has neither lock tool, except under CI,
// where a skip would report success for a script nothing exercised.
func requireLockTool(t *testing.T) {
	t.Helper()

	for _, tool := range []string{"lockf", "flock"} {
		if _, err := exec.LookPath(tool); err == nil {
			return
		}
	}

	if os.Getenv("CI") != "" {
		t.Fatal("neither lockf nor flock is installed under CI, so with-check-lock.sh would go untested")
	}

	t.Skip("neither lockf nor flock is installed")
}

// THE COMMAND'S EXIT STATUS IS THE SCRIPT'S, so `make check` failing under the
// lock still fails.
func TestTheCheckLockReturnsTheCommandsStatus(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	cmd := exec.CommandContext(t.Context(), "./with-check-lock.sh",
		filepath.Join(t.TempDir(), "check.lock"), "sh", "-c", "exit 3")

	err := cmd.Run()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("a command exiting 3 under the lock: %v, want exit status 3", err)
	}
}

// A SECOND RUN WAITS FOR THE FIRST, says so, and starts only once the first has
// finished: the second command reads the file the first writes on its way out,
// and would see only the first half if the two overlapped.
func TestASecondCheckWaitsForTheFirst(t *testing.T) {
	t.Parallel()
	requireLockTool(t)

	dir := t.TempDir()
	lock := filepath.Join(dir, "check.lock")
	trace := filepath.Join(dir, "trace")
	seen := filepath.Join(dir, "seen")

	first := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock,
		"sh", "-c", `echo held > "$1"; sleep 1; echo done >> "$1"`, "first", trace)
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

	// The first run is holding the lock once its command has written.
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(trace); err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the first run never started its command")
		}

		time.Sleep(20 * time.Millisecond)
	}

	var stderr bytes.Buffer

	second := exec.CommandContext(t.Context(), "./with-check-lock.sh", lock,
		"sh", "-c", `cat "$1" > "$2"`, "second", trace, seen)
	second.Stderr = &stderr

	if err := second.Run(); err != nil {
		t.Fatalf("the second run: %v (%s)", err, stderr.String())
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
	if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); len(lines) != 1 ||
		!strings.Contains(lines[0], "waiting for it to finish") {
		t.Errorf("the second run's stderr was %q, want the one line saying it is waiting", stderr.String())
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
