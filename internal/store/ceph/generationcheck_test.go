package ceph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

// A candidate recorded before candidates were checked as published, as a
// journal written by an older release holds one, is checked before PublishCAS
// moves the pointer; one Snapshot checked is not mapped again.
func TestALegacyCandidateIsCheckedAsPublishedBeforeItIsPublished(t *testing.T) {
	t.Parallel()

	damaged := func(spec string) (storecontract.Filesystem, error) {
		if strings.Contains(spec, "/cache-g-") {
			return storecontract.Filesystem{}, errors.New("the ext4 filesystem is not clean: exit status 12")
		}

		return clean(spec)
	}

	for _, tc := range []struct {
		name      string
		checked   bool
		verdict   func(string) (storecontract.Filesystem, error)
		published bool
		maps      int
	}{
		{name: "legacy and damaged", verdict: damaged, maps: 1},
		{name: "legacy and sound", verdict: clean, published: true, maps: 1},
		{name: "checked by Snapshot", checked: true, verdict: damaged, published: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			v := &verifiedSpecs{f: f, verdict: clean}
			c := verifyingClient(t, f, v)
			volume, err := c.Create(t.Context(), "acme/api/go", 1<<30)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			lease, fence, err := c.AcquireWriter(t.Context(), "acme/api/go", "writer", time.Minute)
			if err != nil {
				t.Fatalf("AcquireWriter: %v", err)
			}
			candidate, err := c.Snapshot(t.Context(), volume)
			if err != nil || !candidate.Filesystem.CheckedAsPublished {
				t.Fatalf("Snapshot = %+v, %v; want a candidate checked as published", candidate, err)
			}
			candidate.Filesystem.CheckedAsPublished = tc.checked
			v.verdict, v.specs, v.readOnly = tc.verdict, nil, nil

			err = c.PublishCAS(t.Context(), "acme/api/go", "", candidate, lease, fence)
			if published := err == nil; published != tc.published {
				t.Fatalf("PublishCAS = %v; want published %v", err, tc.published)
			}
			pointer := f.metadata[c.cacheIndex()][pointerKey("acme/api/go")]
			if moved := strings.Contains(pointer, candidate.Generation); moved != tc.published {
				t.Fatalf("the pointer %q names the candidate: %v, want %v", pointer, moved, tc.published)
			}
			spec := candidate.Handle + "@" + candidate.Generation
			if got := len(v.specs); got != tc.maps || (tc.maps == 1 && (v.specs[0] != spec || !v.readOnly[0])) {
				t.Fatalf("PublishCAS checked %v (read-only %v), want %d check(s) of %s read-only",
					v.specs, v.readOnly, tc.maps, spec)
			}
			if len(f.mappings) != 0 {
				t.Errorf("devices are still mapped: %v", f.mappings)
			}
		})
	}
}

// Two clients in one process share the generation-map slot, so one's sweep
// waits for the other's check rather than unmapping the device it is reading.
func TestASweepWaitsForAnotherClientsGenerationCheck(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newCacheFake()
		v := &verifiedSpecs{f: f, verdict: clean}
		checker := verifyingClient(t, f, v)
		sweeper := cacheClient(t, f)
		if checker.checkSlot != sweeper.checkSlot {
			t.Fatal("two clients built by New have generation-map slots of their own")
		}
		// A slot of the bubble's own, the same for both, so other tests' checks
		// are not held behind this one's.
		slot := make(chan struct{}, 1)
		checker.checkSlot, sweeper.checkSlot = slot, slot
		generation := publishedGeneration(t, checker)
		spec := generation.Handle + "@" + generation.Generation

		hold := make(chan struct{})
		checker.damaged = func(context.Context, string) (bool, error) {
			<-hold

			return false, nil
		}
		checked := make(chan error, 1)
		go func() {
			_, err := checker.GenerationDamaged(t.Context(), "acme/api/go", generation.Generation)
			checked <- err
		}()
		synctest.Wait()
		evicted := make(chan error, 1)
		go func() { evicted <- sweeper.Evict(t.Context(), time.Hour) }()
		synctest.Wait()

		if !slices.Contains(mappedSpecs(f), spec) {
			t.Fatalf("the check's device was unmapped while it was being read: %v", f.mappings)
		}
		select {
		case err := <-evicted:
			t.Fatalf("the sweep finished (%v) while the check held its map", err)
		default:
		}

		close(hold)
		if err := <-checked; err != nil {
			t.Fatalf("GenerationDamaged: %v", err)
		}
		if err := <-evicted; err != nil {
			t.Fatalf("Evict: %v", err)
		}
		if len(f.mappings) != 0 {
			t.Errorf("devices are still mapped: %v", f.mappings)
		}
	})
}

