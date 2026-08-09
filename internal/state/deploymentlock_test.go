package state

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useTempCache points the DEFAULT lock location at a directory this test owns,
// so tests do not fight each other or the developer's real state directory.
//
// Deliberately exercising the default rather than passing LockOptions.Dir: the
// default is what nearly every deployment uses, so a test suite that always
// supplies an explicit directory would never notice the default breaking.
func useTempCache(t *testing.T) {
	t.Helper()

	// XDG_STATE_HOME is consulted first on every platform; HOME is the fallback
	// both branches of defaultLockDir end at.
	dir := t.TempDir()

	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
}

// TWO COPIES OF A STATE DIRECTORY CANNOT BOTH RUN.
//
// The hole this closes. `billet.lock` is flocked inside the state directory, so
// a COPY is a different inode and both lock happily — while both carry the same
// deployment identity and therefore manage the same containers against the same
// daemon, each heartbeating leases the other owns.
func TestASecondProcessWithTheSameIdentityIsRefused(t *testing.T) {
	useTempCache(t)

	const id = "0123456789abcdef0123456789abcdef"

	first, err := LockDeployment(id, LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	if first.Path() == "" {
		t.Fatal("no lock was taken, so this test proves nothing")
	}

	releaseAtEnd(t, first)

	_, err = LockDeployment(id, LockOptions{})
	if !errors.Is(err, ErrDeploymentLocked) {
		t.Fatalf("a second process took the same identity: %v", err)
	}
}

// The refusal says what to do about it.
//
// Somebody meets this because they copied a state directory, which is a
// reasonable thing to have done — the message has to name the fix rather than
// just the fact.
func TestTheRefusalExplainsWhatToDo(t *testing.T) {
	useTempCache(t)

	const id = "0123456789abcdef0123456789abcdef"

	first, err := LockDeployment(id, LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, first)

	_, err = LockDeployment(id, LockOptions{})
	if err == nil {
		t.Fatal("the second lock succeeded")
	}

	for _, want := range []string{id, "deployment-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// DIFFERENT identities do not collide. The lock is about one installation
// running twice, not about one machine running one billet.
func TestTwoDeploymentsCoexist(t *testing.T) {
	useTempCache(t)

	a, err := LockDeployment("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, a)

	b, err := LockDeployment("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", LockOptions{})
	if err != nil {
		t.Fatalf("a second, different deployment was refused: %v", err)
	}

	releaseAtEnd(t, b)
}

// Releasing lets the identity be taken again, which is what makes a restart
// work.
func TestReleasingFreesTheIdentity(t *testing.T) {
	useTempCache(t)

	const id = "0123456789abcdef0123456789abcdef"

	first, err := LockDeployment(id, LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := LockDeployment(id, LockOptions{})
	if err != nil {
		t.Fatalf("the identity stayed locked after release, so a restart would fail: %v", err)
	}

	releaseAtEnd(t, second)
}

// The lock file is private, because a world-writable one could be pre-created
// and held by any local user to keep billet from ever starting.
func TestTheLockFileIsPrivate(t *testing.T) {
	useTempCache(t)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, lock)

	info, err := os.Stat(lock.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the lock file is mode %o, want 600", perm)
	}

	dir, err := os.Stat(filepath.Dir(lock.Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}

	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("the lock directory is mode %o, want 700", perm)
	}
}

// unplaceableLock makes the lock impossible to place, by giving the location a
// parent that is a FILE.
func unplaceableLock(t *testing.T) {
	t.Helper()

	blocker := filepath.Join(t.TempDir(), "notadir")

	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("XDG_STATE_HOME", blocker)
	t.Setenv("HOME", blocker)
}

// A LOCK THAT CANNOT BE PLACED IS AN ERROR, and it did not used to be.
//
// The original reasoning: a host with nowhere to put a lock is far more often
// one deployment than two, so refusing to boot trades a rare hazard for a common
// outage. Defensible as a conclusion and wrong as a mechanism — it DERIVED
// AUTHORIZATION FROM AN I/O FAILURE. A permissions change, a symlink loop,
// ENOLCK, descriptor exhaustion, or a service manager providing no HOME all
// reach this line, and every one of them is a misconfiguration that from in here
// looks exactly like the benign case.
func TestALockThatCannotBePlacedRefusesToStart(t *testing.T) {
	unplaceableLock(t)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err == nil {
		t.Fatalf("billet started without the protection and without being asked to; "+
			"lock=%q degraded=%q", lock.Path(), lock.Degraded())
	}

	// The operator has to be able to act on it, which means naming both knobs.
	for _, want := range []string{"lock_dir", "allow_unlocked_deployment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// The operator CAN ask for it, and then it is a degraded lock that says why.
//
// What it must never do is look like a held lock: the caller has to be able to
// tell an operator that a protection is absent, and with a reason.
func TestAnOperatorCanOptOutOfTheLock(t *testing.T) {
	unplaceableLock(t)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{AllowUnplaceable: true})
	if err != nil {
		t.Fatalf("the opt-out did not take effect: %v", err)
	}

	if lock.Degraded() == "" {
		t.Fatal("the lock reports itself as held, but nothing could be locked")
	}

	if lock.Path() != "" {
		t.Errorf("a degraded lock reported a path: %q", lock.Path())
	}

	// And releasing it is a no-op rather than a crash.
	if err := lock.Release(); err != nil {
		t.Errorf("releasing a degraded lock: %v", err)
	}
}

// AN EXPLICIT DIRECTORY IS WHAT MAKES THE LOCK MEAN ANYTHING ACROSS USERS.
//
// The default location is per-user, so a system service and an operator sharing
// one docker socket — or two containers sharing a socket with private
// filesystems — get different directories and never collide, while their
// containers do. server.lock_dir is the only way to put them in one collision
// domain, so it has to actually win over the default.
func TestAnExplicitDirectoryPutsBothProcessesInOneCollisionDomain(t *testing.T) {
	shared := t.TempDir()

	const id = "0123456789abcdef0123456789abcdef"

	// Two DIFFERENT per-user defaults, standing in for two users.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	first, err := LockDeployment(id, LockOptions{Dir: shared})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, first)

	if got := filepath.Dir(first.Path()); got != shared {
		t.Fatalf("the explicit directory was ignored: locked in %q, want %q", got, shared)
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	if _, err := LockDeployment(id, LockOptions{Dir: shared}); !errors.Is(err, ErrDeploymentLocked) {
		t.Fatalf("two processes sharing a lock directory did not collide: %v", err)
	}
}

// THE DEFAULT IS NOT THE CACHE DIRECTORY, and that is a correctness requirement
// rather than a preference.
//
// A cache directory's contract is that anything in it may be deleted at any time
// — by a cleaner, a packager, or a user reclaiming disk. Unlinking a held lock
// file does not release the flock, but it does detach the PATH from the locked
// inode, so the next process creates a fresh file there, locks that, and both
// run. The one property a lock file needs is the one a cache directory
// explicitly refuses to give.
func TestTheDefaultLockLocationIsNotDisposable(t *testing.T) {
	cache := t.TempDir()

	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, lock)

	if strings.HasPrefix(lock.Path(), cache) {
		t.Fatalf("the lock is in the cache directory (%q), whose contents may be deleted "+
			"at any time — and deleting it while held lets a second billet start", lock.Path())
	}
}

// The unlink hazard itself, demonstrated rather than argued.
//
// THIS IS NOT FIXED BY THE CODE, and the test says so on purpose. Path-based
// flock cannot survive its path being unlinked: the holder keeps a lock on an
// inode nobody can name, and the newcomer's own consistency check passes because
// it created the file it just locked. There is no in-process guard that helps —
// which is precisely why the LOCATION had to change. This test exists so the
// claim stays honest if someone later proposes an inode check as the fix.
func TestUnlinkingTheLockFileDefeatsItRegardlessOfAnyCheck(t *testing.T) {
	dir := t.TempDir()

	const id = "0123456789abcdef0123456789abcdef"

	first, err := LockDeployment(id, LockOptions{Dir: dir})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, first)

	if err := os.Remove(first.Path()); err != nil {
		t.Fatalf("remove: %v", err)
	}

	second, err := LockDeployment(id, LockOptions{Dir: dir})
	if err != nil {
		t.Fatalf("unexpected: the lock survived its file being deleted (%v). If this now "+
			"passes, the defence changed and the comment above is stale", err)
	}

	releaseAtEnd(t, second)
}

// A nil lock is safe too, since a caller that failed to obtain one may still
// run its deferred release.
func TestANilLockIsSafeToUse(t *testing.T) {
	t.Parallel()

	var lock *DeploymentLock

	if err := lock.Release(); err != nil {
		t.Errorf("releasing a lock that was never taken: %v", err)
	}

	if lock.Path() != "" {
		t.Error("a lock that was never taken reported a path")
	}

	if lock.Degraded() == "" {
		t.Error("a lock that was never taken claims to be held")
	}
}

// An empty identity is a programming error, not a degradation.
func TestAnEmptyIdentityIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := LockDeployment("", LockOptions{}); err == nil {
		t.Fatal("locked an empty identity, which would collide with every other empty one")
	}
}

// borrowedGroup returns a group this process belongs to that is NOT its primary
// one, skipping the test when there is none.
//
// Skipping rather than falling back to the primary group: a test that quietly
// degrades to comparing a value with itself reports success while checking
// nothing, which is the failure mode this helper exists to prevent.
func borrowedGroup(t *testing.T) int {
	t.Helper()

	groups, err := os.Getgroups()
	if err != nil {
		t.Skipf("cannot read supplementary groups: %v", err)
	}

	for _, gid := range groups {
		if gid != os.Getgid() {
			return gid
		}
	}

	t.Skip("this account has no supplementary group, so a group mismatch cannot be staged")

	return -1
}

// releaseAtEnd drops a lock when the test finishes, and fails if it cannot —
// a lock that will not release is a restart that will not start.
func releaseAtEnd(t *testing.T, lock *DeploymentLock) {
	t.Helper()

	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})
}

// THE WHOLE SCENARIO: a state directory is copied, and the copy cannot run
// alongside the original.
//
// This is what the lock exists for, composed end to end rather than in two
// halves. The copy deliberately KEEPS the original's identity — minting a new
// one would strand every container the original labelled — and its own
// billet.lock is a different inode, so the directory lock lets both start. The
// identity lock is the only thing that notices.
func TestACopiedStateDirectoryCannotRunAlongsideTheOriginal(t *testing.T) {
	useTempCache(t)

	original := t.TempDir()

	id, err := DeploymentID(original)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	// Copy the directory the way an operator taking a backup would.
	copied := t.TempDir()

	raw, err := os.ReadFile(filepath.Join(original, deploymentIDFile))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := os.WriteFile(filepath.Join(copied, deploymentIDFile), raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Both directories lock independently — this is the hole.
	firstDir, err := lockDir(original)
	if err != nil {
		t.Fatalf("lock the original: %v", err)
	}

	t.Cleanup(func() {
		if err := firstDir.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	secondDir, err := lockDir(copied)
	if err != nil {
		t.Fatalf("the COPY could not take its own directory lock, which means this test is "+
			"no longer exercising the situation it was written for: %v", err)
	}

	t.Cleanup(func() {
		if err := secondDir.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	// ...and they carry the same identity, deliberately.
	copiedID, err := DeploymentID(copied)
	if err != nil {
		t.Fatalf("DeploymentID of the copy: %v", err)
	}

	if copiedID != id {
		t.Fatalf("the copy minted a new identity (%q, was %q), which would strand every "+
			"container the original started", copiedID, id)
	}

	// The identity lock is what stops them.
	first, err := LockDeployment(id, LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, first)

	if _, err := LockDeployment(copiedID, LockOptions{}); !errors.Is(err, ErrDeploymentLocked) {
		t.Fatalf("the copy started alongside the original: %v", err)
	}
}

// AN IDENTITY THAT WOULD ESCAPE THE LOCK DIRECTORY IS REFUSED, NOT DEGRADED.
//
// The consequential half. The identity becomes a filename, so a separator in it
// puts the lock somewhere else — and the resulting open failure is
// indistinguishable from a host with nowhere to put a lock, which DEGRADES. So
// the protection would have switched itself off while blaming the cache
// directory. A refusal is the only outcome that cannot be mistaken for working.
func TestAnIdentityThatEscapesTheLockDirectoryIsRefused(t *testing.T) {
	valid := strings.Repeat("a", deploymentIDLen)

	for _, id := range []string{
		"../../../../tmp/escape",
		"a/b",
		valid[:len(valid)-2] + "/x",
		strings.Repeat("A", deploymentIDLen), // uppercase: one identity or two, per filesystem
		valid + "extra",
		valid[:len(valid)-1],
		"deadbeef,label=x",
		"dead=beef",
	} {
		t.Run(id, func(t *testing.T) {
			useTempCache(t)

			lock, err := LockDeployment(id, LockOptions{})
			if err == nil {
				releaseAtEnd(t, lock)

				t.Fatalf("identity %q was accepted, and its lock went to %q", id, lock.Path())
			}

			if lock != nil {
				t.Errorf("a refused identity still produced a lock: degraded=%q", lock.Degraded())
			}

			// Specifically NOT a degradation — that is the failure this test exists
			// for, and it is the silent one.
			if strings.Contains(err.Error(), "cache") {
				t.Errorf("the refusal reads as a cache-directory problem: %v", err)
			}
		})
	}
}

// A HAND-EDITED IDENTITY FILE IS REFUSED AT THE SOURCE.
//
// Not sanitised. A sanitised identity is a DIFFERENT identity from the one
// already written onto running containers, so billet would come up unable to see
// its own compute while believing it could.
func TestADeploymentIDFileBilletWouldNotHaveMintedIsRefused(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"../../etc/passwd",
		"my-deployment",
		strings.Repeat("A", deploymentIDLen),
		"deadbeef",
	} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			if err := os.WriteFile(filepath.Join(dir, deploymentIDFile), []byte(content+"\n"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			id, err := DeploymentID(dir)
			if err == nil {
				t.Fatalf("%q was accepted as an identity (%q), and it would be written onto "+
					"every container this billet starts", content, id)
			}

			// The operator has to be able to act on it.
			if !strings.Contains(err.Error(), deploymentIDFile) {
				t.Errorf("the error does not name the file to fix: %v", err)
			}
		})
	}
}

// The identity billet MINTS passes its own check — otherwise the check above is
// a guarantee that billet cannot start.
func TestAMintedIdentityIsAccepted(t *testing.T) {
	t.Parallel()

	id, err := DeploymentID(t.TempDir())
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	if err := validDeploymentID(id); err != nil {
		t.Fatalf("billet minted an identity it refuses to accept: %v", err)
	}
}

// A WORLD-WRITABLE LOCK DIRECTORY IS REFUSED.
//
// The interaction between two fixes, and the reason this check exists at all.
// Failing closed made an untrusted lock directory WORSE rather than better:
// before, a local user who could write there could defeat the lock by unlinking
// the file; now they can hold the filename and keep billet from ever starting.
// That is exactly the denial of service that ruled out /tmp in the first place,
// so the cost of failing closed is paid here.
func TestAWorldWritableLockDirectoryIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{Dir: dir})
	if err == nil {
		t.Fatal("billet locked in a directory any local user can write, where they could " +
			"hold the filename and keep it from ever starting")
	}

	if !strings.Contains(err.Error(), "world-writable") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// MkdirAll's mode applies only to components it CREATES, so an existing loose
// directory keeps whatever permissions it had. state.Open already knew this and
// tightens its own state directory; this did not, one screen away.
func TestAnExistingLooseDefaultDirectoryIsTightened(t *testing.T) {
	home := t.TempDir()

	t.Setenv("XDG_STATE_HOME", home)
	t.Setenv("HOME", home)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	dir := filepath.Dir(lock.Path())

	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Loosen it the way an umask, an earlier billet, or an operator would.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	again, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, again)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("billet's own lock directory stayed mode %o; MkdirAll does not tighten an "+
			"existing directory and nothing else did either", perm)
	}
}

// A SHARED DIRECTORY PRODUCES A FILE THE OTHER USER CAN ACTUALLY OPEN.
//
// The whole documented purpose of server.lock_dir is a setgid directory shared
// by a service account and an operator who both reach one docker socket. With
// the lock file created 0600, the first process to run made it permanently
// unopenable by the other — not merely while the lock was held, but ever after —
// so the advertised use case failed forever and its only escape was to turn the
// protection off.
func TestASharedLockDirectoryProducesAGroupOpenableLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// A GROUP THAT IS NOT THIS PROCESS'S PRIMARY ONE, or the assertion below is
	// true by construction and can never fail. MEASURED, not assumed: a t.TempDir
	// comes out owned by the primary group, so comparing the lock file's group
	// against the directory's compares 20 with 20 and would pass just as happily
	// against the defect it exists to catch.
	//
	// A supplemental group is exactly the situation the defect arises from — the
	// service account reaching the shared directory through one — so borrowing
	// one here makes the test discriminate on Linux, where a non-setgid directory
	// really does hand a new file the creator's primary group. On darwin it still
	// cannot fail, because BSD gives a new file its directory's group whether or
	// not setgid is set; the Linux CI runner is where this earns its keep.
	if err := os.Chown(dir, -1, borrowedGroup(t)); err != nil {
		t.Skipf("cannot give the directory a non-primary group: %v", err)
	}

	// SETGID group-writable: the shape an administrator provisions for two
	// accounts, and the only shape that actually works. See the sibling test.
	if err := os.Chmod(dir, os.ModeSetgid|0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{Dir: dir})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, lock)

	info, err := os.Stat(lock.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	perm := info.Mode().Perm()

	if perm&0o060 != 0o060 {
		t.Errorf("the lock file is mode %o in a group-shared directory, so the other account "+
			"in that group can never open it and server.lock_dir does not do what it says", perm)
	}

	if perm&0o007 != 0 {
		t.Errorf("the lock file is world-accessible (%o)", perm)
	}

	// THE BITS ARE ONLY HALF THE QUESTION. 0660 says "a group may open this", not
	// WHICH group — and the group that matters is the directory's, because that is
	// the one both accounts were put in. A file carrying the creator's primary
	// group instead passes every mode assertion above and is still unopenable by
	// the account it was widened for.
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}

	got, gotOK := fileGID(info)

	want, wantOK := fileGID(dirInfo)
	if !gotOK || !wantOK {
		t.Fatal("the filesystem did not report group owners, so this proves nothing")
	}

	if got != want {
		t.Errorf("the lock file belongs to group %d but the shared directory is group %d, so "+
			"the other account in that group cannot open it", got, want)
	}
}

