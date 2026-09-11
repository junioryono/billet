package wirecert

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/retirement"
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

// Exclusion is what a caller holds, or is entitled to, when it takes the inner
// lock. It is resolved once per command from the host's metadata and handed to
// every helper the command calls; nothing re-resolves it further down.
type Exclusion struct {
	// Hold is the global authority lock, held and admitted, on a host an
	// installer has prepared. Borrowed by the inner lock, released by whoever
	// resolved it.
	Hold *retirement.Hold
	// Legacy says the host has no service-account record and positively no
	// global lock, so the inner lock is the whole exclusion, as it was before the
	// global one existed, with one addition: after taking it the caller rechecks
	// for the global lock and hands off to it when an installer has published one
	// meanwhile.
	Legacy bool
	// Wait bounds a blocking acquisition. Zero is the operator command's
	// non-blocking take, which reports what already holds the lock rather than
	// queueing silently behind it.
	Wait time.Duration
	// Account is the recorded service account when the host is prepared; a
	// privileged creator of the inner lock gives the file to it.
	Account *retirement.ServiceAccount
	// Create says the caller may CREATE the identity directory when it is
	// positively absent: a fresh host with no metadata at all (nothing a
	// retirement could have moved), or a platform without the global
	// exclusion. Every other caller refuses an absent directory and recreates
	// nothing, because the retirement world is the one that moves it.
	Create bool

	ownsHold bool
}

// ResolveExclusion classifies the host for stateDir and takes what the
// classification requires: the global lock, admitted, on a prepared host; the
// legacy marker on a host with no metadata; a refusal on a damaged one.
//
// NO RESOLUTION GRANTS CREATION ON LINUX. A FRESH host (no metadata, directory
// absent) resolves as legacy for the lock's purposes and the directory is
// created only by a caller holding the initialisation lock beside it, having
// re-established the four absences under that lock: the command layer's fresh
// branch, or LockAuthority below for a caller with no command around it. A
// classification remembered from before the lock cannot authorise creation,
// because an installer can prepare the host and a retirement archive its
// directory in the gap, and the recreation would land beside the archive. Off
// Linux there is no retirement and the inner lock creates as it always did.
func ResolveExclusion(ctx context.Context, stateDir string, wait time.Duration) (Exclusion, error) {
	if !retirement.SupportedHere() {
		return Exclusion{Legacy: true, Create: true, Wait: wait}, nil
	}

	class, err := retirement.Classify(stateDir)
	if err != nil {
		return Exclusion{}, err
	}

	switch class.Mode {
	case retirement.ModePrepared:
		hold, err := acquireGlobal(ctx, wait, &class.Account)
		if err != nil {
			return Exclusion{}, err
		}

		if err := hold.Admit(); err != nil {
			return Exclusion{}, errors.Join(err, hold.Release())
		}

		acct := class.Account

		return Exclusion{Hold: hold, Wait: wait, Account: &acct, ownsHold: true}, nil
	default:
		return Exclusion{Legacy: true, Wait: wait}, nil
	}
}

// acquireGlobal takes the global lock under wait (a zero wait is one attempt).
func acquireGlobal(ctx context.Context, wait time.Duration, acct *retirement.ServiceAccount) (*retirement.Hold, error) {
	if wait <= 0 {
		wait = time.Millisecond
	}

	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	return retirement.Acquire(ctx, retirement.AcquireOptions{Privileged: os.Geteuid() == 0, Account: acct})
}

