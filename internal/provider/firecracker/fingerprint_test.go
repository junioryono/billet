package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// A HOST'S CPU IS ITS IDENTITY AND ITS FEATURES, read the same however the cores are
// listed: two cores reporting the same flags in a different order are one CPU, and a
// core whose microcode differs is another identity beside the first, never dropped.
func TestTheCPUIdentityIsEveryDistinctCoreAndItsFeatures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	same, sameFeatures, err := cpuIdentity(write("same", aCPUInfo))
	if err != nil {
		t.Fatalf("cpuIdentity: %v", err)
	}

	if strings.Count(same, "vendor_id=") != 1 || strings.Contains(same, "MHz") {
		t.Errorf("two identical cores read as %q, want one identity and no frequency", same)
	}

	for _, want := range []string{"vendor_id=AuthenticAMD", "cpu family=25", "model=1", "stepping=1", "microcode=0xa0011d5"} {
		if !strings.Contains(same, want) {
			t.Errorf("the identity %q leaves out %s", same, want)
		}
	}

	updated, updatedFeatures, err := cpuIdentity(write("updated",
		strings.Replace(aCPUInfo, "microcode\t: 0xa0011d5\ncpu MHz\t\t: 3100.000", "microcode\t: 0xa0011d6\ncpu MHz\t\t: 3100.000", 1)))
	if err != nil {
		t.Fatalf("cpuIdentity: %v", err)
	}

	if strings.Count(updated, "vendor_id=") != 2 || updated == same {
		t.Errorf("a core with other microcode read as %q, want both identities", updated)
	}

	if updatedFeatures != sameFeatures {
		t.Error("the same flags in another order changed the feature digest")
	}

	_, fewer, err := cpuIdentity(write("fewer", strings.ReplaceAll(aCPUInfo, " avx2", "")))
	if err != nil {
		t.Fatalf("cpuIdentity: %v", err)
	}

	if fewer == sameFeatures {
		t.Error("a CPU without avx2 has the same feature digest as one with it")
	}

	arm := "processor\t: 0\nBogoMIPS\t: 50.00\nFeatures\t: fp asimd aes\nCPU implementer\t: 0x41\nCPU part\t: 0xd0c\nCPU revision\t: 1\n"
	armIdentity, _, err := cpuIdentity(write("arm", arm))
	if err != nil || !strings.Contains(armIdentity, "CPU part=0xd0c") {
		t.Errorf("an arm64 core read as %q (%v), want its implementer and part", armIdentity, err)
	}

	if _, _, err := cpuIdentity(write("empty", "nothing here\n")); err == nil {
		t.Error("a cpuinfo naming no processor was accepted as an identity")
	}
}

// A FILE'S DIGEST FOLLOWS THE FILE. Reading it again costs nothing, and a file
// rewritten in place, even to the same size, is digested again.
func TestAFileDigestIsRecomputedWhenTheFileChanges(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(path, []byte("kernel one"), 0o600); err != nil {
		t.Fatal(err)
	}

	// SETTLED, by a clock an hour ahead, so the first digest is remembered.
	d := fileDigests{now: func() time.Time { return time.Now().Add(time.Hour) }}

	first, err := d.of(path)
	if err != nil {
		t.Fatalf("of: %v", err)
	}

	if len(d.seen) != 1 {
		t.Fatalf("a settled file's digest was not remembered: %d entries", len(d.seen))
	}

	if again, err := d.of(path); err != nil || again != first {
		t.Fatalf("a second read = %q (%v), want %q", again, err, first)
	}

	// THE REWRITE, given a modification time of its own: two writes inside one
	// timestamp tick are TestADigestOfAFileJustWrittenIsNotRemembered's case.
	if err := os.WriteFile(path, []byte("kernel two"), 0o600); err != nil {
		t.Fatal(err)
	}

	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	if changed, err := d.of(path); err != nil || changed == first {
		t.Errorf("a rewritten file kept the digest %q (%v)", changed, err)
	}

	if _, err := d.of(t.TempDir()); err == nil {
		t.Error("a directory was digested as a file")
	}
}