// THE CHECK ITSELF, exercised — which the test above still does not do.
//
// Setting setgid BEFORE the lock file is created means the kernel supplies the
// directory's group and billet's comparison is never the reason it matches.
// Deleting the production comparison outright left that test green. The only way
// to reach the check is to present it with a file that ALREADY has the wrong
// group, which is exactly the real case: a lock file left by an earlier billet
// in a directory that was not setgid at the time.
func TestAnExistingLockFileWithTheWrongGroupIsRefused(t *testing.T) {
	t.Parallel()

	const id = "0123456789abcdef0123456789abcdef"

	dir := t.TempDir()
	borrowed := borrowedGroup(t)

	// The lock file is created FIRST, while the directory is still plain, so it
	// takes this process's primary group.
	lockPath := filepath.Join(dir, "deployment-"+id+".lock")

	if err := os.WriteFile(lockPath, nil, 0o660); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Only now is the directory shared, with a DIFFERENT group.
	if err := os.Chown(dir, -1, borrowed); err != nil {
		t.Skipf("cannot give the directory a non-primary group: %v", err)
	}

	if err := os.Chmod(dir, os.ModeSetgid|0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	fi, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if gid, ok := fileGID(fi); !ok || gid == uint32(borrowed) {
		t.Skipf("this platform gave the pre-created file the directory's group (%d), so a "+
			"mismatch cannot be staged here", gid)
	}

	_, err = LockDeployment(id, LockOptions{Dir: dir})
	if err == nil {
		t.Fatal("billet accepted a lock file whose group the other account is not in, so the " +
			"lock excludes nobody")
	}

	if !strings.Contains(err.Error(), "belongs to group") {
		t.Errorf("the refusal is not about the group: %v", err)
	}

	// AND IT DOES NOT CALL IT STALE. The check now runs after the flock, so the
	// advice can safely be about a file nothing holds — but it must still tell the
	// operator to stop every billet first, because another one may hold a
	// DIFFERENT lock in the same directory.
	if !strings.Contains(err.Error(), "Stop every billet") {
		t.Errorf("the refusal does not tell the operator to stop billet before repairing: %v", err)
	}
}

// CONTENTION IS DISCOVERED BEFORE METADATA IS JUDGED.
//
// The ordering, proved rather than asserted in a comment. Metadata used to be
// validated before the flock, so a group mismatch on a lock somebody was HOLDING
// produced advice to delete it — and after the delete, the newcomer creates a
// fresh inode while the holder keeps the old one and neither excludes the other.
// Whatever else is wrong with the file, "someone is using it" has to win.
func TestContentionIsReportedBeforeAGroupComplaint(t *testing.T) {
	t.Parallel()

	const id = "0123456789abcdef0123456789abcdef"

	dir := t.TempDir()
	borrowed := borrowedGroup(t)

	// A lock file with the WRONG group, exactly as in the test above...
	lockPath := filepath.Join(dir, "deployment-"+id+".lock")

	if err := os.WriteFile(lockPath, nil, 0o660); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Chown(dir, -1, borrowed); err != nil {
		t.Skipf("cannot give the directory a non-primary group: %v", err)
	}

	if err := os.Chmod(dir, os.ModeSetgid|0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if gid, ok := fileGID(mustStat(t, lockPath)); !ok || gid == uint32(borrowed) {
		t.Skip("this platform gave the pre-created file the directory's group, so a mismatch " +
			"cannot be staged here")
	}

	// ...and somebody is holding it. A separate open file description, so the
	// flock genuinely conflicts even from this process.
	holder, err := lockFile(lockPath, false)
	if err != nil {
		t.Fatalf("hold the lock: %v", err)
	}

	t.Cleanup(func() {
		if err := holder.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	_, err = LockDeployment(id, LockOptions{Dir: dir})
	if err == nil {
		t.Fatal("a held lock was taken a second time")
	}

	if !errors.Is(err, ErrDeploymentLocked) {
		t.Fatalf("billet judged the file's group before noticing somebody holds it, so it "+
			"would advise repairing or deleting a live lock: %v", err)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return info
}

// A GROUP-WRITABLE DIRECTORY THAT IS NOT SETGID IS REFUSED, with the command to
// fix it.
//
// Group-writable proves somebody intended sharing; setgid is what decides WHO
// gets it. Without setgid a new file takes the creator's PRIMARY group, so a
// service account sharing through a supplemental group produces a lock file with
// every permission bit the checks ask for that the operator still cannot open —
// the same silent-success shape as the umask defect, one layer up.
func TestAGroupWritableDirectoryWithoutSetgidIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{Dir: dir})
	if err == nil {
		t.Fatal("a group-writable directory was accepted as shared without setgid, so the " +
			"lock file can carry a group the other account is not in")
	}

	// BOTH REMEDIES, because only one of them was asserted and it was the wrong
	// one for the commoner case. A single-user operator with their own 0770
	// directory is not sharing with anybody, and `chmod g+s` sends them to build a
	// cross-account arrangement they do not want. Asserting only "g+s" let the
	// `g-w` half be deleted with every test still green.
	for _, want := range []string{"g+s", "g-w"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not offer %q as a way out: %v", want, err)
		}
	}
}

// A SHARED DIRECTORY MUST BE READABLE, and the refusal has to say why.
//
// billet opens the lock directory to validate it, so the 2730 drop-box shape —
// write and traverse but no listing — is refused even though openat of a known
// filename would not need read. That is a deliberate contract rather than an
// oversight (a search-only descriptor is O_PATH on Linux, absent on darwin, and
// cannot be fchmod'd), so the one thing it must not do is fail with a bare
// permission error that sends an operator looking for the wrong problem.
func TestAnUnreadableSharedDirectorySaysItNeedsRead(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks, so this cannot be staged")
	}

	dir := t.TempDir()

	t.Cleanup(func() {
		// Restored, or t.TempDir cannot clean itself up.
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore mode: %v", err)
		}
	})

	// READ REMOVED FROM THE OWNER, not from the group. The shape this models is a
	// 2730 directory seen by the OTHER account, which a single-user test cannot
	// stage: at 2730 the owner still has rwx, so the open succeeds and the case
	// never arises — measured on darwin, where exactly that happened and this test
	// skipped itself into proving nothing. Taking read off the owner reproduces
	// the same EACCES from the same line.
	if err := os.Chmod(dir, os.ModeSetgid|0o370); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{Dir: dir})
	if err == nil {
		t.Fatal("billet opened a directory it cannot read, so the contract this message " +
			"describes is not the one being enforced")
	}

	if !strings.Contains(err.Error(), "2770") {
		t.Errorf("the refusal does not name the mode that works: %v", err)
	}
}

