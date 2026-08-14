package firecracker

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

	// REQUIRED ON ONE PLATFORM AND REDUNDANT ON THE OTHER, which is why the
	// conversion below is suppressed rather than deleted: Rdev is uint64 on linux
	// and int32 on darwin, and this package must COMPILE for darwin because the
	// cross-build matrix includes it. Deleting the conversion satisfies the linter
	// on linux and breaks `make cross`.
	//
	// Two linters are suppressed because each platform provokes a different one: on
	// linux the conversion is redundant, and on darwin it is real — which makes the
	// suppression itself unused there, and an unused suppression is its own error.

	//nolint:unconvert,nolintlint // Rdev is uint64 on linux and int32 on darwin
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
