package wirecert

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

// linuxHost stands a Linux host with its own state root in, so the global
// exclusion's rules apply to this test whatever platform runs it.
func linuxHost(t *testing.T) {
	t.Helper()

	oldRoot, oldPlatform := retirement.Root, retirement.Platform
	retirement.Root, retirement.Platform = t.TempDir(), "linux"

	t.Cleanup(func() { retirement.Root, retirement.Platform = oldRoot, oldPlatform })
}

func flocked(t *testing.T, path string) bool {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}

		t.Fatal(err)
	}

	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	return false
}

// A LIBRARY CALLER CREATES A FRESH DIRECTORY ONLY UNDER THE INIT LOCK: the
// lock beside the directory is held from before the four absences are
// re-established until the authority lock is released, so an installer's
// preparation and this creation never interleave, and a cached classification
// authorises nothing.
func TestTheLibraryLockCreatesAFreshDirectoryOnlyUnderTheInitLock(t *testing.T) {
	linuxHost(t)

	dir := filepath.Join(t.TempDir(), "server")

	lock, err := LockAuthority(t.Context(), dir)
	if err != nil {
		t.Fatalf("LockAuthority on a fresh host: %v", err)
	}

	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the directory must be created 0700, got %v %v", info, err)
	}

	if !flocked(t, retirement.InitLockPath(dir)) {
		t.Fatal("the init lock beside the directory must be held while the authority lock is")
	}

	if !flocked(t, AuthorityLockPath(dir)) {
		t.Fatal("the inner lock must be held")
	}

	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	if flocked(t, retirement.InitLockPath(dir)) || flocked(t, AuthorityLockPath(dir)) {
		t.Fatal("release must drop the init lock with the inner one")
	}

	// A PREPARED HOST'S ABSENT DIRECTORY IS REFUSED, not created: a record and a
	// global lock exist, so the directory's absence is a retirement's move.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	acct := retirement.ServiceAccount{User: "billet", UID: os.Getuid(), Group: "billet", GID: os.Getgid()}
	if acct.UID == 0 {
		acct.UID, acct.GID = 1000, 1000
	}

	if err := retirement.WriteServiceAccount(acct); err != nil {
		t.Fatal(err)
	}

	hold, err := retirement.Acquire(t.Context(), retirement.AcquireOptions{Privileged: true, Account: &acct})
	if err != nil {
		t.Fatal(err)
	}

	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	if _, err := LockAuthority(t.Context(), dir); err == nil {
		t.Fatal("a prepared host's absent directory must refuse")
	}

	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refusal must create nothing")
	}

	// A resolution never grants creation on Linux: the exclusion a command
	// resolves for an absent legacy directory refuses in the inner lock.
	if err := os.Remove(retirement.ServiceAccountPath()); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(retirement.GlobalLockPath()); err != nil {
		t.Fatal(err)
	}

	ex, err := ResolveExclusion(t.Context(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	if ex.Create {
		t.Fatal("a resolution must not grant creation on Linux; only the init-locked paths create")
	}

	if _, err := LockAuthorityWith(t.Context(), dir, ex); err == nil {
		t.Fatal("an absent directory under a resolution that may not create must refuse")
	}

	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refusal must create nothing")
	}
}
