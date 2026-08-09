//go:build unix

package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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
	return flockOwned(f, path, shared, fileGroup{})
}

// openLockDir opens a lock directory as a descriptor, refusing a symlink.
//
// O_DIRECTORY|O_NOFOLLOW is the single resolution everything else hangs off:
// the name is walked once, a symlinked final component is refused outright
// rather than diagnosed afterwards, and the returned descriptor keeps referring
// to THAT inode however the name is rearranged later.
func openLockDir(dir string, mayTighten bool) (*os.File, error) {
	// SEARCH-ONLY WHERE READ IS NOT NEEDED, and the previous version's reason for
	// not doing this was simply wrong. It claimed darwin has neither O_PATH nor
	// O_SEARCH — that was a check of whether x/sys/unix EXPORTS the symbol, read
	// as a statement about the platform. darwin's own fcntl.h defines
	// `O_SEARCH (O_EXEC | O_DIRECTORY)`, and a probe confirmed the whole chain
	// works on a directory the caller cannot read: open, fstat, openat, and
	// openat with O_CREAT.
	//
	// The fchmod objection was misplaced too. Tightening happens ONLY for the
	// default directory billet picked itself; a directory the operator named is
	// never tightened. So the two cases can differ, and the drop-box shape (mode
	// 2730: write and traverse, no listing) works after all.
	if !mayTighten {
		if flag := searchOnlyFlag(); flag != 0 {
			fd, err := unix.Open(dir, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err == nil {
				return os.NewFile(uintptr(fd), dir), nil
			}
			// Falls through to O_RDONLY, which produces the better diagnosis
			// below for the cases that are genuinely about permissions.
		}
	}

	f, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err == nil {
		return f, nil
	}

	// O_RDONLY NEEDS READ ON THE DIRECTORY, which openat of a known filename does
	// not fundamentally require — search and write would do. So a shared directory
	// provisioned 2730 (the classic drop-box shape: write and traverse, no
	// listing) is refused even though every account could open the lock by name.
	//
	// Requiring read rather than working around it, deliberately. A search-only
	// descriptor is O_PATH on Linux and neither O_PATH nor O_SEARCH on darwin —
	// checked, not assumed — and an O_PATH descriptor cannot be fchmod'd, which
	// the tightening path needs. That is platform-divergent handle juggling for an
	// uncommon shape, so billet asks for the simpler contract and says so instead
	// of failing with a bare EACCES.
	// SUGGESTIONS, NOT A DIAGNOSIS. A successful stat proves the directory is
	// reachable and its metadata readable; it does NOT prove why open was
	// refused. The same EACCES comes from a private directory at 0300 (where the
	// fix is 0700, and telling the operator to widen it to 2770 would be actively
	// wrong), from an ACL or MAC policy that permits a lookup and denies an open,
	// and from an ancestor — so the message lists what it could be rather than
	// asserting the one it guessed.
	//
	// Reached far less often now that the operator-chosen directory opens
	// search-only: this is the private path, or a platform with no search-only
	// open, or a search-only open that itself failed.
	if errors.Is(err, fs.ErrPermission) {
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil, fmt.Errorf(
				"%w — billet could reach this directory but not open it. If it is private to "+
					"this account, it needs mode 0700; if two accounts share it, 2770 (or 2730 "+
					"where billet can open a directory for search only); otherwise look for an "+
					"ACL or a MAC policy that allows a lookup and denies an open", err)
		}
	}

	return nil, err
}

// lockFileIn takes the lock relative to an already-resolved directory.
//
// openat, NOT os.Root — and this is the second time a no-follow claim here has
// been wrong, so it is worth stating why. os.Root confines a path to its tree
// but FOLLOWS symlinks that stay inside it, and its Unix implementation applies
// its own O_NOFOLLOW internally, inspects the link on ELOOP and then follows it;
// a caller's syscall.O_NOFOLLOW is indistinguishable from that and is ignored.
// Measured, not read: a relative `link.lock -> real.lock` inside the directory
// was opened and turned out to be the same file as its target. So a lock file
// could be redirected onto another inode within the very directory billet had
// just validated. unix.Openat honours the flag because the kernel does.
func lockFileIn(dirf *os.File, name, path string, shared bool, gid fileGroup) (*dirLock, error) {
	fd, err := unix.Openat(
		int(dirf.Fd()),
		name,
		unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(lockFileMode(shared)),
	)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	return flockOwned(os.NewFile(uintptr(fd), path), path, shared, gid)
}

