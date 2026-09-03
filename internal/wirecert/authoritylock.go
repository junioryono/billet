package wirecert

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// authorityLockFile is the lock's name inside the state directory.
//
// BESIDE THE CA DIRECTORY RATHER THAN INSIDE IT, for the same reason the
// authority marker is: what the lock coordinates includes REPLACING the contents
// of that directory, and a lock file living among the things being renamed is a
// lock whose inode can move out from under its holder.
const authorityLockFile = "ca.lock"

// AuthorityLockPath is where that lock lives for a state directory.
//
// EXPORTED SO NOBODY COPIES THE NAME. A privileged `billet local restore`
// CREATES this file as root inside a directory the service account owns, and
// what hands it back has to name the same file — a second literal somewhere else
// is a control plane that cannot take its own authority lock, discovered on the
// first start after a restore.
func AuthorityLockPath(stateDir string) string {
	return filepath.Join(stateDir, authorityLockFile)
}

// AuthorityLock is an exclusive hold on a deployment's certificate authority.
//
// WHAT IT PREVENTS is a reader capturing half a rotation. `billet ca rotate`
// mutates five files in sequence — it copies the old pair aside as
// ca-previous.*, then renames a freshly minted key and certificate into place —
// so anything reading the directory while that runs can come away with a key
// from one generation beside a certificate from another, or with only half of
// the previous pair. A backup is exactly such a reader, and the archive it
// writes would load cleanly and verify nothing.
//
// WHAT IT DOES NOT COVER, AND WHY THAT IS NOW SAFE. `LoadServing` — which is
// the whole of the control plane's read, and runs ONCE while it starts — does
// not take it, and neither does `LoadOrCreateCA` behind it. The renewal signer
// is not in the set at all: `SignNodeCSR` signs with the in-memory *CA and reads
// nothing from disk.
//
// So the only thing a rotation can collide with is a control plane STARTING, and
// that is closed by publication ORDER rather than by this lock. Rotate writes
// ca-previous.crt before ca-previous.key and Retire removes them the other way
// round, so a certificate with no key beside it always means "started, not
// committed" and the reader presents with the current authority; and the one
// torn read the two renames of the current pair can produce is repaired from
// ca-previous.key, which is durable before the first rename. Every instant of a
// rotation is therefore a state a reader answers correctly. See LoadServing.
//
// TAKING IT IN THE READER WAS THE OTHER CANDIDATE AND WAS REJECTED. It is
// non-blocking on purpose — every holder is a command somebody is waiting on —
// so a reader would need a waiting acquisition, and `billet local backup` holds
// this across the whole ledger snapshot. A control plane that refuses to start
// because a backup is running is a worse failure than the diagnostic being
// fixed, and a startup path that took this lock could not call anything else
// that takes it: a second flock on a separate descriptor in one process is
// denied, so it would deadlock against itself and report another billet.
type AuthorityLock struct {
	f    *os.File
	path string
}

// LockAuthority takes the lock, or names what already holds it.
//
// NON-BLOCKING, on the same argument the lifecycle lock makes: the operator who
// started a second command wants to be told what is already running, not queued
// silently behind a rotation.
//
// It is a real exclusion between PROCESSES and also within one — measured on
// darwin, a second flock on a separate descriptor in the same process is denied
// with EWOULDBLOCK — so a caller holding this must not call anything that takes
// it again.
func LockAuthority(stateDir string) (*AuthorityLock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("wirecert: create %s: %w", stateDir, err)
	}

	path := AuthorityLockPath(stateDir)

	// O_NOFOLLOW: the lock is only worth anything if it is on the inode this
	// path names, and a symlink here would silently move the exclusion somewhere
	// else — after which two commands rewrite one authority believing they are
	// alone.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("wirecert: open the authority lock %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf(
				"wirecert: another billet is working on this deployment's certificate authority "+
					"(%s is held). `billet ca rotate`, `billet ca retire`, `billet local backup` "+
					"and `billet local restore` take it in turn so none of them sees half a "+
					"rotation — wait for the other one to finish", path)
		}

		return nil, fmt.Errorf("wirecert: lock %s: %w", path, err)
	}

	return &AuthorityLock{f: f, path: path}, nil
}

// Release drops the lock. Closing the descriptor releases it, so a process that
// exits cannot leave one held.
func (l *AuthorityLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}

	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil

	if unlockErr != nil {
		return fmt.Errorf("wirecert: unlock %s: %w", l.path, unlockErr)
	}

	if closeErr != nil {
		return fmt.Errorf("wirecert: close %s: %w", l.path, closeErr)
	}

	return nil
}
