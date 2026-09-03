package scripts_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireGNUTimeout skips where GNU timeout's semantics cannot be trusted.
func requireGNUTimeout(t *testing.T) {
	t.Helper()

	// LINUX ONLY, AND NOT MERELY BECAUSE OF THE BINARY. Stock macOS ships no GNU
	// `timeout` at all -- it arrived on the machine this was written on through
	// Homebrew's gnubin path -- but the deciding reason is stranger: there, the
	// identical `timeout --kill-after=30s -- 2 <fake>` returns 0 run directly and
	// 124 run from inside a script, which makes the platform unusable as evidence
	// about a bound. The builder and CI are Linux, the same cases were exercised
	// against real GNU coreutils in an ubuntu:24.04 container, and a test that
	// cannot be trusted on a platform is worse than one that says so.
	if runtime.GOOS != "linux" {
		t.Skip("these assert GNU timeout's semantics under Linux process behaviour; " +
			"macOS disagrees with itself about them")
	}

	out, err := exec.CommandContext(t.Context(), "timeout", "--version").Output()
	if err != nil || !strings.Contains(string(out), "GNU coreutils") {
		t.Skip("no GNU `timeout` on PATH; these assert its semantics and cannot fake it")
	}
}

// psmoduleHarness renders a runnable install_powershell_modules with pwsh faked.
//
// `set -Eeuo pipefail` — THE SAME OPTIONS PRODUCTION USES. The first version ran
// with `set -uo pipefail`, dropping errexit, which is not a detail: under errexit a
// stall in the Set-PSRepository call aborts before the retry loop is ever reached,
// so the harness was exercising a control flow the builder does not have.
func psmoduleHarness(t *testing.T, dir, fake, decl string, timeoutS, tries int) string {
	t.Helper()

	toolset := filepath.Join(dir, "toolset.json")
	if err := os.WriteFile(toolset, []byte(decl), 0o600); err != nil {
		t.Fatalf("write the declaration: %v", err)
	}

	body := "#!/usr/bin/env bash\nset -Eeuo pipefail\n" +
		"BILLET_TC_TOOLSET=" + toolset + "\n" +
		"BILLET_TC_PSMOD_TIMEOUT=" + strconv.Itoa(timeoutS) + "\n" +
		"BILLET_TC_PSMOD_TRIES=" + strconv.Itoa(tries) + "\n" +
		"billet_tc_run() {\n" +
		"  local a=()\n" +
		"  for x in \"$@\"; do\n" +
		"    if [ \"$x\" = /usr/bin/pwsh ]; then x=" + fake + "; fi\n" +
		"    a+=(\"$x\")\n" +
		"  done\n" +
		"  \"${a[@]}\"\n" +
		"}\n" +
		guestImageFunction(t, "install_powershell_modules") + "\n" +
		"install_powershell_modules\n"

	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	return script
}

// runPSModules executes the harness under a deadline it cannot outlive.
//
// WaitDelay, BECAUSE A CANCELLED COMMAND IS NOT A RETURNED ONE. CommandContext
// kills the process it started and then Wait blocks until the output pipes close —
// which the fake's own children hold open. Without it a test against an unbounded
// installer does not fail, it HANGS, which is the bug under test wearing the test
// suite's clothes.
func runPSModules(t *testing.T, script string, within time.Duration) (string, time.Duration, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		t.Fatalf("the installer never returned within %s; a module that hangs owns the "+
			"whole build\n%s", within, out)
	}

	return string(out), elapsed, err
}

// A MODULE INSTALL THAT HANGS MUST FAIL THE BUILD, NOT OWN IT.
//
// A real AMI build sat at 0.26% CPU for fifty minutes inside `Install-Module -Name
// Az` — the machine doing nothing — while the three modules before it took 60s, 6s
// and 9s. Unbounded, that consumes the whole `billet ami build` timeout on a paid
// builder and then reports a timeout against the BUILD rather than naming the
// module that wedged it.
//
// THE FAKE IGNORES SIGTERM, which is the point rather than a detail. `timeout`
// alone sends TERM and then waits forever for a process that will not take it, so a
// fake that dies politely cannot tell a hard bound from a decorative one — and the
// first version of this test could not.
func TestAPowerShellModuleThatHangsIsStoppedAndNamed(t *testing.T) {
	t.Parallel()
	requireGNUTimeout(t)

	dir := t.TempDir()
	entered := filepath.Join(dir, "entered")

	// exec, SO THE SHELL IS REPLACED rather than left with an orphan sleep, and
	// `trap '' TERM` before it so only --kill-after can end this.
	fake := filepath.Join(dir, "pwsh")
	body := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *Install-Module*) ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n" +
		"echo yes > " + entered + "\n" +
		"trap '' TERM\n" +
		"exec sleep 3600\n"

	if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
		t.Fatalf("write the fake pwsh: %v", err)
	}

	script := psmoduleHarness(t, dir, fake,
		`{"powershellModules":[{"name":"Wedged"}],"azureModules":[]}`, 2, 1)

	out, elapsed, err := runPSModules(t, script, 120*time.Second)

	if err == nil {
		t.Fatalf("a module whose install never finished was reported as installed\n%s", out)
	}

	// THE FAKE WAS ACTUALLY ENTERED. Without this the test passes when `timeout`
	// is missing entirely: every call exits 127, the generic failure still names
	// the module, and nothing has been bounded.
	if _, statErr := os.Stat(entered); statErr != nil {
		t.Fatalf("the install fake was never reached, so nothing here was bounded\n%s", out)
	}

	// THE TIMEOUT-SPECIFIC MESSAGE, not merely a failure that mentions the module.
	// A generic failure names it too, which is how a broken bound reads as a
	// working one.
	if !strings.Contains(out, "did not finish within 2s") {
		t.Errorf("the failure does not report a timeout, so it cannot be distinguished "+
			"from an install that simply failed:\n%s", out)
	}

	if !strings.Contains(out, "Wedged") {
		t.Errorf("the failure does not name the module that hung:\n%s", out)
	}

	// NEAR THE BOUND. Two seconds plus the kill grace and teardown; sixty is loose
	// enough for a loaded machine and far under the minutes a real default would
	// take, so a removed bound cannot slip through.
	if elapsed > 60*time.Second {
		t.Errorf("the install took %s against a 2s bound, so the bound is not what "+
			"stopped it", elapsed)
	}
}

