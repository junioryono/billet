package blockfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// someBlockDevice is a block device this host has, for a staged process to hold: only a
// block node's device number can match, and only a real one has a number.
func someBlockDevice(t *testing.T) (string, uint64) {
	t.Helper()

	for _, pattern := range []string{"/dev/loop*", "/dev/sd*", "/dev/vd*", "/dev/nvme*n*", "/dev/disk*"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range matches {
			var st unix.Stat_t
			if unix.Stat(path, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK {
				return path, rdevOf(&st)
			}
		}
	}

	t.Skip("this host has no block device to stage a holder of")

	return "", 0
}

// stageProcess makes procRoot/<pid>/task/<pid>/fd with one descriptor per target: a
// process of one thread, whose table is its leader's.
func stageProcess(t *testing.T, procRoot, pid string, targets ...string) {
	t.Helper()

	stageThread(t, procRoot, pid, pid, targets...)
}

// stageThread makes procRoot/<pid>/task/<tid>/fd with one descriptor per target.
func stageThread(t *testing.T, procRoot, pid, tid string, targets ...string) {
	t.Helper()

	fds := filepath.Join(procRoot, pid, "task", tid, "fd")
	if err := os.MkdirAll(fds, 0o755); err != nil {
		t.Fatal(err)
	}

	for i, target := range targets {
		if err := os.Symlink(target, filepath.Join(fds, string(rune('3'+i)))); err != nil {
			t.Fatal(err)
		}
	}
}

// A PROCESS HOLDING THE DEVICE IS NAMED, among others that hold only other files: a
// VMM serving the volume to its guest opens it without O_EXCL, so only its open files
// say it holds it.
func TestAProcessWithTheDeviceOpenIsFound(t *testing.T) {
	device, rdev := someBlockDevice(t)
	procRoot := t.TempDir()

	stageProcess(t, procRoot, "100", "/dev/null", os.TempDir())
	stageProcess(t, procRoot, "200", "/dev/null", device)
	// Not a process: /proc holds other names too, and this one's threads would be read
	// as unreadable if it were taken for one.
	if err := os.MkdirAll(filepath.Join(procRoot, "sys", "task"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(procRoot, "sys", "task"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(procRoot, "sys", "task"), 0o755); err != nil {
			t.Error(err)
		}
	})

	holder, err := openedBy(procRoot, rdev)
	if err != nil {
		t.Fatal(err)
	}

	if holder != "200" {
		t.Fatalf("the holder of %s was reported as %q, want 200", device, holder)
	}
}

// A THREAD WITH A TABLE OF ITS OWN HOLDS THE DEVICE FOR ITS PROCESS, though the leader's
// table does not have it: a thread that unshared its table, or that outlived its leader.
func TestAThreadsOwnTableIsRead(t *testing.T) {
	device, rdev := someBlockDevice(t)
	procRoot := t.TempDir()

	stageThread(t, procRoot, "400", "400", "/dev/null")
	stageThread(t, procRoot, "400", "401", "/dev/null", device)

	holder, err := openedBy(procRoot, rdev)
	if err != nil {
		t.Fatal(err)
	}
	if holder != "400" {
		t.Fatalf("a thread holding %s left its process reported as %q, want 400", device, holder)
	}
}

// NOTHING HOLDING THE DEVICE IS NOTHING FOUND, and a descriptor whose file is gone, or a
// process that ended between the listing and the read, holds nothing.
func TestNoHolderIsNoneFound(t *testing.T) {
	_, rdev := someBlockDevice(t)
	procRoot := t.TempDir()

	stageProcess(t, procRoot, "100", "/dev/null", filepath.Join(procRoot, "gone"))
	stageProcess(t, procRoot, "101")
	// A process that ended between the listing and the read: its directory is listed
	// and its descriptors are not there.
	if err := os.MkdirAll(filepath.Join(procRoot, "102"), 0o755); err != nil {
		t.Fatal(err)
	}

	holder, err := openedBy(procRoot, rdev)
	if err != nil {
		t.Fatal(err)
	}

	if holder != "" {
		t.Fatalf("a holder %q was reported where none holds the device", holder)
	}
}

// A PROCESS WHOSE OPEN FILES CANNOT BE READ IS COULD-NOT-TELL, never "holds nothing":
// the caller is about to take the device away.
func TestUnreadableOpenFilesAreCouldNotTell(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory its mode denies")
	}

	_, rdev := someBlockDevice(t)
	procRoot := t.TempDir()

	stageProcess(t, procRoot, "100", "/dev/null")

	fds := filepath.Join(procRoot, "100", "task", "100", "fd")
	if err := os.Chmod(fds, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(fds, 0o755); err != nil {
			t.Error(err)
		}
	})

	holder, err := openedBy(procRoot, rdev)
	if err == nil {
		t.Fatalf("an unreadable process was read as holding nothing (holder %q)", holder)
	}

	if !strings.Contains(err.Error(), "process 100") {
		t.Errorf("the refusal %q does not name the process", err)
	}
}