// lockFileMode is the mode a NEW lock file is created with. The umask takes bits
// back off it, which is what ensureGroupAccess repairs.
func lockFileMode(shared bool) os.FileMode {
	if shared {
		return 0o660
	}

	return 0o600
}

func flockOwned(f *os.File, path string, shared bool, gid fileGroup) (*dirLock, error) {
	// THE LOCK COMES FIRST, and the ordering is a correctness fix rather than
	// tidiness. Metadata was validated before flocking, and a group mismatch told
	// the operator to remove a "stale" lock file — advice given without ever
	// discovering whether another process was holding it. An administrator who
	// re-groups the directory while a billet is running would have been told to
	// delete a live lock, after which the newcomer creates a fresh inode and both
	// run. Nothing may be called stale until this succeeds.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	// THE UMASK SILENTLY REMOVES WHAT THE MODE ASKED FOR. A typical 022 turns
	// 0660 into 0640, and 0640 cannot be opened O_RDWR by the other account — so
	// the shared case would fail exactly as it did before, with the fix in place
	// and looking applied. Corrected explicitly rather than trusted.
	if shared {
		if err := ensureGroupAccess(f, path, gid); err != nil {
			// Unlocked explicitly rather than left to the close, so a caller that
			// retries is not racing this descriptor's teardown for the lock it just
			// gave up. A failure here changes nothing the caller can act on — the
			// close below drops the flock regardless — so it rides along with the
			// error that actually matters.
			if unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); unlockErr != nil {
				err = errors.Join(err, unlockErr)
			}

			_ = f.Close()

			return nil, err
		}
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
//
// Runs AFTER the flock, so anything it calls leftover really is unheld.
func ensureGroupAccess(f *os.File, path string, gid fileGroup) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect lock file %s: %w", path, err)
	}

	got, ok := fileGID(info)
	if !ok {
		return fmt.Errorf(
			"the filesystem does not report a group owner for %s, so billet cannot tell whether "+
				"the other account sharing this directory could open it", path)
	}

	if !gid.known || got != gid.gid {
		// "No longer matches", not "stale" — this lock is HELD by this process as
		// of a few lines ago, so the file is definitely not an abandoned leftover,
		// and telling an operator to delete it while another billet holds a
		// different one would be the same mistake one layer along.
		return fmt.Errorf(
			"lock file %s belongs to group %d but the shared directory is group %d, so the other "+
				"account sharing that directory cannot open it. Stop every billet using this "+
				"directory, then either chgrp the file to %d or delete it so the next start "+
				"recreates it — and chmod g+s the directory so it does not happen again",
			path, got, gid.gid, gid.gid)
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

// fileGroup is a group owner that may not be known.
//
// A STRUCT RATHER THAN A -1 SENTINEL, because Stat_t.Gid is UNSIGNED and this
// package compiles for 32-bit hosts too: a gid above MaxInt32 converted to a
// 32-bit int comes out negative, so "no group owner" and "a very high group id"
// became the same value. A shared directory with such a gid would have been
// refused as unreadable. Absence is now its own field and cannot be spelled by
// any real gid.
type fileGroup struct {
	gid   uint32
	known bool
}

// fileGID reports the group that owns a file, and whether the platform said.
//
// An unknown group is never treated as a MATCH: it is exactly the case where
// widening the mode would look right and lock the other account out anyway.
func fileGID(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}

	return st.Gid, true
}

// dirGID is the same question asked of the shared lock directory, whose group is
// the one every participant must end up in.
func dirGID(info os.FileInfo) (fileGroup, error) {
	gid, ok := fileGID(info)
	if !ok {
		return fileGroup{}, errors.New("the filesystem did not report a group owner")
	}

	return fileGroup{gid: gid, known: true}, nil
}
