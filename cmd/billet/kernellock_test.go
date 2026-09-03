package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// NOTHING IN THIS FILE IS PARALLEL, and that is the rule rather than caution: these
// tests swap the package-level window and retry, which every other acquisition in the
// process reads. What decides parallelism here is shared process state, not whether a
// test touches a disk.

// shortKernelLockWait makes the wait observable in a test suite.
//
// TEN MINUTES IS RIGHT IN PRODUCTION AND UNUSABLE HERE, and a decision only ever
// exercised with its window stubbed out is one nobody has tested -- so the window is
// shortened rather than removed, and the retry stays smaller than it so the loop
// still goes round at least once before the window closes.
func shortKernelLockWait(t *testing.T) {
	t.Helper()

	window, retry := kernelLockWindow, kernelLockRetry

	t.Cleanup(func() { kernelLockWindow, kernelLockRetry = window, retry })

	// SMALL, BECAUSE EVERY REFUSED PROBE PAYS THE WHOLE WINDOW and this package
	// already sits close to go test's default per-package timeout under -race
	// -covermode=atomic. The retry stays well under it so the loop still goes round
	// several times, which is the part being exercised.
	kernelLockWindow = 120 * time.Millisecond
	kernelLockRetry = 5 * time.Millisecond
}

// heldKernelLock takes the lock and releases it when the test ends.
//
// ONE CLEANUP, AND THE "ALREADY RELEASED" QUESTION IS ASKED OF THE LOCK ITSELF.
// The first version of this registered a second t.Cleanup to record that a test had
// released the lock by hand -- and cleanups run LAST-IN-FIRST-OUT, so that one ran
// BEFORE the release it was meant to suppress and suppressed it always. Every lock
// this helper took was then held for the rest of the binary's run, which is invisible
// here because each test has a directory of its own and would not be in a suite that
// shared one.
func heldKernelLock(t *testing.T, dir string) *kernelLock {
	t.Helper()

	lock, err := takeKernelDirLock(t.Context(), dir, "hold it for this test")
	if err != nil {
		t.Fatalf("takeKernelDirLock(%s): %v", dir, err)
	}

	t.Cleanup(func() {
		if err := lock.release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	return lock
}

// THE LOCK IS AT THE PATH ITS OWN DIAGNOSTIC PRINTS, and the directory is created if
// it is not there.
//
// The creation is not incidental: the pull takes this lock BEFORE installKernel, and
// installKernel is what creates the kernel directory today. A lock that required the
// directory to exist would refuse every first pull on a fresh host.
func TestTheKernelDirectoryLockIsPlacedInTheDirectoryItProtects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kernels")

	heldKernelLock(t, dir)

	if _, err := os.Stat(filepath.Join(dir, ".billet-kernels.lock")); err != nil {
		t.Errorf("the lock is not at the path the contention message would print: %v", err)
	}

	// The literal above and kernelLockPath must agree, because the reaper's skip and
	// the lock's own open are two readers of one name.
	if got := kernelLockPath(dir); got != filepath.Join(dir, ".billet-kernels.lock") {
		t.Errorf("kernelLockPath = %q", got)
	}
}

// AND THE FILE IS ACTUALLY LOCKED, WHICH THE TEST ABOVE CANNOT SEE.
//
// Asserting the lock file exists passes against a takeKernelDirLock that opens it and
// flocks nothing, and what that costs is the whole reason this exists: a reap decides
// what to delete from the generations that exist when it looks, so one running
// alongside a pull deletes the kernel a moment before the generation naming it is
// published.
//
// ONE PROCESS IS ENOUGH HERE, for the reason probelock_test.go gives: nothing in
// kernellock.go holds process-local state, and a flock is owned by the open file
// description rather than by the process, so a second descriptor on one file contends
// inside a single process exactly as it would across two. That is asserted by
// mutation rather than by this paragraph -- remove the unix.Flock call and this test
// goes red.
func TestASecondKernelCollectionWaitsAndThenSaysWhatHasTheLock(t *testing.T) {
	shortKernelLockWait(t)

	dir := t.TempDir()
	held := heldKernelLock(t, dir)

	second, err := takeKernelDirLock(t.Context(), dir, "collect kernels")
	if err == nil {
		if releaseErr := second.release(); releaseErr != nil {
			t.Errorf("release the second lock: %v", releaseErr)
		}

		t.Fatal("a second holder took the kernel directory lock while the first held it, so " +
			"a reap can still delete the kernel a pull is about to publish a generation for")
	}

	// CONTENTION, SPECIFICALLY. An error is the cheapest thing this could return, and
	// a missing directory or a filesystem without flock would satisfy a bare
	// err != nil while meaning the opposite -- that the mechanism is not there at all.
	if !errors.Is(err, errKernelLockBusy) {
		t.Errorf("the refusal is not contention, so the lock may never have been placed: %v", err)
	}

	// THE DIAGNOSTIC AN OPERATOR ACTS ON: the path they can look at, what billet did
	// not do, and what to look for. It deliberately does not name a process.
	for _, want := range []string{
		kernelLockPath(dir),
		"did not collect kernels",
		"billet images pull",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	// AND RELEASING IT LETS THE NEXT ONE IN, or a crashed pull would wedge every later
	// reap on a host with nothing running.
	if err := held.release(); err != nil {
		t.Fatalf("release the first lock: %v", err)
	}

	after, err := takeKernelDirLock(t.Context(), dir, "collect kernels")
	if err != nil {
		t.Fatalf("the lock stayed held after it was released: %v", err)
	}

	if err := after.release(); err != nil {
		t.Errorf("release: %v", err)
	}
}

// A SYMLINK AT THE LOCK PATH IS REFUSED RATHER THAN FOLLOWED.
//
// Following one moves the lock to an inode no other billet flocks, which is a lock
// that excludes nothing while every holder believes it is held -- and this runs as
// root in a directory an operator can write to.
//
// THE ASSERTION IS THAT THE LOCK WAS NOT TAKEN, not that an error came back: the
// failure being guarded against is a SUCCESSFUL acquisition on the wrong file. Drop
// O_NOFOLLOW and the open succeeds, the flock is taken on the target, and this goes
// red.
func TestTheKernelDirectoryLockRefusesASymlinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.lock")

	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write the target: %v", err)
	}

	if err := os.Symlink(target, kernelLockPath(dir)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	lock, err := takeKernelDirLock(t.Context(), dir, "collect kernels")
	if err == nil {
		if releaseErr := lock.release(); releaseErr != nil {
			t.Errorf("release: %v", releaseErr)
		}

		t.Fatal("the kernel directory lock was taken through a symlink, so it is held on an " +
			"inode no other billet flocks and excludes nothing")
	}

	// NOT CONTENTION, which is the other half: a failure that waiting cannot fix must
	// not be retried until the window closes and then reported as somebody else
	// holding the lock. That sends an operator hunting for a process that is not
	// there, about a host that can never take this lock at all.
	if errors.Is(err, errKernelLockBusy) {
		t.Errorf("a symlinked lock file was reported as contention: %v", err)
	}
}

// A CALLER RUNNING OUT IS A DIFFERENT FACT FROM BILLET'S OWN WINDOW EXPIRING, and
// reporting one as the other sends an operator looking for a second billet that is
// not there.
//
// Both are reachable only by having been refused the flock, so both carry
// errKernelLockBusy; what differs is whose clock ran out. Collapse the two branches
// and this goes red on the message.
func TestTheKernelDirectoryLockStopsWaitingWhenItsCallerDoes(t *testing.T) {
	shortKernelLockWait(t)

	dir := t.TempDir()

	heldKernelLock(t, dir)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := takeKernelDirLock(ctx, dir, "collect kernels")
	if err == nil {
		t.Fatal("a cancelled caller took the lock somebody else was holding")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("the failure does not name the caller's own cancellation: %v", err)
	}

	if !errors.Is(err, errKernelLockBusy) {
		t.Errorf("a cancelled wait on a held lock is still contention: %v", err)
	}

	if strings.Contains(err.Error(), "stayed held for more than") {
		t.Errorf("the caller's cancellation was reported as billet's own window expiring, "+
			"which sends an operator hunting for a stuck billet: %v", err)
	}
}

// AND A CALLER WITH NO BUDGET LEFT IS NOT HANDED THE LOCK, even when it is free.
//
// `select` picks at RANDOM when both its cases are ready, so a caller whose deadline
// expires in the same instant as a retry tick can go round the loop again, find the
// holder gone, and be given a lock it has no time to use. What follows is
// uncancellable -- installKernel copies the kernel and fsyncs it before the first
// context-aware call refuses -- so the pull does the local work anyway and leaves a
// kernel no generation will ever name.
//
// THE LOCK IS FREE HERE, which is the point: with a holder present the refusal comes
// from the flock and this branch is never reached. The property is about the way OUT
// of the loop, not the way round it.
func TestTheKernelDirectoryLockIsNotHandedToACallerWithNoBudgetLeft(t *testing.T) {
	shortKernelLockWait(t)

	dir := t.TempDir()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lock, err := takeKernelDirLock(ctx, dir, "collect kernels")
	if err == nil {
		if releaseErr := lock.release(); releaseErr != nil {
			t.Errorf("release: %v", releaseErr)
		}

		t.Fatal("a cancelled caller was handed the kernel directory lock; everything it does " +
			"next is local and uncancellable, so it installs a kernel and then fails to " +
			"publish the generation that would name it")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not name the caller's own cancellation: %v", err)
	}

	// AND IT DOES NOT CLAIM CONTENTION, which is the half an assertion on
	// context.Canceled alone would ratify. Nothing held this lock -- the flock
	// SUCCEEDED -- so errKernelLockBusy would be a claim about a holder that never
	// existed. kernelLockProbe counts that sentinel as "the lock was held", so a
	// cancelled probe carrying it would report contention inside the very test that
	// proves a reap waits for a pull.
	if errors.Is(err, errKernelLockBusy) {
		t.Errorf("an expired caller on a FREE lock was told the lock was held: %v", err)
	}

	// AND THE LOCK IS NOT LEFT HELD. It was taken before the budget was checked, so a
	// refusal that forgot to give it back would wedge every later pull and reap on
	// this host until the process exited.
	after, err := takeKernelDirLock(t.Context(), dir, "collect kernels")
	if err != nil {
		t.Fatalf("the refused acquisition kept the lock: %v", err)
	}

	if err := after.release(); err != nil {
		t.Errorf("release: %v", err)
	}
}

// TWO KERNEL DIRECTORIES DO NOT CONTEND, because they are not the same directory.
//
// Keying this on a deployment, a config path or anything else that is not the
// directory itself would serialize two hosts' worth of unrelated work -- and, in the
// direction that matters, would FAIL to serialize two deployments that share one
// directory, which is the case the lock exists for.
func TestTwoKernelDirectoriesDoNotContend(t *testing.T) {
	shortKernelLockWait(t)

	first := heldKernelLock(t, t.TempDir())
	second := heldKernelLock(t, t.TempDir())

	if first.file.Name() == second.file.Name() {
		t.Fatal("two different kernel directories resolved to one lock file")
	}
}

// A LOCK WITH NO DIRECTORY NAMED IS REFUSED NEXT TO THE INTERPOLATION.
//
// An empty directory joins to ./.billet-kernels.lock, in whatever directory the
// command happened to be run from -- so two runs of the same command from two working
// directories would take two locks and exclude nothing, while both looked held. This
// is the sink re-applying an invariant its callers already hold; state.LockDeployment
// and takeProbeLock check the same thing in the same place for the same reason.
func TestTheKernelDirectoryLockRefusesAnEmptyDirectory(t *testing.T) {
	_, err := takeKernelDirLock(t.Context(), "", "collect kernels")
	if err == nil {
		t.Fatal("the kernel directory lock accepted an empty directory, which puts the file " +
			"it locks wherever the command was run from")
	}

	if !strings.Contains(err.Error(), "no directory was named") {
		t.Errorf("the refusal does not say the directory is why: %v", err)
	}
}
