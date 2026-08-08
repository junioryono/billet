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
	lock, err := lockFile(filepath.Join(stateDir, "billet.lock"))
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
func lockFile(path string) (*dirLock, error) {
	// gosec traces a path here from configuration (server.lock_dir) and from the
	// deployment identity, and it is right that both reach this call. Both are
	// accounted for: the directory is the operator's own choice, which is the
	// point of the key — it exists so they can put the lock where every billet
	// sharing a container runtime will meet — and the identity is checked against
	// validDeploymentID immediately before it is interpolated, precisely so a
	// separator cannot leave that directory.
	//
	//nolint:gosec // G703: both taint sources are validated or operator-supplied; see above.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
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
