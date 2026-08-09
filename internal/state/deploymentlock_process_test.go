package state

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A REAL SECOND PROCESS, because every other contention test in this package
// calls LockDeployment twice inside ONE process.
//
// That is a weaker claim than the names suggest, and the gap is not theoretical:
// an implementation backed by a package-level mutex, or one that put the PID in
// the lock's filename, would satisfy every one of those tests while letting two
// billets start against the same docker daemon. The whole point of the lock is
// the boundary those tests never cross.
//
// The helper is this same test binary re-executed — the only way to reach an
// internal package from another process, and the reason an earlier attempt at
// this with a standalone script failed outright.
const (
	lockHelperEnv   = "BILLET_TEST_LOCK_HELPER_DIR"
	lockHelperIDEnv = "BILLET_TEST_LOCK_HELPER_ID"
	lockHelperReady = "HELPER-HOLDS-THE-LOCK"
)

// TestLockHelperProcess is not a test. It is the child: it takes the lock,
// announces it, and then blocks until the parent kills it or closes its stdin.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		t.Skip("not the helper process")
	}

	lock, err := LockDeployment(os.Getenv(lockHelperIDEnv), LockOptions{Dir: dir})
	if err != nil {
		t.Fatalf("helper could not take the lock: %v", err)
	}

	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("helper could not release: %v", err)
		}
	}()

	if _, err := os.Stdout.WriteString(lockHelperReady + "\n"); err != nil {
		t.Fatalf("helper could not announce: %v", err)
	}

	// Blocks until the parent closes the pipe or kills us. Reading rather than
	// sleeping so the child cannot outlive a parent that failed early. Both the
	// error and the EOF are expected outcomes here — the read exists to wait, not
	// to deliver anything.
	if _, err := os.Stdin.Read(make([]byte, 1)); err != nil {
		return
	}
}

// A SECOND PROCESS IS REFUSED, AND THE LOCK DIES WITH IT.
//
// Two properties in one test because they are two halves of the same promise: a
// lock that does not exclude another process is useless, and a lock that
// outlives its holder is worse than useless — every restart after a crash would
// be refused, turning the protection into an outage.
func TestARealSecondProcessIsRefusedAndTheLockDiesWithIt(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"

	dir := t.TempDir()

	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestLockHelperProcess", "-test.v")
	helper.Env = append(os.Environ(), lockHelperEnv+"="+dir, lockHelperIDEnv+"="+id)

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

	// Reaped no matter which assertion below fails first. Every error here is
	// expected once the test has already killed the helper itself, so this
	// reports nothing — its job is to make sure no child outlives the run.
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
	// be reliable is a slow test, and a sleep short enough to be fast is a test
	// that passes when the helper never took the lock at all — which is exactly
	// the vacuous result this file exists to rule out.
	ready := make(chan bool, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), lockHelperReady) {
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

	// The property: a different process cannot take the same identity.
	if _, err := LockDeployment(id, LockOptions{Dir: dir}); !errors.Is(err, ErrDeploymentLocked) {
		t.Fatalf("a second PROCESS took an identity another process holds: %v", err)
	}

	// And the other half: when that process dies, the kernel drops the lock.
	//
	// KILLED WITH STDIN STILL OPEN, and the order is the whole point. Closing
	// first lets the helper read EOF, return, and run its deferred Release on the
	// way out — after which reacquisition succeeds because the helper let go
	// politely, not because the kernel took the lock from a corpse. The test
	// would pass while proving the opposite of its name, and would flake whenever
	// the helper finished exiting before the Kill landed.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill the helper: %v", err)
	}

	// A SIGKILLed process always reports an error here. Reaping before the
	// assertion below also matters on its own: without it the kernel may not have
	// dropped the flock yet and the test would be racing its own subject.
	err = helper.Wait()
	if err == nil {
		t.Fatal("the helper exited cleanly despite being SIGKILLed, so it released the lock " +
			"on its way out — which is not the property under test")
	}

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the helper did not exit on a signal: %v", err)
	}

	if status, ok := exit.Sys().(syscall.WaitStatus); ok && !status.Signaled() {
		t.Fatalf("the helper exited on its own (%v) rather than being killed, so nothing here "+
			"shows the kernel releasing a HELD lock", exit)
	}

	// SIGKILL, so nothing in the helper ran on the way out — no defer, no
	// release. If the identity is free now, it is the kernel that freed it, which
	// is the property that makes a crashed billet restartable.
	after, err := LockDeployment(id, LockOptions{Dir: dir})
	if err != nil {
		t.Fatalf("the identity stayed locked after its holder was SIGKILLed, so every "+
			"restart after a crash would be refused: %v", err)
	}

	releaseAtEnd(t, after)
}
