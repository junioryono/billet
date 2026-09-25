package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bootScriptFunction extracts one shell function from boot-guest-image.sh.
func bootScriptFunction(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile("boot-guest-image.sh")
	if err != nil {
		t.Fatal(err)
	}

	source := string(raw)

	start := strings.Index(source, name+"() {")
	if start < 0 {
		t.Fatalf("boot-guest-image.sh has no %s function", name)
	}

	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s", name)
	}

	return source[start : start+end+2]
}

// watchConsole runs watch_console against a console file that later is appended
// to after the given delay, with a live guest process, and returns what it saw.
func watchConsole(t *testing.T, first, later string, delay time.Duration, timeout int) string {
	t.Helper()

	dir := t.TempDir()
	console := filepath.Join(dir, "console.log")

	if err := os.WriteFile(console, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}

	guest := exec.CommandContext(t.Context(), "sleep", "60")
	if err := guest.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := guest.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop the stand-in guest: %v", err)
		}

		// A killed process's Wait reports the signal; only the reap matters.
		if err := guest.Wait(); err != nil && guest.ProcessState == nil {
			t.Errorf("reap the stand-in guest: %v", err)
		}
	})

	if later != "" {
		go func() {
			time.Sleep(delay)

			f, err := os.OpenFile(console, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Errorf("open the console to append: %v", err)

				return
			}

			if _, err := f.WriteString(later); err != nil {
				t.Errorf("append to the console: %v", err)
			}

			if err := f.Close(); err != nil {
				t.Errorf("close the console: %v", err)
			}
		}()
	}

	script := "#!/usr/bin/env bash\nset -euo pipefail\n" + bootScriptFunction(t, "watch_console") + "\n" +
		"CONSOLE=$1 FCPID=$2 BOOT_TIMEOUT=$3 REFUSED_CONTRACT=999\n" +
		"watch_console\n" +
		"echo \"multiuser=$saw_multiuser agent=$saw_agent docker=$docker_started\"\n"

	path := filepath.Join(dir, "watch.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(t.Context(), "bash", path, console,
		strconv.Itoa(guest.Process.Pid), strconv.Itoa(timeout)).CombinedOutput()
	if err != nil {
		t.Fatalf("watch_console did not run to its verdict: %v\n%s", err, out)
	}

	return strings.TrimSpace(string(out))
}

const (
	agentRefusal = "[    8.559756] billet-agent[743]: billet-agent: billet speaks metadata contract 999 and this image understands 10\n"
	multiUser    = "[  OK  ] Reached target multi-user.target - Multi-User System.\n"
)

// THE AGENT CAN ANSWER BEFORE SYSTEMD REACHES ITS TARGET. Measured 2026-09-21: a
// guest build's agent refused the contract at 8.56 s while other wanted units were
// still starting, the watch stopped at the agent's line, and the gate reported
// that systemd never reached multi-user.target for an image that was still
// booting normally. The watch waits for both.
func TestTheBootWatchWaitsForTheTargetAfterTheAgentAnswers(t *testing.T) {
	t.Parallel()

	got := watchConsole(t, agentRefusal, multiUser, 2*time.Second, 30)
	if got != "multiuser=1 agent=1 docker=0" {
		t.Fatalf("the watch reported %q, want both the target and the agent seen", got)
	}
}

// AND A TARGET THAT NEVER COMES IS STILL A FAILURE, within the timeout.
func TestTheBootWatchReportsATargetThatNeverCame(t *testing.T) {
	t.Parallel()

	start := time.Now()
	got := watchConsole(t, agentRefusal, "", 0, 3)

	if got != "multiuser=0 agent=1 docker=0" {
		t.Fatalf("the watch reported %q, want the agent seen and the target not", got)
	}

	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the watch took %s against a 3 s timeout", elapsed)
	}
}

// THE USUAL ORDER still ends as soon as both are seen.
func TestTheBootWatchEndsWhenBothAreSeen(t *testing.T) {
	t.Parallel()

	start := time.Now()
	got := watchConsole(t, multiUser+agentRefusal, "", 0, 30)

	if got != "multiuser=1 agent=1 docker=0" {
		t.Fatalf("the watch reported %q", got)
	}

	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the watch kept waiting %s after it had seen both", elapsed)
	}
}