// BILLET DOES NOT DISMANTLE A SHARE IT DID NOT MAKE.
//
// The default path is tightened to 0700 when it is loose, which is right for a
// directory nobody declared shared. A SETGID one was declared shared, and
// stripping that bit on the evidence that this invocation happened to omit
// lock_dir would lock the other account out of a collision domain an
// administrator built on purpose.
func TestASetgidDefaultDirectoryIsRefusedRatherThanTightened(t *testing.T) {
	home := t.TempDir()

	t.Setenv("XDG_STATE_HOME", home)
	t.Setenv("HOME", home)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	dir := filepath.Dir(lock.Path())

	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// An administrator shares it deliberately.
	if err := os.Chmod(dir, os.ModeSetgid|0o770); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{}); err == nil {
		t.Fatal("billet silently took over a deliberately shared directory")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if info.Mode()&os.ModeSetgid == 0 {
		t.Error("billet stripped the setgid bit off a directory an administrator shared, " +
			"locking out every other account that used it")
	}
}

// The per-user default stays 0600 — the widening is for a shared directory only,
// not a blanket loosening.
func TestTheDefaultLockFileStaysPrivate(t *testing.T) {
	useTempCache(t)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, lock)

	info, err := os.Stat(lock.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the default lock file is mode %o, want 600", perm)
	}
}

