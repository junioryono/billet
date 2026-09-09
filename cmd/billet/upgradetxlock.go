package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/regularfile"
)

// txLockName is the exclusion one upgrade TRANSACTION holds for its lifetime.
//
// THE CLAIM IS A RECORD; THIS IS A LOCK, and they are not the same thing.
//
// The claim (`active`) is a durable pointer that survives a crash — that is its
// whole job, because a machine that lost power mid-upgrade must still be able to
// find the transaction it was running. But precisely because it survives a crash,
// its presence cannot mean "a process is working on this right now": `--resume`
// exists to pick up a claim whose owner is gone. So the claim excludes a second
// `start` and excludes nothing else, and a review found the gap — a `--resume`
// run while the detached updater is alive, or two resumes, both entered the
// transaction and would concurrently stop services, migrate the ledger, advance
// one journal and release each other's pointers.
//
// A FLOCK IS THE OPPOSITE SHAPE: the kernel drops it when the holder dies, which
// is exactly what "is anybody working on this" needs. Held for the length of the
// transaction, taken NON-BLOCKING everywhere, because the thing it guards
// contains an unbounded drain and a caller that waited would wait for days.
//
// LOCK ORDER: this one first, then decision.lock. Never the reverse. The
// transaction lock is held across long work and the decision lock is held across
// milliseconds, so a path that took them the other way round would leave the
// short one waiting behind the long one — and the two orders together deadlock.
const txLockName = "transaction.lock"

// ErrUpgradeInProgress means another process is running an upgrade on this
// machine right now.
var ErrUpgradeInProgress = errors.New("an upgrade is already running on this machine")

// txLock is a held upgrade-transaction lock, with the descriptor of the
// validated upgrade root it was taken inside.
type txLock struct {
	f *os.File
	// dir is the upgrade root, opened after its trust boundary was proved;
	// everything a holder opens under the root opens relative to it.
	dir *os.File
}

