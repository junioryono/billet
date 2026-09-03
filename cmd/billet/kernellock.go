package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// kernelLockName is the file every billet flocks around a change to the kernel
// directory. It lives INSIDE that directory on purpose -- see takeKernelDirLock.
const kernelLockName = ".billet-kernels.lock"

// kernelLockWindow and kernelLockRetry bound the wait for the lock.
//
// PACKAGE VARIABLES SO A TEST CAN SHORTEN THEM, the same seam kernelInstaller and
// hostLockDir are. Ten minutes is sized against what the holder does: a pull holds
// this across rbd's raw write of a multi-gigabyte filesystem, which is deliberately
// unbounded, while a reap holds it across a handful of metadata reads and some
// unlinks.
var (
	kernelLockWindow = 10 * time.Minute
	kernelLockRetry  = 250 * time.Millisecond
)

// errKernelLockBusy is contention, as distinct from every other reason a lock
// cannot be taken.
//
// THE DISTINCTION IS NOT COSMETIC. Contention PROVES the mechanism works -- the
// file exists, it is openable, and something else holds it -- while a failure to
// place the lock proves the opposite, and only one of the two improves by waiting.
// It is also what lets a test assert the refusal it is named for: an assertion
// satisfied by "an error" cannot tell a busy lock from a directory billet could not
// write, and one of those is the bug.
var errKernelLockBusy = errors.New("the kernel directory lock is held")

// onKernelLockContended runs the first time an acquisition is refused the flock.
//
// nil IN PRODUCTION, AND IT EARNS ITS KEEP IN ONE TEST: proving that a reap waits for
// a pull rather than deleting the kernel it is about to publish a generation for
// means proving the reap was BLOCKED, and the only honest evidence of that is the
// moment it discovers the lock is held. Everything else a test could watch -- a fixed
// wait for the absence of a cluster read, a goroutine that has not got there yet --
// is also satisfied by a reap that took no lock and merely started late.
var onKernelLockContended func()

// kernelLockPath is where the lock for a kernel directory lives.
func kernelLockPath(dir string) string { return filepath.Join(dir, kernelLockName) }

// kernelLock is a held exclusive lock over one kernel directory.
//
// WHAT IT PREVENTS is a pull and a reap that each behave correctly alone. `billet
// images pull` installs a kernel and then commits Ceph metadata naming that exact
// filename; `billet images reap` computes which kernels are still needed from the
// generations that exist RIGHT NOW and unlinks the rest. Interleaved, the reap
// deletes a kernel a moment before the generation naming it is published -- after
// which every node resolves a verified generation and cannot boot the exact kernel
// it was verified against, and re-pulling repairs the local half while the cluster
// keeps advertising a pair that never existed.
//
// THE CEPH PUBLISH LOCK DOES NOT COVER IT. That lock makes generation removal and
// generation publication exclude each other, and neither the local install nor the
// local unlink is inside it: the install happens before ImportGeneration acquires
// it, and reapKernels runs after Reap has released it. This is exactly the interval
// that lock was never asked to cover, and it could not be extended to cover it: it
// is CLUSTER-WIDE, so holding it across one machine's disk work would serialize
// every node's publishing, and its stale bound is six hours.
//
// AND A RE-CHECK IS NOT A FIX. Confirming the kernel is still there immediately
// before ImportGeneration reads as a proof and is not one -- the reap can unlink it
// a microsecond later. Nor does an open descriptor help: reuseInstalledKernel
// hashes, chmods and flushes through one, all of which still succeed after the
// pathname is gone. A descriptor proves the bytes; it does not prove the NAME, and
// the name is what a generation records.
//
// WHAT IT DOES NOT PREVENT, said out loud because a lock reads as absolute. Only
// billet takes it, so an operator removing files in the kernel directory by hand is
// unaffected. And it is a LOCAL lock: two hosts sharing a Ceph pool each keep their
// own kernel directory, so this says nothing about the other one.
type kernelLock struct{ file *os.File }

