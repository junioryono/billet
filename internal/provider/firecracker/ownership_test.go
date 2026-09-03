package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClaimMakesTheDeploymentMarkerDurableBeforeReturning(t *testing.T) {
	t.Parallel()

	var synced []string
	h := newHarness(t, withDurability(
		func(file *os.File) error {
			synced = append(synced, "file:"+filepath.Base(file.Name()))

			return file.Sync()
		},
		func(path string) error {
			synced = append(synced, "dir:"+path)

			return syncTestDirectory(path)
		},
	))
	j := h.p.jailFor(theInstance)

	if err := h.p.claim(j); err != nil {
		t.Fatalf("claim: %v", err)
	}

	want := ownerDurabilityOperations(j)
	if strings.Join(synced, "\n") != strings.Join(want, "\n") {
		t.Fatalf("durability operations = %q, want %q", synced, want)
	}
}

func TestClaimMakesNewChrootAncestorsDurable(t *testing.T) {
	t.Parallel()

	var synced []string
	h := newHarness(t, withDurability(
		func(file *os.File) error {
			synced = append(synced, "file:"+filepath.Base(file.Name()))

			return file.Sync()
		},
		func(path string) error {
			synced = append(synced, "dir:"+path)

			return syncTestDirectory(path)
		},
	))
	existing := t.TempDir()
	h.p.cfg.ChrootBase = filepath.Join(existing, "site", "jailer")
	j := h.p.jailFor(theInstance)

	if err := h.p.claim(j); err != nil {
		t.Fatalf("claim: %v", err)
	}

	want := ownerDurabilityOperations(j)
	if strings.Join(synced, "\n") != strings.Join(want, "\n") {
		t.Fatalf("durability operations = %q, want %q", synced, want)
	}
}

func TestClaimSyncsTheBaseWhenTheExecDirectoryAlreadyExists(t *testing.T) {
	t.Parallel()

	var synced []string
	h := newHarness(t, withDurability(
		nil,
		func(path string) error {
			synced = append(synced, "dir:"+path)

			return syncTestDirectory(path)
		},
	))
	j := h.p.jailFor(theInstance)
	if err := os.MkdirAll(filepath.Dir(j.dir()), 0o700); err != nil {
		t.Fatalf("stage an executable directory another process made visible: %v", err)
	}

	if err := h.p.claim(j); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if !slices.Contains(synced, "dir:"+j.base) {
		t.Fatalf("claim treated a visible executable directory as durable: syncs=%q", synced)
	}
}

func TestResourceRecordIsDurableBeforeAllocationContinues(t *testing.T) {
	t.Parallel()

	var synced []string
	h := newHarness(t, withDurability(
		func(file *os.File) error {
			synced = append(synced, "file:"+filepath.Base(file.Name()))

			return file.Sync()
		},
		func(path string) error {
			synced = append(synced, "dir:"+path)

			return syncTestDirectory(path)
		},
	))
	j := h.p.jailFor(theInstance)
	if err := os.MkdirAll(j.dir(), 0o700); err != nil {
		t.Fatalf("make jail: %v", err)
	}

	if err := h.p.writeResources(j, resources{UID: 900001, GID: 900001, Tap: "billet0"}); err != nil {
		t.Fatalf("writeResources: %v", err)
	}

	if len(synced) != 2 || !strings.HasPrefix(synced[0], "file:.resources-") || synced[1] != "dir:"+j.dir() {
		t.Fatalf("durability operations = %q, want the staged file then %s", synced, j.dir())
	}
	if _, err := os.Stat(j.resourceFile()); err != nil {
		t.Fatalf("resource record was not installed before the directory sync returned: %v", err)
	}
}

func TestAFailedOwnerSyncLeavesNoAmbiguousJail(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withDurability(
		func(*os.File) error { return fmt.Errorf("injected owner sync failure") },
		nil,
	))
	j := h.p.jailFor(theInstance)

	if err := h.p.claim(j); err == nil || !strings.Contains(err.Error(), "injected owner sync failure") {
		t.Fatalf("claim error = %v, want the durability failure", err)
	}
	if _, err := os.Stat(j.dir()); !os.IsNotExist(err) {
		t.Fatalf("failed claim left an ownerless jail that blocks inventory: %v", err)
	}
}

