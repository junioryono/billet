package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE RUNNER OWNS ITS WHOLE HOME.
//
// MEASURED (2026-09-21) inside a running guest: /home/runner/.config and the
// NuGet config under it belonged to root, written by the image build, so any job
// whose tool creates ~/.config/<tool> failed with EACCES before doing its work.
// The gate has to refuse such an image, so a future installer that writes there
// as root fails the build instead of a user's job.
func TestTheGateNamesAFileUnderTheHomeTheRunnerDoesNotOwn(t *testing.T) {
	t.Parallel()

	home := homeFixture(t)

	// ANOTHER ACCOUNT'S IDS stand in for the runner's, so everything in the fixture
	// is "not the runner's" without the test needing root to chown it.
	out, code := runHomeOwnerCheck(t, home, os.Getuid()+1, os.Getgid())
	if code != 1 {
		t.Fatalf("the check exited %d for a home the runner does not own, want 1:\n%s", code, out)
	}

	if !strings.Contains(out, filepath.Join(home, ".config", "NuGet", "nuget.config")) {
		t.Errorf("the check does not name the file the runner cannot write:\n%s", out)
	}
}

func TestTheGatePassesAHomeTheRunnerOwnsEntirely(t *testing.T) {
	t.Parallel()

	home := homeFixture(t)

	out, code := runHomeOwnerCheck(t, home, os.Getuid(), os.Getgid())
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("the check exited %d with %q for a home the runner owns, want 0 and nothing", code, out)
	}
}

// AND A HOME IT COULD NOT LOOK AT IS NOT A PASS: find failing is could-not-tell.
func TestAHomeTheGateCannotReadIsNotAPass(t *testing.T) {
	t.Parallel()

	out, code := runHomeOwnerCheck(t, filepath.Join(t.TempDir(), "absent"), os.Getuid(), os.Getgid())
	if code != 2 {
		t.Fatalf("the check exited %d for a home that does not exist, want 2:\n%s", code, out)
	}
}

// A GROUP THE RUNNER IS NOT IN is a finding as much as an owner it is not.
func TestTheGateNamesAPathInAnotherGroup(t *testing.T) {
	t.Parallel()

	home := homeFixture(t)

	out, code := runHomeOwnerCheck(t, home, os.Getuid(), os.Getgid()+1)
	if code != 1 || !strings.Contains(out, "nuget.config") {
		t.Fatalf("the check exited %d for a home in another group, want 1 naming the file:\n%s", code, out)
	}
}

// THE GATE ITSELF FAILS on a finding and on a home it could not read, and passes
// a home the runner owns: check_runner_home is executed as the gate runs it,
// under the gate's own shell options, with its pass and fail.
func TestTheImageGateFailsOnWhatTheCheckFinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		noHome     bool
		uid        int
		wantFailed string
	}{
		{"owned", false, os.Getuid(), "0"},
		{"foreign", false, os.Getuid() + 1, "1"},
		{"unreadable", true, os.Getuid(), "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			if !tc.noHome {
				root = filepath.Dir(filepath.Dir(homeFixture(t)))
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\n" +
				"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
				checkImageFunction(t, "files_not_owned_by") + "\n" +
				checkImageFunction(t, "check_runner_home") + "\n" +
				"check_runner_home \"$1\" \"$2\" \"$3\"\necho \"FAILED=$FAILED\"\n"

			path := filepath.Join(t.TempDir(), "gate.sh")
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			out, err := exec.CommandContext(t.Context(), "bash", path, root, strconv.Itoa(tc.uid),
				strconv.Itoa(os.Getgid())).CombinedOutput()
			if err != nil {
				t.Fatalf("the gate's check did not run to its verdict: %v\n%s", err, out)
			}

			if !strings.Contains(string(out), "FAILED="+tc.wantFailed+"\n") {
				t.Errorf("want FAILED=%s:\n%s", tc.wantFailed, out)
			}
		})
	}
}

// THE BUILD RUNS AS ROOT UNDER ROOT'S HOME, before it does anything else. chroot
// and debootstrap keep the caller's environment, so a HOME inherited from the
// machine running the build is where every installer's first-run state lands
// inside the image; the export is the first command after the shell options, so
// no spelling of either can precede it.
func TestTheBuildSetsRootsHomeBeforeAnythingElse(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatal(err)
	}

	var commands []string

	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		commands = append(commands, line)
		if len(commands) == 2 {
			break
		}
	}

	if len(commands) < 2 || commands[0] != "set -euo pipefail" || commands[1] != "export HOME=/root" {
		t.Errorf("build-guest-image.sh's first commands are %q, want the shell options and then export HOME=/root", commands)
	}
}

func homeFixture(t *testing.T) string {
	t.Helper()

	home := filepath.Join(t.TempDir(), "home", "runner")
	if err := os.MkdirAll(filepath.Join(home, ".config", "NuGet"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, ".config", "NuGet", "nuget.config"), []byte("<configuration/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return home
}

func runHomeOwnerCheck(t *testing.T, home string, uid, gid int) (string, int) {
	t.Helper()

	script := "#!/usr/bin/env bash\nset -uo pipefail\n" +
		checkImageFunction(t, "files_not_owned_by") + "\n" +
		"files_not_owned_by \"$1\" \"$2\" \"$3\"\nexit $?\n"

	path := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "bash", path, home, strconv.Itoa(uid), strconv.Itoa(gid)).CombinedOutput()
	if err == nil {
		return string(out), 0
	}

	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("running the ownership check: %v\n%s", err, out)
	}

	return string(out), exit.ExitCode()
}
