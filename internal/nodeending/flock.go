package nodeending

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// noFollow refuses a symlink planted at the lock's name.
const noFollow = syscall.O_NOFOLLOW

// flock takes an exclusive lock on f, waiting for the holder: a read-modify-write
// of the record is a few milliseconds.
func flock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

// ownedByReader refuses a record this process's user did not write, or one
// another user could have.
func ownedByReader(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("its owner cannot be read")
	}

	if euid := os.Geteuid(); int(st.Uid) != euid {
		return fmt.Errorf("it is owned by uid %d, not %d, so it is not this host's evidence", st.Uid, euid)
	}

	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("its mode %04o lets another user write it", perm)
	}

	return nil
}