// The lock file extends the exclusion to another process: a client with a slot
// of its own waits on it, gives up with its context, and takes it once it is
// free.
func TestTheGenerationMapLockExcludesAnotherProcess(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		lock := filepath.Join(t.TempDir(), "ceph-generation-maps.lock")
		holder := cacheClient(t, newCacheFake())
		waiter := cacheClient(t, newCacheFake())
		holder.checkSlot, waiter.checkSlot = make(chan struct{}, 1), make(chan struct{}, 1)
		holder.mapLock, waiter.mapLock = lock, lock

		release, err := holder.holdGenerationMaps(t.Context())
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := waiter.holdGenerationMaps(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a second process's hold answered %v while the lock was held", err)
		}
		if len(waiter.checkSlot) != 0 {
			t.Fatal("the waiter kept its slot after giving up on the lock")
		}

		release()
		again, err := waiter.holdGenerationMaps(t.Context())
		if err != nil {
			t.Fatalf("hold once the lock was free: %v", err)
		}
		again()

		// GROUP-WRITABLE WHATEVER THE UMASK, for a lock_dir two accounts share,
		// and nothing left beside it.
		info, err := os.Stat(lock)
		if err != nil || info.Mode().Perm() != 0o660 {
			t.Fatalf("the lock file is %v (%v), want 0660", info.Mode().Perm(), err)
		}
		entries, err := os.ReadDir(filepath.Dir(lock))
		if err != nil || len(entries) != 1 {
			t.Fatalf("the lock directory holds %v (%v), want the lock file alone", entries, err)
		}
	})
}

// A sweep starts no unmap once its budget is spent; the mappings left wait for
// the next sweep.
func TestASweepStartsNoUnmapPastItsBudget(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newCacheFake()
		c := cacheClient(t, f)
		for i, snap := range []string{"1791000001-a", "1791000002-b", "1791000003-c", "1791000004-d"} {
			f.mappings["/dev/rbd4"+string(rune('0'+i))] = "billet-cache/cache-g-1791000000-0000000000000000000000ab@" + snap
		}
		unmaps := 0
		c.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
			if slices.Contains(args, "unmap") {
				unmaps++
				time.Sleep(c.wait * 2 / 3)
			}

			return f.run(ctx, bin, args)
		}

		if err := c.sweepGenerationMaps(t.Context()); err == nil {
			t.Fatal("a sweep that ran out of time reported nothing left")
		}
		if unmaps != 2 || len(f.mappings) != 2 {
			t.Fatalf("%d unmaps started and %d mappings left; want 2 and 2 with the budget spent", unmaps, len(f.mappings))
		}
	})
}

// A cancelled check runs nothing.
func TestACancelledDamageCheckRunsNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	e2fsck := filepath.Join(dir, "e2fsck")
	marker := filepath.Join(dir, "ran")
	if err := os.WriteFile(e2fsck, []byte("#!/bin/sh\n: > "+marker+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if damaged, err := checkDamageWith(ctx, e2fsck, "/dev/rbd9"); damaged || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled check answered %v, %v; want could-not-tell for the cancellation", damaged, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("e2fsck ran although its check was cancelled: %v", err)
	}
}

