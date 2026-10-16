package images

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

// RELEASING THE LOCK RELEASES IT EVEN WHILE ANOTHER DESCRIPTOR SHARES IT.
//
// A child forked by a parallel goroutine holds a copy of the lock's open file
// description until its exec closes it. The duplicate here stands in for that
// child, deterministically: closing only the original left the flock held, which
// is how the test above failed on CI (2026-09-26).
func TestReleasingTheProbeLockReleasesItWhileADuplicateIsOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	held, err := takeProbeLock(dir, nodeDeployment)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}

	child, err := unix.Dup(int(held.file.Fd()))
	if err != nil {
		t.Fatalf("duplicate the lock's descriptor: %v", err)
	}

	t.Cleanup(func() {
		if err := unix.Close(child); err != nil {
			t.Errorf("close the duplicate: %v", err)
		}
	})

	if err := held.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	after, err := takeProbeLock(dir, nodeDeployment)
	if err != nil {
		t.Fatalf("the lock stayed held by a descriptor that shares it: %v", err)
	}

	if err := after.release(); err != nil {
		t.Errorf("release: %v", err)
	}
}

// A LOCK WHOSE NAME MOVED IS TAKEN AGAIN, UNLESS ANOTHER VERIFICATION HOLDS IT
// (#175). A unit's stop removing its RuntimeDirectory unlinks the lock's name
// under its holder, and the next verification locks a fresh file at the same
// path. hold() asks before every act only the holder may perform: with nobody
// else on the name, it takes the name again, so a later verification is refused
// as it would have been; with another verification already on it, it refuses,
// because a probe by this name may now be that one's.
func TestAProbeLockWhoseNameMovedIsTakenAgainUnlessAnotherHoldsIt(t *testing.T) {
	t.Parallel()

	release := func(t *testing.T, l *probeLock) {
		t.Helper()
		t.Cleanup(func() {
			if err := l.release(); err != nil {
				t.Errorf("release: %v", err)
			}
		})
	}

	t.Run("nobody else on the name", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		first, err := takeProbeLock(dir, nodeDeployment)
		if err != nil {
			t.Fatalf("take the lock: %v", err)
		}
		release(t, first)

		// THE WHOLE DIRECTORY, as a unit's stop removes its RuntimeDirectory.
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}

		if err := first.named(); err == nil || !strings.Contains(err.Error(), "was removed while this verification held it") {
			t.Fatalf("named() over a removed name = %v, want the removal named", err)
		}

		replaced := first.file

		if err := first.hold(); err != nil {
			t.Fatalf("hold() with nobody else on the name = %v, want the name taken again", err)
		}

		if err := first.named(); err != nil {
			t.Errorf("after hold() the name does not lead to the lock held: %v", err)
		}

		if _, err := replaced.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("the descriptor the re-take replaced is still open (%v), holding a lock on a file nobody can name", err)
		}

		if second, err := takeProbeLock(dir, nodeDeployment); err == nil {
			if err := second.release(); err != nil {
				t.Errorf("release: %v", err)
			}

			t.Fatal("a second verification took the name the first had taken again")
		}
	})

	t.Run("another verification on the name", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		first, err := takeProbeLock(dir, nodeDeployment)
		if err != nil {
			t.Fatalf("take the lock: %v", err)
		}
		release(t, first)

		if err := os.Remove(first.path); err != nil {
			t.Fatal(err)
		}

		second, err := takeProbeLock(dir, nodeDeployment)
		if err != nil {
			t.Fatalf("the recreated name was not lockable, so this does not stage the race: %v", err)
		}
		release(t, second)

		if err := first.named(); err == nil || !strings.Contains(err.Error(), "now names a different file") {
			t.Fatalf("named() over a replaced name = %v, want the replacement named", err)
		}

		if err := first.hold(); err == nil || !strings.Contains(err.Error(), "another verification holds it now") {
			t.Fatalf("hold() under another verification's lock = %v, want the overlap named", err)
		}

		if err := second.hold(); err != nil {
			t.Errorf("the lock on the recreated name is not held: %v", err)
		}
	})
}

