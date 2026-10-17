package nodeending

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// noFollow refuses a symlink planted at the lock's name.
const noFollow = syscall.O_NOFOLLOW

// lockWait bounds the wait for the record's lock. A write is milliseconds and
// a judgement asks systemd once under it (its own bound is 30 s); a holder past
// this is hung, and waiting on it would hold every start and stop behind it. It
// leaves a starting node most of its unit's 120 s start budget.
var lockWait = 60 * time.Second

// errLockHeld is the refusal when the lock stays held past lockWait.
var errLockHeld = errors.New("the ending record is locked by another process past the wait for it")

// flock takes an exclusive lock on f, waiting for the holder up to lockWait.
func flock(f *os.File) error {
	deadline := time.Now().Add(lockWait)

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return err
		case time.Now().After(deadline):
			return errLockHeld
		}

		time.Sleep(20 * time.Millisecond)
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
