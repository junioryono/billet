package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/wirecert"
)

// decisionName is the durable high-water mark of fleet decisions this machine has
// acted on.
//
// OUTSIDE THE RECOVERY DIRECTORY, and that is the point. The active claim is
// released the moment an upgrade commits or completes a rollback, so it bounds
// concurrency and nothing else — a delayed instruction arriving one second later
// finds nothing in its way. This file is what remains.
const decisionName = "last-decision"

// ErrSuperseded means a machine was told to install a release the fleet has
// already moved past.
var ErrSuperseded = errors.New("this instruction has been superseded")

// readDecision returns the newest rollout generation this machine has acted on.
//
// THREE ANSWERS, NOT TWO, and collapsing them was a defect. A machine that has
// never taken a fenced upgrade reports zero and is fine — that is every host
// before its first one. A file billet cannot READ or cannot PARSE is a different
// thing entirely: the fence is the only record that a newer decision exists, so
// answering zero for it hands a stale instruction permission to overwrite the
// evidence and install a release the fleet has left behind. That one is an error,
// and a fenced instruction refuses on it.
func readDecision() (int64, error) { return readMark(decisionPath()) }

// settledName is the newest fleet decision this machine has SETTLED on: one whose
// transaction committed here, or one this machine was found already on.
//
// A SECOND MARK, BECAUSE THE FIRST IS RAISED BEFORE THE WORK. last-decision is
// written before a transaction stages anything, so a superseded instruction is
// refused whatever happens next, and it survives that transaction's rollback —
// which is right for a fence and wrong for a question of whether the decision
// was carried out. A completed rollout is taken by a host that has not settled
// on it, so a standby that attempted it and rolled back is asked again, and one
// that committed it or was found on it is not moved back to it after an operator
// moved it by hand.
const settledName = "settled-decision"

func settledPath() string { return filepath.Join(upgradeRoot, settledName) }

// readSettled returns the newest fleet decision this machine has settled on.
func readSettled() (int64, error) { return readMark(settledPath()) }

// recordSettled raises the settled mark to a decision, never lowering it.
func recordSettled(generation int64) error {
	if generation <= 0 {
		return nil
	}

	return withDecisionLock(func() error { return raiseMark(settledPath(), generation) })
}

// readMark reads one decision mark.
func readMark(path string) (int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}

		return 0, fmt.Errorf("%w: %s could not be read: %w", ErrUnreadableDecision, path, err)
	}

	generation, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil || generation < 0 {
		return 0, fmt.Errorf("%w: %s does not hold a fleet decision number. Remove it once "+
			"you know which release this machine should be on; until then billet cannot "+
			"tell whether an instruction has been superseded", ErrUnreadableDecision, path)
	}

	return generation, nil
}

// raiseMark writes a mark if the generation is above what it holds.
func raiseMark(path string, generation int64) error {
	held, err := readMark(path)
	if err != nil {
		return err
	}

	if generation <= held {
		return nil
	}

	if err := wirecert.WriteFileAtomic(path,
		[]byte(strconv.FormatInt(generation, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("record which fleet decision this machine is acting on: %w", err)
	}

	// FLUSHED, because this is the record that outlives the crash it exists for.
	return syncUpgradeDir(upgradeRoot)
}

// ErrUnreadableDecision means the fence exists and billet cannot read it.
var ErrUnreadableDecision = errors.New("this machine's record of which fleet decision it " +
	"last acted on is unreadable")

// decisionLockName is the exclusion around the high-water mark.
//
// A LOCK OF ITS OWN, AND NOT THE UPGRADE CLAIM. The claim excludes two
// TRANSACTIONS, and the mark is touched by a path that runs no transaction at
// all: finding the release already installed is acting on a decision, records
// one, and has nothing to claim. So the two exclusions cover different sets, and
// using the claim for both left the sequence unserialised exactly where the
// review found it — generation 10 and generation 9 both reading 4, 10 writing
// 10, and 9 then writing 9 over it. An atomic rename makes each write whole; it
// does not make read-then-write atomic, and a mark that can go backwards is a
// fence a superseded release walks straight through.
const decisionLockName = "decision.lock"

// withDecisionLock runs fn while holding the mark exclusively.
//
// IT WAITS, unlike the deployment lock, and for the opposite reason: a second
// holder there means a second control plane, which is a thing to refuse. Here it
// means another billet is a few syscalls from finishing its own read-modify-write.
//
// A FRESH DESCRIPTOR PER ACQUISITION, because a second flock on the SAME
// descriptor succeeds — a cached one would make every acquisition after the first
// a silent no-op while both callers believed they held it. Measured in
// internal/provider/tart, which learned it the same way.
func withDecisionLock(fn func() error) error {
	if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
		return fmt.Errorf("prepare %s: %w", upgradeRoot, err)
	}

	f, err := os.OpenFile(filepath.Join(upgradeRoot, decisionLockName),
		os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open the decision lock: %w", err)
	}

	defer func() { _ = f.Close() }()

	if err := acquireDecisionLock(f); err != nil {
		return err
	}

	// UNLOCKED EXPLICITLY FOR THE READER, NOT FOR THE KERNEL: the deferred Close
	// above releases a flock on its own, so this states intent and its result
	// changes nothing.
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }() //nolint:errcheck // the deferred Close releases it whatever this returns

	return fn()
}