func TestLaunchAllocatesNothingUntilTheOwnerIsDurable(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withDurability(
		func(file *os.File) error {
			if filepath.Base(file.Name()) == "billet-owner" {
				return fmt.Errorf("injected owner sync failure")
			}

			return file.Sync()
		},
		nil,
	))

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil ||
		!strings.Contains(err.Error(), "injected owner sync failure") {
		t.Fatalf("Launch error = %v, want the owner durability failure", err)
	}
	if got := h.disk.clonedFrom(); got != "" {
		t.Fatalf("Launch cloned %s before its deployment owner was durable", got)
	}
	if got := h.commands(); len(got) != 0 {
		t.Fatalf("Launch changed host resources before its deployment owner was durable: %v", got)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !os.IsNotExist(err) {
		t.Fatalf("failed claim left a lifecycle reservation behind: %v", err)
	}
}

func TestLifecycleCleanupSyncsTheRemovalAfterAPublishFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	wantFailure := fmt.Errorf("injected lifecycle directory sync failure")
	var lifecycleSyncs int
	h.p.syncDir = func(path string) error {
		if path == h.p.lifecycleDir() {
			lifecycleSyncs++
			if lifecycleSyncs == 1 {
				return wantFailure
			}
		}

		return syncTestDirectory(path)
	}

	err := h.p.writeLifecycleOwner(theInstance)
	if err == nil || !strings.Contains(err.Error(), wantFailure.Error()) {
		t.Fatalf("writeLifecycleOwner error = %v, want %v", err, wantFailure)
	}
	if lifecycleSyncs != 2 {
		t.Fatalf("lifecycle directory syncs = %d, want publish failure plus removal sync", lifecycleSyncs)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !os.IsNotExist(err) {
		t.Fatalf("failed lifecycle publication left its record behind: %v", err)
	}
}

func TestLifecycleAuthorityIsCompleteBeforeItsCanonicalNameIsVisible(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	var ownerSyncs, execSyncs int
	h.p.syncFile = func(file *os.File) error {
		if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lifecycle authority became visible before its staged record was complete: %v", err)
		}
		switch filepath.Dir(file.Name()) {
		case h.p.lifecycleDir():
			ownerSyncs++
		case h.p.lifecycleExecDir():
			execSyncs++
		}

		return file.Sync()
	}

	if err := h.p.writeLifecycleOwner(theInstance); err != nil {
		t.Fatalf("writeLifecycleOwner: %v", err)
	}

	owner, err := lifecycleOwnerOf(h.p.lifecycleFile(theInstance))
	if err != nil {
		t.Fatalf("read the published lifecycle authority: %v", err)
	}
	execName, err := lifecycleExecOf(h.p.lifecycleExecFile(theInstance))
	if err != nil {
		t.Fatalf("read the published executable identity: %v", err)
	}
	if owner != testDeployment || execName != h.p.execName {
		t.Fatalf("published lifecycle authority = owner %q, executable %q", owner, execName)
	}
	if ownerSyncs != 1 || execSyncs != 1 {
		t.Fatalf("lifecycle syncs = owner %d, executable %d; want one of each", ownerSyncs, execSyncs)
	}
}

func TestFailedLifecycleRecordSyncPublishesNoAuthority(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	wantFailure := errors.New("injected staged lifecycle sync failure")
	h.p.syncFile = func(file *os.File) error {
		if filepath.Dir(file.Name()) == h.p.lifecycleDir() {
			return wantFailure
		}

		return file.Sync()
	}

	if err := h.p.writeLifecycleOwner(theInstance); !errors.Is(err, wantFailure) {
		t.Fatalf("writeLifecycleOwner error = %v, want %v", err, wantFailure)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed staged sync published lifecycle authority: %v", err)
	}
	if _, err := os.Stat(h.p.lifecycleExecFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed authority publication retained executable metadata: %v", err)
	}
}

