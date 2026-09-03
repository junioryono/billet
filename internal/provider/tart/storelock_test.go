package tart

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newLockProvider builds a provider that shares a store with its siblings and
// nothing else. No stub binary: lockStore never invokes tart.
func newLockProvider(t *testing.T, home string) *Provider {
	t.Helper()

	p, err := New(testOwner, WithHome(home))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	p.storeLockRetry = time.Millisecond

	return p
}

// A SECOND HOLDER IS EXCLUDED, which is the whole mechanism.
//
// Two Providers rather than two calls on one, because two billet processes is
// what this models — and because a second call on one Provider would pass even
// if the descriptor were cached on it, which is precisely the implementation
// this test must be able to reject. A cached descriptor re-flocked succeeds:
// measured, and the reason lockStore opens per acquisition.
func TestTheStoreLockExcludesASecondHolder(t *testing.T) {
	home := t.TempDir()
	p := newLockProvider(t, home)

	unlock, err := p.lockStore(t.Context(), "hold the store")
	if err != nil {
		t.Fatalf("lockStore: %v", err)
	}

	defer unlock()

	q := newLockProvider(t, home)
	q.storeLockWindow = 50 * time.Millisecond

	release, err := q.lockStore(t.Context(), "take it as well")
	if err == nil {
		release()
		t.Fatal("two holders were inside the store lock at once, so a rename and a delete " +
			"can still interleave")
	}

	// THE SPECIFIC REFUSAL. "Some error" would be satisfied by a failure to open
	// the file at all, which is the opposite situation and needs the opposite fix.
	if !strings.Contains(err.Error(), "stayed held for more than") {
		t.Errorf("lockStore = %v, want a contention refusal", err)
	}

	// AND IT IS DISTINGUISHABLE WITHOUT READING THE PROSE, because CheckHost acts
	// on the difference: contention proves the lock works, while failing to place
	// one proves the opposite, and a caller that cannot tell them apart has to
	// treat a healthy busy host as broken.
	if !errors.Is(err, errStoreLockBusy) {
		t.Errorf("lockStore = %v, want it to carry errStoreLockBusy", err)
	}

	// And it names the path, because which file two billets contend on is
	// decided by TART_HOME and is the first thing to compare when they do not.
	if !strings.Contains(err.Error(), p.storeLockPath()) {
		t.Errorf("lockStore = %v, want it to name %s", err, p.storeLockPath())
	}
}

// A RELEASED LOCK IS TAKEABLE AGAIN. Without this the first teardown of the day
// would wedge every later one, which is a worse outage than the race the lock
// closes.
func TestTheStoreLockIsReleasedAndReacquirable(t *testing.T) {
	home := t.TempDir()
	p := newLockProvider(t, home)
	p.storeLockWindow = 250 * time.Millisecond

	unlock, err := p.lockStore(t.Context(), "hold the store")
	if err != nil {
		t.Fatalf("lockStore: %v", err)
	}

	unlock()

	again, err := newLockProvider(t, home).lockStore(t.Context(), "take it after the release")
	if err != nil {
		t.Fatalf("the store lock was not released: %v", err)
	}

	again()
}

// A CALLER THAT RAN OUT IS A DIFFERENT FACT from billet's own window expiring,
// and reporting one as the other sends an operator hunting for a second billet
// that is not there.
func TestTheStoreLockTellsACallersDeadlineFromItsOwn(t *testing.T) {
	home := t.TempDir()
	p := newLockProvider(t, home)

	unlock, err := p.lockStore(t.Context(), "hold the store")
	if err != nil {
		t.Fatalf("lockStore: %v", err)
	}

	defer unlock()

	q := newLockProvider(t, home)
	q.storeLockWindow = time.Minute

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	release, err := q.lockStore(ctx, "take it as well")
	if err == nil {
		release()
		t.Fatal("a second holder took the lock")
	}

	if !strings.Contains(err.Error(), "waiting for the tart store lock") {
		t.Errorf("lockStore = %v, want the caller's deadline attributed to the caller", err)
	}

	// The attribution changes; the FACT does not. Whose clock ran out says
	// nothing about why the lock could not be taken, so this branch carries the
	// sentinel too — CheckHost passes a short deadline and reaches exactly here.
	if !errors.Is(err, errStoreLockBusy) {
		t.Errorf("lockStore = %v, want it to carry errStoreLockBusy", err)
	}
}

