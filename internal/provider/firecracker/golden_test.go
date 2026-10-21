package firecracker

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// The value ceph.GoldenPrefix holds. Asserted as a literal because importing it is
// forbidden here, and the store asserts the same literal: a drift would publish a
// golden under a name the trash purge or the orphan report treats as its own.
func TestTheGoldenDiskPrefixMatchesTheStoreSpelling(t *testing.T) {
	t.Parallel()

	if goldenDiskPrefix != "golden-" {
		t.Errorf("goldenDiskPrefix is %q, and the store publishes goldens as golden-*", goldenDiskPrefix)
	}
}

// plainDisk is a root disk that keeps no goldens: only RootDisk's methods.
type plainDisk struct{ RootDisk }

// A CAPTURE THAT COULD NOT PUBLISH ITS SYSTEM DISK STOPS NO GUEST: a disk that keeps
// no goldens, a snapshot name that cannot name one, and a jail launched before
// billet recorded what it booted are each refused before the pause, with nothing
// flushed or published.
func TestACaptureThatCannotPublishItsDiskLeavesTheGuestRunning(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		snapshot string
		stage    func(t *testing.T, h *harness)
		want     string
	}{
		{
			name: "a disk that keeps no goldens", snapshot: "gold-1",
			stage: func(_ *testing.T, h *harness) { h.p.disk = plainDisk{h.disk} },
			want:  "cannot keep a golden's system disk",
		},
		{
			name: "a snapshot name that cannot name a disk", snapshot: "gold@1",
			stage: func(*testing.T, *harness) {},
			want:  "cannot name a golden's system disk",
		},
		{
			name: "a jail launched before billet recorded what it booted", snapshot: "gold-1",
			stage: func(t *testing.T, h *harness) {
				t.Helper()

				if err := os.WriteFile(h.p.jailFor(theInstance).shapeFile(),
					[]byte(`{"vcpus":2,"memory_bytes":2147483648}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "before billet recorded what it booted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			_, vmm := h.launch(t)
			tc.stage(t, h)

			err := h.p.Capture(t.Context(), theInstance, tc.snapshot)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Capture = %v, want a refusal saying %q", err, tc.want)
			}

			if state := vmm.currentState(); state != stateRunning {
				t.Errorf("a refused capture left the guest %s, want it running", state)
			}

			h.mu.Lock()
			flushed := len(h.flushed)
			h.mu.Unlock()

			if published, _, _ := h.disk.goldenCalls(); len(published) != 0 || flushed != 0 {
				t.Errorf("a refused capture published %v and flushed %d devices", published, flushed)
			}
		})
	}
}

// A FAILED FLUSH PUBLISHES NOTHING. A disk whose writes are not all in the cluster is
// not the one the memory remembers, so it is never published as the golden's.
func TestACaptureWhoseFlushFailsPublishesNoDisk(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)
	h.flushErr = errors.New("input/output error")

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err == nil ||
		!strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("Capture = %v, want the flush's failure", err)
	}

	if published, _, _ := h.disk.goldenCalls(); len(published) != 0 {
		t.Errorf("a disk that failed its flush was published: %v", published)
	}

	assertNoStaging(t, h)
}

// A PUBLICATION OF THE MEMORY THAT FAILS WITHDRAWS THE DISK, so the builder's teardown
// finds its clone and discards it, and a golden no snapshot names never stays; and a
// withdrawal that fails says the disk stayed published.
func TestACaptureWhoseMemoryIsNotPublishedWithdrawsItsDisk(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		withdrawErr error
		want        string
	}{
		{"withdrawn", nil, "already published"},
		{"left published", errors.New("rbd: rename refused"), "stays published"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			// THE NAME IS TAKEN, so the memory's publication fails after the disk's.
			publish(t, h, "gold-1", 0o444)
			h.launch(t)
			h.disk.withdrawErr = tc.withdrawErr

			err := h.p.Capture(t.Context(), theInstance, "gold-1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Capture = %v, want a failure saying %q", err, tc.want)
			}

			ref := "golden-gold-1@" + fakeGeneration

			published, withdrawn, _ := h.disk.goldenCalls()
			if !slices.Equal(published, []string{theInstance + " -> " + ref}) ||
				!slices.Equal(withdrawn, []string{ref + " -> " + theInstance}) {
				t.Errorf("the disk was published %v and withdrawn %v, want it withdrawn back to "+
					"the builder's clone", published, withdrawn)
			}
		})
	}
}

// A RESTORE'S DISK IS THE ONE ITS SNAPSHOT NAMES, cloned from the golden, never the
// image the caller named; and the kernel is the one of the generation the golden was
// built from, though the tier names an alias for it.
func TestARestoreClonesTheDiskItsSnapshotNames(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	publish(t, h, "gold-1", 0o444)

	spec := aRestoreSpec()
	spec.Image = "ubuntu-2404-x64@" + VerifiedAlias

	h.mu.Lock()
	h.onJailer = func(id string) { h.serveVMM(t, id) }
	h.mu.Unlock()

	if _, err := h.p.Restore(t.Context(), spec, "gold-1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	_, _, cloned := h.disk.goldenCalls()
	if !slices.Equal(cloned, []string{testGoldenDisk + " -> " + spec.Name}) {
		t.Errorf("the restore cloned %v, want the snapshot's system disk", cloned)
	}

	if from := h.disk.clonedFrom(); from != "" {
		t.Errorf("the restore also cloned an image-pool generation, %s", from)
	}

	h.disk.mu.Lock()
	asked := append([]string(nil), h.disk.kernelAsked...)
	h.disk.mu.Unlock()

	if !slices.Equal(asked, []string{aRestoreSpec().Image}) {
		t.Errorf("the restore looked up the kernels of %v, want the golden's own generation %s",
			asked, aRestoreSpec().Image)
	}
}

// A SNAPSHOT THAT NAMES NO DISK, OR ANOTHER IMAGE'S, IS REFUSED BEFORE ANYTHING IS
// CLAIMED: no jail, no clone, no VMM.
func TestARestoreOfAGoldenThatIsNotThisTiersIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		edit func(*compatibility)
	}{
		{"another image's golden", func(c *compatibility) { c.Image = "debian-12@g1" }},
		{"no system disk", func(c *compatibility) { c.Disk = "" }},
		{"no generation it was cloned from", func(c *compatibility) { c.Image = "" }},
		{"a generation with no image", func(c *compatibility) { c.Image = "@g1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			dir := publish(t, h, "gold-1", 0o444)
			writeCompatibility(t, h, dir, tc.edit)

			_, _, err := h.restoreWith(t, "gold-1")
			if !errors.Is(err, errSnapshotIncompatible) {
				t.Fatalf("Restore = %v, want errSnapshotIncompatible", err)
			}

			if _, _, cloned := h.disk.goldenCalls(); len(cloned) != 0 {
				t.Errorf("a refused restore cloned %v", cloned)
			}

			if _, err := os.Lstat(h.p.jailFor(aRestoreSpec().Name).dir()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused restore claimed a jail: %v", err)
			}
		})
	}
}