func TestLifecycleAuthorityKeepsTheRollbackCompatibleOwnerFormat(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.p.writeLifecycleOwner(theInstance); err != nil {
		t.Fatalf("writeLifecycleOwner: %v", err)
	}

	raw, err := os.ReadFile(h.p.lifecycleFile(theInstance))
	if err != nil {
		t.Fatalf("read lifecycle authority: %v", err)
	}
	if got, want := string(raw), testDeployment+"\n"; got != want {
		t.Fatalf("lifecycle authority = %q, want rollback-compatible %q", got, want)
	}
}

func TestLifecycleAuthorityMigratesThePredecessorRecordFormat(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := os.MkdirAll(h.p.lifecycleDir(), 0o700); err != nil {
		t.Fatalf("make lifecycle directory: %v", err)
	}
	if err := os.WriteFile(h.p.lifecycleFile(theInstance),
		[]byte(testDeployment+"\n"+h.p.execName+"\n"), 0o600); err != nil {
		t.Fatalf("stage predecessor lifecycle authority: %v", err)
	}

	authorized, err := h.p.authorizeLifecycle(theInstance, h.p.jailFor(theInstance), true)
	if err != nil {
		t.Fatalf("authorize predecessor lifecycle authority: %v", err)
	}
	if !authorized {
		t.Fatal("the predecessor lifecycle authority did not authorize its own jail")
	}
	if execName, err := lifecycleExecOf(h.p.lifecycleExecFile(theInstance)); err != nil ||
		execName != h.p.execName {
		t.Fatalf("migrated executable = %q, err=%v; want %q", execName, err, h.p.execName)
	}
}

func TestPredecessorAuthorityReplacesAStaleSidecarFromAnInterruptedPublication(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.p.writeLifecycleOwner(theInstance); err != nil {
		t.Fatalf("stage interrupted current publication: %v", err)
	}
	if err := os.Remove(h.p.lifecycleFile(theInstance)); err != nil {
		t.Fatalf("leave only the stale executable sidecar: %v", err)
	}

	rollbackExec := "firecracker-after-rollback"
	if err := os.WriteFile(h.p.lifecycleFile(theInstance),
		[]byte(testDeployment+"\n"+rollbackExec+"\n"), 0o600); err != nil {
		t.Fatalf("stage the rollback binary's lifecycle authority: %v", err)
	}
	j := jail{base: h.p.cfg.ChrootBase, execName: rollbackExec, id: theInstance}
	authorized, err := h.p.authorizeLifecycle(theInstance, j, true)
	if err != nil {
		t.Fatalf("authorize the rollback relaunch: %v", err)
	}
	if !authorized {
		t.Fatal("the rollback relaunch did not retain teardown authority")
	}
	if execName, err := lifecycleExecOf(h.p.lifecycleExecFile(theInstance)); err != nil ||
		execName != rollbackExec {
		t.Fatalf("recovered executable = %q, err=%v; want %q", execName, err, rollbackExec)
	}
}

func TestLifecycleExecutableNamesCannotBreakTheirRecordBoundary(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.p.writeLifecycleRecord(theInstance, "firecracker\nother"); err == nil {
		t.Fatal("a newline-containing executable name was accepted for lifecycle authority")
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an invalid executable name published lifecycle authority: %v", err)
	}
}

func TestProviderRejectsAResolvedBinaryNameThatCannotRoundTrip(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	badName := "firecracker" + string([]byte{'\n'}) + "other"
	badBinary := filepath.Join(filepath.Dir(h.p.execPath), badName)
	if err := os.WriteFile(badBinary, []byte("#!/bin/true\n"), 0o700); err != nil {
		t.Fatalf("write the invalidly named binary: %v", err)
	}
	cfg := h.p.cfg
	cfg.BinaryPath = badBinary

	if _, err := New(testDeployment, cfg, h.disk); err == nil {
		t.Fatal("New accepted a resolved binary name lifecycle recovery cannot round-trip")
	}
}