// A SYMLINK AT THE LOCK PATH IS REFUSED, because following one would flock an
// inode no other billet flocks: a lock that excludes nothing while looking held.
func TestTheStoreLockRefusesASymlinkedPath(t *testing.T) {
	home := t.TempDir()
	p := newLockProvider(t, home)

	if err := os.MkdirAll(filepath.Dir(p.storeLockPath()), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	elsewhere := filepath.Join(t.TempDir(), "elsewhere.lock")
	if err := os.Symlink(elsewhere, p.storeLockPath()); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	release, err := p.lockStore(t.Context(), "take a redirected lock")
	if err == nil {
		release()
		t.Fatal("the store lock followed a symlink, so it would be taken on an inode no " +
			"other billet locks")
	}

	if _, statErr := os.Lstat(elsewhere); statErr == nil {
		t.Error("the symlink target was created, so the open followed the link")
	}
}

// The child of the process test below: it takes the lock, announces it, and
// blocks until the parent kills it or closes its stdin. Not a test.
func TestStoreLockHelperProcess(t *testing.T) {
	home := os.Getenv(storeLockHelperEnv)
	if home == "" {
		t.Skip("not the helper process")
	}

	p, err := New(testOwner, WithHome(home))
	if err != nil {
		t.Fatalf("helper: New: %v", err)
	}

	unlock, err := p.lockStore(t.Context(), "hold the store from another process")
	if err != nil {
		t.Fatalf("helper could not take the store lock: %v", err)
	}

	defer unlock()

	if _, err := os.Stdout.WriteString(storeLockHelperReady + "\n"); err != nil {
		t.Fatalf("helper could not announce: %v", err)
	}

	// Reading rather than sleeping, so the child cannot outlive a parent that
	// failed early. Both the error and the EOF are expected: the read waits, it
	// does not deliver.
	if _, err := os.Stdin.Read(make([]byte, 1)); err != nil {
		return
	}
}

const (
	storeLockHelperEnv   = "BILLET_TEST_STORE_LOCK_HOME"
	storeLockHelperReady = "HELPER-HOLDS-THE-STORE-LOCK"
)

// A REAL SECOND PROCESS IS REFUSED, AND THE LOCK DIES WITH IT.
//
// Every other test in this file calls lockStore twice inside ONE process, and
// that is a weaker claim than the names suggest: a package-level mutex, or a
// lock file named after the pid, satisfies all of them while two billets rename
// and delete over each other. Crossing the process boundary is the entire point
// of the mechanism, so it gets the test that crosses it — the same shape
// internal/state uses for the deployment lock, and for the same reason.
//
// The other half is that the lock must not outlive its holder: a node SIGKILLed
// mid-teardown must not leave a store no billet can ever mutate again.
func TestARealSecondProcessIsRefusedTheStoreLock(t *testing.T) {
	home := t.TempDir()

	helper := exec.CommandContext(t.Context(),
		os.Args[0], "-test.run=TestStoreLockHelperProcess", "-test.v")
	helper.Env = append(os.Environ(), storeLockHelperEnv+"="+home)

	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}

	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}

	if err := helper.Start(); err != nil {
		t.Fatalf("start the helper: %v", err)
	}

	// Reaped whichever assertion fails first. Every error here is expected once
	// the test has killed the helper itself, so this reports rather than fails.
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Logf("closing the helper's stdin: %v", err)
		}

		if err := helper.Process.Kill(); err != nil {
			t.Logf("killing the helper: %v", err)
		}

		if err := helper.Wait(); err != nil {
			t.Logf("reaping the helper: %v", err)
		}
	})

	// WAITING FOR THE ANNOUNCEMENT rather than sleeping. A sleep long enough to
	// be reliable is slow, and one short enough to be fast passes when the helper
	// never took the lock at all — the vacuous result this file exists to rule
	// out.
	ready := make(chan bool, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), storeLockHelperReady) {
				ready <- true

				return
			}
		}

		ready <- false
	}()

	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the helper exited without taking the lock, so this test proves nothing")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the helper never announced the lock")
	}

	p := newLockProvider(t, home)
	p.storeLockWindow = 100 * time.Millisecond

	release, err := p.lockStore(t.Context(), "take a lock another process holds")
	if err == nil {
		release()
		t.Fatal("a second PROCESS took the store lock, so two billets can rename and delete " +
			"over each other on one Mac")
	}

	// KILLED WITH STDIN STILL OPEN, and the order is the point. Closing first
	// lets the helper read EOF, return, and run its deferred release on the way
	// out — after which reacquisition succeeds because it let go politely rather
	// than because the kernel took the lock from a corpse.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill the helper: %v", err)
	}

	// Reaping before the assertion matters on its own: without it the kernel may
	// not have dropped the flock yet and the test would race its own subject.
	err = helper.Wait()
	if err == nil {
		t.Fatal("the helper exited cleanly despite being SIGKILLed, so it released the lock " +
			"on its way out — which is not the property under test")
	}

	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("the helper did not exit on a signal: %v", err)
	}

	if status, ok := exit.Sys().(syscall.WaitStatus); ok && !status.Signaled() {
		t.Fatalf("the helper exited on its own (%v) rather than being killed, so nothing here "+
			"shows the kernel releasing a HELD lock", exit)
	}

	after, err := p.lockStore(t.Context(), "take it after the holder was killed")
	if err != nil {
		t.Fatalf("the store stayed locked after its holder was SIGKILLed, so a node killed "+
			"mid-teardown would wedge every later launch and destroy: %v", err)
	}

	after()
}
