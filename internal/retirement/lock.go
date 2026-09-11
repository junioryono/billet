package retirement

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// ErrNoGlobalLock is the positive absence of the global lock file for a caller
// that may not create it: an unprivileged caller on a host no installer has
// prepared, which keeps the identity directory's own `ca.lock` as its whole
// exclusion, as before this package existed.
var ErrNoGlobalLock = errors.New("retirement: no global authority lock exists on this host")

// ErrRetiring is an ordinary authority operation refused because the authority
// is closed: the status names a phase from `stopped` on.
type ErrRetiring struct{ Phase Phase }

func (e ErrRetiring) Error() string {
	return fmt.Sprintf("retirement: this controller's authority is closed (its retirement is at %q); "+
		"finish or repair the retirement before touching its identity", e.Phase)
}

// ErrStatusUnknown is a status that could not be read or parsed. It is
// could-not-tell: a caller that needs admission refuses on it, and never reads
// it as absence.
type ErrStatusUnknown struct{ Cause error }

func (e ErrStatusUnknown) Error() string {
	return fmt.Sprintf("retirement: the authority status could not be judged: %v", e.Cause)
}

func (e ErrStatusUnknown) Unwrap() error { return e.Cause }

// Hold is an exclusive hold on the global authority lock: one descriptor,
// BORROWED by every helper the holding command calls (a second flock on a
// separate descriptor in the same process is denied on darwin and would
// deadlock on Linux), and released once.
type Hold struct {
	f    *os.File
	path string
}

// AcquireOptions says who is asking and how long they will wait.
type AcquireOptions struct {
	// Privileged says the caller is root and may CREATE the lock file when it is
	// absent. An unprivileged caller with no file gets ErrNoGlobalLock.
	Privileged bool
	// Account is the recorded service account when there is one: the file is
	// created root:<gid> 0660 so that account can open it. Without one a
	// privileged creator makes it root:root 0600.
	Account *ServiceAccount
	// Poll is how often a blocked acquisition retries; the zero value is 50ms.
	Poll time.Duration
}

// Acquire takes the global lock, BLOCKING under ctx.
//
// EXCLUSION ONLY: it reads no status and admits nothing. `Admit` is the
// ordinary writers' gate; a retirement acquires and never admits, since it is
// the one writer allowed under a closed status.
//
// Blocking is a LOCK_NB retry under the context rather than a blocking flock,
// because flock(2) has no deadline and a command somebody is waiting on must
// give up at its bound rather than hang.
func Acquire(ctx context.Context, opts AcquireOptions) (*Hold, error) {
	path := GlobalLockPath()

	f, err := openGlobalLock(path, opts)
	if err != nil {
		return nil, err
	}

	poll := opts.Poll
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}

	if err := FlockUnder(ctx, f, poll); err != nil {
		return nil, errors.Join(fmt.Errorf("retirement: lock %s: %w", path, err), f.Close())
	}

	return &Hold{f: f, path: path}, nil
}

// FlockUnder takes LOCK_EX on f, retrying LOCK_NB every poll until ctx ends,
// since flock(2) itself has no deadline. The timer is stopped on every exit so
// a short poll leaks nothing.
func FlockUnder(ctx context.Context, f *os.File, poll time.Duration) error {
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}

	timer := time.NewTimer(poll)
	defer timer.Stop()

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("held by another billet and the wait for it ended: %w", ctx.Err())
		case <-timer.C:
			timer.Reset(poll)
		}
	}
}

// openGlobalLock opens the lock file READ-ONLY, since flock(2) needs no write
// access and the backup service's sandbox can then open a root-owned file it
// may read; a privileged caller creates it when absent with the owner and mode
// the recorded account needs.
func openGlobalLock(path string, opts AcquireOptions) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == nil {
		return f, nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("retirement: open the global authority lock %s: %w", path, err)
	}

	if !opts.Privileged {
		return nil, ErrNoGlobalLock
	}

	// O_EXCL, so two privileged creators racing do not each own a different
	// inode under one name; the loser opens the winner's.
	created, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return openGlobalLock(path, AcquireOptions{})
		}

		return nil, fmt.Errorf("retirement: create the global authority lock %s: %w", path, err)
	}

	if err := ownLockFile(created, opts.Account); err != nil {
		_ = created.Close()

		return nil, err
	}

	if err := syncDir(Root); err != nil {
		_ = created.Close()

		return nil, err
	}

	return created, nil
}

// ownLockFile gives a lock file the owner and mode the recorded account needs,
// through the descriptor, so the name cannot be swapped between the check and
// the change: root:<gid> 0660 when an account is recorded, root:root 0600 when
// none is.
func ownLockFile(f *os.File, acct *ServiceAccount) error {
	gid, mode := 0, os.FileMode(0o600)
	if acct != nil {
		gid, mode = acct.GID, 0o660
	}

	// Only root re-owns; a creator that is not root already owns what it made,
	// and a chown to root would be refused.
	if os.Geteuid() == 0 {
		if err := f.Chown(0, gid); err != nil {
			return fmt.Errorf("retirement: own %s: %w", f.Name(), err)
		}
	}

	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("retirement: set the mode of %s: %w", f.Name(), err)
	}

	return nil
}

// Admit is the ordinary authority writers' gate under a hold: the status is
// read and a closed one refuses. An absent status admits; a status that cannot
// be read or parsed is could-not-tell and refuses.
func (h *Hold) Admit() error {
	if h == nil || h.f == nil {
		return errors.New("retirement: admit without a hold")
	}

	st, presence, err := ReadStatus()

	switch presence {
	case StatusAbsent:
		return nil
	case StatusPresent:
		if st.Phase.Closed() {
			return ErrRetiring{Phase: st.Phase}
		}

		return nil
	default:
		return ErrStatusUnknown{Cause: err}
	}
}

// Path is the lock's path, for a message.
func (h *Hold) Path() string {
	if h == nil {
		return ""
	}

	return h.path
}

// Release drops the hold. Closing the descriptor releases it, so a process
// that exits cannot leave one held.
func (h *Hold) Release() error {
	if h == nil || h.f == nil {
		return nil
	}

	unlockErr := syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	closeErr := h.f.Close()
	h.f = nil

	if unlockErr != nil {
		return fmt.Errorf("retirement: unlock %s: %w", h.path, unlockErr)
	}

	if closeErr != nil {
		return fmt.Errorf("retirement: close %s: %w", h.path, closeErr)
	}

	return nil
}