func TestAbsentLifecycleAuthorityRetriesAFailedReleaseSync(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.p.writeLifecycleOwner(theInstance); err != nil {
		t.Fatalf("stage lifecycle authority: %v", err)
	}

	wantFailure := fmt.Errorf("injected lifecycle release sync failure")
	var lifecycleSyncs int
	h.p.syncDir = func(path string) error {
		if path == h.p.lifecycleDir() {
			lifecycleSyncs++
			if lifecycleSyncs == 1 {
				return wantFailure
			}
		}

		return syncTestDirectory(path)
	}

	if err := h.p.releaseLifecycle(theInstance); err == nil ||
		!strings.Contains(err.Error(), wantFailure.Error()) {
		t.Fatalf("releaseLifecycle error = %v, want %v", err, wantFailure)
	}
	authorized, err := h.p.authorizeLifecycle(theInstance, jail{}, false)
	if err != nil {
		t.Fatalf("retry absent lifecycle authority: %v", err)
	}
	if authorized {
		t.Fatal("an absent lifecycle record authorized destruction")
	}
	if lifecycleSyncs != 2 {
		t.Fatalf("lifecycle directory syncs = %d, want failed release plus retry", lifecycleSyncs)
	}
}

func TestLifecycleLockWaitHonorsCancellation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first, err := h.p.lockLifecycle(t.Context())
	if err != nil {
		t.Fatalf("take the first lifecycle lock: %v", err)
	}
	defer func() { _ = first.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if second, err := h.p.lockLifecycle(ctx); err == nil {
		_ = second.Close()
		t.Fatal("a lifecycle lock waiter ignored its canceled context")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lifecycle lock wait error = %v, want context deadline", err)
	}
}

func TestLifecycleLockRefusesAnAlreadyCanceledContext(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lock, err := h.p.lockLifecycle(ctx)
	if err == nil {
		_ = lock.Close()
		t.Fatal("the lifecycle lock admitted an already-canceled operation")
	}
	if lock != nil {
		t.Fatal("a canceled lifecycle operation retained the host-wide lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lifecycle lock error = %v, want context cancellation", err)
	}
}

func TestCanceledDestroyDoesNotActOnALiveMicroVM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)
	j := h.p.jailFor(theInstance)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := h.p.Destroy(ctx, theInstance); !errors.Is(err, context.Canceled) {
		t.Fatalf("Destroy error = %v, want context cancellation", err)
	}
	if _, err := os.Stat(j.dir()); err != nil {
		t.Fatalf("canceled Destroy removed the live microVM jail: %v", err)
	}
	if got := h.disk.discards(); len(got) != 0 {
		t.Fatalf("canceled Destroy discarded the live microVM disk: %v", got)
	}
}

func TestListReconcilesALifecycleRecordWhoseJailWasNeverCreated(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.p.writeLifecycleOwner(theInstance); err != nil {
		t.Fatalf("stage an interrupted launch reservation: %v", err)
	}

	instances, err := h.p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("List reported a lifecycle-only record as compute: %+v", instances)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !os.IsNotExist(err) {
		t.Fatalf("List left an interrupted launch permanently reserved: %v", err)
	}
	if got := h.disk.discards(); len(got) != 1 || got[0] != theInstance {
		t.Fatalf("List did not conservatively reconcile the interrupted launch disk: %v", got)
	}
}

func TestListReconcilesTheOwnerOnlyPredecessorCrashState(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := os.MkdirAll(h.p.lifecycleDir(), 0o700); err != nil {
		t.Fatalf("make lifecycle directory: %v", err)
	}
	if err := os.WriteFile(h.p.lifecycleFile(theInstance), []byte(testDeployment+"\n"), 0o600); err != nil {
		t.Fatalf("stage the owner-only predecessor record: %v", err)
	}

	instances, err := h.p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("List reported an owner-only crash record as compute: %+v", instances)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("List left the predecessor's interrupted launch reserved: %v", err)
	}
}

