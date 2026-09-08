//go:build linux

package regularfile

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openForIdentity names the inode without opening it: an O_PATH descriptor
// waits on no FIFO and performs no device open, and is what the fstat below
// looks at.
func openForIdentity(path string, opts Options) (*os.File, error) {
	flags := unix.O_PATH | unix.O_CLOEXEC
	if opts.NoFollow {
		flags |= unix.O_NOFOLLOW
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// configfsMagic is configfs's superblock magic, which x/sys/unix does not name:
// its attributes are regular-mode files whose reads dispatch to a subsystem's
// callback (a TSM report attribute produces an attestation report when read).
const configfsMagic = 0x62656570

// pseudoFilesystems are the kernel filesystems whose regular-mode files are not
// stored bytes: a read of /proc/kmsg waits for the next message and consumes a
// shared position, a sysfs or configfs attribute is a call into a driver. None
// of billet's inputs live on one, and a name that resolves there is refused
// before it is read. THIS IS AN ENUMERATED DENYLIST of the kernel filesystems a
// supported host mounts, not a proof about every filesystem: one outside it
// that also dispatches reads is the residual the package comment names.
// Statfs_t.Type is int32 on some architectures, so the comparison widens.
var pseudoFilesystems = map[int64]string{
	configfsMagic:              "configfs",
	unix.PROC_SUPER_MAGIC:      "procfs",
	unix.SYSFS_MAGIC:           "sysfs",
	unix.DEBUGFS_MAGIC:         "debugfs",
	unix.TRACEFS_MAGIC:         "tracefs",
	unix.SECURITYFS_MAGIC:      "securityfs",
	unix.CGROUP_SUPER_MAGIC:    "cgroup",
	unix.CGROUP2_SUPER_MAGIC:   "cgroup2",
	unix.DEVPTS_SUPER_MAGIC:    "devpts",
	unix.BPF_FS_MAGIC:          "bpf",
	unix.PSTOREFS_MAGIC:        "pstore",
	unix.EFIVARFS_MAGIC:        "efivarfs",
	unix.SELINUX_MAGIC:         "selinuxfs",
	unix.SMACK_MAGIC:           "smackfs",
	unix.BINFMTFS_MAGIC:        "binfmt_misc",
	unix.NSFS_MAGIC:            "nsfs",
	unix.SOCKFS_MAGIC:          "sockfs",
	unix.PIPEFS_MAGIC:          "pipefs",
	unix.ANON_INODE_FS_MAGIC:   "anon_inodefs",
	unix.USBDEVICE_SUPER_MAGIC: "usbfs",
}

// pseudoFilesystemName is the denylist's name for a superblock magic, or "".
func pseudoFilesystemName(magic int64) string { return pseudoFilesystems[magic] }

// reopen reads THE INODE ALREADY HELD, through /proc/self/fd, so no pathname is
// resolved a second time and a replacement between the identity open and this
// one cannot be read for the original. The rule runs first: a regular file, on a
// filesystem that stores bytes (fstatfs works on an O_PATH descriptor, the
// kernel taking it raw). The reopen is NON-BLOCKING, because a plain open of a
// regular file breaks a write lease another process holds on it and waits for
// that; with O_NONBLOCK the kernel answers EWOULDBLOCK instead, which is
// could-not-tell. WHAT THAT AVOIDS IS THE WAIT, NOT THE BREAK: the kernel marks
// the lease for downgrade and signals its holder (SIGIO) before it looks at
// O_NONBLOCK, so a lease holder still learns of the open; that is a residual
// the inspector's promise excludes by name. An O_PATH descriptor of a symlink
// taken with O_NOFOLLOW is the link itself and fails the regular-file rule.
func reopen(id *os.File) (*os.File, os.FileInfo, error) {
	info, err := requireRegular(id)
	if err != nil {
		return nil, nil, err
	}
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(id.Fd()), &st); err != nil {
		return nil, nil, reopenError(id, fmt.Errorf("fstatfs: %w", err))
	}
	if name := pseudoFilesystemName(int64(st.Type)); name != "" { //nolint:unconvert // Statfs_t.Type is int32 on the 32-bit architectures
		return nil, nil, &os.PathError{Op: "open", Path: id.Name(), Err: fmt.Errorf("%w (%s)", ErrUnsupportedFilesystem, name)}
	}
	if reopenFailure != nil {
		if err := reopenFailure(); err != nil {
			return nil, nil, reopenError(id, err)
		}
	}
	// The name is this process's own descriptor number and nothing from the
	// host: a reopen of an inode already held, never a path resolved again.
	fd, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", id.Fd()), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, reopenError(id, err)
	}
	return os.NewFile(uintptr(fd), id.Name()), info, nil
}