// A relative lock_dir puts two billets sharing one config into different
// collision domains while logging the same string — so the diagnostic that
// exists to expose a mismatch would conceal this one.
func TestARelativeLockDirectoryIsRefused(t *testing.T) {
	t.Parallel()

	_, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{Dir: "locks"})
	if err == nil {
		t.Fatal("a relative lock directory was accepted, so two billets started from " +
			"different working directories would not collide")
	}

	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// WHICH LOCATION WINS, asked with the two candidates set to DIFFERENT values.
//
// The rest of the suite points XDG_STATE_HOME and HOME at one directory, which
// is convenient and proves nothing about precedence: every ordering passes. The
// ordering is not cosmetic — it was a real defect, where the comment promised
// Application Support on darwin while the code consulted XDG on every platform,
// so setting that variable moved a macOS lock somewhere the docs said it could
// not go.
func TestTheDefaultLocationFollowsThePlatformConvention(t *testing.T) {
	xdg := t.TempDir()
	home := t.TempDir()

	t.Setenv("XDG_STATE_HOME", xdg)
	t.Setenv("HOME", home)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{})
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, lock)

	if runtime.GOOS == "darwin" {
		if !strings.HasPrefix(lock.Path(), home) {
			t.Errorf("on darwin the lock went to %q, but Apple's convention is Application "+
				"Support under HOME (%q) and XDG must not override it", lock.Path(), home)
		}

		return
	}

	if !strings.HasPrefix(lock.Path(), xdg) {
		t.Errorf("XDG_STATE_HOME was set to %q and the lock went to %q instead",
			xdg, lock.Path())
	}
}