// An older generation is checked through its own image; a generation with no
// record is found through the pointer only when the pointer names it.
func TestAGenerationIsResolvedByItsOwnRecord(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	v := &verifiedSpecs{f: f, verdict: clean}
	c := verifyingClient(t, f, v)
	key := "acme/api/go"
	older := publishedGeneration(t, c)
	volume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	lease, fence, err := c.AcquireWriter(t.Context(), key, "second", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	newer, err := c.Snapshot(t.Context(), volume)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := c.PublishCAS(t.Context(), key, older.Generation, newer, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	var checked []string
	c.damaged = func(_ context.Context, device string) (bool, error) {
		checked = append(checked, f.mappings[device])

		return false, nil
	}

	if _, err := c.GenerationDamaged(t.Context(), key, older.Generation); err != nil ||
		!slices.Equal(checked, []string{older.Handle + "@" + older.Generation}) {
		t.Fatalf("the older generation answered %v after checking %v, want its own image", err, checked)
	}

	index := f.metadata[c.cacheIndex()]
	delete(index, generationKey(key, newer.Generation))
	delete(index, generationKey(key, older.Generation))
	checked = nil
	if _, err := c.GenerationDamaged(t.Context(), key, newer.Generation); err != nil ||
		!slices.Equal(checked, []string{newer.Handle + "@" + newer.Generation}) {
		t.Fatalf("the current generation without a record answered %v after checking %v, want the pointer's image",
			err, checked)
	}
	checked = nil
	if _, err := c.GenerationDamaged(t.Context(), key, older.Generation); !errors.Is(err, storecontract.ErrMiss) ||
		len(checked) != 0 {
		t.Fatalf("a generation the pointer has moved past answered %v after checking %v, want a miss", err, checked)
	}
}

func mappedSpecs(f *cacheFake) []string {
	specs := []string{}
	for _, spec := range f.mappings {
		specs = append(specs, spec)
	}

	return specs
}

// Damage the check proved stands when the unmap after it fails: GenerationDamaged
// answers true with the unmap's error, and the mapping is left to the sweep.
func TestDamageProvedBeforeAFailedUnmapIsStillProof(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	v := &verifiedSpecs{f: f, verdict: clean}
	c := verifyingClient(t, f, v)
	generation := publishedGeneration(t, c)
	c.damaged = func(context.Context, string) (bool, error) { return true, nil }
	c.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "unmap") {
			return nil, errors.New("exit status 5: rbd: sysfs write failed: (5) Input/output error")
		}

		return f.run(ctx, bin, args)
	}

	damaged, err := c.GenerationDamaged(t.Context(), "acme/api/go", generation.Generation)
	if !damaged || err == nil {
		t.Fatalf("GenerationDamaged = %v, %v; want the proof with the unmap's failure", damaged, err)
	}
}

// An existing lock file is never replaced, and a temporary file an interrupted
// creation left behind does not stand in for it.
func TestTheGenerationMapLockIsNeverReplaced(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lock := filepath.Join(dir, "ceph-generation-maps.lock")
	stray := filepath.Join(dir, ".ceph-generation-maps.lock.123")
	for _, file := range []string{lock, stray} {
		if err := os.WriteFile(file, []byte(file), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}

	f, err := openGenerationMapLock(lock)
	if err != nil {
		t.Fatalf("openGenerationMapLock: %v", err)
	}
	defer func() { _ = f.Close() }()
	after, err := os.Stat(lock)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the existing lock file was replaced (%v)", err)
	}

	// AND ONE ANOTHER PROCESS PUT IN PLACE WHILE THIS ONE WAS MAKING ITS OWN is
	// the one opened, so both lock the same file.
	raced := filepath.Join(t.TempDir(), "ceph-generation-maps.lock")
	var theirs os.FileInfo
	g, err := openGenerationMapLockRacing(raced, func(string) {
		if err := os.WriteFile(raced, []byte("theirs"), 0o660); err != nil {
			t.Error(err)
		}
		var err error
		if theirs, err = os.Stat(raced); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatalf("openGenerationMapLockRacing: %v", err)
	}
	defer func() { _ = g.Close() }()
	ours, err := g.Stat()
	if err != nil || theirs == nil || !os.SameFile(theirs, ours) {
		t.Fatalf("the file another process put in place first was not the one opened (%v)", err)
	}
}

// A legacy candidate's check counts against the writer lease: a lease that
// expires while the check waits publishes nothing.
func TestALeaseThatExpiresDuringALegacyCheckPublishesNothing(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newCacheFake()
		v := &verifiedSpecs{f: f, verdict: clean}
		c := verifyingClient(t, f, v)
		volume, err := c.Create(t.Context(), "acme/api/go", 1<<30)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		lease, fence, err := c.AcquireWriter(t.Context(), "acme/api/go", "writer", time.Minute)
		if err != nil {
			t.Fatalf("AcquireWriter: %v", err)
		}
		candidate, err := c.Snapshot(t.Context(), volume)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		candidate.Filesystem.CheckedAsPublished = false
		v.verdict = func(spec string) (storecontract.Filesystem, error) {
			time.Sleep(2 * time.Minute)

			return clean(spec)
		}

		err = c.PublishCAS(t.Context(), "acme/api/go", "", candidate, lease, fence)
		if !errors.Is(err, storecontract.ErrConflict) {
			t.Fatalf("PublishCAS after the lease expired during the check = %v, want a conflict", err)
		}
		if pointer := f.metadata[c.cacheIndex()][pointerKey("acme/api/go")]; strings.Contains(pointer, candidate.Generation) {
			t.Fatalf("the pointer moved to the candidate: %s", pointer)
		}
	})
}

