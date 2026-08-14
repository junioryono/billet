package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// probeLock keeps two verifications on one machine from being the same microVM.
//
// A LOCK OF ITS OWN RATHER THAN state.LockDeployment, which was the first attempt
// and refused for the wrong reason: that one validates a 32-character deployment
// identity, so a probe identity failed its length check and reported "another
// verification is already running" when nothing was. A wrong diagnosis is worse than
// no lock, because it sends somebody looking for a process that does not exist.
//
// WHY IT IS NEEDED AT ALL: the probe's name is deliberately the same on every run,
// which is what makes cleaning up after a crashed one exact. That also means two
// verifications at once are two names for one microVM — the second one's pre-launch
// cleanup destroys the first one's live probe, and the first then reports a healthy
// image as never having reported back.
type probeLock struct{ file *os.File }

// takeProbeLock acquires it, or says who has it in terms an operator can act on.
func takeProbeLock(dir, identity string) (*probeLock, error) {
	if dir == "" {
		dir = "/var/lock"
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("billet images verify: make the lock directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, "billet-imageverify-"+identity+".lock")

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("billet images verify: open the lock at %s: %w", path, err)
	}

	// NON-BLOCKING, because waiting would turn a mistake into a hang: the thing
	// being waited for takes minutes, and the operator who started a second one
	// wants to be told, not queued.
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()

		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("billet images verify: another verification is already "+
				"running on this machine, and two would be the same microVM — the second one's "+
				"cleanup would destroy the first one's probe. Wait for it, or look at %s", path)
		}

		return nil, fmt.Errorf("billet images verify: lock %s: %w", path, err)
	}

	return &probeLock{file: file}, nil
}

// release drops the lock. Closing the descriptor releases it, so this cannot leave
// one held by a process that has exited.
func (l *probeLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}

	return l.file.Close()
}