// takeTxLock takes the transaction lock, or reports who has it.
//
// THE TRUST BOUNDARY FIRST, THROUGH DESCRIPTORS. The root's parent, the root
// and the lock are each examined for the shape only the expected account could
// have made: the parent a directory writable by nobody else, the root a
// directory of mode 0700, the lock a regular file, every one owned by that
// account (root on a Linux host, the launch agent's account on a Mac: the
// process's own effective uid, which is what "owned by whoever runs billet's
// transactions" means on both) and none of the three a symlink. The parent is
// opened by its name (the components above it are the system's), the root
// relative to the parent's descriptor and the lock relative to the root's, each
// O_NOFOLLOW, and each judged on the descriptor that is then used, so a name
// replaced between the check and the use is not the thing used. A root that does
// not exist is created inside the validated parent; a parent that does not exist
// is refused, because the package and the role make it and nothing here should.
//
// A FRESH DESCRIPTOR PER ACQUISITION. A second flock on the SAME descriptor
// succeeds, so a cached one would make every acquisition after the first a silent
// no-op while both callers believed they held it.
func takeTxLock() (*txLock, error) {
	parentPath := filepath.Dir(upgradeRoot)

	// THE PARENT IS REACHED BY AN ANCHORED WALK from the filesystem root, one
	// component at a time relative to the descriptor of the one above it, so no
	// ancestor's name is resolved through a link and every ancestor is judged:
	// a directory owned by root or by the expected account, writable by nobody
	// else unless the sticky bit keeps others from renaming its entries. A
	// parent chain another account could redirect would otherwise make the
	// whole root a name that account chooses.
	parent, err := openTrustedDir(parentPath)
	if err != nil {
		return nil, err
	}

	defer func() { _ = parent.Close() }()

	parentInfo, err := parent.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: examine %s: %w", errTrustBoundary, parentPath, err)
	}

	if err := requireTrustedOwner(parentPath, parentInfo); err != nil {
		return nil, err
	}

	rootName := filepath.Base(upgradeRoot)

	if err := guardObserve("lstat", upgradeRoot, nil); err != nil {
		return nil, err
	}

	rootInfo, err := os.Lstat(upgradeRoot)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := guardObserve("mkdir", upgradeRoot, nil); err != nil {
			return nil, err
		}

		if err := unix.Mkdirat(int(parent.Fd()), rootName, 0o700); err != nil {
			return nil, fmt.Errorf("prepare %s: %w", upgradeRoot, err)
		}
	case err != nil:
		return nil, fmt.Errorf("%w: examine %s: %w", errTrustBoundary, upgradeRoot, err)
	case rootInfo.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: %s is a symlink", errTrustBoundary, upgradeRoot)
	}

	// THE PARENT IS FLUSHED BY EVERY ACQUISITION, whether or not this one made
	// the root: an acquisition that made it and died before its flush leaves a
	// root whose entry is not durable and which the next acquisition finds
	// existing, so a flush that only the creator performed would be owed by
	// nobody. One fsync of a directory that rarely changes is the price.
	if err := guardObserve("fsync", parentPath, parent); err != nil {
		return nil, err
	}

	if err := parent.Sync(); err != nil {
		return nil, fmt.Errorf("flush %s: %w", parentPath, err)
	}

	if err := guardObserve("openat", upgradeRoot, nil); err != nil {
		return nil, err
	}

	rootFD, err := unix.Openat(int(parent.Fd()), rootName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %w", errTrustBoundary, upgradeRoot, err)
	}

	root := os.NewFile(uintptr(rootFD), upgradeRoot)

	rootOpened, err := root.Stat()
	if err != nil {
		_ = root.Close()

		return nil, fmt.Errorf("examine %s: %w", upgradeRoot, err)
	}

	if err := requireTrustedDir(upgradeRoot, rootOpened, 0); err != nil {
		_ = root.Close()

		return nil, err
	}

	if err := guardObserve("openat", filepath.Join(upgradeRoot, txLockName), nil); err != nil {
		_ = root.Close()

		return nil, err
	}

	// THE LOCK FILE, RELATIVE TO THE VALIDATED ROOT, never followed: a symlink at
	// its name is ELOOP, a directory ENOTDIR through the fstat below, a FIFO is
	// never waited on.
	f, err := openLockFile(root)
	if err != nil {
		_ = root.Close()

		return nil, fmt.Errorf("%w: open the upgrade transaction lock: %w", errTrustBoundary, err)
	}

	lockInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = root.Close()

		return nil, fmt.Errorf("examine the upgrade transaction lock: %w", err)
	}

	if err := requireTrustedFile(filepath.Join(upgradeRoot, txLockName), lockInfo); err != nil {
		_ = f.Close()
		_ = root.Close()

		return nil, err
	}

	if err := guardObserve("flock", filepath.Join(upgradeRoot, txLockName), f); err != nil {
		_ = f.Close()
		_ = root.Close()

		return nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = root.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w. Wait for it to finish — it may be draining, which "+
				"takes as long as the work already on this host — or read its journal in %s",
				ErrUpgradeInProgress, upgradeRoot)
		}

		return nil, fmt.Errorf("take the upgrade transaction lock: %w", err)
	}

	tx := &txLock{f: f, dir: root}

	// THE ROOT MUST STILL BE THE ROOT AT ITS NAME once the lock is held: the
	// transaction's own claim and journal are named by path under it, and a
	// root replaced at its name between the walk and the lock would resolve
	// those through another tree.
	if err := requireRootInPlace(tx); err != nil {
		tx.release()

		return nil, err
	}

	return tx, nil
}

// requireRootInPlace refuses when the name of the upgrade root no longer holds
// the directory the lock was taken inside.
func requireRootInPlace(tx *txLock) error {
	held, err := tx.dir.Stat()
	if err != nil {
		return fmt.Errorf("%w: examine the held upgrade root: %w", errTrustBoundary, err)
	}

	named, err := os.Lstat(upgradeRoot)
	if err != nil {
		return fmt.Errorf("%w: examine %s under the lock: %w", errTrustBoundary, upgradeRoot, err)
	}

	if !os.SameFile(held, named) {
		return fmt.Errorf("%w: %s no longer names the directory the lock was taken inside; it was replaced "+
			"under the lock", errTrustBoundary, upgradeRoot)
	}

	return nil
}