// AND A STALL THAT CLEARS IS RETRIED, NOT REPORTED.
//
// The bound alone turns a fifty-minute hang into a failed build, which is better
// and still a failed build. If the cause is a stalled socket — the best available
// explanation, since PowerShellGet's downloads carry no read timeout and a
// builder's egress goes through NAT that drops idle flows — then the attempt that
// matters is the second one. This is why every curl in that file passes
// --retry-all-errors.
func TestAStalledModuleInstallIsRetried(t *testing.T) {
	t.Parallel()
	requireGNUTimeout(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "attempts")

	// ONLY THE INSTALL COUNTS. This same fake serves the Set-PSRepository call and
	// the post-install verification, and counting those made the first version see
	// four attempts and call it a bug in the retry — a fixture miscounting and
	// blaming the code.
	//
	// THE COUNT IS ON DISK because each attempt is a separate process; a shell
	// variable would reset every time and the fake would stall forever.
	fake := filepath.Join(dir, "pwsh")
	body := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *Install-Module*) ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n" +
		"n=$(cat " + counter + " 2>/dev/null || echo 0)\n" +
		"n=$((n + 1))\n" +
		"echo $n > " + counter + "\n" +
		"if [ \"$n\" -le 2 ]; then trap '' TERM; exec sleep 3600; fi\n" +
		"exit 0\n"

	if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
		t.Fatalf("write the fake pwsh: %v", err)
	}

	script := psmoduleHarness(t, dir, fake,
		`{"powershellModules":[{"name":"Stally"}],"azureModules":[]}`, 2, 3)

	out, _, err := runPSModules(t, script, 180*time.Second)

	if err != nil {
		t.Fatalf("two stalls that cleared were reported as a failure; a build fails on "+
			"a condition that fixed itself: %v\n%s", err, out)
	}

	// THE COUNT IS ASSERTED, because succeeding on the FIRST attempt would also
	// produce a zero exit and prove nothing about retrying.
	raw, readErr := os.ReadFile(counter)
	if readErr != nil {
		t.Fatalf("read the attempt count: %v", readErr)
	}

	if got := strings.TrimSpace(string(raw)); got != "3" {
		t.Errorf("the installer made %s attempt(s), want 3; the fake stalls twice, so "+
			"anything less means it did not retry and anything more means it ignored "+
			"its own limit", got)
	}
}

// AN OVERRIDE THAT WOULD DISABLE THE BOUND IS REFUSED, NOT OBEYED.
//
// Both values reach shell constructs where a wrong one inverts the guard: GNU
// documents `timeout 0` as DISABLING the timeout, `timeout --help` prints usage and
// exits ZERO so every module is reported installed while nothing ran, and a
// non-numeric try count makes `[ -ge ]` exit 2 — which as an `if` condition errexit
// does not act on, so the retry loop never ends.
//
// The second is the one worth the test: a green build over an image with no modules
// in it is the worst outcome this file can produce.
func TestAPowerShellModuleOverrideThatBreaksTheBoundIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		timeout, trie string
	}{
		{name: "a zero timeout disables the bound", timeout: "0", trie: "3"},
		{name: "an option-shaped timeout runs nothing", timeout: "--help", trie: "3"},
		{name: "a non-numeric try count never terminates", timeout: "600", trie: "abc"},
		{name: "a zero try count attempts nothing", timeout: "600", trie: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			toolset := filepath.Join(dir, "toolset.json")
			if err := os.WriteFile(toolset, []byte(
				`{"powershellModules":[{"name":"Anything"}],"azureModules":[]}`), 0o600); err != nil {
				t.Fatalf("write the declaration: %v", err)
			}

			// billet_tc_run SUCCEEDS AT EVERYTHING. If the override were obeyed this
			// test's failure mode is a PASS, which is exactly the danger being
			// tested: the settings must be refused before anything runs at all.
			harness := "#!/usr/bin/env bash\nset -Eeuo pipefail\n" +
				"BILLET_TC_TOOLSET=" + toolset + "\n" +
				"BILLET_TC_PSMOD_TIMEOUT=" + tc.timeout + "\n" +
				"BILLET_TC_PSMOD_TRIES=" + tc.trie + "\n" +
				"billet_tc_run() { :; }\n" +
				guestImageFunction(t, "install_powershell_modules") + "\n" +
				"install_powershell_modules\n"

			script := filepath.Join(dir, "run.sh")
			if err := os.WriteFile(script, []byte(harness), 0o700); err != nil {
				t.Fatalf("write the harness: %v", err)
			}

			out, _, err := runPSModules(t, script, 60*time.Second)

			if err == nil {
				t.Fatalf("TIMEOUT=%q TRIES=%q was accepted; the bound this exists for is "+
					"disabled and the build would report success\n%s",
					tc.timeout, tc.trie, out)
			}

			// THE REFUSAL NAMES THE SETTING, because an operator who set it needs to
			// know which one was wrong and what it must be.
			if !strings.Contains(out, "BILLET_TC_PSMOD_") {
				t.Errorf("the refusal does not name the setting that was wrong:\n%s", out)
			}
		})
	}
}
