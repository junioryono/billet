package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTempCache points the host-wide lock at a directory this test owns, so
// tests do not fight each other or the developer's real cache.
func useTempCache(t *testing.T) {
	t.Helper()

	// XDG_CACHE_HOME on Linux, HOME on darwin — set both so the same test works
	// on either.
	dir := t.TempDir()

	t.Setenv("XDG_CACHE_HOME", dir)
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

	first, err := LockDeployment(id)
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	if first.Path() == "" {
		t.Fatal("no lock was taken, so this test proves nothing")
	}

	releaseAtEnd(t, first)

	_, err = LockDeployment(id)
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

	first, err := LockDeployment(id)
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, first)

	_, err = LockDeployment(id)
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

	a, err := LockDeployment("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, a)

	b, err := LockDeployment("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
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

	first, err := LockDeployment(id)
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := LockDeployment(id)
	if err != nil {
		t.Fatalf("the identity stayed locked after release, so a restart would fail: %v", err)
	}

	releaseAtEnd(t, second)
}

// The lock file is private, because a world-writable one could be pre-created
// and held by any local user to keep billet from ever starting.
func TestTheLockFileIsPrivate(t *testing.T) {
	useTempCache(t)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef")
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

// A DEGRADED lock is safe to use and says why it is not a lock.
//
// The downgrade is deliberate: a host with no usable cache directory is a
// legitimate single deployment far more often than it is two, and refusing to
// boot there would trade a hazard nobody has hit for an outage everybody would.
// What it must not do is look like a held lock — the caller has to be able to
// tell an operator that a protection is absent, and with a reason.
func TestADegradedLockSaysWhy(t *testing.T) {
	// A cache directory that cannot be created, because its parent is a FILE.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")

	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("XDG_CACHE_HOME", blocker)
	t.Setenv("HOME", blocker)

	lock, err := LockDeployment("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("a host that cannot place a lock should degrade, not fail: %v", err)
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

	if _, err := LockDeployment(""); err == nil {
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
	first, err := LockDeployment(id)
	if err != nil {
		t.Fatalf("LockDeployment: %v", err)
	}

	releaseAtEnd(t, first)

	if _, err := LockDeployment(copiedID); !errors.Is(err, ErrDeploymentLocked) {
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

			lock, err := LockDeployment(id)
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
