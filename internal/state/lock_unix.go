//go:build unix

package state

import (
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
	path := filepath.Join(stateDir, "billet.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("%w: %s", ErrLocked, stateDir)
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