func TestOwnerOnlyRecoveryRetainsAuthorityUntilEveryHistoricalCgroupIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := os.MkdirAll(h.p.lifecycleDir(), 0o700); err != nil {
		t.Fatalf("make lifecycle directory: %v", err)
	}
	if err := os.WriteFile(h.p.lifecycleFile(theInstance), []byte(testDeployment+"\n"), 0o600); err != nil {
		t.Fatalf("stage the owner-only predecessor record: %v", err)
	}

	cgroupBase := t.TempDir()
	wantDirs := []string{"firecracker-v1.15.0", "firecracker-v1.16.1"}
	for _, dir := range wantDirs {
		if err := os.Mkdir(filepath.Join(cgroupBase, dir), 0o700); err != nil {
			t.Fatalf("make historical cgroup directory: %v", err)
		}
	}
	mounts := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(mounts, []byte("cgroup2 "+cgroupBase+" cgroup2 rw 0 0\n"), 0o600); err != nil {
		t.Fatalf("write the test mount table: %v", err)
	}
	h.p.procMountsPath = mounts
	wantFailure := errors.New("legacy cgroup is still occupied")
	var removed []string
	h.p.removeCgroupAtFn = func(root string, j jail) error {
		if root != cgroupBase {
			t.Fatalf("cgroup root = %q, want discovered %q", root, cgroupBase)
		}
		removed = append(removed, j.execName)
		if j.execName == "firecracker-v1.15.0" {
			return wantFailure
		}

		return nil
	}

	if _, err := h.p.List(t.Context()); !errors.Is(err, wantFailure) {
		t.Fatalf("List error = %v, want %v", err, wantFailure)
	}
	if !slices.Equal(removed, wantDirs) {
		t.Fatalf("historical cgroup removals = %v, want %v", removed, wantDirs)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); err != nil {
		t.Fatalf("failed legacy cgroup cleanup retired its retry authority: %v", err)
	}
	if got := h.disk.discards(); len(got) != 0 {
		t.Fatalf("failed legacy cgroup cleanup discarded the root disk: %v", got)
	}

	removed = nil
	h.p.removeCgroupAtFn = func(root string, j jail) error {
		removed = append(removed, j.execName)

		return nil
	}
	if _, err := h.p.List(t.Context()); err != nil {
		t.Fatalf("List retry: %v", err)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful legacy cgroup retry retained lifecycle authority: %v", err)
	}
	if !slices.Equal(removed, wantDirs) {
		t.Fatalf("retried cgroup removals = %v, want %v", removed, wantDirs)
	}
}

func TestOwnerOnlyRecoveryPreservesAuthorityWhenTheCgroupHierarchyIsUnverifiable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := os.MkdirAll(h.p.lifecycleDir(), 0o700); err != nil {
		t.Fatalf("make lifecycle directory: %v", err)
	}
	if err := os.WriteFile(h.p.lifecycleFile(theInstance), []byte(testDeployment+"\n"), 0o600); err != nil {
		t.Fatalf("stage the owner-only predecessor record: %v", err)
	}
	mounts := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(mounts, []byte("proc /proc proc rw 0 0\n"), 0o600); err != nil {
		t.Fatalf("write the test mount table: %v", err)
	}
	h.p.procMountsPath = mounts

	if _, err := h.p.List(t.Context()); err == nil {
		t.Fatal("List retired legacy authority without a verifiable cgroup-v2 hierarchy")
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); err != nil {
		t.Fatalf("unverifiable cgroup hierarchy retired lifecycle authority: %v", err)
	}
	if got := h.disk.discards(); len(got) != 0 {
		t.Fatalf("unverifiable cgroup hierarchy discarded the root disk: %v", got)
	}
}

