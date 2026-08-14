package firecracker

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// mknodBlock mirrors a host block device as a node inside a jail.
//
// THE JAILER DOES NOT DO THIS. It creates the four devices it knows a VMM needs —
// measured: /dev/kvm, /dev/net/tun, /dev/userfaultfd and /dev/urandom — and the
// root disk is not one of them, because it is different for every guest. A chroot
// means a path from the host is meaningless inside, so the device has to be
// present under the new root by its major and minor number.
//
// THE NUMBERS COME FROM THE DEVICE ITSELF rather than from a rule about rbd's major
// number. The kernel client's major is allocated dynamically, so it is not a
// constant billet could know, and a wrong pair produces a node that opens
// successfully onto a DIFFERENT device — which is a guest booting somebody else's
// disk rather than an error.
func mknodBlock(path, hostDevice string, uid, gid int) error {
	info, err := os.Stat(hostDevice)
	if err != nil {
		return fmt.Errorf("firecracker: stat the root disk %s: %w", hostDevice, err)
	}

	// syscall.Stat_t, NOT unix.Stat_t. The two are structurally identical and are
	// DIFFERENT TYPES, so asserting to the x/sys one always fails — os.Stat fills
	// Sys() with the standard library's. Every launch then failed with "does not
	// report a device number", about a device that reports one perfectly well.
	//
	// No unit test could reach this: mknod needs root and a real block device, so
	// it is stubbed everywhere except against a live host. Found on the first real
	// launch, which is the argument for having one.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("firecracker: %s does not report a device number", hostDevice)
	}

	// REFUSED IF IT IS NOT A BLOCK DEVICE. A regular file at that path would be
	// mknod'd as a block device with whatever numbers its filesystem reported,
	// pointing the guest at something arbitrary.
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("firecracker: %s is not a block device, so it is not a root disk billet "+
			"can attach", hostDevice)
	}

	dev := uint64(stat.Rdev)

	if err := unix.Mknod(path, unix.S_IFBLK|0o600, int(dev)); err != nil {
		return fmt.Errorf("firecracker: create the root disk node at %s (major %d, minor %d): %w",
			path, unix.Major(dev), unix.Minor(dev), err)
	}

	// 0600 AND OWNED BY THE JAILED ACCOUNT. The VMM opens this after dropping
	// privileges, so it has to be readable by that uid — and by nobody else, since
	// it is the job's whole filesystem.
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("firecracker: give the root disk node at %s to uid %d: %w", path, uid, err)
	}

	return nil
}

// chownTree gives a jail to the account the VMM will run as.
//
// AFTER EVERYTHING IS IN PLACE, never as each file appears: a partly-built jail
// that is already writable by the unprivileged account is a window in which the
// kernel image or the root-disk node could be replaced before the VMM opens them.
//
// CONFINED TO THE JAIL, AND WITHOUT FOLLOWING LINKS. A plain walk resolves each
// path again for the operation, so a symlink appearing between the two hands a file
// OUTSIDE the jail to the account the guest's VMM runs as — and this runs as root.
// os.Root resolves every step against a directory handle rather than against a
// name, and Lchown acts on the link rather than its target, so neither half of that
// is reachable. Nothing billet writes into a jail is a link; this is about what
// something else could put there in between.
func chownTree(root string, uid, gid int) error {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("firecracker: open the jail at %s: %w", root, err)
	}

	defer dir.Close()

	return fs.WalkDir(dir.FS(), ".", func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := dir.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("firecracker: give %s to uid %d: %w",
				filepath.Join(root, path), uid, err)
		}

		return nil
	})
}

// lookupJailUser resolves the account the jailer drops the VMM to.
//
// RESOLVED AT CONSTRUCTION, so a deployment whose account does not exist is told
// once at startup rather than on every launch — and told what to do about it. A
// jailer that cannot resolve the uid fails per job, in a message about a number.
//
// ROOT IS REFUSED, and that is the point of the setting rather than a hardening
// detail. The jailer's whole purpose is to drop privileges before the VMM parses
// anything; --uid 0 keeps every one of them, in front of a process whose input is
// somebody's CI job.
func lookupJailUser(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if strings.Contains(err.Error(), "unknown user") || asUnknownUser(err, &unknown) {
			return 0, 0, fmt.Errorf("firecracker: there is no %q account on this host for the "+
				"jailer to drop the vmm to; create one with `useradd --system --no-create-home "+
				"--shell /usr/sbin/nologin %s`, or name another with "+
				"node.firecracker.jail_user: %w", name, name, err)
		}

		return 0, 0, fmt.Errorf("firecracker: look up the %q account: %w", name, err)
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("firecracker: the %q account's uid %q is not a number: %w",
			name, u.Uid, err)
	}

	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("firecracker: the %q account's gid %q is not a number: %w",
			name, u.Gid, err)
	}

	if uid == 0 || gid == 0 {
		return 0, 0, fmt.Errorf("firecracker: node.firecracker.jail_user is %q, which is uid %d "+
			"and gid %d; the jailer exists to drop privileges before the vmm reads anything, so "+
			"running it as root would leave the guest's vmm with all of them", name, uid, gid)
	}

	return uid, gid, nil
}

// asUnknownUser reports whether an error is os/user's unknown-user error.
func asUnknownUser(err error, target *user.UnknownUserError) bool {
	if e, ok := err.(user.UnknownUserError); ok { //nolint:errorlint // the stdlib returns it unwrapped
		*target = e

		return true
	}

	return false
}
