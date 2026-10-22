//go:build linux

package regularfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// RealMounts builds, under a temporary directory, the layouts the mount rule is
// for and asks the kernel about each: an overlay whose lower layer is on the
// test's filesystem and whose upper is a tmpfs, mounted without xino (the layout
// the retirement caller's gate found refusing billet's own lock, #123), and a
// single file bind-mounted onto a name from another filesystem and from the
// same one. It mounts, so it runs only as root on a disposable Linux host, and
// only when BILLET_TEST_REAL_MOUNTS=1 asks for it.
func TestRealMountsAnswerTheMountNotTheDevice(t *testing.T) {
	root := realMountRoot(t)

	lower := filepath.Join(root, "lower")
	layers := filepath.Join(root, "layers")
	merged := filepath.Join(root, "merged")
	for _, dir := range []string{lower, layers, merged} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mountForTest(t, "tmpfs", layers, "-t", "tmpfs", "tmpfs")
	for _, dir := range []string{"upper", "work"} {
		if err := os.Mkdir(filepath.Join(layers, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Mkdir(filepath.Join(lower, "billet"), 0o755); err != nil {
		t.Fatal(err)
	}

	mountForTest(t, "overlay", merged, "-t", "overlay", "-o",
		"lowerdir="+lower+",upperdir="+layers+"/upper,workdir="+layers+"/work,xino=off", "overlay")

	dir := filepath.Join(merged, "billet")
	lock := filepath.Join(dir, "authority.lock")
	if err := os.WriteFile(lock, nil, 0o660); err != nil {
		t.Fatal(err)
	}

	// THE LAYOUT, PROVED BEFORE THE RULE IS JUDGED ON IT: a run where the kernel
	// reported one device for both would pass the device comparison too, and
	// prove nothing about the mount rule.
	var dirSt, lockSt unix.Stat_t
	if err := unix.Stat(dir, &dirSt); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(lock, &lockSt); err != nil {
		t.Fatal(err)
	}
	if dirSt.Dev == lockSt.Dev {
		t.Fatalf("the overlay reported device %d for both the directory and its file, so this "+
			"host does not reproduce #123's layout", dirSt.Dev)
	}

	t.Logf("overlay without xino: directory device %d, file device %d", dirSt.Dev, lockSt.Dev)

	if !holdsAt(t, dir, lock) {
		t.Error("a lock created in an overlay directory is not on that directory's mount")
	}

	// A FILE FROM ANOTHER FILESYSTEM, bind-mounted onto a name in a directory
	// on the test's own filesystem.
	plain := filepath.Join(root, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	foreign := filepath.Join(layers, "foreign")
	if err := os.WriteFile(foreign, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	elsewhere := filepath.Join(plain, "bound-from-another-filesystem")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	mountForTest(t, "bind", elsewhere, "--bind", foreign)

	if holdsAt(t, plain, elsewhere) {
		t.Error("a file bind-mounted from another filesystem is on its directory's mount")
	}

	// A FILE FROM THE SAME FILESYSTEM shares the device, which is the residual
	// the device comparison could never catch.
	same := filepath.Join(root, "same-source")
	if err := os.WriteFile(same, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(plain, "bound-from-the-same-filesystem")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	mountForTest(t, "bind", target, "--bind", same)

	var plainSt, targetSt unix.Stat_t
	if err := unix.Stat(plain, &plainSt); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(target, &targetSt); err != nil {
		t.Fatal(err)
	}
	if plainSt.Dev != targetSt.Dev {
		t.Fatalf("the same-filesystem bind mount reported another device (%d, %d), so it does "+
			"not stage the case the device comparison misses", plainSt.Dev, targetSt.Dev)
	}

	if holdsAt(t, plain, target) {
		t.Error("a file bind-mounted from the same filesystem is on its directory's mount")
	}

	unbound := filepath.Join(plain, "unbound")
	if err := os.WriteFile(unbound, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if !holdsAt(t, plain, unbound) {
		t.Error("an ordinary file is not on its directory's mount")
	}
}

// realMountRoot is a temporary directory on the test's own disk for a test that
// mounts, or a skip without BILLET_TEST_REAL_MOUNTS=1, and a failure when asked
// anywhere but as root on Linux.
func realMountRoot(t *testing.T) string {
	t.Helper()

	if os.Getenv("BILLET_TEST_REAL_MOUNTS") != "1" {
		t.Skip("requires BILLET_TEST_REAL_MOUNTS=1 as root on a disposable Linux host")
	}

	if os.Geteuid() != 0 {
		t.Fatal("BILLET_TEST_REAL_MOUNTS=1 needs root, to mount")
	}

	return t.TempDir()
}

// mountForTest runs mount(8) with args then target, and unmounts target when
// the test ends.
func mountForTest(t *testing.T, what, target string, args ...string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "mount", append(args, target)...).CombinedOutput()
	if err != nil {
		t.Fatalf("mount the %s at %s: %v: %s", what, target, err, out)
	}

	t.Cleanup(func() {
		if err := unix.Unmount(target, 0); err != nil {
			t.Errorf("unmount %s: %v", target, err)
		}
	})
}

func holdsAt(t *testing.T, dir, path string) bool {
	t.Helper()

	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	dirMount, err := MountOf(d)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := MountOf(f)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("%s: directory %+v, entry %+v", path, dirMount, entry)

	// THE MOUNT IDENTITY IS WHAT THESE LAYOUTS ARE ABOUT: a kernel that reports
	// none answers by device, which is the rule this test proves wrong.
	if !dirMount.Known || !entry.Known {
		t.Fatalf("this kernel reported no mount identity for %s or its directory", path)
	}

	return dirMount.Holds(entry)
}