// openTrustedDir walks to a directory from the filesystem root, each component
// opened relative to the descriptor of the one above it and never followed as
// a link by that open, THE COMPONENTS AS WRITTEN: a `..` is opened where it
// stands, relative to the directory reached (as the kernel resolves it, after
// the links before it; a lexical collapse would climb out of a link's target
// through the link's own parent), and a `.` is nothing. A component that is a
// link is admitted only when THE LINK ITSELF is owned by root or by the
// expected account, because a sticky directory keeps another account from
// renaming billet's entries and says nothing about a link that account made
// there, which it can repoint at will; its target is then walked the same way
// (an absolute target from the root, a relative one from where the link
// stands), at most maxTrustedLinks times, because macOS spells /var as a link
// to /private/var. Every directory on the way is judged: owned by root or by
// the expected account, and writable by group or others only under the sticky
// bit, which keeps another account from renaming what it did not make.
func openTrustedDir(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: %s is not an absolute path", errTrustBoundary, path)
	}

	dir, err := os.OpenFile("/", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open /: %w", errTrustBoundary, err)
	}

	remaining := splitComponents(path)
	walked := "/"
	links := 0

	for len(remaining) > 0 {
		name := remaining[0]
		remaining = remaining[1:]

		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)

		switch {
		case err == nil:
		case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
			// A LINK, OR NOT A DIRECTORY: examined as itself, judged, then read.
			target, lerr := readTrustedLink(dir, walked, name)
			if lerr != nil {
				_ = dir.Close()

				return nil, lerr
			}

			links++
			if links > maxTrustedLinks {
				_ = dir.Close()

				return nil, fmt.Errorf("%w: %s: too many links on the way", errTrustBoundary, path)
			}

			if filepath.IsAbs(target) {
				_ = dir.Close()

				dir, err = os.OpenFile("/", os.O_RDONLY|syscall.O_DIRECTORY, 0)
				if err != nil {
					return nil, fmt.Errorf("%w: open /: %w", errTrustBoundary, err)
				}

				walked = "/"
			}

			remaining = append(splitComponents(target), remaining...)

			continue
		default:
			_ = dir.Close()

			return nil, fmt.Errorf("%w: open %s: %w", errTrustBoundary, filepath.Join(walked, name), err)
		}

		_ = dir.Close()

		if name == ".." {
			walked = filepath.Dir(walked)
		} else {
			walked = filepath.Join(walked, name)
		}

		dir = os.NewFile(uintptr(fd), walked)

		info, err := dir.Stat()
		if err != nil {
			_ = dir.Close()

			return nil, fmt.Errorf("%w: examine %s: %w", errTrustBoundary, walked, err)
		}

		if err := requireTrustedAncestor(walked, info); err != nil {
			_ = dir.Close()

			return nil, err
		}
	}

	return dir, nil
}

// readTrustedLink is a link component on the way to the root: opened as itself
// relative to the directory it stands in, required to BE a link owned by root
// or by the expected account, and only then read. A component that is neither
// a directory nor a link is refused here.
func readTrustedLink(dir *os.File, walked, name string) (string, error) {
	at := filepath.Join(walked, name)

	id, err := regularfile.OpenIdentityAt(dir, name)
	if err != nil {
		return "", fmt.Errorf("%w: examine %s: %w", errTrustBoundary, at, err)
	}

	defer func() { _ = id.Close() }()

	info, err := id.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: examine %s: %w", errTrustBoundary, at, err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%w: %s is not a directory (%s)", errTrustBoundary, at, info.Mode().Type())
	}

	uid, ok := guardOwnerOf(info)
	if !ok {
		return "", fmt.Errorf("%w: the link %s carries no owner this platform reports", errTrustBoundary, at)
	}

	if uid != 0 && uid != guardExpectedOwner() {
		return "", fmt.Errorf("%w: the link %s is owned by uid %d, want root or uid %d, and a link another "+
			"account made is a name it can repoint", errTrustBoundary, at, uid, guardExpectedOwner())
	}

	return readlinkAt(dir, name)
}

