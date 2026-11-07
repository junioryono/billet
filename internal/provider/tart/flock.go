package tart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// flockRetry is the gap between attempts on a lock something else holds.
const flockRetry = 25 * time.Millisecond

// openLockFile opens a lock file, creating it and its directory. O_NOFOLLOW for the
// reason lockStore has it: a symlink there would move the lock to an inode other
// billets do not flock.
func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}

// tryFlock takes a lock on f without waiting. It answers false only for contention,
// and anything else is an error, because a lock that cannot be placed proves nothing
// about who holds it.
func tryFlock(f *os.File, how int) (bool, error) {
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}

	return false, err
}

// waitFlock retries tryFlock until the window or the caller's context ends. Retried
// rather than blocking, because a blocking flock pins a thread and cannot be cancelled.
func waitFlock(ctx context.Context, f *os.File, how int, window time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	retry := time.NewTimer(flockRetry)
	defer retry.Stop()

	for {
		held, err := tryFlock(f, how)
		if err != nil {
			return err
		}

		if held {
			return nil
		}

		retry.Reset(flockRetry)

		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}

			return fmt.Errorf("it stayed held for more than %s", window)
		case <-retry.C:
		}
	}
}

// heldFile is one lock this process holds, released by closing its descriptor.
type heldFile struct {
	f *os.File
}

// release drops the lock. A descriptor already handed to a child keeps it held there.
func (h heldFile) release() {
	if h.f != nil {
		_ = h.f.Close()
	}
}

// lockPath takes a lock on a file by name, waiting up to window.
func lockPath(ctx context.Context, path string, how int, window time.Duration) (heldFile, error) {
	f, err := openLockFile(path)
	if err != nil {
		return heldFile{}, err
	}

	if err := waitFlock(ctx, f, how, window); err != nil {
		_ = f.Close()

		return heldFile{}, err
	}

	return heldFile{f: f}, nil
}

// lockTartHome takes tart's own lock on its store, the one its clone holds around
// moving a VM in: a flock on the TART_HOME directory itself.
//
// NOTHING RUNS TART UNDER IT. Some of tart's commands, `clone` among them, take this
// lock again and would wait on billet forever, so it covers a rename and nothing else.
// billet's store lock is always taken first.
func (p *Provider) lockTartHome(ctx context.Context) (heldFile, error) {
	f, err := os.Open(p.home)
	if err != nil {
		return heldFile{}, fmt.Errorf("open tart's store %s: %w", p.home, err)
	}

	if err := waitFlock(ctx, f, syscall.LOCK_EX, p.storeLockWindow); err != nil {
		_ = f.Close()

		return heldFile{}, fmt.Errorf("take tart's own lock on %s: %w", p.home, err)
	}

	return heldFile{f: f}, nil
}
