package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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

// txLock is a held upgrade-transaction lock.
type txLock struct{ f *os.File }

// takeTxLock takes the transaction lock, or reports who has it.
//
// A FRESH DESCRIPTOR PER ACQUISITION. A second flock on the SAME descriptor
// succeeds, so a cached one would make every acquisition after the first a silent
// no-op while both callers believed they held it.
func takeTxLock() (*txLock, error) {
	if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", upgradeRoot, err)
	}

	f, err := os.OpenFile(filepath.Join(upgradeRoot, txLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the upgrade transaction lock: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w. Wait for it to finish — it may be draining, which "+
				"takes as long as the work already on this host — or read its journal in %s",
				ErrUpgradeInProgress, upgradeRoot)
		}

		return nil, fmt.Errorf("take the upgrade transaction lock: %w", err)
	}

	return &txLock{f: f}, nil
}

// release drops the lock. Closing the descriptor would do it too; this says so.
func (l *txLock) release() {
	if l == nil || l.f == nil {
		return
	}

	// UNLOCKED EXPLICITLY FOR THE READER, NOT FOR THE KERNEL. Closing the
	// descriptor releases a flock on its own, so this call is a statement of intent
	// and its result changes nothing: if it fails, the Close on the next line
	// releases the lock anyway.
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) //nolint:errcheck // the Close below releases it whatever this returns
	_ = l.f.Close()
	l.f = nil
}
