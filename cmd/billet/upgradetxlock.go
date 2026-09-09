package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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
	// created says this acquisition made the root, so its parent needs a flush
	// before anything durable is claimed inside it.
	created bool
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

	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		return nil, fmt.Errorf("%w: examine %s: %w", errTrustBoundary, parentPath, err)
	}

	if parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", errTrustBoundary, parentPath)
	}

	if !parentInfo.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", errTrustBoundary, parentPath)
	}

	if err := requireTrustedOwner(parentPath, parentInfo); err != nil {
		return nil, err
	}

	parent, err := os.OpenFile(parentPath, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", parentPath, err)
	}

	defer func() { _ = parent.Close() }()

	opened, err := parent.Stat()
	if err != nil || !os.SameFile(opened, parentInfo) {
		return nil, fmt.Errorf("%w: %s changed while it was being examined", errTrustBoundary, parentPath)
	}

	rootName := filepath.Base(upgradeRoot)
	created := false

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

		created = true
	case err != nil:
		return nil, fmt.Errorf("%w: examine %s: %w", errTrustBoundary, upgradeRoot, err)
	case rootInfo.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: %s is a symlink", errTrustBoundary, upgradeRoot)
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
	fd, err := openLockFile(rootFD)
	if err != nil {
		_ = root.Close()

		return nil, fmt.Errorf("%w: open the upgrade transaction lock: %w", errTrustBoundary, err)
	}

	f := os.NewFile(uintptr(fd), filepath.Join(upgradeRoot, txLockName))

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

	return &txLock{f: f, dir: root, created: created}, nil
}

// openLockFile opens the lock relative to the root, creating it when absent.
//
// CREATE-OR-OPEN AS TWO STEPS, RETRIED: two processes preparing the same root
// at once both try to create the lock, and a single O_CREAT open answered
// ENOENT to the loser on APFS while the winner's entry was being made
// (measured 2026-09-09 in the two-holds fixture). An exclusive create that
// finds the file existing opens it plainly; an open that finds it absent
// creates it; a few rounds of that cover the window.
func openLockFile(rootFD int) (int, error) {
	var err error

	for range 8 {
		var fd int

		fd, err = unix.Openat(rootFD, txLockName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
		if err == nil {
			return fd, nil
		}

		if !errors.Is(err, unix.EEXIST) {
			return -1, err
		}

		fd, err = unix.Openat(rootFD, txLockName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			return fd, nil
		}

		if !errors.Is(err, unix.ENOENT) {
			return -1, err
		}
	}

	return -1, err
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
