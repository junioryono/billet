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

// THE GATE CALLS IT on the image's runner home with the runner's own ids, and a
// finding fails the gate.
func TestTheImageGateChecksTheRunnerHome(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("check-guest-image.sh")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `files_not_owned_by "$MNT/home/runner" "$runner_uid" "$runner_gid"`) {
		t.Error("check-guest-image.sh does not check the runner's home with the runner's ids")
	}
}

// THE BUILD RUNS AS ROOT UNDER ROOT'S HOME. chroot keeps the caller's
// environment, so a HOME inherited from the machine running the build is where
// every installer's first-run state lands inside the image.
func TestTheBuildSetsRootsHomeBeforeItsFirstChroot(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatal(err)
	}

	source := string(raw)

	home := strings.Index(source, "\nexport HOME=/root\n")
	if home < 0 {
		t.Fatal("build-guest-image.sh does not export HOME=/root")
	}

	first := strings.Index(source, "\tchroot ")
	if first >= 0 && first < home {
		t.Errorf("build-guest-image.sh runs chroot (offset %d) before it sets HOME (offset %d)", first, home)
	}
}

func homeFixture(t *testing.T) string {
	t.Helper()

	home := filepath.Join(t.TempDir(), "runner")
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
