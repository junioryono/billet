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

// The store lock: an exclusive advisory lock every billet process takes around a
// mutation of a LEASE NAME in tart's store.
//
// THE SCOPE IS THE LEASE NAME, NOT EVERY NAME, and the difference was worth a
// review round. What the lock makes atomic is a check and an act on a name that
// ANOTHER BILLET COULD PUT A VM UNDER: the rename that publishes a lease name,
// and the delete that removes one. A `.staging` name is outside it by
// construction — it carries a lease id unique to one control plane, and
// checkSpec RESERVES the suffix so nothing can be launched under it — and the
// clone that creates one is deliberately outside too, because it takes minutes.
// Writing the rule as "every name mutation" was both false and expensive: it put
// cleanup behind a lock it did not need, where losing the lock leaks a
// multi-gigabyte clone nothing reclaims. See discardStaging.
//
// WHY A LOCK AND NOT A CLEVERER SEQUENCE OF CLI CALLS. Both of this backend's
// name-resolving operations decide something about a name and then act on that
// name in a second call: Destroy proves a VM is this deployment's and then
// `tart delete`s it, and Launch finds a name free and then `tart rename`s a
// clone into it. Each pair has a window, and the window is only closable by the
// two of them agreeing — a Destroy that locks while a Launch does not is not
// serialized at all. The alternative that was tried, renaming a VM aside and
// deleting the new name, makes the delete act on an object rather than a name
// and was REVERTED: a quarantine name is durable state that List, the inventory
// cross-check, the next Launch and a resuming Destroy must all reason about, and
// one review round of it produced three defects that each either freed a running
// guest's capacity or deleted another deployment's VM.
//
// WHAT IT DOES NOT CLOSE, said out loud because a lock reads as absolute. Only
// billet takes it. An operator running `tart delete` or `tart rename` by hand is
// unaffected, and nothing billet owns can change that — tart has no lock of its
// own and billet does not own the store.
//
// Four facts it rests on, MEASURED on a real Mac with tart 2.36.0 rather than
// read:
//
//   - TWO SEPARATE OPENS OF ONE FILE CONFLICT INSIDE ONE PROCESS, and a second
//     flock on the SAME descriptor SUCCEEDS. That is why a descriptor is opened
//     per acquisition and never cached on the Provider: a cached one would make
//     every acquisition after the first a silent no-op, so two goroutines would
//     both believe they held the store while neither excluded the other.
//
//   - A DOT-DIRECTORY AT THE TOP OF TART_HOME IS INVISIBLE TO TART. `tart
//     create`, `tart list --format json` and `tart get` behave identically with
//     `.billet/store.lock` present; tart reads `vms/` and `cache/`.
//
//   - `tart prune` TARGETS `caches` OR `vms` AND NOTHING ELSE — its own
//     `--entries` vocabulary. The most aggressive form of both (`--older-than 0`)
//     left the lock file untouched, so ordinary traffic cannot unlink the path
//     out from under a holder, which is the failure that ruled out a cache
//     directory for the deployment lock one package over.
//
//   - `tart rename` REFUSES AN OCCUPIED TARGET: exit 64, "failed to rename VM
//     <src>, target VM <dst> already exists, delete it first!". So publishing a
//     lease name cannot overwrite anything even without this lock. What the lock
//     adds there is the other half of the pair — the name is free at the instant
//     a concurrent Destroy cannot be between its proof and its delete.
const (
	// storeStateDir is billet's own directory inside tart's store.
	storeStateDir = ".billet"
	// storeLockName is the file flocked inside it.
	storeLockName = "store.lock"
)

// errStoreLockBusy is contention, as distinct from every other reason a lock
// cannot be taken.
//
// THE DISTINCTION IS NOT COSMETIC. Contention PROVES the mechanism works — the
// file exists, it is openable, and something else is holding it — while a
// failure to place the lock proves the opposite. A caller that only wants to
// know whether this host can serialize at all (CheckHost) must read the first
// as a pass, and a caller that wanted to mutate a name must read both as a
// refusal. A single error value cannot say that, which is the same "could not
// tell is not no" rule the credential paths are built on.
var errStoreLockBusy = errors.New("the tart store lock is held")