// A symlinked lock directory is somebody redirecting where the lock lands.
func TestASymlinkedLockDirectoryIsRefused(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "locks")

	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := LockDeployment("0123456789abcdef0123456789abcdef", LockOptions{Dir: link}); err == nil {
		t.Fatal("locked through a symlink, so where the lock lands can be redirected")
	}
}

// A SYMLINK AT THE LOCK FILE ITSELF is a different attack from a symlinked
// directory, and it is the one O_NOFOLLOW answers.
//
// The directory check cannot cover this: the directory is a perfectly ordinary
// one, and only the final component — whose name is predictable, since it is
// derived from the deployment identity — has been replaced. Following it would
// write and lock somewhere else entirely while reporting the expected path.
// TWO SYMLINKS, and only the second one tests what this is named for.
//
// The first version pointed the lock's name at an ABSOLUTE path outside the
// directory, which os.Root rejected as an escape — so it passed while saying
// nothing about no-follow, and it kept passing when os.Root silently followed an
// INTERNAL link. Measured afterwards: a relative `link.lock -> real.lock` inside
// the directory really was opened as its target.
//
// So the internal case is the one that matters, and both stay: the escape is
// worth keeping honest too, and having them side by side is what makes the
// distinction visible to whoever reads this next.
func TestASymlinkedLockFileIsRefused(t *testing.T) {
	t.Parallel()

	const id = "0123456789abcdef0123456789abcdef"

	name := "deployment-" + id + ".lock"

	t.Run("pointing outside the directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		elsewhere := filepath.Join(t.TempDir(), "real.lock")

		if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := os.Symlink(elsewhere, filepath.Join(dir, name)); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		if _, err := LockDeployment(id, LockOptions{Dir: dir}); err == nil {
			t.Fatalf("the lock followed a symlink at its own path and locked %s instead", elsewhere)
		}
	})

	// The one a path-confinement API does NOT catch, because nothing escapes.
	t.Run("pointing inside the directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		if err := os.WriteFile(filepath.Join(dir, "real.lock"), nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := os.Symlink("real.lock", filepath.Join(dir, name)); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		if _, err := LockDeployment(id, LockOptions{Dir: dir}); err == nil {
			t.Fatal("the lock followed a symlink to another file INSIDE its own directory, so " +
				"two identities can be redirected onto one inode and neither excludes the other")
		}
	})
}
