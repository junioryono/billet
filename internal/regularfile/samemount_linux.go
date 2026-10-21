//go:build linux

package regularfile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// statx is unix.Statx behind a seam, so a test can be a kernel that reports no
// mount identity.
var statx = unix.Statx

// mountOf asks statx about the descriptor itself (AT_EMPTY_PATH), so the answer
// is about the inode and the mount the open reached, never a name resolved
// again. STATX_MNT_ID arrived in Linux 5.8; a kernel that does not report it
// answers with the device alone, and a kernel without statx (ENOSYS) or a
// seccomp filter that denies it (EPERM, as older container runtimes' default
// profiles do) is answered by fstat.
func mountOf(f *os.File) (Mount, error) {
	var stx unix.Statx_t

	err := statx(int(f.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT,
		unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stx)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			return Mount{}, err
		}

		return Mount{Dev: st.Dev}, nil
	}

	if err != nil {
		return Mount{}, err
	}

	m := Mount{Dev: unix.Mkdev(stx.Dev_major, stx.Dev_minor)}

	if stx.Mask&unix.STATX_MNT_ID != 0 {
		m.Known = true
		m.ID = stx.Mnt_id
	}

	return m, nil
}