// A LINK MADE WHILE A FILE IS READ IS NOT A CHANGE, and a write is. Every launch
// links the guest kernel into its jail, which moves the inode's change time; on the
// reference node eight of ten concurrent restores were refused for it (2026-10-22).
func TestALinkMadeWhileAFileIsDigestedIsNotAChange(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, path string)
		refused bool
	}{
		{"a hard link, as a launch makes", func(t *testing.T, path string) {
			t.Helper()

			if err := os.Link(path, path+".jailed"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a write of more bytes", func(t *testing.T, path string) {
			t.Helper()

			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			if _, err := f.WriteString(" and more"); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a rewrite of the same size, later", func(t *testing.T, path string) {
			t.Helper()

			if err := os.WriteFile(path, []byte("kernel 2"), 0o600); err != nil {
				t.Fatal(err)
			}

			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "vmlinux")
			if err := os.WriteFile(path, []byte("kernel 1"), 0o600); err != nil {
				t.Fatal(err)
			}

			d := fileDigests{afterRead: func() { tc.change(t, path) }}

			got, err := d.of(path)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "changed while it was being read") {
					t.Fatalf("of = %q, %v, want it refused as changed while it was read", got, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("of: %v, want the digest of the bytes read", err)
			}

			if want := digestOf("kernel 1"); got != want {
				t.Errorf("of = %s, want %s", got, want)
			}
		})
	}
}

// A SNAPSHOT TAKEN ANYWHERE ELSE IS REFUSED BEFORE IT IS LOADED: each field of the
// record that differs from this host, and a record that is missing, of another
// format, or not a record at all. Nothing is linked and the VMM is never asked to load.
func TestARestoreRefusesASnapshotTakenWhereItCannotRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, h *harness, dir string)
	}{
		{"another CPU", editRecord(func(c *compatibility) { c.CPU = "vendor_id=GenuineIntel" })},
		{"other CPU features", editRecord(func(c *compatibility) { c.CPUFeatures = "sha256:00" })},
		{"another host kernel", editRecord(func(c *compatibility) { c.HostKernel = "5.10.0 #1" })},
		{"another Firecracker build", editRecord(func(c *compatibility) { c.VMM = "sha256:00" })},
		{"another guest kernel", editRecord(func(c *compatibility) { c.GuestKernel = "sha256:00" })},
		{"another record format", editRecord(func(c *compatibility) { c.Format = compatibilityFormat + 1 })},
		{"a format 1 record, which names no disk", editRecord(func(c *compatibility) { c.Format = 1 })},
		{"no record", func(t *testing.T, _ *harness, dir string) {
			t.Helper()

			if err := os.Remove(filepath.Join(dir, compatibilityFile)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a record with a field this billet does not know", hostRecordAnd(func(raw []byte) []byte {
			return append(raw[:len(raw)-1], []byte(`,"tpm":"on"}`)...)
		})},
		{"a record that is not one", writeRaw(`not json`)},
		{"a record too large to be one", hostRecordAnd(func(raw []byte) []byte {
			return append(raw, []byte(strings.Repeat(" ", maxCompatibilityRecord))...)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			dir := publish(t, h, "gold-1", 0o444)
			tc.stage(t, h, dir)

			_, vmm, err := h.restoreWith(t, "gold-1")
			if !errors.Is(err, errSnapshotIncompatible) {
				t.Fatalf("Restore = %v, want errSnapshotIncompatible", err)
			}

			assertNothingLoaded(t, h, vmm)
		})
	}
}

// AND A RECORD THAT CANNOT BE READ IS NOT A YES, nor a no: the restore stops and
// says it could not tell.
func TestARestoreStopsAtACompatibilityRecordItCannotRead(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)

	record := filepath.Join(dir, compatibilityFile)
	if err := os.Chmod(record, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(record, 0o444); err != nil {
			t.Errorf("restore the mode of %s: %v", record, err)
		}
	})

	_, vmm, err := h.restoreWith(t, "gold-1")
	if err == nil || errors.Is(err, errSnapshotIncompatible) || !strings.Contains(err.Error(), "could not tell") {
		t.Fatalf("Restore = %v, want could-not-tell and not a refusal", err)
	}

	assertNothingLoaded(t, h, vmm)
}

