package nodeending

import (
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
