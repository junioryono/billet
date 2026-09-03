package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE LOCK'S NAME IS BUILT OUT OF THE IDENTITY, SO THE IDENTITY IS CHECKED HERE.
//
// A SEPARATOR is what does the damage, and it is worth being exact about what the
// damage IS, because two plausible answers are both wrong. It is NOT that two runs
// miss each other: they get the same malformed value, join to the same path, and
// still contend. And a leading `..` alone relocates nothing, because the prefix is
// glued on and produces a component merely NAMED `billet-imageverify-..`.
//
// What it actually costs is that billet cannot say where the lock lands. Enough `..`
// components and the file leaves `node.lock_dir` entirely — which exists precisely so
// a service and an operator share ONE collision domain — and a path outside it is
// shared with whatever else is there. Where the relocated path does not exist the
// open fails instead, and "cannot open" is the same answer a host with nowhere to put
// a lock gives, so the protection reports a directory problem rather than a bad
// identity.
//
// This stages the escape that SUCCEEDS, because an identity that merely fails to open
// would let the test pass against a lock that had been silently moved.
//
// state.LockDeployment states the same rule for the same reason: the check that
// matters is the one next to the interpolation.
func TestTheProbeLockRefusesAnIdentityThatWouldMoveIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// `billet-imageverify-` + this + `.lock` cleans to the PARENT of the configured
	// directory: the prefixed component and one `..` cancel, and the second `..`
	// leaves `dir`. That parent exists and is writable, so without the guard the lock
	// is taken there and nothing says so.
	const escaping = "x/../../taken"

	outside := filepath.Join(filepath.Dir(dir), "taken.lock")

	_, err := takeProbeLock(dir, escaping)
	if err == nil {
		t.Fatal("the probe lock accepted an identity that puts the file it locks outside " +
			"the directory it was configured into, silently, so nothing else aiming at " +
			"that directory shares a collision domain with it")
	}

	if !strings.Contains(err.Error(), "refusing to lock") {
		t.Errorf("the refusal does not say the identity is why: %v", err)
	}

	// AND NOTHING WAS CREATED THERE. An error is the cheapest thing this could produce
	// and says nothing about whether the file was placed first.
	if _, statErr := os.Stat(outside); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refusal still left a lock at %s: %v", outside, statErr)
	}
}

// AND A MINTED IDENTITY IS STILL TAKEN, so the refusal above cannot be satisfied by
// refusing everything.
func TestTheProbeLockIsHeldForAMintedIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	lock, err := takeProbeLock(dir, nodeDeployment)
	if err != nil {
		t.Fatalf("takeProbeLock: %v", err)
	}

	t.Cleanup(func() {
		if err := lock.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	// NAMED AFTER THE DEPLOYMENT rather than after the identity derived from it: this
	// path is what an operator is handed when a second verification is refused, so it
	// has to name something they can look up.
	if _, err := os.Stat(filepath.Join(dir, "billet-imageverify-"+nodeDeployment+".lock")); err != nil {
		t.Errorf("the lock is not at the path the contention message would print: %v", err)
	}
}

// AND THE FILE IS ACTUALLY LOCKED, WHICH THE TEST ABOVE CANNOT SEE.
//
// Asserting the lock file exists passes against a takeProbeLock that opens it and
// flocks nothing, and what that costs is the whole reason the lock is here: the
// probe's name is the same on every run, so a second verification's pre-launch
// cleanup destroys the first one's live microVM and the first reports a healthy
// image as never having come back.
//
// ONE PROCESS IS ENOUGH HERE, and that is worth saying because for the DEPLOYMENT
// lock it is not: there, a package-local mutex satisfies an in-process test while
// two billets start against one daemon. Nothing in this file holds process-local
// state — flock is the only mechanism — and a flock is owned by the open file
// description rather than by the process, so a second descriptor on the same file
// contends with the first inside one process exactly as it would across two. That
// is asserted by mutation rather than by this paragraph: remove the unix.Flock call
// and this test goes red.
func TestASecondVerificationOnThisMachineIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	held, err := takeProbeLock(dir, nodeDeployment)
	if err != nil {
		t.Fatalf("take the first lock: %v", err)
	}

	// RELEASED WHATEVER HAPPENS. The interesting failure is the second acquisition
	// SUCCEEDING, and that path ends in t.Fatal — so without this the descriptor that
	// holds the lock outlives the test.
	releasedHeld := false

	t.Cleanup(func() {
		if releasedHeld {
			return
		}

		if err := held.release(); err != nil {
			t.Errorf("release the first lock: %v", err)
		}
	})

	second, err := takeProbeLock(dir, nodeDeployment)
	if err == nil {
		if releaseErr := second.release(); releaseErr != nil {
			t.Errorf("release the second lock: %v", releaseErr)
		}

		t.Fatal("a second verification took the lock while the first held it, so two runs " +
			"would be one microVM and the second one's cleanup would destroy the first " +
			"one's live probe")
	}

	// THE DIAGNOSTIC AN OPERATOR ACTS ON. An error is the cheapest thing this could
	// return, and a permissions problem or a missing directory would satisfy a bare
	// `err != nil` while meaning something entirely different.
	if !strings.Contains(err.Error(), "another verification is already running") {
		t.Errorf("the refusal does not say what is holding the lock: %v", err)
	}

	// AND RELEASING IT LETS THE NEXT RUN IN, or a crashed verification would wedge
	// every later one on a host with nothing running.
	releasedHeld = true

	if err := held.release(); err != nil {
		t.Fatalf("release the first lock: %v", err)
	}

	after, err := takeProbeLock(dir, nodeDeployment)
	if err != nil {
		t.Fatalf("the lock stayed held after it was released: %v", err)
	}

	if err := after.release(); err != nil {
		t.Errorf("release: %v", err)
	}
}
