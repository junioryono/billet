package ceph

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// realExt4 makes an ext4 filesystem image of dir with the host's mkfs.ext4, or
// skips where e2fsprogs is not installed.
func realExt4(t *testing.T, dir string) string {
	t.Helper()

	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		t.Skip("mkfs.ext4 is not installed")
	}

	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs is not installed")
	}

	img := filepath.Join(t.TempDir(), "rootfs.img")
	if out, err := exec.CommandContext(t.Context(), mkfs, "-q", "-F", "-d", dir, img, "16M").CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}

	return img
}

// THE REAL debugfs READS THE PROBE'S ELF HEADER OFF A FILESYSTEM IT DOES NOT MOUNT,
// and the machine it names is this host's when the probe is this host's env.
func TestDebugfsReadsTheMachineOfARealRootFilesystem(t *testing.T) {
	t.Parallel()

	env, err := os.ReadFile("/usr/bin/env")
	if err != nil {
		t.Skip("this host has no /usr/bin/env to put in a filesystem")
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "usr", "bin", "env"), env, 0o755); err != nil {
		t.Fatal(err)
	}

	img := realExt4(t, root)

	head, err := readProbeWithDebugfs(t.Context(), img)
	if err != nil {
		t.Fatalf("readProbeWithDebugfs: %v", err)
	}

	got, err := elfMachine(head)
	if want := config.HostMachine(runtime.GOARCH); err != nil || got != want {
		t.Fatalf("read %q, %v from a filesystem carrying this host's env, want %s", got, err, want)
	}
}

// AND A FILESYSTEM WITHOUT IT IS COULD-NOT-TELL, though debugfs exits 0 for a path
// it cannot find.
func TestDebugfsOnAFilesystemWithoutTheProbeIsCouldNotTell(t *testing.T) {
	t.Parallel()

	img := realExt4(t, t.TempDir())

	head, err := readProbeWithDebugfs(t.Context(), img)
	if err != nil {
		t.Fatalf("readProbeWithDebugfs: %v (debugfs answers 0 here, so only its bytes judge)", err)
	}

	if got, err := elfMachine(head); err == nil {
		t.Fatalf("a filesystem with no %s was read as %s", archProbePath, got)
	}
}
