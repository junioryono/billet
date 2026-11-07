package guestassets

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runJobStartedHook runs the hook as the runner does, under `bash -e`, with env as its whole
// environment, and answers what it printed and its status.
func runJobStartedHook(t *testing.T, env ...string) (string, int) {
	t.Helper()

	path, err := filepath.Abs("job-started-hook.sh")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", "-e", path)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run the hook: %v", err)
		}

		return out.String(), exit.ExitCode()
	}

	return out.String(), 0
}

// THE LOG SAYS HOW TO ATTACH TO THIS JOB, by its lease, which is what the command takes.
func TestTheJobStartedHookNamesTheLease(t *testing.T) {
	t.Parallel()

	out, status := runJobStartedHook(t, "RUNNER_NAME=billet-0123abcd")
	if status != 0 {
		t.Errorf("status %d: %s", status, out)
	}

	if !strings.Contains(out, "billet jobs attach 0123abcd\n") {
		t.Errorf("the log says %q", out)
	}

	if strings.Contains(out, "kept") {
		t.Errorf("a tier that holds nothing says it does: %q", out)
	}
}

// AND WHAT A FAILURE WOULD KEEP, only on a tier that keeps it.
func TestTheJobStartedHookSaysTheHold(t *testing.T) {
	t.Parallel()

	out, status := runJobStartedHook(t, "RUNNER_NAME=billet-0123abcd", "BILLET_ATTACH_HOLD=5m0s")
	if status != 0 || !strings.Contains(out, "kept for up to 5m0s, or until a session's shell has exited and the last session has closed") {
		t.Errorf("status %d, log %q", status, out)
	}
}

// IT STANDS IN FRONT OF THE HOOK IT REPLACED: that one still runs, its status is the answer, as it
// was before, and this one adds no failure of its own.
func TestTheJobStartedHookRunsTheOneBehindIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	behind := filepath.Join(dir, "behind.sh")
	if err := writeExecutable(behind, []byte("#!/bin/sh\necho behind ran\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, status := runJobStartedHook(t, "RUNNER_NAME=billet-0123abcd", "BILLET_JOB_STARTED_NEXT="+behind)
	if status != 7 {
		t.Errorf("status %d, want the hook behind it's 7", status)
	}

	if !strings.Contains(out, "behind ran") || !strings.Contains(out, "billet jobs attach 0123abcd") {
		t.Errorf("the log says %q", out)
	}
}

// NOTHING IT LACKS FAILS THE JOB.
func TestTheJobStartedHookCannotFailOnItsOwn(t *testing.T) {
	t.Parallel()

	out, status := runJobStartedHook(t)
	if status != 0 {
		t.Errorf("with no runner name: status %d, log %q", status, out)
	}

	// AND A COMMAND WITH NO LEASE IN IT WOULD NOT RUN.
	if strings.Contains(out, "billet jobs attach") {
		t.Errorf("with no runner name the log offers %q", out)
	}
}