// takeKernelDirLock takes the lock over dir, or says why it could not. why names
// the operation, in the caller's words, so a timeout says what was not done.
//
// KEYED ON THE DIRECTORY ITSELF rather than on a deployment or a config file, for
// the reason the tart store lock is keyed on TART_HOME: what has to be serialized
// is every billet that changes the same directory. Two deployments on one host may
// share one, --kernel-dir overrides the config on either command, and a symlinked
// directory resolves to the same FILE here where a key derived from the path would
// spell two -- which is the identity mistake `billet local recover` made with its
// journal.
//
// INSIDE THE DIRECTORY, WHICH IS THE OPPOSITE OF wirecert.AuthorityLockPath, and
// the difference is what the lock coordinates. That one guards operations that
// REPLACE the contents of the directory it names, so a lock file among them is one
// whose inode can move out from under its holder. Nothing replaces this directory
// or renames anything over the lock: a reap unlinks individual kernel files, and
// reapKernelDir is proved never to unlink this one. That proof is the requirement --
// unlinking a held lock file does not release the flock, it detaches the PATH from
// the locked inode, after which the next process creates a new file there, locks
// that, and both run.
//
// IT WAITS RATHER THAN FAILING FAST, which is the opposite of state.LockDeployment
// and right for the opposite reason: there, a second holder means a second control
// plane and refusing IS the remedy; here it means another billet is midway through
// installing or collecting kernels, which is ordinary maintenance. The deciding
// cost is that the ansible host role runs `billet images pull --verify` as a
// required step of a transactional upgrade, so a refusal there rolls the whole
// upgrade back.
//
// RETRYING A NON-BLOCKING FLOCK rather than taking a blocking one, because a
// blocking flock pins an OS thread and cannot be cancelled -- every deadline above
// it would go back to being advisory, which is the lesson cmd.WaitDelay exists for.
//
// A FRESH DESCRIPTOR PER ACQUISITION. MEASURED one package over: two separate opens
// of one file conflict inside a single process, while a second flock on the SAME
// descriptor SUCCEEDS -- so a cached descriptor would make every acquisition after
// the first a silent no-op.
//
// IT FAILS CLOSED. A kernel directory billet cannot place a lock in is one it can
// neither install into nor delete from, so there is no legitimate operation this
// refuses and no degraded mode to offer.
func takeKernelDirLock(ctx context.Context, dir, why string) (*kernelLock, error) {
	// CHECKED NEXT TO THE INTERPOLATION, the rule state.LockDeployment and
	// takeProbeLock both follow: this builds a path out of the value, and an empty
	// directory puts the lock at ./.billet-kernels.lock in whatever directory the
	// command happened to be run from -- protecting nothing, while looking held.
	if dir == "" {
		return nil, fmt.Errorf("billet images: refusing to lock the kernel directory to %s: "+
			"no directory was named", why)
	}

	// CREATED IF ABSENT, because the pull takes this BEFORE installKernel and that
	// is what creates the directory today. Safe for the pull's durability:
	// durablefile.Installer.MkdirAll flushes every ancestor every time, including
	// when the directory already exists.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("billet images: make the kernel directory %s (to %s): %w",
			dir, why, err)
	}

	path := kernelLockPath(dir)

	// O_NOFOLLOW: the final component must be the lock file itself. A symlink there
	// would move the lock to an inode other billets do not flock, which is a lock
	// that excludes nothing while looking held.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("billet images: open the kernel directory lock %s (to %s): %w",
			path, why, err)
	}

	// DERIVED ONCE, OUTSIDE THE LOOP, and bounded by the earlier of the caller's
	// deadline and billet's own window: claiming a window of its own while ignoring
	// the caller's bounds nothing.
	waitCtx, cancel := context.WithTimeout(ctx, kernelLockWindow)
	defer cancel()

	retry := time.NewTimer(kernelLockRetry)
	defer retry.Stop()

	// expired is the answer when the budget has run out, wherever that is noticed.
	//
	// THE SENTINEL IS A CLAIM ABOUT CONTENTION, AND IT IS ONLY MADE WHEN A FLOCK WAS
	// ACTUALLY REFUSED. errKernelLockBusy means something else holds this lock, which
	// is what tells a caller the mechanism WORKS -- the file exists, it is openable,
	// and somebody has it -- apart from a host where the lock could not be placed at
	// all. The expiry branch below is reachable with the lock FREE, by a caller whose
	// own context had already ended, and attaching the sentinel there asserts a holder
	// that never existed. That is not cosmetic: kernelLockProbe counts the sentinel as
	// "the lock was held", so a cancelled probe would report contention inside the very
	// test that proves a reap waits for a pull.
	//
	// THE CALLER RUNNING OUT IS ALSO A DIFFERENT FACT from billet's own window
	// expiring, and reporting one as the other sends an operator looking for a second
	// billet that is not there.
	//
	// WHAT IS KNOWN IN THE LAST CASE IS THAT THE FLOCK WAS REFUSED, and the message
	// says only that. Naming another process would be an assertion billet cannot make:
	// flock refuses a second descriptor from the holder's OWN process too, so a future
	// path that took this lock inside its own critical section would be told to hunt
	// for a billet that does not exist. Advice, not a diagnosis.
	expired := func(contended bool) error {
		if !contended {
			cause := ctx.Err()
			if cause == nil {
				cause = waitCtx.Err()
			}

			return fmt.Errorf("billet images: the kernel directory lock at %s was free, but "+
				"the budget for %s ran out before it could be used: %w", path, why, cause)
		}

		if ctx.Err() != nil {
			return fmt.Errorf("billet images: waiting for the kernel directory lock at %s "+
				"(to %s): %w: %w", path, why, ctx.Err(), errKernelLockBusy)
		}

		return fmt.Errorf("billet images: the kernel directory lock at %s stayed held for "+
			"more than %s, so billet did not %s; every billet that installs or deletes a "+
			"kernel in this directory takes it, so look for a `billet images pull` or "+
			"`billet images reap` stuck inside one: %w",
			path, kernelLockWindow, why, errKernelLockBusy)
	}

	// contended records whether any attempt was actually refused the flock. It is what
	// expired reads, and it is the only thing that can establish a holder existed.
	//
	// AN EARLY ctx.Err() CHECK WOULD ALSO SPARE AN EXPIRED CALLER THE MKDIR AND THE
	// LOCK FILE, and it is deliberately not here: it would move the refusal out of the
	// branch below, which is the one that matters and the one two tests pin from both
	// sides. The side effect it would avoid is a directory this command owns and a
	// zero-byte dotfile the next acquisition would create anyway.
	contended, announced := false, false

	for {
		flockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if flockErr == nil {
			lock := &kernelLock{file: file}

			// TAKEN, BUT ONLY HANDED OVER IF THERE IS STILL A BUDGET TO USE IT.
			//
			// `select` picks at RANDOM when both its cases are ready, so a caller whose
			// deadline expires in the same instant as a retry tick can go round again,
			// find the holder gone, and be given a lock it has no time left to do
			// anything with. What follows that is uncancellable: installKernel copies
			// fifty megabytes and fsyncs it before the first context-aware call refuses,
			// leaving a kernel nothing names. The bound is documented, so it has to hold
			// on the way OUT of the loop as well as on the way round it.
			if waitCtx.Err() != nil {
				if releaseErr := lock.release(); releaseErr != nil {
					return nil, errors.Join(expired(contended), releaseErr)
				}

				return nil, expired(contended)
			}

			return lock, nil
		}

		// CONTENTION IS THE ONLY THING WORTH WAITING ON. ENOLCK, a filesystem with
		// no flock, a descriptor problem: none of those improve with time, and
		// retrying them until the window expires reports "somebody else held it"
		// about a host that can never take this lock at all.
		if !errors.Is(flockErr, unix.EWOULDBLOCK) {
			closeKernelLock(file, path)

			return nil, fmt.Errorf("billet images: lock the kernel directory at %s (to %s): %w",
				path, why, flockErr)
		}

		// SOMEBODY HELD IT, AND THAT IS NOW ESTABLISHED FOR THE REST OF THIS CALL.
		// It is the only evidence a holder ever existed, and it is what decides whether
		// the error on the way out may claim contention.
		contended = true

		// SAID BEFORE THE FIRST WAIT, ONCE. A bounded wait an operator cannot see is
		// a command that looks wedged for ten minutes, and the next thing anybody
		// does to a wedged command is kill it -- which for a pull is a killed import.
		if !announced {
			announced = true

			fmt.Printf("waiting up to %s for the kernel directory lock at %s; another billet "+
				"is installing or collecting kernels there, and this must not %s from a "+
				"generation set that is still being written\n", kernelLockWindow, path, why)

			// THE ONE POINT AT WHICH A CALLER IS PROVABLY BLOCKED ON THIS LOCK, exposed
			// because a test of the reap-versus-pull race has to ESTABLISH that
			// ordering rather than sleep through it. Waiting a fixed time for the
			// ABSENCE of a cluster read is satisfied by a goroutine that was simply
			// never scheduled, so it passes against a reap that takes no lock at all
			// and merely started late.
			//
			// Inside the announce branch, so it fires exactly once per acquisition and
			// only after a flock has actually been refused.
			if onKernelLockContended != nil {
				onKernelLockContended()
			}
		}

		retry.Reset(kernelLockRetry)

		select {
		case <-waitCtx.Done():
			closeKernelLock(file, path)

			return nil, expired(contended)
		case <-retry.C:
		}
	}
}