// Release drops whatever the resolution took. A hold the caller supplied is
// the caller's to release and is left alone.
func (e *Exclusion) Release() error {
	if e == nil || !e.ownsHold || e.Hold == nil {
		return nil
	}

	err := e.Hold.Release()
	e.Hold, e.ownsHold = nil, false

	return err
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
// AND, SINCE THE RETIREMENT, WHAT ELSE: a controller's retirement renames the
// whole identity directory, and every writer of it now holds this lock (and,
// on a prepared host, the global one) from before its first identity access,
// so the rename waits for a writer inside the directory and a writer arriving
// after it finds the directory gone and creates nothing.
//
// WHAT IT DOES NOT COVER, AND WHY THAT IS SAFE. `LoadServing` — the control
// plane's read while it starts — takes it only through the server's own
// resolution on a prepared host; on a host no installer has prepared it takes
// nothing, as before, because no retirement can run there and a rotation's
// publication ORDER already makes every instant a state a reader answers
// correctly (see LoadServing).
type AuthorityLock struct {
	f    *os.File
	path string
	// hold is a global hold this lock took itself during a legacy handoff, and
	// releases with itself; a hold the exclusion already carried is not this
	// lock's to release.
	hold *retirement.Hold
	// init is the initialisation lock LockAuthority took itself to create a
	// fresh host's directory, released with the lock.
	init *retirement.InitHold
}

// LockAuthority takes the inner lock with the exclusion resolved here: one
// non-blocking attempt at both locks, as an operator command wants (it is told
// what already holds the lock rather than queued behind a rotation).
//
// It is a real exclusion between PROCESSES and also within one — measured on
// darwin, a second flock on a separate descriptor in the same process is denied
// with EWOULDBLOCK — so a caller holding this must not call anything that takes
// it again. A command that has already resolved its exclusion passes it to
// LockAuthorityWith instead.
func LockAuthority(ctx context.Context, stateDir string) (*AuthorityLock, error) {
	ex, init, err := resolveForLibraryCaller(ctx, stateDir)
	if err != nil {
		return nil, err
	}

	lock, err := LockAuthorityWith(ctx, stateDir, ex)
	if err != nil {
		return nil, errors.Join(err, ex.Release(), init.Release())
	}

	// The resolution's hold and init lock travel with the lock, so one Release
	// drops all of them.
	if ex.ownsHold {
		lock.hold, ex.ownsHold = ex.Hold, false
	}

	lock.init = init

	return lock, nil
}

// resolveForLibraryCaller is ResolveExclusion for a caller with no command
// around it, with the one thing such a caller may need that a resolution never
// grants: on a FRESH Linux host it takes the initialisation lock beside the
// directory, re-establishes the four absences under it, and only then answers
// an exclusion that may create. A host that stopped being fresh while the lock
// was awaited is answered as what it is now.
func resolveForLibraryCaller(ctx context.Context, stateDir string) (Exclusion, *retirement.InitHold, error) {
	if !retirement.SupportedHere() {
		return Exclusion{Legacy: true, Create: true}, nil, nil
	}

	class, err := retirement.Classify(stateDir)
	if err != nil {
		return Exclusion{}, nil, err
	}

	if class.Mode != retirement.ModeFresh {
		ex, err := ResolveExclusion(ctx, stateDir, 0)

		return ex, nil, err
	}

	initCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()

	init, err := retirement.AcquireInit(initCtx, stateDir, nil, 0)
	if err != nil {
		return Exclusion{}, nil, fmt.Errorf("wirecert: take the initialisation lock beside %s: %w", stateDir, err)
	}

	again, err := retirement.Classify(stateDir)
	if err != nil {
		return Exclusion{}, nil, errors.Join(err, init.Release())
	}

	if again.Mode == retirement.ModeFresh {
		return Exclusion{Legacy: true, Create: true}, init, nil
	}

	// An installer or another initialisation got there first; proceed on what
	// the host is now, still holding the init lock against a third party.
	ex, err := ResolveExclusion(ctx, stateDir, 0)
	if err != nil {
		return Exclusion{}, nil, errors.Join(err, init.Release())
	}

	return ex, init, nil
}

// LockAuthorityWith takes the inner lock under an exclusion the caller resolved.
//
// A nil or released hold on a non-legacy exclusion refuses: nothing may reach
// the identity directory on a prepared host without the global lock admitted.
// AN ABSENT DIRECTORY REFUSES unless the exclusion says the caller may create
// it (a fresh host, or a platform without the global exclusion): on a prepared
// host, or on a legacy host whose directory has vanished since it was
// classified, the directory was moved by a retirement or damaged by hand, and
// an ordinary lock that recreated it would mint a second identity beside the
// archived one. On the legacy path the inner lock is taken and then the global
// lock is RECHECKED under it: an installer that published one meanwhile is
// handed off to in the one lock order (global, then inner), the configured
// pathname is looked up afresh, and the status admits or refuses.
func LockAuthorityWith(ctx context.Context, stateDir string, ex Exclusion) (*AuthorityLock, error) {
	if !ex.Legacy && ex.Hold == nil {
		return nil, errors.New("wirecert: the authority lock needs the global hold on a prepared host, and none was passed")
	}

	before, err := identityOf(stateDir)
	if err != nil {
		return nil, err
	}

	if before.absent && !ex.Create {
		return nil, fmt.Errorf("wirecert: %s does not exist, and this command will not recreate it: a "+
			"retirement moves a controller's identity directory to its archive, and a fresh host is "+
			"initialised by `billet check`, `billet local up` or the host role", stateDir)
	}

	lock, err := lockInner(ctx, stateDir, ex)
	if err != nil {
		return nil, err
	}

	if !ex.Legacy || !retirement.SupportedHere() {
		return lock, nil
	}

	present, err := retirement.GlobalLockPresent()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("wirecert: recheck for the global authority lock: %w", err), lock.Release())
	}

	if !present {
		return lock, nil
	}

	// THE HANDOFF: an installer published the global lock while this caller was
	// on the legacy path. Release the inner lock (never wait on the global one
	// while holding it: the installer waits on this one), take the global lock,
	// take the inner lock again, and re-validate: the configured pathname must
	// still be the directory seen before (a retirement in the gap moved it, and
	// an absent or replaced pathname refuses), and the status must admit.
	if err := lock.Release(); err != nil {
		return nil, err
	}

	hold, err := acquireGlobal(ctx, ex.Wait, nil)
	if err != nil {
		return nil, fmt.Errorf("wirecert: hand off to the global authority lock an installer published: %w", err)
	}

	after, err := identityOf(stateDir)
	if err != nil {
		return nil, errors.Join(err, hold.Release())
	}

	if after.absent {
		return nil, errors.Join(fmt.Errorf("wirecert: %s is gone since this command looked at it; a retirement moved it, "+
			"and billet will not recreate it", stateDir), hold.Release())
	}

	if !before.absent && !os.SameFile(before.info, after.info) {
		return nil, errors.Join(fmt.Errorf("wirecert: %s is not the directory this command first saw; "+
			"billet will not act on a replacement", stateDir), hold.Release())
	}

	if err := hold.Admit(); err != nil {
		return nil, errors.Join(err, hold.Release())
	}

	relocked, err := lockInner(ctx, stateDir, Exclusion{Hold: hold, Wait: ex.Wait})
	if err != nil {
		return nil, errors.Join(err, hold.Release())
	}

	relocked.hold = hold

	return relocked, nil
}

