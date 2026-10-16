package firecracker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// publish writes a snapshot as billet publishes one, and returns its directory.
func publish(t *testing.T, h *harness, name string, mode os.FileMode) string {
	t.Helper()

	dir := filepath.Join(h.p.snapshotsDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make the snapshot directory: %v", err)
	}

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, []byte("captured "+file), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}

		// CHMOD, because WriteFile's mode goes through the umask.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %s: %v", file, err)
		}
	}

	return dir
}

// aRestoreSpec is a restore that should succeed: a launch's spec with the disk size
// the snapshot already has.
func aRestoreSpec() provider.Spec {
	spec := aSpec()
	spec.Disk = 0

	return spec
}

// restoreWith runs Restore against a fake VMM and returns it.
func (h *harness) restoreWith(t *testing.T, snapshot string) (*provider.Instance, *fakeVMM, error) {
	t.Helper()

	var vmm *fakeVMM

	h.mu.Lock()
	h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }
	h.mu.Unlock()

	inst, err := h.p.Restore(t.Context(), aRestoreSpec(), snapshot)

	return inst, vmm, err
}

// A RESTORE LOADS THE SNAPSHOT IT LINKED, AND BOOTS NOTHING. Every resource a cold
// boot configures is in the snapshot already, and Firecracker refuses most of them
// once one is loaded, so a restore that configured a machine first would be a boot
// with a snapshot on the side.
func TestARestoreLoadsTheLinkedSnapshotAndConfiguresNoMachine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	src := publish(t, h, "gold-1", 0o444)

	inst, vmm, err := h.restoreWith(t, "gold-1")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if !inst.Running {
		t.Error("Restore reported an instance that is not running")
	}

	if got, want := vmm.calls(), []string{"PUT /snapshot/load", "PUT /mmds", "PATCH /vm"}; !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v: a restore configures and starts nothing itself", got, want)
	}

	load, _ := vmm.bodyPut("/snapshot/load")
	if load["snapshot_path"] != "/snapshot.state" {
		t.Errorf("snapshot_path = %v, want /snapshot.state inside the chroot", load["snapshot_path"])
	}

	if mem := field[map[string]any](t, load, "mem_backend"); mem["backend_path"] != "/snapshot.mem" {
		t.Errorf("mem_backend = %v, want /snapshot.mem inside the chroot", mem)
	}

	// THE LINK IS THE PUBLISHED FILE, never a copy and never a chowned one.
	j := h.p.jailFor(theInstance)
	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		published, err := os.Lstat(filepath.Join(src, file))
		if err != nil {
			t.Fatalf("stat the published %s: %v", file, err)
		}

		linked, err := os.Lstat(filepath.Join(j.root(), file))
		if err != nil {
			t.Fatalf("the jail has no %s: %v", file, err)
		}

		if !os.SameFile(published, linked) {
			t.Errorf("%s in the jail is not the published file", file)
		}

		if linked.Mode().Perm() != 0o444 {
			t.Errorf("%s in the jail has mode %04o, want 0444", file, linked.Mode().Perm())
		}
	}

	// AND THE CREDENTIAL REACHED THE GUEST THE WAY A LAUNCH'S DOES: through MMDS,
	// never through argv.
	md, _ := vmm.bodyPut("/mmds")
	if encoded, err := json.Marshal(md); err != nil || !strings.Contains(string(encoded), theCredential) {
		t.Error("the lease's registration was not in the restored guest's metadata")
	}

	if strings.Contains(h.everyArgument(), theCredential) {
		t.Error("the registration appeared in a command line")
	}
}

