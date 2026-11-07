//go:build unix

package state

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"

	"github.com/junioryono/billet/internal/regularfile"
)

// LockHeld reports whether a process holds the lock at path, by trying it without
// waiting and releasing it at once: DirectoryLockPath for a control plane,
// DeploymentLockPath for a node. It creates nothing and changes no file's owner or
// mode, so asking as another account leaves nothing a service could not open later.
//
// A path that does not exist is a lock nobody holds, because a holder keeps open the
// file it locked; a lock file removed while held is the one case that answer misses.
// Any other failure to look is an error, never "not held".
func LockHeld(path string) (bool, error) {
	f, _, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe the lock %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}

		return false, fmt.Errorf("probe the lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return false, fmt.Errorf("release the probe of %s: %w", path, err)
	}

	return false, nil
}