// pathIdentity is what a fresh lookup of the configured pathname answers: the
// directory's identity for os.SameFile, or its positive absence.
type pathIdentity struct {
	absent bool
	info   os.FileInfo
}

func identityOf(path string) (pathIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return pathIdentity{absent: true}, nil
		}

		return pathIdentity{}, fmt.Errorf("wirecert: examine %s: %w", path, err)
	}

	return pathIdentity{info: info}, nil
}

// lockInner opens and flocks the inner lock, creating it when absent, and the
// directory too when the exclusion allows it.
func lockInner(ctx context.Context, stateDir string, ex Exclusion) (*AuthorityLock, error) {
	if ex.Create {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("wirecert: create %s: %w", stateDir, err)
		}
	}

	path := AuthorityLockPath(stateDir)

	// NEVER FOLLOWING A LINK, AND ONLY A REGULAR FILE: the lock is only worth
	// anything if it is on the inode this path names, and a symlink here would
	// silently move the exclusion somewhere else — after which two commands
	// rewrite one authority believing they are alone; a FIFO here would block
	// the open itself, before any deadline could end the wait. READ-ONLY when
	// it exists, because flock(2) needs no write access and the service account
	// has to be able to take a root-created one.
	f, err := openExistingLock(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("wirecert: open the authority lock %s: %w", path, err)
		}

		f, err = createInnerLock(path, ex.Account)
		if err != nil {
			return nil, err
		}
	}

	if ex.Wait <= 0 {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errors.Join(fmt.Errorf(
					"wirecert: another billet is working on this deployment's certificate authority "+
						"(%s is held). `billet ca rotate`, `billet ca retire`, `billet local backup` "+
						"and `billet local restore` take it in turn so none of them sees half a "+
						"rotation — wait for the other one to finish", path), f.Close())
			}

			return nil, errors.Join(fmt.Errorf("wirecert: lock %s: %w", path, err), f.Close())
		}

		return &AuthorityLock{f: f, path: path}, nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, ex.Wait)
	defer cancel()

	if err := retirement.FlockUnder(waitCtx, f, 0); err != nil {
		return nil, errors.Join(fmt.Errorf("wirecert: lock %s: %w", path, err), f.Close())
	}

	return &AuthorityLock{f: f, path: path}, nil
}

// createInnerLock creates the lock file, O_EXCL so two creators share one
// inode, and gives it to the recorded service account when a privileged caller
// creates it on a prepared host: a root-owned 0600 lock is one the server
// cannot open at its next start.
func createInnerLock(path string, acct *retirement.ServiceAccount) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			f, err = openExistingLock(path)
			if err != nil {
				return nil, fmt.Errorf("wirecert: open the authority lock %s: %w", path, err)
			}

			return f, nil
		}

		return nil, fmt.Errorf("wirecert: create the authority lock %s: %w", path, err)
	}

	if acct != nil && os.Geteuid() == 0 {
		if err := f.Chown(acct.UID, acct.GID); err != nil {
			return nil, errors.Join(fmt.Errorf("wirecert: give %s to the service account: %w", path, err), f.Close())
		}
	}

	return f, nil
}

// openExistingLock opens an existing lock file for flock(2) through the one
// identity-first open: read-only, no link followed, a regular file or a refusal,
// and never a wait inside the open. An absent file is fs.ErrNotExist.
func openExistingLock(path string) (*os.File, error) {
	f, _, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
	if err != nil {
		return nil, err
	}

	return f, nil
}

// Release drops the lock, and the global hold it took itself in a handoff.
// Closing the descriptor releases it, so a process that exits cannot leave one
// held.
func (l *AuthorityLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}

	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil

	var holdErr error
	if l.hold != nil {
		holdErr = l.hold.Release()
		l.hold = nil
	}

	if l.init != nil {
		holdErr = errors.Join(holdErr, l.init.Release())
		l.init = nil
	}

	if unlockErr != nil {
		return errors.Join(fmt.Errorf("wirecert: unlock %s: %w", l.path, unlockErr), closeErr, holdErr)
	}

	if closeErr != nil {
		return errors.Join(fmt.Errorf("wirecert: close %s: %w", l.path, closeErr), holdErr)
	}

	return holdErr
}