// The lock file has its mode before it has its name: at the link, nothing is at
// the name yet and the temporary file is already 0660, so a crash can leave no
// lock file another account cannot open.
func TestTheGenerationMapLockHasItsModeBeforeItsName(t *testing.T) {
	t.Parallel()

	lock := filepath.Join(t.TempDir(), "ceph-generation-maps.lock")
	linked := false
	f, err := openGenerationMapLockRacing(lock, func(tmp string) {
		linked = true
		if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("something is at the lock's name before the link: %v", err)
		}
		info, err := os.Stat(tmp)
		if err != nil || info.Mode().Perm() != 0o660 {
			t.Errorf("the temporary file is %v (%v) at the link, want 0660", info.Mode().Perm(), err)
		}
	})
	if err != nil {
		t.Fatalf("openGenerationMapLockRacing: %v", err)
	}
	_ = f.Close()
	if !linked {
		t.Fatal("the file was put in place without a link")
	}
}

// The lock file excludes another process's sweep from a check in progress,
// through the real check and eviction paths: two clients with slots of their
// own, as two processes have, and one lock file.
func TestAnotherProcesssSweepWaitsForACheckThroughTheLockFile(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		lock := filepath.Join(t.TempDir(), "ceph-generation-maps.lock")
		f := newCacheFake()
		v := &verifiedSpecs{f: f, verdict: clean}
		checker := verifyingClient(t, f, v)
		sweeper := cacheClient(t, f)
		checker.checkSlot, sweeper.checkSlot = make(chan struct{}, 1), make(chan struct{}, 1)
		checker.mapLock, sweeper.mapLock = lock, lock
		generation := publishedGeneration(t, checker)
		spec := generation.Handle + "@" + generation.Generation

		// THE ONE CLUSTER BOTH PROCESSES REACH, serialised as a cluster would be:
		// the flock orders the two clients, but the race detector cannot see it.
		var cluster sync.Mutex
		shared := func(ctx context.Context, bin string, args []string) ([]byte, error) {
			cluster.Lock()
			defer cluster.Unlock()

			return f.run(ctx, bin, args)
		}
		checker.run, sweeper.run = shared, shared
		mapped := func() []string {
			cluster.Lock()
			defer cluster.Unlock()

			return mappedSpecs(f)
		}

		hold := make(chan struct{})
		checker.damaged = func(context.Context, string) (bool, error) {
			<-hold

			return false, nil
		}
		checked := make(chan error, 1)
		go func() {
			_, err := checker.GenerationDamaged(t.Context(), "acme/api/go", generation.Generation)
			checked <- err
		}()
		synctest.Wait()
		evicted := make(chan error, 1)
		go func() { evicted <- sweeper.Evict(t.Context(), time.Hour) }()
		time.Sleep(10 * generationMapLockPoll)

		if specs := mapped(); !slices.Contains(specs, spec) {
			t.Fatalf("another process's sweep unmapped the check's device: %v", specs)
		}

		close(hold)
		if err := <-checked; err != nil {
			t.Fatalf("GenerationDamaged: %v", err)
		}
		if err := <-evicted; err != nil {
			t.Fatalf("Evict: %v", err)
		}
		if specs := mapped(); len(specs) != 0 {
			t.Errorf("devices are still mapped: %v", specs)
		}
	})
}

// The check a client built by New runs is e2fsck itself, read-only: a stub on
// PATH answering 12 is damage, and it was asked about the mapped device.
func TestTheBuiltClientsGenerationCheckRunsE2fsck(t *testing.T) {
	// Not parallel: the check finds e2fsck on PATH, and a stub stands in for it,
	// which t.Setenv refuses in a parallel test.
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s ' \"$@\" > " + args + "\nexit 12\n"
	if err := os.WriteFile(filepath.Join(dir, "e2fsck"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	f := newCacheFake()
	v := &verifiedSpecs{f: f, verdict: clean}
	c := verifyingClient(t, f, v)
	generation := publishedGeneration(t, c)
	spec := generation.Handle + "@" + generation.Generation
	var device string
	c.run = func(ctx context.Context, bin string, rbdArgs []string) ([]byte, error) {
		out, err := f.run(ctx, bin, rbdArgs)
		if err == nil && slices.Contains(rbdArgs, "map") && rbdArgs[len(rbdArgs)-1] == spec {
			device = strings.TrimSpace(string(out))
		}

		return out, err
	}

	damaged, err := c.GenerationDamaged(t.Context(), "acme/api/go", generation.Generation)
	if !damaged || err != nil {
		t.Fatalf("GenerationDamaged = %v, %v; want the damage e2fsck's 12 proves", damaged, err)
	}
	ran, err := os.ReadFile(args)
	if err != nil || device == "" || string(ran) != "-f -n "+device+" " {
		t.Fatalf("e2fsck ran with %q (%v), want -f -n on %q, the generation's own map", ran, err, device)
	}
}
