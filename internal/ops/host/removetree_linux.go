package host

import (
	"errors"

	"golang.org/x/sys/unix"
)

// isMountRootAt reports whether the directory name under dir is the root of a
// mount, a bind mount of the same filesystem included, which a device number
// cannot tell. A kernel that does not say is answered as could-not-tell.
func isMountRootAt(dir int, name string) (bool, error) {
	var stx unix.Statx_t

	if err := unix.Statx(dir, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS, &stx); err != nil {
		return false, err
	}

	if stx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT == 0 {
		return false, errors.New("this kernel does not say whether a directory is a mount")
	}

	return stx.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0, nil
}