// A WRITABLE RECORD IS NOT A PUBLISHED ONE: anyone who could write it could make a
// snapshot claim a host it was not taken on.
func TestARestoreRefusesAWritableCompatibilityRecord(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)

	if err := os.Chmod(filepath.Join(dir, compatibilityFile), 0o644); err != nil {
		t.Fatal(err)
	}

	_, vmm, err := h.restoreWith(t, "gold-1")
	if !errors.Is(err, errSnapshotNotPublished) {
		t.Fatalf("Restore = %v, want errSnapshotNotPublished", err)
	}

	assertNothingLoaded(t, h, vmm)
}

// THE HOST IS JUDGED AS IT IS NOW. A record matching the host it was taken on is
// refused once that host's kernel has changed under it.
func TestARestoreRefusesASnapshotAfterTheHostKernelChanged(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	publish(t, h, "gold-1", 0o444)

	h.p.hostKernel = func() (string, error) { return "6.8.0 #2 SMP", nil }

	_, vmm, err := h.restoreWith(t, "gold-1")
	if !errors.Is(err, errSnapshotIncompatible) || !strings.Contains(err.Error(), "host kernel") {
		t.Fatalf("Restore = %v, want errSnapshotIncompatible naming the host kernel", err)
	}

	assertNothingLoaded(t, h, vmm)
}

// assertNothingLoaded holds a refused restore to having linked nothing and loaded
// nothing.
func assertNothingLoaded(t *testing.T, h *harness, vmm *fakeVMM) {
	t.Helper()

	if vmm != nil {
		for _, call := range vmm.calls() {
			if strings.Contains(call, "/snapshot/load") {
				t.Errorf("a refused snapshot was loaded: %s", call)
			}
		}
	}

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		if _, err := os.Lstat(filepath.Join(h.p.jailFor(theInstance).root(), file)); err == nil {
			t.Errorf("a refused snapshot's %s was linked into the jail", file)
		}
	}

	if slices.ContainsFunc(h.commands(), func(r recordedRun) bool { return strings.HasSuffix(r.bin, "jailer") }) {
		t.Error("a refused snapshot started a VMM")
	}
}

// editRecord stages a record that differs from the harness host by edit.
func editRecord(edit func(*compatibility)) func(*testing.T, *harness, string) {
	return func(t *testing.T, h *harness, dir string) {
		t.Helper()

		writeCompatibility(t, h, dir, edit)
	}
}

