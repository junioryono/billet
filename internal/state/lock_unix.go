//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// dirLock is an exclusive advisory lock on the state directory, held for the
// lifetime of the DB.
//
// SQLite's own single-writer rule stops two connections writing simultaneously.
// It does not stop two billet control planes from both long-polling GitHub,
// both issuing commands to nodes, and taking turns writing mutually
// inconsistent scheduling decisions to the same ledger. That failure is quiet
// and produces double-admitted jobs, so the second process must not start at
// all.
//
// flock is used rather than a PID file: the kernel releases it automatically if
// the process dies, so a crashed server never leaves a stale lock that needs
// manual cleanup at 3am.
type dirLock struct {
	f *os.File
}

func lockDir(stateDir string) (*dirLock, error) {
	lock, err := lockFile(filepath.Join(stateDir, "billet.lock"), false)
	if err != nil && errors.Is(err, ErrLocked) {
		// Reported against the DIRECTORY, which is what the operator named.
		return nil, fmt.Errorf("%w: %s", ErrLocked, stateDir)
	}

	return lock, err
}

// lockFile takes an exclusive advisory lock on one path.
//
// Split out from lockDir because the host-wide deployment lock needs the same
// mechanism at a path of its own — and having two flock implementations in one
// package is how they come to disagree about whether a close releases.
//
// shared widens the new file to the owning group, and exists for exactly one
// deployment: a setgid directory shared by a service account and an operator who
// both reach the same docker socket. WITHOUT IT server.lock_dir DOES NOT DELIVER
// WHAT IT ADVERTISES — the first process to run creates the file 0600, and the
// other user can then never open it, not merely while the lock is held but ever
// after, so the documented cross-user case failed permanently and its only
// escape was to turn the protection off. The narrow widening is safe because the
// group is the operator's own choice and world access is refused separately.
func lockFile(path string, shared bool) (*dirLock, error) {
	// gosec traces a path here from configuration (server.lock_dir) and from the
	// deployment identity, and it is right that both reach this call. Both are
	// accounted for: the directory is the operator's own choice, which is the
	// point of the key — it exists so they can put the lock where every billet
	// sharing a container runtime will meet — and the identity is checked against
	// validDeploymentID immediately before it is interpolated, precisely so a
	// separator cannot leave that directory.
	//
	mode := os.FileMode(0o600)
	if shared {
		mode = 0o660
	}

	// O_NOFOLLOW so the final component cannot be swapped for a symlink pointing
	// somewhere else — the directory check rules out an untrusted party creating
	// one, and this makes the open itself refuse rather than depending on that
	// check having run first.
	//
	//nolint:gosec // G703: both taint sources are validated or operator-supplied; see above.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	// THE UMASK SILENTLY REMOVES WHAT THE MODE ASKED FOR. A typical 022 turns the
	// 0660 above into 0640, and 0640 cannot be opened O_RDWR by the other account
	// — so the shared case would fail exactly as it did before, with the fix in
	// place and looking applied. Corrected explicitly rather than trusted.
	if shared {
		if err := ensureGroupAccess(f, path); err != nil {
			_ = f.Close()

			return nil, err
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &dirLock{f: f}, nil
}

// ensureGroupAccess makes a lock file in a shared directory openable by the
// owning group, whatever the umask was.
//
// A failure to chmod is NOT itself an error: in the shared case the file may
// have been created by the other account, in which case it is not ours to change
// and already has the mode we want. What matters is the mode that ends up there,
// so that is what is checked.
func ensureGroupAccess(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect lock file %s: %w", path, err)
	}

	if info.Mode().Perm()&0o060 == 0o060 {
		return nil
	}

	chmodErr := f.Chmod(0o660)

	info, err = f.Stat()
	if err != nil {
		return fmt.Errorf("inspect lock file %s: %w", path, err)
	}

	if info.Mode().Perm()&0o060 != 0o060 {
		return fmt.Errorf(
			"lock file %s is mode %o in a group-shared directory and could not be widened "+
				"(%v); the other account sharing this directory would never be able to open "+
				"it, so the lock would not exclude anything",
			path, info.Mode().Perm(), chmodErr)
	}

	return nil
}

func (l *dirLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Closing the descriptor drops the flock; unlocking first keeps the intent
	// explicit for anyone reading this.
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
