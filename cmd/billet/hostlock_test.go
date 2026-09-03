package main

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
	previous := hostLockDir
	t.Cleanup(func() { hostLockDir = previous })

	hostLockDir = t.TempDir()

	first, err := takeHostLock()
	if err != nil {
		t.Fatalf("the first command could not take the lock: %v", err)
	}

	_, err = takeHostLock()
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
	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := takeHostLock()
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}

	if err := second.release(); err != nil {
		t.Fatalf("release the second: %v", err)
	}
}
