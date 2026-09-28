package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A NODE GROWS EVERY CLONE WITH resize2fs BEFORE BOOT, and resize2fs refuses a
// filesystem last checked before it was last mounted. A generation whose build
// mounted the image one second after mkfs stamped the check time failed every
// launch on the fleet (2026-09-26). The build checks the image after its last
// unmount, and the gate refuses an image whose check predates its mount.
func TestTheImageIsCheckedAfterItsLastUnmount(t *testing.T) {
	t.Parallel()

	build := readScriptFile(t, "build-guest-image.sh")

	unmount := strings.LastIndex(build, "\tunmount_rootfs\n")
	fsck := strings.Index(build, "e2fsck -f -y \"$img\"")
	built := strings.Index(build, "echo \"built $img")

	if unmount < 0 || fsck < 0 || built < 0 {
		t.Fatalf("build-guest-image.sh no longer has the final unmount (%d), the check (%d) or the built line (%d)",
			unmount, fsck, built)
	}

	if fsck < unmount || fsck > built {
		t.Error("e2fsck must run after the last unmount_rootfs and before the image is reported built; " +
			"a check before the unmount leaves the mount time newer than the check time")
	}

	gate := readScriptFile(t, "check-guest-image.sh")
	if !strings.Contains(gate, "\ncheck_filesystem_ready \"$IMAGE\"\n") {
		t.Error("check-guest-image.sh does not run check_filesystem_ready against the image")
	}
}

// The gate's judgement, against real ext4 images: one never mounted passes, and
// one whose mount time is newer than its check time is refused by name.
func TestTheGateRefusesAnImageResize2fsWouldRefuse(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{"mkfs.ext4", "debugfs", "dumpe2fs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the ordering test above still holds the build", tool)
		}
	}

	run := func(img string) string {
		t.Helper()

		script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\n" +
			"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
			checkImageFunction(t, "check_filesystem_ready") + "\ncheck_filesystem_ready \"$1\"\n"

		path := filepath.Join(t.TempDir(), "fs.sh")
		if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "bash", path, img).CombinedOutput()
		if err != nil {
			t.Fatalf("the check did not run: %v\n%s", err, out)
		}

		return string(out)
	}

	img := filepath.Join(t.TempDir(), "root.img")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Truncate(img, 64<<20); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.CommandContext(t.Context(), "mkfs.ext4", "-q", "-F", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}

	if out := run(img); !strings.HasPrefix(out, "ok ") {
		t.Errorf("a freshly made, never mounted filesystem was refused:\n%s", out)
	}

	// A mount time a day after the check time, the state the published image had.
	if out, err := exec.CommandContext(t.Context(), "debugfs", "-w", "-R", "ssv mtime 20991231000000", img).CombinedOutput(); err != nil {
		t.Fatalf("debugfs: %v\n%s", err, out)
	}

	if out := run(img); !strings.Contains(out, "FAIL") || !strings.Contains(out, "e2fsck -f") {
		t.Errorf("an image last checked before its last mount passed the gate:\n%s", out)
	}
}
