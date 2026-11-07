//go:build unix

package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// THE PROBE ANSWERS THREE WAYS AND CREATES NOTHING: a lock file that is not there is
// not held and is not made, a held one is held and a released one is not, and a path
// that is not a lock file is an error, never "not held".
func TestLockHeldAnswersWithoutCreatingAnything(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := DirectoryLockPath(dir)
	if held, err := LockHeld(path); err != nil || held {
		t.Fatalf("a lock file that is not there: held %v, %v", held, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the probe created %s: %v", path, err)
	}

	lock, err := LockStateDir(dir)
	if err != nil {
		t.Fatalf("LockStateDir: %v", err)
	}
	if held, err := LockHeld(path); err != nil || !held {
		t.Fatalf("a held lock: held %v, %v", held, err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if held, err := LockHeld(path); err != nil || held {
		t.Fatalf("a released lock: held %v, %v", held, err)
	}
	// AND THE PROBE DID NOT KEEP IT: the holder takes it again.
	again, err := LockStateDir(dir)
	if err != nil {
		t.Fatalf("the lock after a probe: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}

	notALock := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(notALock, 0o700); err != nil {
		t.Fatal(err)
	}
	if held, err := LockHeld(notALock); err == nil {
		t.Fatalf("a directory at the lock's path answered held %v and no error", held)
	}
}