func TestAbsentLifecycleSidecarRetriesAFailedReleaseSync(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := os.MkdirAll(h.p.lifecycleExecDir(), 0o700); err != nil {
		t.Fatalf("make lifecycle executable directory: %v", err)
	}
	if err := os.WriteFile(h.p.lifecycleExecFile(theInstance), []byte(h.p.execName+"\n"), 0o600); err != nil {
		t.Fatalf("stage lifecycle executable: %v", err)
	}

	wantFailure := errors.New("injected executable directory sync failure")
	var execSyncs int
	h.p.syncDir = func(path string) error {
		if path == h.p.lifecycleExecDir() {
			execSyncs++
			if execSyncs == 1 {
				return wantFailure
			}
		}

		return syncTestDirectory(path)
	}

	if err := h.p.removeLifecycleExec(theInstance); !errors.Is(err, wantFailure) {
		t.Fatalf("first removal error = %v, want %v", err, wantFailure)
	}
	if err := h.p.removeLifecycleExec(theInstance); err != nil {
		t.Fatalf("retry absent executable removal: %v", err)
	}
	if execSyncs != 2 {
		t.Fatalf("executable directory syncs = %d, want failed removal plus absent retry", execSyncs)
	}
}

func TestListSkipsAnUnreadableLifecycleOnlyRecord(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := os.MkdirAll(h.p.lifecycleDir(), 0o700); err != nil {
		t.Fatalf("make lifecycle directory: %v", err)
	}
	if err := os.WriteFile(h.p.lifecycleFile(theInstance), nil, 0o600); err != nil {
		t.Fatalf("stage an interrupted lifecycle publication: %v", err)
	}
	staged := filepath.Join(h.p.lifecycleDir(), "."+theInstance+"-interrupted")
	if err := os.WriteFile(staged, []byte(testDeployment+"\n"), 0o600); err != nil {
		t.Fatalf("stage the unpublished lifecycle record: %v", err)
	}

	instances, err := h.p.List(t.Context())
	if err != nil {
		t.Fatalf("List let an unreadable lifecycle-only record invalidate jail inventory: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("List reported an unreadable lifecycle-only record as compute: %+v", instances)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); err != nil {
		t.Fatalf("List destroyed the unreadable authority it could not attribute: %v", err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("List destroyed the unpublished record it could not attribute: %v", err)
	}
	if got := h.disk.discards(); len(got) != 0 {
		t.Fatalf("List destroyed resources using unreadable lifecycle authority: %v", got)
	}
}

func TestLifecycleCleanupRetriesTheRecordedCgroupBeforeRetiringAuthority(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	recordedExec := h.p.execName
	if err := h.p.writeLifecycleOwner(theInstance); err != nil {
		t.Fatalf("stage lifecycle authority: %v", err)
	}

	h.p.execName = "firecracker-after-upgrade"
	wantFailure := errors.New("cgroup is still occupied")
	var removed jail
	h.p.removeCgroupFn = func(j jail) error {
		removed = j

		return wantFailure
	}

	if _, err := h.p.List(t.Context()); !errors.Is(err, wantFailure) {
		t.Fatalf("List error = %v, want the cgroup cleanup failure", err)
	}
	if removed.execName != recordedExec {
		t.Fatalf("cgroup cleanup used executable %q, want recorded %q", removed.execName, recordedExec)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); err != nil {
		t.Fatalf("failed cgroup cleanup retired its only retry authority: %v", err)
	}
	if got := h.disk.discards(); len(got) != 0 {
		t.Fatalf("cleanup discarded a disk while its cgroup could still contain a VMM: %v", got)
	}

	h.p.removeCgroupFn = func(j jail) error {
		removed = j

		return nil
	}
	if _, err := h.p.List(t.Context()); err != nil {
		t.Fatalf("List retry: %v", err)
	}
	if _, err := os.Stat(h.p.lifecycleFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful cgroup retry retained lifecycle authority: %v", err)
	}
	if _, err := os.Stat(h.p.lifecycleExecFile(theInstance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful cgroup retry retained executable metadata: %v", err)
	}
}

func TestLaunchAllocatesNothingWhenAnAncestorCannotBeSynced(t *testing.T) {
	t.Parallel()

	var failPath string
	h := newHarness(t, withDurability(nil, func(path string) error {
		if path == failPath {
			return fmt.Errorf("injected ancestor sync failure")
		}

		return syncTestDirectory(path)
	}))
	j := h.p.jailFor(theInstance)
	failPath = j.base

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil ||
		!strings.Contains(err.Error(), "injected ancestor sync failure") {
		t.Fatalf("Launch error = %v, want the ancestor durability failure", err)
	}
	if got := h.disk.clonedFrom(); got != "" {
		t.Fatalf("Launch cloned %s before its directory chain was durable", got)
	}
	if _, err := os.Stat(j.dir()); !os.IsNotExist(err) {
		t.Fatalf("failed claim left an ambiguous jail: %v", err)
	}
}

func TestAJailWithoutADeploymentMarkerCannotBeClaimedOrDestroyed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	j := h.p.jailFor(theInstance)
	if err := os.MkdirAll(j.root(), 0o700); err != nil {
		t.Fatalf("stage an ownerless jail: %v", err)
	}
	h.writeExitedPID(theInstance)

	if _, found, err := h.p.Find(t.Context(), theInstance); err == nil || found {
		t.Fatalf("Find trusted a jail with no deployment marker: found=%v err=%v", found, err)
	} else if !strings.Contains(err.Error(), "deployment") {
		t.Fatalf("Find returned an unrelated error: %v", err)
	}

	if _, err := h.p.List(t.Context()); err == nil {
		t.Fatal("List returned an authoritative inventory while a jail had no deployment marker")
	} else if !strings.Contains(err.Error(), "deployment") {
		t.Fatalf("List returned an unrelated error: %v", err)
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("Destroy acted on a jail whose deployment ownership was unknown")
	} else if !strings.Contains(err.Error(), "deployment") {
		t.Fatalf("Destroy returned an unrelated error: %v", err)
	}

	if _, err := os.Stat(j.dir()); err != nil {
		t.Errorf("Destroy removed the ownerless jail: %v", err)
	}
	if got := h.disk.discards(); len(got) != 0 {
		t.Errorf("Destroy discarded a disk whose deployment ownership was unknown: %v", got)
	}
	for _, command := range h.commands() {
		if len(command.args) >= 2 && command.args[0] == "link" && command.args[1] == "del" {
			t.Error("Destroy removed networking whose deployment ownership was unknown")
		}
	}
}

func TestDestroyWithoutDeploymentAuthorityDoesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy an unknown microVM: %v", err)
	}
	if got := h.disk.discards(); len(got) != 0 {
		t.Fatalf("Destroy discarded by lease name without deployment authority: %v", got)
	}
}