// writeRaw stages a record with exactly this content.
func writeRaw(content string) func(*testing.T, *harness, string) {
	return func(t *testing.T, _ *harness, dir string) {
		t.Helper()

		path := filepath.Join(dir, compatibilityFile)
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

// hostRecordAnd stages this host's own record, changed only by edit, so a refusal can
// only be for what edit did.
func hostRecordAnd(edit func([]byte) []byte) func(*testing.T, *harness, string) {
	return func(t *testing.T, h *harness, dir string) {
		t.Helper()

		guest, err := h.p.digests.of(h.p.cfg.KernelImage)
		if err != nil {
			t.Fatal(err)
		}

		record, err := h.p.hostCompatibility(guest)
		if err != nil {
			t.Fatal(err)
		}

		record.VCPUs, record.Memory = aRestoreSpec().VCPU, int64(aRestoreSpec().Memory)
		record.Image, record.Disk = aRestoreSpec().Image, testGoldenDisk
		record.Deployment, record.Builder = h.p.owner, theBuilder
		record.StateDigest = digestOf("captured " + snapshotStateFile)
		record.MemoryDigest = digestOf("captured " + snapshotMemoryFile)

		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}

		writeRaw(string(edit(raw)))(t, h, dir)
	}
}

// THE GUEST KERNEL IS JUDGED AS IT IS NOW, too: a snapshot published while the
// configured kernel was one file is refused once that file has been replaced.
func TestARestoreRefusesASnapshotAfterTheGuestKernelChanged(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	publish(t, h, "gold-1", 0o444)

	if err := os.WriteFile(h.p.cfg.KernelImage, []byte("another kernel entirely"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, vmm, err := h.restoreWith(t, "gold-1")
	if !errors.Is(err, errSnapshotIncompatible) || !strings.Contains(err.Error(), "guest kernel") {
		t.Fatalf("Restore = %v, want errSnapshotIncompatible naming the guest kernel", err)
	}

	assertNothingLoaded(t, h, vmm)
}

// TWO HOSTS LISTING THE SAME FEATURES IN ANOTHER ORDER HAVE ONE FEATURE DIGEST.
func TestTheCPUFeatureDigestIgnoresTheOrderOfTheFlags(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	core := "processor\t: 0\nvendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 1\nflags\t\t: %s\n"

	var digests []string

	for i, flags := range []string{"fpu sse avx2", "avx2 fpu sse"} {
		path := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(path, []byte(fmt.Sprintf(core, flags)), 0o600); err != nil {
			t.Fatal(err)
		}

		_, features, err := cpuIdentity(path)
		if err != nil {
			t.Fatal(err)
		}

		digests = append(digests, features)
	}

	if digests[0] != digests[1] {
		t.Errorf("the same flags in another order digest to %s and %s", digests[0], digests[1])
	}
}

// THE CHECK COMES BEFORE ANY LINK: placeSnapshot itself, asked to place a snapshot
// taken elsewhere, leaves the jail root as it found it.
func TestPlaceSnapshotLinksNothingForASnapshotTakenElsewhere(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)
	writeCompatibility(t, h, dir, func(c *compatibility) { c.HostKernel = "5.10.0 #1" })

	j := h.p.jailFor(theInstance)
	if err := os.MkdirAll(j.root(), 0o700); err != nil {
		t.Fatal(err)
	}

	err := h.p.placeSnapshot(j, "gold-1", h.p.cfg.KernelImage, aRestoreSpec(), h.theEvidence())
	if !errors.Is(err, errSnapshotIncompatible) {
		t.Fatalf("placeSnapshot = %v, want errSnapshotIncompatible", err)
	}

	if entries, err := os.ReadDir(j.root()); err != nil || len(entries) != 0 {
		t.Errorf("a refused snapshot left %v in the jail root (%v)", entries, err)
	}
}

// A RESTORE MUST ASK FOR THE SHAPE THE SNAPSHOT FIXES, or admission would charge it
// for a guest it is not.
func TestARestoreRefusesASpecOfAnotherShape(t *testing.T) {
	t.Parallel()

	for name, spec := range map[string]func() provider.Spec{
		"more vCPUs": func() provider.Spec { s := aRestoreSpec(); s.VCPU = 4; return s },
		"less memory": func() provider.Spec {
			s := aRestoreSpec()
			s.Memory /= 2

			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			publish(t, h, "gold-1", 0o444)

			_, err := h.p.Restore(t.Context(), spec(), "gold-1", h.theEvidence())
			if !errors.Is(err, errSnapshotIncompatible) {
				t.Fatalf("Restore = %v, want errSnapshotIncompatible", err)
			}
		})
	}
}

// A FILE STILL BEING WRITTEN IS NEVER REMEMBERED. Two same-size writes inside one
// timestamp tick leave the inode, the size and both times as they were (measured on
// Linux CI), so only a file whose last change has settled may be cached.
func TestADigestOfAFileJustWrittenIsNotRemembered(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(path, []byte("kernel one"), 0o600); err != nil {
		t.Fatal(err)
	}

	var d fileDigests

	first, err := d.of(path)
	if err != nil {
		t.Fatal(err)
	}

	// A STAT CANNOT STAGE THE TICK (a utimes call moves the change time), so the
	// cache itself is the evidence: nothing remembered means nothing to go stale.
	if len(d.seen) != 0 {
		t.Fatalf("a file written moments ago had its digest remembered: %v", d.seen)
	}

	if err := os.WriteFile(path, []byte("kernel two"), 0o600); err != nil {
		t.Fatal(err)
	}

	if second, err := d.of(path); err != nil || second == first {
		t.Errorf("a file rewritten moments after its first write kept the digest %q (%v)", second, err)
	}
}
