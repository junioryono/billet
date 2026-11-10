package blockfs

import (
	"errors"
	"fmt"
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

// A LOOP DEVICE BACKED BY THE DEVICE HOLDS IT, by the kernel's own answer for its backing
// and never its pathname; an unconfigured loop and one backed by something else hold
// nothing; and a loop the kernel cannot answer for is could-not-tell.
func TestALoopDeviceBackedByTheDeviceHoldsIt(t *testing.T) {
	const rdev = 0x10300
	sysBlock := t.TempDir()
	for _, name := range []string{"loop0", "loop1", "loop2", "nvme0n1"} {
		if err := os.MkdirAll(filepath.Join(sysBlock, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	answers := map[string]struct {
		rdev       uint64
		configured bool
		err        error
	}{
		"loop0": {},
		"loop1": {rdev: 0x10301, configured: true},
		"loop2": {rdev: 0, configured: true},
	}
	backing := func(loop string) (uint64, bool, error) {
		if loop == "nvme0n1" {
			t.Errorf("a device that is not a loop was asked for its backing")
		}
		a := answers[loop]

		return a.rdev, a.configured, a.err
	}

	if loop, err := loopBackedBy(sysBlock, rdev, backing); err != nil || loop != "" {
		t.Fatalf("no loop backed by the device was answered %q, %v", loop, err)
	}

	answers["loop2"] = struct {
		rdev       uint64
		configured bool
		err        error
	}{rdev: rdev, configured: true}
	if loop, err := loopBackedBy(sysBlock, rdev, backing); err != nil || loop != "loop2" {
		t.Fatalf("a loop backed by the device was answered %q, %v; want loop2", loop, err)
	}

	answers["loop1"] = struct {
		rdev       uint64
		configured bool
		err        error
	}{err: errors.New("permission denied")}
	if loop, err := loopBackedBy(sysBlock, rdev, backing); err == nil || !strings.Contains(err.Error(), "loop1") {
		t.Fatalf("a loop the kernel would not answer for was answered %q, %v", loop, err)
	}
}

// A SHARED MAPPING HOLDS THE DEVICE AFTER ITS DESCRIPTOR IS CLOSED, so a process whose
// mappings name it holds it, with nothing in any descriptor table, read through a thread
// that is left when the leader has exited and shows none.
func TestAMappingHoldsTheDevice(t *testing.T) {
	device, rdev := someBlockDevice(t)
	procRoot := t.TempDir()

	// THE LEADER HAS EXITED, SHOWING NO MAPPINGS; a thread left holds the mapping.
	stageProcess(t, procRoot, "500", "/dev/null")
	if err := os.MkdirAll(filepath.Join(procRoot, "500", "map_files"), 0o755); err != nil {
		t.Fatal(err)
	}
	stageThread(t, procRoot, "500", "501")
	maps := filepath.Join(procRoot, "501", "map_files")
	if err := os.MkdirAll(maps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(maps, "7f0000000000-7f0000001000")); err != nil {
		t.Fatal(err)
	}

	holder, err := openedBy(procRoot, rdev)
	if err != nil {
		t.Fatal(err)
	}
	if holder != "500" {
		t.Fatalf("a process mapping %s was reported as %q, want 500", device, holder)
	}
}

// A DEVICE WITH A PARTITION IS HELD for all its own number's scans can tell, and one
// sysfs does not show is could-not-tell.
func TestADeviceWithAPartitionIsRefused(t *testing.T) {
	const rdev = 0x10300
	sysDev := t.TempDir()
	dir := filepath.Join(sysDev, fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev)))
	if err := os.MkdirAll(filepath.Join(dir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	// AN ATTRIBUTE IS A FILE, under which nothing is a partition.
	if err := os.WriteFile(filepath.Join(dir, "alignment_offset"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := refusePartitions(sysDev, "/dev/nvme3n1", rdev); err != nil {
		t.Fatalf("a whole disk with no partition was answered %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "nvme3n1p1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nvme3n1p1", "partition"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := refusePartitions(sysDev, "/dev/nvme3n1", rdev); !errors.Is(err, ErrHeld) ||
		!strings.Contains(err.Error(), "nvme3n1p1") {
		t.Fatalf("a disk with a partition was answered %v", err)
	}

	if err := refusePartitions(t.TempDir(), "/dev/nvme3n1", rdev); err == nil || errors.Is(err, ErrHeld) {
		t.Fatalf("a device sysfs does not show was answered %v; want could-not-tell", err)
	}
}

// A PATH THAT IS NOT A BLOCK DEVICE IS REFUSED before anything is opened.
func TestUnheldRefusesWhatIsNotABlockDevice(t *testing.T) {
	err := unheld("/dev/null", hostView{})
	if err == nil || errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "not a block device") {
		t.Fatalf("a character device was answered %v", err)
	}
}

// A HELD DEVICE IS ErrHeld, through the real exclusive open and a staged process.
func TestAHeldDeviceIsErrHeld(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("an exclusive open of a block device needs root")
	}

	device, rdev := someBlockDevice(t)
	// A DEVICE IN USE ON THIS HOST is refused by the exclusive open before the staged
	// process is read, which is not what this test is about.
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_EXCL|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("%s cannot be opened exclusively here (%v)", device, err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}

	procRoot := t.TempDir()
	stageProcess(t, procRoot, "300", device)
	sysDev := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysDev, fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev))), 0o755); err != nil {
		t.Fatal(err)
	}

	err = unheld(device, hostView{proc: procRoot, sysBlock: t.TempDir(), sysDev: sysDev,
		loopBacking: func(string) (uint64, bool, error) { return 0, false, nil }})
	if !errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "process 300 has") {
		t.Fatalf("a device a process holds was answered %v; want it named as process 300's", err)
	}
}