// decisionLockWait bounds the wait for the mark.
//
// ENORMOUS RELATIVE TO THE WORK IT GUARDS, which is a read, an atomic write and
// an fsync — milliseconds. It is not sized against contention; it is sized so
// that the one thing which could hold it forever produces a sentence instead.
const decisionLockWait = 30 * time.Second

// acquireDecisionLock takes the mark's lock, or explains why it could not.
//
// NON-BLOCKING IN A LOOP RATHER THAN A BLOCKING FLOCK, because of how the one
// serious mistake here would present. A flock is held by the OPEN FILE
// DESCRIPTION, so a second acquisition from a second descriptor in the SAME
// process conflicts exactly as another process would — measured in
// internal/provider/tart, which learned it the same way. Nothing nests today, and
// the *Locked helpers exist so that nothing has to; but a blocking LOCK_EX turns
// the day somebody does into a host that hangs silently forever, mid-upgrade,
// with no output and nothing in any log. This turns it into an error that names
// the possibility.
func acquireDecisionLock(f *os.File) error {
	deadline := time.Now().Add(decisionLockWait)

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("take the decision lock: %w", err)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("could not take %s within %s. It guards a read and a write "+
				"that take milliseconds, so something is holding it: either another billet "+
				"is wedged on this machine, or this build takes it twice on one path, which "+
				"would deadlock against itself",
				filepath.Join(upgradeRoot, decisionLockName), decisionLockWait)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// recordDecision makes this machine's newest acted-on generation durable.
//
// WRITTEN BEFORE THE TRANSACTION STARTS, not after it finishes. What it protects
// against is a SECOND instruction, and the window that matters is the whole
// length of the first — which is unbounded, because it contains a drain. A record
// written at the end would leave the entire upgrade unfenced.
//
// A GENERATION THAT DOES NOT MOVE FORWARD IS NOT WRITTEN, and the read that
// decides it happens under the lock, so the mark cannot regress.
func recordDecision(generation int64) error {
	if generation <= 0 {
		return nil
	}

	return withDecisionLock(func() error { return recordLocked(generation) })
}

func recordLocked(generation int64) error { return raiseMark(decisionPath(), generation) }

// checkDecision refuses an instruction older than one this machine has acted on.
//
// EQUAL IS ALLOWED AND CHANGES NOTHING. Instructions are delivered over a network
// and retried, so the same decision arriving twice is the ordinary case rather
// than an attack — and by the time it does, the machine is either already on that
// release (in which case there is nothing to do) or still working on it (in which
// case the claim refuses).
//
// STRICTLY OLDER IS REFUSED, because it is the one case with no other guard. A
// rollout's generation is monotonic across the deployment, so a lower one is an
// instruction from a decision the fleet has replaced — and after the newer one
// finished, the claim it held is gone. Without this the machine would dutifully
// install the release the operator moved away from.
func checkDecision(target hostUpgradeTarget) error {
	if target.generation <= 0 {
		return nil
	}

	return withDecisionLock(func() error { return checkLocked(target) })
}

// checkAndRecordDecision refuses a superseded instruction and marks this machine
// as having acted on the one that survives, as ONE atomic step.
//
// THE PAIR IS WHAT HAS TO BE ATOMIC. Checking and then recording under two
// separate acquisitions leaves the same window the lock was added to close: a
// newer decision can land between them, and the recording then overwrites it.
func checkAndRecordDecision(target hostUpgradeTarget) error {
	if target.generation <= 0 {
		return nil
	}

	return withDecisionLock(func() error {
		if err := checkLocked(target); err != nil {
			return err
		}

		return recordLocked(target.generation)
	})
}

func checkLocked(target hostUpgradeTarget) error {
	acted, err := readDecision()
	if err != nil {
		return err
	}

	if target.generation < acted {
		return fmt.Errorf("%w: this machine has already acted on fleet decision %d and this "+
			"one is %d, so installing it would move the host to a release the deployment "+
			"has left behind; nothing was installed", ErrSuperseded, acted, target.generation)
	}

	return nil
}

func decisionPath() string { return filepath.Join(upgradeRoot, decisionName) }
