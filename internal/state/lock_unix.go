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
	// No suppression here any more, and the reason is worth keeping: this call
	// used to take a path built from server.lock_dir and the deployment identity,
	// which gosec traced as tainted. Those now arrive through lockFileIn and its
	// directory descriptor instead, so the only caller left passes a path built
	// from the state directory alone.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, lockFileMode(shared))
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	// Never shared: this path is the state directory's own lock, which is private
	// by construction, so there is no group to match against.
	return flockOwned(f, path, shared, -1)
}

// lockFileIn is the same thing relative to an already-resolved directory.
//
// Relative to a HELD DESCRIPTOR, so the directory that was validated is the
// directory the lock lands in — separate resolutions of the same name can refer
// to different inodes, and everything between the check and the open is a window
// for exactly that.
func lockFileIn(root *os.Root, name, path string, shared bool, gid int) (*dirLock, error) {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, lockFileMode(shared))
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	return flockOwned(f, path, shared, gid)
}

// lockFileMode is the mode a NEW lock file is created with. The umask takes bits
// back off it, which is what ensureGroupAccess repairs.
func lockFileMode(shared bool) os.FileMode {
	if shared {
		return 0o660
	}

	return 0o600
}

func flockOwned(f *os.File, path string, shared bool, gid int) (*dirLock, error) {
	// THE UMASK SILENTLY REMOVES WHAT THE MODE ASKED FOR. A typical 022 turns
	// 0660 into 0640, and 0640 cannot be opened O_RDWR by the other account — so
	// the shared case would fail exactly as it did before, with the fix in place
	// and looking applied. Corrected explicitly rather than trusted.
	if shared {
		if err := ensureGroupAccess(f, path, gid); err != nil {
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
// group that directory is shared through, whatever the umask was.
//
// THE MODE BITS ARE ONLY HALF THE QUESTION, and checking them alone is what the
// previous version got wrong: 0660 says a group may open the file, not WHICH
// group. A service account whose primary group is `service` and whose
// supplemental group is `billet` creates a file owned by `service` in a
// non-setgid directory — every bit the check asks for, and still unopenable by
// the operator it was widened for. So the directory's gid is required to match.
//
// NOT a repair path for a file another account left too restrictive. That case
// cannot reach here at all: billet must open the file O_RDWR first, and an
// other-owned 0600 or 0640 file fails at the open. The earlier comment claimed
// otherwise, which made a hole look handled — it fails closed, but it fails, and
// the caller says how to fix it rather than pretending it recovered.
func ensureGroupAccess(f *os.File, path string, gid int) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect lock file %s: %w", path, err)
	}

	if fileGID(info) != gid {
		return fmt.Errorf(
			"lock file %s belongs to group %d, but the shared directory is group %d: the other "+
				"account sharing that directory would not be able to open it. Make the directory "+
				"setgid (chmod g+s) and remove the stale lock file so it is recreated",
			path, fileGID(info), gid)
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

// fileGID reports the group that owns a file, or -1 when the platform does not
// say. A gid the caller cannot read is never treated as a MATCH: an unknown
// group is exactly the case where widening the mode would look right and lock
// the other account out anyway.
func fileGID(info os.FileInfo) int {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}

	return int(st.Gid)
}

// dirGID is the same question asked of the shared lock directory, whose group is
// the one every participant must end up in.
func dirGID(info os.FileInfo) (int, error) {
	gid := fileGID(info)
	if gid < 0 {
		return -1, errors.New("the filesystem did not report a group owner")
	}

	return gid, nil
}