// release drops the lock. Closing the descriptor releases it, so this cannot leave
// one held by a process that has exited -- which is the whole reason this is a flock
// and not an rbd lock with a six-hour age bound. Unlocking before the close is
// explicit rather than necessary, so a reader is not left inferring it.
//
// THE CLOSE HAPPENS EITHER WAY. A descriptor that stays open is one this process
// keeps the lock on, so a failed unlock must not skip it -- and both failures are
// reported, because the second is not a consequence of the first.
//
// AND IT IS IDEMPOTENT, WHICH IS WHAT LETS ONE CALLER RELEASE EARLY AND STILL DEFER.
// A pull gives the lock back the moment its generation names the kernel, long before
// it returns, and still needs a deferred release for every path that does not get
// there. The alternative -- a second variable recording whether it has been released
// -- is one more thing that can disagree with the truth, and it did: the first version
// of this file's own test helper tracked it in a t.Cleanup, which runs LAST-IN-FIRST-
// OUT, so the flag was set before the release that read it and every lock it took was
// held for the rest of the run.
func (l *kernelLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}

	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()

	// CLEARED WHATEVER HAPPENED. A Close that failed leaves a descriptor a second
	// Close cannot rescue -- Go answers ErrClosed -- so keeping the handle would turn
	// one real failure into a second, misleading one on the deferred path.
	l.file = nil

	if unlockErr != nil {
		return errors.Join(
			fmt.Errorf("billet images: release the kernel directory lock: %w", unlockErr),
			closeErr,
		)
	}

	return closeErr
}

// closeKernelLock closes a descriptor whose flock was never taken.
//
// A FAILURE HERE IS REPORTED RATHER THAN RETURNED, because it happens on the way out
// of a path that is already returning the reason it could not lock, and replacing
// that reason would send an operator after the consequence instead of the cause.
func closeKernelLock(file *os.File, path string) {
	if err := file.Close(); err != nil {
		fmt.Printf("warning: could not close the kernel directory lock at %s: %v\n", path, err)
	}
}