// A LOOP DEVICE BACKED BY THE DEVICE HOLDS IT, with no process's descriptor to show for
// it; an unconfigured loop device and one backed by another file hold nothing; and a
// backing file that cannot be read, or no longer resolves, is could-not-tell.
func TestALoopDeviceBackedByTheDeviceHoldsIt(t *testing.T) {
	device, rdev := someBlockDevice(t)
	sysBlock := t.TempDir()

	backing := func(name, body string) {
		t.Helper()
		dir := filepath.Join(sysBlock, name, "loop")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "backing_file"), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(sysBlock, "loop0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysBlock, "nvme0n1", "loop"), 0o755); err != nil {
		t.Fatal(err)
	}
	backing("loop1", "/dev/null")

	if loop, err := loopBackedBy(sysBlock, rdev); err != nil || loop != "" {
		t.Fatalf("no loop backed by %s was answered %q, %v", device, loop, err)
	}

	backing("loop3", device)
	if loop, err := loopBackedBy(sysBlock, rdev); err != nil || loop != "loop3" {
		t.Fatalf("a loop backed by %s was answered %q, %v; want loop3", device, loop, err)
	}
	if err := os.RemoveAll(filepath.Join(sysBlock, "loop3")); err != nil {
		t.Fatal(err)
	}

	// A BACKING FILE THAT NO LONGER RESOLVES may have been a second node of this device.
	backing("loop2", filepath.Join(sysBlock, "gone")+" (deleted)")
	if loop, err := loopBackedBy(sysBlock, rdev); err == nil || !strings.Contains(err.Error(), "cannot be identified") {
		t.Fatalf("a loop whose backing file is gone was answered %q, %v", loop, err)
	}
	if err := os.RemoveAll(filepath.Join(sysBlock, "loop2")); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() != 0 {
		unreadable := filepath.Join(sysBlock, "loop1", "loop", "backing_file")
		if err := os.Chmod(unreadable, 0); err != nil {
			t.Fatal(err)
		}
		if loop, err := loopBackedBy(sysBlock, rdev); err == nil {
			t.Fatalf("an unreadable backing file was read as holding nothing (%q)", loop)
		}
	}
}

// A PATH THAT IS NOT A BLOCK DEVICE IS REFUSED before anything is opened.
func TestUnheldRefusesWhatIsNotABlockDevice(t *testing.T) {
	err := unheld("/dev/null", t.TempDir(), t.TempDir())
	if err == nil || errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "not a block device") {
		t.Fatalf("a character device was answered %v", err)
	}
}

// A HELD DEVICE IS ErrHeld, through the real exclusive open and a staged process.
func TestAHeldDeviceIsErrHeld(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("an exclusive open of a block device needs root")
	}

	device, _ := someBlockDevice(t)
	procRoot := t.TempDir()

	stageProcess(t, procRoot, "300", device)

	err := unheld(device, procRoot, t.TempDir())
	// BY THE STAGED PROCESS, not by an exclusive open a mount refused, which the device
	// the test found may well have.
	if !errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "process 300 has") {
		t.Fatalf("a device a process holds was answered %v; want it named as process 300's", err)
	}
}