// A NAME MOVED BETWEEN THE OPEN AND THE FLOCK IS TAKEN AGAIN, ONCE MORE THAN
// THAT IS A REFUSAL. The hook stands in for a unit's stop landing in the window:
// moved once, the second attempt holds the name it opened; moved every time,
// the verification refuses rather than hold a lock nobody else can open. Serial:
// it sets probeLockOpened.
func TestTakingTheProbeLockRetakesANameMovedUnderIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		moves  int
		refuse bool
	}{
		{name: "moved once", moves: 1},
		{name: "moved every time", moves: probeLockAttempts, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opened := 0

			probeLockOpened = func(path string) {
				opened++
				if opened > tc.moves {
					return
				}

				if err := os.Remove(path); err != nil {
					t.Error(err)
				}

				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Error(err)
				}
			}
			t.Cleanup(func() { probeLockOpened = nil })

			lock, err := takeProbeLock(dir, nodeDeployment)
			if tc.refuse {
				if err == nil {
					if err := lock.release(); err != nil {
						t.Errorf("release: %v", err)
					}

					t.Fatal("a lock whose name moved on every attempt was taken")
				}

				if !strings.Contains(err.Error(), "now names a different file") {
					t.Errorf("the refusal does not name the race: %v", err)
				}

				if opened != probeLockAttempts {
					t.Errorf("opened %d times, want %d", opened, probeLockAttempts)
				}

				return
			}

			if err != nil {
				t.Fatalf("takeProbeLock after one move: %v", err)
			}

			defer func() {
				if err := lock.release(); err != nil {
					t.Errorf("release: %v", err)
				}
			}()

			if err := lock.named(); err != nil {
				t.Errorf("the retaken lock is not held: %v", err)
			}

			if opened != 2 {
				t.Errorf("opened %d times, want 2", opened)
			}
		})
	}
}

// A RE-TAKE WHOSE NAME MOVES AGAIN UNDER IT IS NOT A HOLD. hold() re-checks the
// lock it just took, because a lock on a file nobody can name excludes nobody;
// the hook moves the name between the re-take's open and its flock, and the
// answer is a refusal naming the move rather than a lock kept on the old inode.
// Serial: it sets probeLockOpened.
func TestARetakeWhoseNameMovesAgainIsNotAHold(t *testing.T) {
	dir := t.TempDir()

	lock, err := takeProbeLock(dir, nodeDeployment)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}

	t.Cleanup(func() {
		if err := lock.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	if err := os.Remove(lock.path); err != nil {
		t.Fatal(err)
	}

	probeLockOpened = func(path string) {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}

		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { probeLockOpened = nil })

	held := lock.file
	before := openDescriptors(t)

	if err := lock.hold(); err == nil || !strings.Contains(err.Error(), "now names a different file") {
		t.Fatalf("hold() over a name that moved under the re-take = %v, want the move named", err)
	}

	if lock.file != held {
		t.Error("a re-take that failed replaced the descriptor the verification held")
	}

	// THE RE-TAKEN DESCRIPTOR IS CLOSED: its lock is on the inode the hook
	// unlinked, so no second lock can show it held; the descriptor count can.
	if after := openDescriptors(t); after != before {
		t.Errorf("the failed re-take left %d descriptors open, %d before it", after, before)
	}
}

// openDescriptors counts this process's open file descriptors. Serial callers
// only: a parallel test opening files changes the count.
func openDescriptors(t *testing.T) int {
	t.Helper()

	// NAMES, NOT ENTRIES: the listing includes the descriptor reading it, which
	// is closed by the time an entry would be examined.
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = dir.Close() }()

	names, err := dir.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}

	return len(names)
}