// A SNAPSHOT SOMEONE ELSE COULD WRITE IS NOT RESTORED. Every fork of a golden maps
// the one file, so a writable one is a golden any of them could rewrite for all the
// rest.
func TestARestoreRefusesASnapshotThatIsNotReadOnly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	publish(t, h, "gold-1", 0o644)

	_, _, err := h.restoreWith(t, "gold-1")
	if !errors.Is(err, errSnapshotNotPublished) {
		t.Fatalf("Restore = %v, want errSnapshotNotPublished", err)
	}

	if !strings.Contains(err.Error(), "mode 0644") {
		t.Errorf("the refusal does not say what was wrong with the file: %v", err)
	}

	// AND NOTHING IS LEFT: no VMM started, the jail gone, the clone discarded.
	if h.ranWith("--id") {
		t.Error("a VMM was started for a snapshot that was refused")
	}

	if _, statErr := os.Stat(h.p.jailFor(theInstance).dir()); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refused restore left its jail: %v", statErr)
	}

	if !slices.Contains(h.disk.discards(), theInstance) {
		t.Errorf("the refused restore's root clone was not discarded: %v", h.disk.discards())
	}
}

// A SYMLINK IS NOT A PUBLISHED SNAPSHOT, whatever it points at.
func TestARestoreRefusesASnapshotThatIsALink(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)

	elsewhere := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(elsewhere, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	mem := filepath.Join(dir, snapshotMemoryFile)
	if err := os.Remove(mem); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(elsewhere, mem); err != nil {
		t.Fatal(err)
	}

	if _, _, err := h.restoreWith(t, "gold-1"); !errors.Is(err, errSnapshotNotPublished) {
		t.Fatalf("Restore of a symlinked memory file = %v, want errSnapshotNotPublished", err)
	}
}

// A NAME THAT IS A PATH IS NOT A SNAPSHOT NAME. The name is joined under the
// snapshots directory, so `..` would reach whatever sits beside it. Each case
// publishes a valid snapshot exactly where the name would lead, so the name check is
// the only thing that can refuse it.
func TestARestoreRefusesASnapshotNameThatIsAPath(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"../gold-1", "a/b", ".hidden"} {
		h := newHarness(t)
		publish(t, h, name, 0o444)

		if _, _, err := h.restoreWith(t, name); !errors.Is(err, errSnapshotNotPublished) {
			t.Errorf("Restore(%q) = %v, want errSnapshotNotPublished", name, err)
		} else if !strings.Contains(err.Error(), "is not a plain snapshot name") {
			t.Errorf("Restore(%q) was refused for another reason: %v", name, err)
		}
	}
}

// A RESTORE NEVER GROWS THE DISK, because the restored kernel has the filesystem
// mounted at the size it was captured at.
func TestARestoreRefusesToGrowTheRootDisk(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	publish(t, h, "gold-1", 0o444)

	spec := aRestoreSpec()
	spec.Disk = aSpec().Disk

	if _, err := h.p.Restore(t.Context(), spec, "gold-1"); err == nil ||
		!strings.Contains(err.Error(), "keeps the size the snapshot was taken at") {
		t.Fatalf("Restore with a disk size = %v, want the refusal", err)
	}

	if len(h.commands()) != 0 {
		t.Errorf("a refused restore ran %v", h.commands())
	}
}

// A LINK TO A VALID SNAPSHOT IS STILL NOT ONE. Whatever it points at today, a
// symlink is a name someone could repoint, so it is refused as itself rather than
// judged by its target.
func TestAPublishedFileIsNeverJudgedThroughASymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	target := filepath.Join(dir, "real.mem")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(target, 0o444); err != nil {
		t.Fatal(err)
	}

	if err := checkPublished(target); err != nil {
		t.Fatalf("the target itself must pass, or this test proves nothing: %v", err)
	}

	link := filepath.Join(dir, "snapshot.mem")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	err := checkPublished(link)
	if !errors.Is(err, errSnapshotNotPublished) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("checkPublished(symlink to a valid file) = %v, want refusal as not a regular file", err)
	}
}

// NOR IS A DIRECTORY with the right mode and owner.
func TestAPublishedFileMustBeARegularFile(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "snapshot.mem")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o444); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Errorf("make the directory removable again: %v", err)
		}
	})

	err := checkPublished(dir)
	if !errors.Is(err, errSnapshotNotPublished) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("checkPublished(a 0444 directory) = %v, want refusal as not a regular file", err)
	}
}
