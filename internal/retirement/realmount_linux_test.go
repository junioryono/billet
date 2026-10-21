//go:build linux

package retirement

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// THE BOOTSTRAP'S OWN LOCK ON AN OVERLAY OF TWO FILESYSTEMS IS REPAIRED (#123).
// The retirement caller's gate overlays /var/lib with a tmpfs upper and no xino,
// where a file reports the upper's device and its directory the overlay's; the
// repair refused the lock it had just created. And a lock bind-mounted from the
// same filesystem, which shares the directory's device, is still refused.
func TestRealMountsRepairTheLockOnAnOverlayAndRefuseABoundOne(t *testing.T) {
	if os.Getenv("BILLET_TEST_REAL_MOUNTS") != "1" {
		t.Skip("requires BILLET_TEST_REAL_MOUNTS=1 as root on a disposable Linux host")
	}

	if os.Geteuid() != 0 {
		t.Fatal("BILLET_TEST_REAL_MOUNTS=1 needs root, to mount")
	}

	acct := ServiceAccount{User: "billet", UID: 990, Group: "billet", GID: 991}
	base := t.TempDir()

	t.Run("overlay", func(t *testing.T) {
		lower := filepath.Join(base, "lower")
		layers := filepath.Join(base, "layers")
		merged := filepath.Join(base, "merged")
		for _, dir := range []string{filepath.Join(lower, "billet"), layers, merged} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}

		mountForTest(t, layers, "-t", "tmpfs", "tmpfs")
		for _, dir := range []string{"upper", "work"} {
			if err := os.Mkdir(filepath.Join(layers, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		mountForTest(t, merged, "-t", "overlay", "-o",
			"lowerdir="+lower+",upperdir="+layers+"/upper,workdir="+layers+"/work,xino=off", "overlay")

		useRootAt(t, filepath.Join(merged, "billet"))

		if err := os.WriteFile(GlobalLockPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		var dirSt, lockSt unix.Stat_t
		if err := unix.Stat(Root, &dirSt); err != nil {
			t.Fatal(err)
		}
		if err := unix.Stat(GlobalLockPath(), &lockSt); err != nil {
			t.Fatal(err)
		}
		if dirSt.Dev == lockSt.Dev {
			t.Fatalf("the overlay reported one device (%d) for the directory and its lock, so "+
				"this host does not reproduce #123", dirSt.Dev)
		}

		repaired, err := RepairLocks(acct, "")
		if err != nil {
			t.Fatalf("the lock billet created on the overlay was refused: %v", err)
		}

		if len(repaired) != 1 || repaired[0] != GlobalLockPath() {
			t.Errorf("repaired %v, want the global lock", repaired)
		}

		assertOwned(t, GlobalLockPath(), 0, acct.GID, 0o660)
	})

	t.Run("a lock bound from the same filesystem", func(t *testing.T) {
		dir := filepath.Join(base, "plain")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		useRootAt(t, dir)

		source := filepath.Join(base, "elsewhere.lock")
		for _, path := range []string{source, GlobalLockPath()} {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		mountForTest(t, GlobalLockPath(), "--bind", source)

		_, err := RepairLocks(acct, "")
		if err == nil || !strings.Contains(err.Error(), "is not on its directory's mount") {
			t.Fatalf("a bind-mounted lock was not refused for its mount: %v", err)
		}

		assertOwned(t, source, 0, 0, 0o600)
	})
}

func useRootAt(t *testing.T, dir string) {
	t.Helper()

	old := Root
	Root = dir
	t.Cleanup(func() { Root = old })
}

func mountForTest(t *testing.T, target string, args ...string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "mount", append(args, target)...).CombinedOutput()
	if err != nil {
		t.Fatalf("mount at %s: %v: %s", target, err, out)
	}

	t.Cleanup(func() {
		if err := unix.Unmount(target, 0); err != nil {
			t.Errorf("unmount %s: %v", target, err)
		}
	})
}

func assertOwned(t *testing.T, path string, uid, gid int, mode os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	st := info.Sys().(*syscall.Stat_t)
	if int(st.Uid) != uid || int(st.Gid) != gid || info.Mode().Perm() != mode {
		t.Errorf("%s is %d:%d %o, want %d:%d %o", path, st.Uid, st.Gid, info.Mode().Perm(), uid, gid, mode)
	}
}
