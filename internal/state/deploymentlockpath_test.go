package state

import (
	"path/filepath"
	"testing"
)

// THE PATH A UNIT'S DECLARATION REPORTS IS WHERE THE LOCK IS (#174), for a
// configured directory and for the per-user default alike: DeploymentLockPath
// and LockDeployment resolve the directory through one function.
func TestDeploymentLockPathIsWhereLockDeploymentLocks(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"

	for _, dir := range []string{"", filepath.Join(t.TempDir(), "locks")} {
		useTempCache(t)

		want, err := DeploymentLockPath(dir, id)
		if err != nil {
			t.Fatal(err)
		}

		lock, err := LockDeployment(id, LockOptions{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}

		if got := lock.Path(); got != want {
			t.Errorf("dir %q: LockDeployment locked %s, and DeploymentLockPath reports %s", dir, got, want)
		}

		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
	}
}
