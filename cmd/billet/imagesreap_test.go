package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// fakeReapStore stands in for the cluster and JUDGES NOTHING.
//
// It returns what it is told and records what it was asked. A fake that
// reimplemented the retention rule would make every assertion below an assertion
// about the fake -- which is the sharpest version of this mistake, because it looks
// like faithfulness.
//
// onCall runs at the top of each method, named, so a test can ask what is true at
// that instant. That is the only way to observe a lock: whether the kernel directory
// is held DURING the read and the delete is the whole property, and nothing about it
// survives to the end of the command.
type fakeReapStore struct {
	mu sync.Mutex

	generations []ceph.Generation
	verified    map[string]bool
	contracts   map[string]string
	images      []string
	kernels     map[string]bool
	unknown     int

	calls  []string
	onCall func(method string)
}

func (f *fakeReapStore) record(method string) {
	if f.onCall != nil {
		f.onCall(method)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, method)
}

func (f *fakeReapStore) called(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Contains(f.calls, method)
}

func (f *fakeReapStore) Generations(_ context.Context, _ string) ([]ceph.Generation, error) {
	f.record("Generations")

	return f.generations, nil
}

func (f *fakeReapStore) VerifiedGenerations(_ context.Context, _ string) (map[string]bool, error) {
	f.record("VerifiedGenerations")

	return f.verified, nil
}

func (f *fakeReapStore) GenerationGuestContracts(
	_ context.Context,
	_ string,
) (map[string]string, error) {
	f.record("GenerationGuestContracts")

	return f.contracts, nil
}

func (f *fakeReapStore) Reap(
	_ context.Context,
	_ string,
	plan []ceph.Reapable,
	_ ceph.Retention,
) ([]string, error) {
	f.record("Reap")

	removed := make([]string, 0, len(plan))

	for _, item := range plan {
		if item.Reason == "" {
			removed = append(removed, item.Generation.Name)
		}
	}

	return removed, nil
}

func (f *fakeReapStore) Images(_ context.Context) ([]string, error) {
	f.record("Images")

	return f.images, nil
}

func (f *fakeReapStore) NeededKernels(
	_ context.Context,
	_ string,
	_ []ceph.Generation,
) (map[string]bool, int, error) {
	f.record("NeededKernels")

	f.mu.Lock()
	defer f.mu.Unlock()

	needed := make(map[string]bool, len(f.kernels))
	for name := range f.kernels {
		needed[name] = true
	}

	return needed, f.unknown, nil
}

// needs makes a kernel one a surviving generation names, as publishing a generation
// for it would.
func (f *fakeReapStore) needs(kernel string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.kernels[kernel] = true
}

// reapFixture is a kernel directory plus a cluster that has one generation nothing
// needs, so a reap has something to remove at both ends.
type reapFixture struct {
	kernelDir string
	store     *fakeReapStore
	cfg       *config.Config
}

// orphanKernel is a kernel no generation in the fixture names.
const orphanKernel = "vmlinux-6.1.140-bbbbbbbbbbbb"

// pulledKernel is the one a pull is in the middle of publishing a generation for.
const pulledKernel = "vmlinux-6.1.155-ea1d42638d13"

func newReapFixture(t *testing.T, onDisk ...string) *reapFixture {
	t.Helper()

	dir := t.TempDir()

	for _, name := range onDisk {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}

	return &reapFixture{
		kernelDir: dir,
		store: &fakeReapStore{
			generations: []ceph.Generation{{Name: "g20260101000000"}},
			verified:    map[string]bool{},
			contracts:   map[string]string{},
			images:      []string{"ubuntu-2404-x64"},
			kernels:     map[string]bool{},
		},
		// NO FIRECRACKER SECTION, so configuredKernelName answers "" and nothing is
		// protected by being this node's configured kernel. That protection is real and
		// tested elsewhere; leaving it out here is what makes these tests about the lock.
		cfg: &config.Config{Node: &config.NodeConfig{}},
	}
}