// storeLockPath is where this provider's store lock lives.
//
// UNDER TART_HOME, so it is keyed to the STORE rather than to a deployment, a
// user or a config file. Two billets sharing a store find the same file by
// construction, and two billets with different TART_HOMEs correctly do not
// contend — they are not managing the same VMs.
func (p *Provider) storeLockPath() string {
	return filepath.Join(p.home, storeStateDir, storeLockName)
}

// lockStore takes the store lock, or says why it could not. The returned func
// releases it.
//
// why names the mutation, in the caller's words, so a contention error says what
// was not done rather than only that a lock was busy.
//
// IT WAITS RATHER THAN FAILING FAST, which is the opposite of state.LockDeployment
// and right for the opposite reason: there, contention means a second control
// plane exists and refusing is the remedy; here it means another billet is
// midway through a rename or a delete, which is the ordinary case and something
// to wait out rather than fail over. RETRYING A NON-BLOCKING FLOCK rather than
// taking a blocking one, because a blocking flock pins an OS thread and cannot
// be cancelled — every deadline in this package would go back to being
// advisory, which is the lesson cmd.WaitDelay exists for.
//
// IT FAILS CLOSED, and the reason is NOT that an unwritable `.billet/` implies
// an unwritable `vms/` — that was the first version's argument and it does not
// follow, since they are siblings and permissions, an ACL or ownership can
// differ between them. The reason is narrower and holds regardless: billet must
// not mutate a name it cannot serialize, because the whole point of the lock is
// that the check and the act are one. There is no degraded mode and no config
// knob, so a caller that cannot take it does not act.
func (p *Provider) lockStore(ctx context.Context, why string) (func(), error) {
	path := p.storeLockPath()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("prepare the tart store lock directory (to %s): %w", why, err)
	}

	// O_NOFOLLOW: the final component must be the lock file itself. A symlink
	// there would move the lock to an inode other billets do not flock, which is
	// a lock that excludes nothing while looking held.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the tart store lock %s (to %s): %w", path, why, err)
	}

	// DERIVED ONCE, OUTSIDE THE LOOP. Bounded by the earlier of the caller's
	// deadline and billet's own window, for the reason Destroy's stop poll is:
	// waiting on the node's single command slot for the caller's whole ten
	// minutes while this function claims a window of its own bounds nothing.
	waitCtx, cancel := context.WithTimeout(ctx, p.storeLockWindow)
	defer cancel()

	retry := time.NewTimer(p.storeLockRetry)
	defer retry.Stop()

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { p.unlockStore(f, path) }, nil
		}

		// CONTENTION IS THE ONLY THING WORTH WAITING ON. ENOLCK, a filesystem
		// with no flock, a descriptor problem: none of those improve with time,
		// and retrying them until the window expires reports "somebody else held
		// it" about a host that can never take it at all.
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			p.closeStoreLock(f, path)

			return nil, fmt.Errorf("lock the tart store at %s (to %s): %w", path, why, err)
		}

		retry.Reset(p.storeLockRetry)

		select {
		case <-waitCtx.Done():
			p.closeStoreLock(f, path)

			// THE CALLER RUNNING OUT IS A DIFFERENT FACT from billet's own window
			// expiring, and reporting one as the other sends an operator looking
			// for a second billet that is not there — the same attribution
			// Destroy makes around its stop poll. But both branches carry
			// errStoreLockBusy, because both are reachable only by having been
			// refused the flock: what differs is whose clock ran out, not why the
			// lock could not be taken.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("waiting for the tart store lock at %s (to %s): %w: %w",
					path, why, ctx.Err(), errStoreLockBusy)
			}

			// WHAT IS KNOWN IS THAT THE FLOCK WAS REFUSED, and the message says
			// only that. Naming another process would be an assertion billet
			// cannot make: flock refuses a second DESCRIPTOR from the holder's own
			// process too (measured), so a future path that took this lock inside
			// its own critical section would be told to hunt for a second billet
			// that does not exist. Advice, not a diagnosis.
			return nil, fmt.Errorf("the tart store lock at %s stayed held for more than %s, "+
				"so billet did not %s; every billet sharing this TART_HOME takes this lock "+
				"around a rename or a delete, so look for another billet process stuck "+
				"inside one: %w",
				path, p.storeLockWindow, why, errStoreLockBusy)
		case <-retry.C:
		}
	}
}

