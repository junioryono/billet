package retirement

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// InitHold is the fresh-initialisation exclusion: a lock in the identity
// directory's PARENT that a caller about to create the directory and an
// installer about to prepare the host both take, so the two never interleave.
//
// Beside the directory rather than the global lock because the global lock is
// root's file under the state root, and an unprivileged owner initialising an
// identity directory under a parent it owns (the laptop layout `billet init`
// emits) has to be able to exclude itself against a root installer too.
type InitHold struct {
	f    *os.File
	path string
}

// AcquireInit takes the init lock for identityDir, blocking under ctx.
//
// The file is created by whoever first needs it, in the parent, as that
// caller: an unprivileged owner in a parent it can write, root elsewhere. A
// parent the caller cannot write is a refusal naming the installers, which is
// what the packaged layout wants (`/var/lib/billet` is root's 0755, and the
// service account cannot create the directory there either).
func AcquireInit(ctx context.Context, identityDir string, acct *ServiceAccount, poll time.Duration) (*InitHold, error) {
	path := InitLockPath(identityDir)

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("retirement: open the initialisation lock %s: %w", path, err)
		}

		created, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return AcquireInit(ctx, identityDir, acct, poll)
			}

			if errors.Is(err, os.ErrPermission) {
				return nil, fmt.Errorf("retirement: %s cannot be created by this account, so this host's identity "+
					"directory is one an installer creates: run `billet local up` or the host role first (%w)", path, err)
			}

			return nil, fmt.Errorf("retirement: create the initialisation lock %s: %w", path, err)
		}

		// A root creator with a recorded account gives the file to root:<gid>
		// 0660 so that account can take it later; every other creator keeps it
		// its own at 0600.
		if os.Geteuid() == 0 && acct != nil {
			if err := ownLockFile(created, acct); err != nil {
				_ = created.Close()

				return nil, err
			}
		}

		f = created
	}

	if poll <= 0 {
		poll = 50 * time.Millisecond
	}

	if err := FlockUnder(ctx, f, poll); err != nil {
		return nil, errors.Join(fmt.Errorf("retirement: lock %s: %w", path, err), f.Close())
	}

	return &InitHold{f: f, path: path}, nil
}

// Path is the lock's path, for a message.
func (h *InitHold) Path() string {
	if h == nil {
		return ""
	}

	return h.path
}

// Release drops the hold.
func (h *InitHold) Release() error {
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