func (f *reapFixture) run(t *testing.T, dryRun bool) error {
	t.Helper()

	return runImagesReap(t.Context(), f.store, f.cfg, "ubuntu-2404-x64", f.kernelDir, 3, dryRun)
}

// A REAP HOLDS THE KERNEL DIRECTORY WHILE IT DECIDES AND WHILE IT DELETES.
//
// Both halves matter and for different reasons. The DELETE is obvious. The
// DECISION is the one that was missed in the first reading of the race: the
// needed set describes the generations that exist at the moment it is read, so
// computing it outside the lock and unlinking inside one acts on an answer a
// concurrent pull has already made wrong. Taking the lock around os.Remove alone
// would have closed nothing.
//
// AND IT IS HELD ACROSS THE GENERATION HALF TOO, which is what gives the two commands
// ONE order: this lock is always outside the Ceph publish lock. Taken only around the
// kernel half, a reap holding the publish lock while a pull holds the kernel lock
// makes that pull's ImportGeneration fail outright, because TakePublishLock never
// waits.
func TestAReapHoldsTheKernelDirectoryLockWhileItDecidesAndDeletes(t *testing.T) {
	shortKernelLockWait(t)

	fixture := newReapFixture(t, orphanKernel)

	probes := map[string]*kernelLockProbe{}

	for _, method := range []string{"Reap", "Images", "NeededKernels"} {
		probes[method] = &kernelLockProbe{t: t, dir: fixture.kernelDir}
	}

	fixture.store.onCall = func(method string) {
		if probe, watched := probes[method]; watched {
			probe.observe()
		}
	}

	if err := fixture.run(t, false); err != nil {
		t.Fatalf("images reap: %v", err)
	}

	for _, method := range []string{"Reap", "Images", "NeededKernels"} {
		if !probes[method].held() {
			t.Errorf("the kernel directory was not locked during %s (%d observations, %d "+
				"refused)", method, probes[method].tried, probes[method].refused)
		}
	}

	// AND IT ACTUALLY REAPED, or every assertion above is satisfied by a command that
	// took a lock and did nothing.
	if present(t, fixture.kernelDir, orphanKernel) {
		t.Error("the orphan kernel survived a reap that reported success")
	}
}

// A DRY RUN TAKES NO LOCK.
//
// It deletes nothing, so there is nothing to exclude -- and taking a lock means
// CREATING A FILE, so requiring one would start refusing a preview on a read-only
// kernel directory over an operation that changes nothing. That is the failure
// ADR-005 names, and the next thing anybody does about a check that refuses correct
// state is delete it.
//
// THE PROBE ASSERTS THE LOCK WAS FREE, not that the command succeeded. A dry run that
// took the lock and released it would still succeed.
func TestAReapDryRunTakesNoLock(t *testing.T) {
	shortKernelLockWait(t)

	fixture := newReapFixture(t, orphanKernel)

	probe := &kernelLockProbe{t: t, dir: fixture.kernelDir}

	fixture.store.onCall = func(method string) {
		if method == "Images" {
			probe.observe()
		}
	}

	if err := fixture.run(t, true); err != nil {
		t.Fatalf("images reap --dry-run: %v", err)
	}

	if probe.tried == 0 {
		t.Fatal("the probe never ran, so this proves nothing about the lock")
	}

	if probe.refused != 0 {
		t.Errorf("a dry run held the kernel directory lock (%d of %d observations refused); "+
			"it deletes nothing, and taking a lock means creating a file, which refuses a "+
			"preview on a read-only kernel directory", probe.refused, probe.tried)
	}

	if !present(t, fixture.kernelDir, orphanKernel) {
		t.Error("a DRY RUN deleted the kernel")
	}
}