func TestMalformedOwnerRecordsCannotAuthorizeDestroy(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		" " + testDeployment + "\n",
		testDeployment + " \n",
		testDeployment,
		testDeployment + "\nextra\n",
		strings.ToUpper(testDeployment) + "\n",
		testDeployment[:len(testDeployment)-1] + "\n",
		testDeployment[:len(testDeployment)-1] + "\x00\n",
		testDeployment[:len(testDeployment)-1] + "\xff\n",
	} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			j := h.p.jailFor(theInstance)
			if err := os.MkdirAll(j.root(), 0o700); err != nil {
				t.Fatalf("stage jail: %v", err)
			}
			if err := os.WriteFile(j.ownerFile(), []byte(raw), 0o600); err != nil {
				t.Fatalf("write malformed owner record: %v", err)
			}
			h.writeExitedPID(theInstance)

			if _, err := h.p.Destroy(t.Context(), theInstance); err == nil {
				t.Fatal("Destroy accepted a malformed deployment owner record")
			}
			if got := h.disk.discards(); len(got) != 0 {
				t.Fatalf("Destroy discarded with malformed deployment authority: %v", got)
			}
			if _, err := os.Stat(j.dir()); err != nil {
				t.Fatalf("Destroy removed a jail with malformed deployment authority: %v", err)
			}
		})
	}
}

func syncTestDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	return dir.Close()
}

func ownerDurabilityOperations(j jail) []string {
	operations := []string{
		"file:" + filepath.Base(j.ownerFile()),
		"dir:" + j.dir(),
		"dir:" + filepath.Dir(j.dir()),
	}

	for current := j.base; ; current = filepath.Dir(current) {
		operations = append(operations, "dir:"+current)
		if filepath.Dir(current) == current {
			return operations
		}
	}
}
