package hostauthority

import (
	"strings"
	"testing"
)

// THE LOCK IS EXCLUSIVE, and this is the one place it is exercised against a
// real directory — every command test stubs it, because it lives somewhere only
// root can create.
//
// NOT PARALLEL: it moves a package variable.
func TestTheLifecycleLockAdmitsOneCommandAtATime(t *testing.T) {
	previous := LockDir
	t.Cleanup(func() { LockDir = previous })

	LockDir = t.TempDir()

	first, err := TakeLifecycleLock()
	if err != nil {
		t.Fatalf("the first command could not take the lock: %v", err)
	}

	_, err = TakeLifecycleLock()
	if err == nil {
		t.Fatal("two lifecycle commands took the lock at once; one can start the services " +
			"the other has just proved idle and is about to stop")
	}

	// NAMED, because the operator's next question is what to wait for.
	if !strings.Contains(err.Error(), "already running on this machine") {
		t.Errorf("the refusal does not say what is holding it: %v", err)
	}

	// AND IT IS RELEASED, or the first crash on a host would need a reboot to
	// clear.
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := TakeLifecycleLock()
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}

	if err := second.Release(); err != nil {
		t.Fatalf("release the second: %v", err)
	}
}