// THE RACE ITSELF: A REAP WAITS FOR A PULL RATHER THAN DELETING THE KERNEL IT IS
// ABOUT TO PUBLISH A GENERATION FOR.
//
// The failing sequence is a pull that has durably installed a kernel and not yet
// called ImportGeneration, so nothing anywhere names that file. A reap looking at the
// cluster right then correctly concludes it is an orphan and unlinks it; the pull
// then publishes a generation naming a kernel that is gone, and every node resolves a
// verified generation it cannot boot.
//
// THE ORDERING IS ESTABLISHED, NOT SLEPT THROUGH. The pull's hold is taken before the
// reap starts, and the release is gated on having OBSERVED that the reap has not
// reached the cluster -- a sleep would pass against a reap that took no lock at all
// and simply lost a race on the day the test ran.
func TestAReapWaitsForAPullRatherThanDeletingTheKernelItIsAboutToPublish(t *testing.T) {
	// LONG ENOUGH TO OUTLAST THE HANDOVER BELOW. This test is about the reap WAITING,
	// so a window that expires first would turn the property into its opposite and
	// report it as a pass of a different test.
	window, retry := kernelLockWindow, kernelLockRetry

	t.Cleanup(func() { kernelLockWindow, kernelLockRetry = window, retry })

	kernelLockWindow = 30 * time.Second
	kernelLockRetry = 5 * time.Millisecond

	fixture := newReapFixture(t, pulledKernel, orphanKernel)

	// The pull: it has installed the kernel and holds the directory, and no generation
	// names the file yet.
	pull, err := takeKernelDirLock(t.Context(), fixture.kernelDir,
		"install the kernel this generation will name")
	if err != nil {
		t.Fatalf("take the pull's lock: %v", err)
	}

	reached := make(chan struct{})
	blocked := make(chan struct{})
	reachedOnce, blockedOnce := sync.Once{}, sync.Once{}

	fixture.store.onCall = func(string) { reachedOnce.Do(func() { close(reached) }) }

	// THE ORDERING IS ESTABLISHED, NOT SLEPT THROUGH. This fires the first time an
	// acquisition is refused the flock, so receiving on it PROVES the reap asked for
	// the lock and was told no. The obvious alternative -- wait a fixed time for the
	// absence of a cluster read -- is satisfied by a goroutine that was simply never
	// scheduled, so it passes against a reap that takes no lock at all and merely
	// started late.
	oldHook := onKernelLockContended

	t.Cleanup(func() { onKernelLockContended = oldHook })

	onKernelLockContended = func() { blockedOnce.Do(func() { close(blocked) }) }

	// EVERYTHING THE GOROUTINE NEEDS IS TAKEN FIRST, and it touches nothing on t.
	// Every failure path below ends in t.Fatal while this is still running, and a
	// t.Helper() or a t.Context() from a goroutine after the test has finished is a
	// panic reported as something else entirely. The channel is buffered so the send
	// cannot block on the way out either.
	ctx := t.Context()
	store, cfg, kernelDir := fixture.store, fixture.cfg, fixture.kernelDir
	done := make(chan error, 1)

	var running sync.WaitGroup

	running.Add(1)

	go func() {
		defer running.Done()

		done <- runImagesReap(ctx, store, cfg, "ubuntu-2404-x64", kernelDir, 3, false)
	}()

	// AND THE GOROUTINE IS STOPPED BEFORE THE CLEANUPS THAT WOULD RACE IT. Every
	// failure below is a t.Fatal taken while the reap is still waiting, and the
	// cleanups registered ABOVE this one restore kernelLockWindow, kernelLockRetry
	// and onKernelLockContended -- package variables the reap's retry loop is
	// reading. Cleanups run last-in-first-out, so releasing and joining here happens
	// FIRST.
	//
	// A WAITGROUP RATHER THAN A RECEIVE ON done, because the happy path has already
	// taken that value and a second receive would block here forever. Releasing twice
	// is safe: release is idempotent.
	t.Cleanup(func() {
		if err := pull.release(); err != nil {
			t.Errorf("release the pull's lock: %v", err)
		}

		running.Wait()
	})

	// THE REAP IS BLOCKED ON THE LOCK, and only then is it safe to say it has not
	// read the cluster: a reap that reached the cluster would have to have taken the
	// lock, which this one provably could not.
	select {
	case <-blocked:
	case <-reached:
		t.Fatal("the reap read the cluster while a pull held the kernel directory, so it is " +
			"deciding what to delete from a generation set that is still being written")
	case err := <-done:
		t.Fatalf("the reap finished while a pull held the kernel directory: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the reap neither blocked on the kernel directory lock nor did anything else")
	}

	if fixture.store.called("Generations") {
		t.Fatal("the reap read the cluster before it blocked on the lock")
	}

	// The pull publishes: from here a generation names the kernel, so it is in every
	// reap's needed set.
	fixture.store.needs(pulledKernel)

	if err := pull.release(); err != nil {
		t.Fatalf("release the pull's lock: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the reap failed after the pull released the lock: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reap never proceeded after the pull released the kernel directory")
	}

	if !present(t, fixture.kernelDir, pulledKernel) {
		t.Error("the reap deleted the kernel the pull had just published a generation for; " +
			"every node now resolves a generation it cannot boot")
	}

	// AND IT STILL DID ITS JOB, or a reap that waited and then gave up would pass.
	if present(t, fixture.kernelDir, orphanKernel) {
		t.Error("the orphan kernel survived")
	}
}