// unlockStore drops the lock. Unlocking before the close is explicit rather than
// necessary — closing the descriptor releases the flock — so that a reader is not
// left inferring it, which is the same reason state's dirLock does both.
func (p *Provider) unlockStore(f *os.File, path string) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		p.log.Error("could not release the tart store lock", "path", path, "error", err)
	}

	p.closeStoreLock(f, path)
}

// closeStoreLock closes the descriptor, reporting a failure rather than dropping
// it: a descriptor that will not close is one this process keeps the lock on,
// which is a node that stops being able to launch or destroy anything.
func (p *Provider) closeStoreLock(f *os.File, path string) {
	if err := f.Close(); err != nil {
		p.log.Error("could not close the tart store lock", "path", path, "error", err)
	}
}

// publishStaging renames a marked clone to its lease name, under the store lock.
//
// THE LOCK IS THE POINT, not the rename. `tart rename` already refuses an
// occupied target (measured above), so nothing here is preventing an overwrite.
// What it prevents is the OTHER half of the pair: a Destroy elsewhere that has
// proved a VM stopped and ours and is about to `tart delete` it BY NAME cannot
// be between those two steps while this rename makes that name refer to a
// different VM. Both sides take one lock, so the check and the act are one.
//
// Not a defer in Launch, deliberately: what follows the rename is a boot and a
// registration delivery that legitimately take minutes, and a host-wide lock
// held across those serializes every guest on the Mac.
func (p *Provider) publishStaging(ctx context.Context, staging, name string) error {
	unlock, err := p.lockStore(ctx, "publish "+name)
	if err != nil {
		return err
	}

	defer unlock()

	// ASKED UNDER THE LOCK, so the answer is still true when the rename runs.
	// tart's own refusal would catch this, and its wording ("delete it first!")
	// describes a stale VM an operator should remove — which is the wrong advice
	// for a name that appeared since this launch's own inventory read.
	if _, err := os.Stat(p.vmDir(name)); err == nil {
		return errors.New("a VM appeared under this lease's name after billet found the name " +
			"free; refusing to launch a second guest for one lease")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check that %s is free before renaming into it: %w", name, err)
	}

	if _, err := p.run(ctx, "rename", staging, name); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// deleteProvenOurs re-proves ownership and deletes, with both under the store
// lock so they are one act.
//
// This is the name-resolution race. `tart delete` resolves a NAME, so ownership
// proved before the call is a statement about whatever held the name THEN; every
// billet that could put a different VM under it takes this same lock around the
// rename that would.
func (p *Provider) deleteProvenOurs(ctx context.Context, id string) error {
	unlock, err := p.lockStore(ctx, "delete "+id)
	if err != nil {
		return err
	}

	defer unlock()

	if err := p.stillOurs(id); err != nil {
		return err
	}

	if _, err := p.run(ctx, "delete", id); err != nil {
		// A MISSING VM HERE IS NOT SUCCESS: it was observed stopped and proved
		// ours moments ago, so its absence means something else acted on this
		// store and billet cannot claim it destroyed anything.
		return fmt.Errorf("delete %s: %w", id, err)
	}

	return nil
}
