package state

import (
	"errors"
	"os"
	"path/filepath"
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

	// Group-writable: the shape an administrator provisions for two accounts.
	if err := os.Chmod(dir, 0o770); err != nil {
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
func TestASymlinkedLockFileIsRefused(t *testing.T) {
	t.Parallel()

	const id = "0123456789abcdef0123456789abcdef"

	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "real.lock")

	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Symlink(elsewhere, filepath.Join(dir, "deployment-"+id+".lock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := LockDeployment(id, LockOptions{Dir: dir}); err == nil {
		t.Fatalf("the lock followed a symlink at its own path and locked %s instead", elsewhere)
	}
}
