package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/deploymentid"
)

// probeLock keeps two verifications on one machine from being the same microVM.
//
// A LOCK OF ITS OWN RATHER THAN state.LockDeployment, and the reason is what that
// lock MEANS rather than what it accepts. It answers "one billet owns this identity",
// and its refusal is written for that question: it talks about two billets managing
// the same containers and about copied state directories. Neither describes a second
// verification, and a wrong diagnosis is worse than no lock, because it sends
// somebody after a problem that does not exist.
//
// Keying it on the real deployment would go further wrong, since a running
// `billet node` on this machine holds exactly that key from start to exit — every
// healthy host would refuse. Keying it on the derived probe identity would avoid
// that and still answer the wrong question.
//
// WHY IT IS NEEDED AT ALL: the probe's name is deliberately the same on every run,
// which is what makes cleaning up after a crashed one exact. That also means two
// verifications at once are two names for one microVM — the second one's pre-launch
// cleanup destroys the first one's live probe, and the first then reports a healthy
// image as never having reported back.
type probeLock struct{ file *os.File }

// takeProbeLock acquires it, or says who has it in terms an operator can act on.
func takeProbeLock(dir, deployment string) (*probeLock, error) {
	// CHECKED NEXT TO THE INTERPOLATION, which is the rule state.LockDeployment
	// follows for the same reason: this builds a filename out of the value, and an
	// identity carrying a path separator lands outside the lock directory or fails to
	// open — after which the protection is silently off, reported as a host with
	// nowhere to put a lock.
	//
	// The one caller today parses the identity before it gets here, so this is the
	// sink re-applying an invariant its source already holds rather than the only
	// thing standing between a certificate's organization field and a pathname. It is
	// here because the next caller is not obliged to know that.
	if err := deploymentid.Validate(deployment); err != nil {
		return nil, fmt.Errorf("billet images verify: refusing to lock: %w", err)
	}

	if dir == "" {
		dir = "/var/lock"
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("billet images verify: make the lock directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, "billet-imageverify-"+deployment+".lock")

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

// release drops the lock.
//
// UNLOCKED BEFORE IT IS CLOSED. A flock belongs to the open file description, and
// a child forked by any goroutine holds a copy of that description until its exec
// closes it; closing only this descriptor inside that window leaves the lock held.
// Measured 2026-09-26: TestASecondVerificationOnThisMachineIsRefused failed on CI
// with "the lock stayed held after it was released" while parallel tests forked.
// LOCK_UN releases the description's lock whoever else refers to it.
func (l *probeLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}

	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)

	return errors.Join(unlockErr, l.file.Close())
}