// maxTrustedLinks bounds the links openTrustedDir follows on one walk.
const maxTrustedLinks = 32

// splitComponents is a path's components as written, `.` and empty ones
// dropped and `..` kept for the walk to open where it stands.
func splitComponents(path string) []string {
	var out []string

	for _, c := range strings.Split(path, string(filepath.Separator)) {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}

	return out
}

// requireTrustedAncestor is the rule for a directory on the way to the root:
// owned by root or by the expected account, and writable by group or others
// only when the sticky bit is set.
func requireTrustedAncestor(path string, info os.FileInfo) error {
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", errTrustBoundary, path)
	}

	perm := info.Mode().Perm()
	if perm&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%w: %s is mode %04o, writable by group or others without the sticky bit, so another "+
			"account could rename what lies under it", errTrustBoundary, path, perm)
	}

	uid, ok := guardOwnerOf(info)
	if !ok {
		return fmt.Errorf("%w: %s carries no owner this platform reports", errTrustBoundary, path)
	}

	if uid != 0 && uid != guardExpectedOwner() {
		return fmt.Errorf("%w: %s is owned by uid %d, want root or uid %d", errTrustBoundary, path, uid,
			guardExpectedOwner())
	}

	return nil
}

// openLockFile opens the lock relative to the root, creating it when absent.
//
// CREATE-OR-OPEN AS TWO STEPS, RETRIED: two processes preparing the same root
// at once both try to create the lock, and a single O_CREAT open answered
// ENOENT to the loser on APFS while the winner's entry was being made
// (measured 2026-09-09 in the two-holds fixture). An exclusive create that
// finds the file existing opens it plainly; an open that finds it absent
// creates it; a few rounds of that cover the window.
func openLockFile(root *os.File) (*os.File, error) {
	path := filepath.Join(upgradeRoot, txLockName)

	var err error

	for range 8 {
		var fd int

		fd, err = unix.Openat(int(root.Fd()), txLockName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
		if err == nil {
			return os.NewFile(uintptr(fd), path), nil
		}

		if !errors.Is(err, unix.EEXIST) {
			return nil, err
		}

		// AN EXISTING LOCK IS OPENED IDENTITY FIRST, relative to the root, so a
		// device planted at its name is refused without its driver being invoked
		// and a link at its name is the link's own inode, refused as not regular;
		// the readable descriptor is of the inode that was judged.
		f, _, err := regularfile.OpenAt(root, txLockName)
		if err == nil {
			return f, nil
		}

		if !errors.Is(err, unix.ENOENT) {
			return nil, err
		}
	}

	return nil, err
}

// prepareUpgradeRoot is takeTxLock under the name the guard's commands use: the
// trust boundary proved and the lock held.
func prepareUpgradeRoot() (*txLock, error) { return takeTxLock() }

// release drops the lock. Closing the descriptor would do it too; this says so.
func (l *txLock) release() {
	if l == nil || l.f == nil {
		return
	}

	// UNLOCKED EXPLICITLY FOR THE READER, NOT FOR THE KERNEL. Closing the
	// descriptor releases a flock on its own, so this call is a statement of intent
	// and its result changes nothing: if it fails, the Close on the next line
	// releases the lock anyway.
	_ = guardObserve("unlock", l.f.Name(), l.f)       //nolint:errcheck // an observation; the Close below releases the lock whatever it says
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) //nolint:errcheck // the Close below releases it whatever this returns
	_ = l.f.Close()
	l.f = nil

	if l.dir != nil {
		_ = l.dir.Close()
		l.dir = nil
	}
}

// close is release under the name the guard's commands use.
func (l *txLock) close() { l.release() }
