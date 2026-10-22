//go:build linux

package lifeops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// THE STATE REPAIR, THROUGH THE REAL ROOT, ON THE TWO MOUNT LAYOUTS (#123): a
// ledger a privileged billet left on an overlay of two filesystems is given
// back although its device is not its directory's, and one bind-mounted from
// the same filesystem is refused although its device is.
func TestRealMountsRepairServerStateByMount(t *testing.T) {
	if os.Getenv("BILLET_TEST_REAL_MOUNTS") != "1" {
		t.Skip("requires BILLET_TEST_REAL_MOUNTS=1 as root on a disposable Linux host")
	}

	if os.Geteuid() != 0 {
		t.Fatal("BILLET_TEST_REAL_MOUNTS=1 needs root, to mount")
	}

	base := t.TempDir()
	c := NewConverger(NewInspector())

	t.Run("overlay", func(t *testing.T) {
		lower := filepath.Join(base, "lower")
		layers := filepath.Join(base, "layers")
		merged := filepath.Join(base, "merged")
		for _, dir := range []string{filepath.Join(lower, "server"), layers, merged} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}

		realMount(t, layers, "-t", "tmpfs", "tmpfs")
		for _, dir := range []string{"upper", "work"} {
			if err := os.Mkdir(filepath.Join(layers, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		realMount(t, merged, "-t", "overlay", "-o",
			"lowerdir="+lower+",upperdir="+layers+"/upper,workdir="+layers+"/work,xino=off", "overlay")

		dir := filepath.Join(merged, "server")
		if err := os.Chown(dir, 990, 991); err != nil {
			t.Fatal(err)
		}

		ledger := filepath.Join(dir, "billet.db")
		if err := os.WriteFile(ledger, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		if devOf(t, dir) == devOf(t, ledger) {
			t.Fatal("the overlay reported one device for the directory and its file, so this " +
				"host does not reproduce #123")
		}

		repaired, err := c.RepairServerState(dir, 990, 991)
		if err != nil {
			t.Fatalf("the ledger a privileged billet left on the overlay was refused: %v", err)
		}

		if len(repaired) != 1 || repaired[0] != ledger {
			t.Errorf("repaired %v, want %s", repaired, ledger)
		}
	})

	t.Run("a ledger bound from the same filesystem", func(t *testing.T) {
		dir := filepath.Join(base, "plain")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, 990, 991); err != nil {
			t.Fatal(err)
		}

		source := filepath.Join(base, "elsewhere.db")
		ledger := filepath.Join(dir, "billet.db")
		for _, path := range []string{source, ledger} {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		realMount(t, ledger, "--bind", source)

		if devOf(t, dir) != devOf(t, ledger) {
			t.Fatal("the same-filesystem bind mount reported another device, so it does not " +
				"stage the case the device comparison missed")
		}

		_, err := c.RepairServerState(dir, 990, 991)
		if err == nil || !strings.Contains(err.Error(), "not on the mount of the directory") {
			t.Fatalf("a bind-mounted ledger was not refused for its mount: %v", err)
		}

		info, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s has no ownership to read", source)
		}

		if st.Uid != 0 {
			t.Errorf("the bound file was given to uid %d", st.Uid)
		}
	})
}

func realMount(t *testing.T, target string, args ...string) {
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

func devOf(t *testing.T, path string) uint64 {
	t.Helper()

	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}

	return st.Dev
}