// A REAP NEVER REMOVES ITS OWN LOCK FILE.
//
// Unlinking a HELD lock file does not release the flock; it detaches the path from
// the locked inode, so the next billet creates a fresh file under that name, locks
// that, and both run with neither able to tell. That is the failure that moved the
// deployment lock out of a cache directory, and it would be reintroduced here by one
// change to the kernel filename pattern.
//
// THE REAP IS DRIVEN, not reapKernelDir alone: the lock file only exists because the
// command placed it, and a test that staged one by hand would keep passing if the
// command stopped taking the lock at all.
//
// WHAT MUTATION SAYS ABOUT THE GUARD THIS COVERS, said out loud rather than assumed.
// Removing reapKernelDir's kernelLockName skip ALONE leaves this green: PlanKernelReap's
// filename pattern already excludes the lock file, so the skip is redundant today and
// is defence in depth rather than the thing standing between the reap and its own
// lock. Removing the skip AND widening that pattern turns this red, and restoring the
// skip under the widened pattern turns it green again -- which is exactly the
// combination the guard exists for, since the pattern and the reaper are two functions
// that can change without the other noticing.
func TestAReapNeverRemovesItsOwnLockFile(t *testing.T) {
	shortKernelLockWait(t)

	fixture := newReapFixture(t, orphanKernel)

	if err := fixture.run(t, false); err != nil {
		t.Fatalf("images reap: %v", err)
	}

	if present(t, fixture.kernelDir, orphanKernel) {
		t.Fatal("the reap removed nothing, so it never reached the deletion this is about")
	}

	if !present(t, fixture.kernelDir, kernelLockName) {
		t.Error("the reap deleted its own lock file; the flock stays held on a detached " +
			"inode, so the next billet locks a new file under that name and both run")
	}
}

// AND A LOCK IT CANNOT TAKE STOPS THE REAP BEFORE IT READS ANYTHING.
//
// Failing to place a lock is an error, not a downgrade: a reap that carried on
// unlocked would be the exact command this exists to exclude. The refusal has to
// arrive before the cluster is read, because the read is what the lock is protecting.
func TestAReapThatCannotTakeTheLockReadsNothing(t *testing.T) {
	shortKernelLockWait(t)

	fixture := newReapFixture(t, orphanKernel)

	heldKernelLock(t, fixture.kernelDir)

	err := fixture.run(t, false)
	if err == nil {
		t.Fatal("a reap ran while another billet held the kernel directory")
	}

	if !errors.Is(err, errKernelLockBusy) {
		t.Errorf("the refusal is not contention: %v", err)
	}

	if !strings.Contains(err.Error(), "did not collect kernels") {
		t.Errorf("the refusal does not say what billet did not do: %v", err)
	}

	if fixture.store.called("Generations") {
		t.Error("the reap read the cluster before it held the lock, so its plan describes a " +
			"moment nothing was excluded during")
	}

	if !present(t, fixture.kernelDir, orphanKernel) {
		t.Error("the refused reap still deleted a kernel")
	}
}
