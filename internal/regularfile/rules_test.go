package regularfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// linuxOPath is Linux's O_PATH, spelled here so the assertion compiles on every
// platform and runs where the flag exists.
const linuxOPath = 0x200000

// THE ACQUISITION IS IDENTITY-ONLY ON LINUX: a descriptor opened O_RDONLY, even
// non-blocking, would pass every other test here while opening a device it
// names, so the flag itself is asserted.
func TestLinuxAcquiresAnIdentityOnlyDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("O_PATH exists on Linux only; elsewhere the package documents the non-blocking open")
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := openForIdentity(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = id.Close() }()
	flags, err := unix.FcntlInt(id.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&linuxOPath == 0 {
		t.Fatalf("the identity descriptor's flags are %#x, without O_PATH: the file was opened, not merely named", flags)
	}
}

// A REOPEN THAT FAILS IS NEVER ABSENCE. Without /proc/self/fd the readable open
// fails with ENOENT for a file that exists and may be held by another process;
// a caller that reads fs.ErrNotExist as "absent" would then report an unheld
// lock or a missing record. The seam stands in for the platform condition.
func TestAFailedReopenIsNotTheFilesAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reopenFailure = func() error { return syscall.ENOENT }
	t.Cleanup(func() { reopenFailure = nil })
	_, _, err := Open(path, Options{})
	if err == nil {
		t.Fatal("a failed reopen was reported as success")
	}
	if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
		t.Fatalf("a failed reopen reads as the file's absence: %v", err)
	}
	if !errors.Is(err, ErrReopen) || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("the refusal does not say it is a reopen failure with its cause quoted: %v", err)
	}
}

// A REGULAR-MODE FILE ON PROCFS IS NOT STORED BYTES: /proc/self/status is
// S_ISREG and a read of /proc/kmsg waits for the kernel's next message, so the
// filesystem is asked for before anything is read.
func TestARegularFileOnAPseudoFilesystemIsRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs exists on Linux only")
	}
	_, _, err := Open("/proc/self/status", Options{})
	if !errors.Is(err, ErrUnsupportedFilesystem) || !strings.Contains(err.Error(), "procfs") {
		t.Fatalf("a procfs file was not refused as one: %v", err)
	}
	if _, err := ReadFile("/proc/self/status", 1<<20, Options{}); !errors.Is(err, ErrUnsupportedFilesystem) {
		t.Fatalf("ReadFile read a procfs file: %v", err)
	}
}

// THE DENYLIST NAMES THE PSEUDO-FILESYSTEMS A SUPPORTED HOST MOUNTS, configfs
// among them, whose regular-mode attributes dispatch reads to a subsystem; a
// fixture cannot mount one, so the rule's table is what is asserted.
func TestTheDenylistNamesTheKernelPseudoFilesystems(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the denylist exists on Linux only")
	}
	for magic, name := range map[int64]string{0x62656570: "configfs", 0x9fa0: "procfs", 0x62656572: "sysfs"} {
		if got := pseudoFilesystemName(magic); got != name {
			t.Errorf("magic %#x is %q in the denylist, want %q", magic, got, name)
		}
	}
}

// THE READABLE DESCRIPTOR IS NON-BLOCKING ON LINUX, so a write lease another
// process holds on the file answers EWOULDBLOCK instead of a wait.
func TestTheReadableDescriptorIsNonBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&syscall.O_NONBLOCK == 0 {
		t.Fatalf("the readable descriptor's flags are %#x, without O_NONBLOCK", flags)
	}
}
