package firecracker

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A CAPTURE PUBLISHES COPIES, READ-ONLY, WITH THE HOST IT WAS TAKEN ON, and leaves the
// guest paused. The published files are billet's own inodes, not the VMM's: the jail's
// copies are gone, and what was published restores.
func TestACaptureIsPublishedAsReadOnlyCopiesARestoreAccepts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)
	root := h.p.jailFor(theInstance).root()

	var written map[string]unix.Stat_t

	vmm.mu.Lock()
	vmm.onCreate = func(root string) {
		written = map[string]unix.Stat_t{}

		for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
			var st unix.Stat_t
			if err := unix.Lstat(filepath.Join(root, file), &st); err != nil {
				t.Errorf("stat what the VMM wrote: %v", err)
			}

			written[file] = st
		}
	}
	vmm.mu.Unlock()

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if state := vmm.currentState(); state != statePaused {
		t.Errorf("after a capture the guest is %s, want it left paused", state)
	}

	dir := filepath.Join(h.p.snapshotsDir(), "gold-1")

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile, compatibilityFile} {
		path := filepath.Join(dir, file)
		if err := checkPublished(path); err != nil {
			t.Errorf("%s is not published as billet publishes: %v", file, err)
		}

		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			t.Fatal(err)
		}

		if src, ok := written[file]; ok && src.Ino == st.Ino && src.Dev == st.Dev {
			t.Errorf("%s was published as the VMM's own inode, which its account may still hold open", file)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, snapshotMemoryFile))
	if err != nil || string(raw) != "written by the VMM at /"+snapshotMemoryFile {
		t.Errorf("the published memory reads %q (%v), want what the VMM wrote", raw, err)
	}

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		if _, err := os.Lstat(filepath.Join(root, file)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the jail's own %s was left behind: %v", file, err)
		}
	}

	assertNoStaging(t, h)

	var record compatibility
	if raw, err := os.ReadFile(filepath.Join(dir, compatibilityFile)); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("the published record: %v", err)
	}

	guest, err := h.p.digests.of(h.p.cfg.KernelImage)
	if err != nil {
		t.Fatal(err)
	}

	if host, err := h.p.hostCompatibility(guest); err != nil || record != host {
		t.Errorf("the published record = %+v (%v), want this host's %+v", record, err, host)
	}

	// AND IT RESTORES, as another lease.
	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy the golden: %v", err)
	}

	if _, _, err := h.restoreWith(t, "gold-1"); err != nil {
		t.Errorf("a published capture would not restore: %v", err)
	}
}

// NOT EVEN OVER AN EMPTY DIRECTORY, which a plain rename would replace without a word.
func TestACaptureNeverReplacesAnEmptyDirectoryOfTheSameName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	if err := os.MkdirAll(filepath.Join(h.p.snapshotsDir(), "gold-1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err == nil {
		t.Fatal("Capture replaced a directory already under its name")
	}

	if entries, err := os.ReadDir(filepath.Join(h.p.snapshotsDir(), "gold-1")); err != nil || len(entries) != 0 {
		t.Errorf("the directory under the name now holds %v (%v)", entries, err)
	}

	assertNoStaging(t, h)
}

// A HOST THAT CANNOT DESCRIBE ITSELF NEVER PAUSES A GUEST: the record is made before
// the capture, and a capture that cannot be published is not taken.
func TestACaptureThatCannotDescribeTheHostLeavesTheGuestRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)
	h.p.hostKernel = func() (string, error) { return "", errors.New("uname: not permitted") }

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err == nil {
		t.Fatal("Capture succeeded without describing the host")
	}

	if state := vmm.currentState(); state == statePaused {
		t.Error("a capture that could not describe the host paused the guest")
	}
}

// A NAME IS PUBLISHED ONCE. A second capture under it is refused, and what was
// published first is untouched, because something may be running from it.
func TestACaptureNeverReplacesAPublishedSnapshot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)
	h.launch(t)

	before, err := os.ReadFile(filepath.Join(dir, snapshotMemoryFile))
	if err != nil {
		t.Fatal(err)
	}

	err = h.p.Capture(t.Context(), theInstance, "gold-1")
	if err == nil || !strings.Contains(err.Error(), "already published") {
		t.Fatalf("Capture over a published name = %v, want a refusal", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, snapshotMemoryFile))
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("the published memory was changed by a refused capture: %q (%v)", after, err)
	}

	assertNoStaging(t, h)
}

// WHAT THE VMM LEAVES IN ITS JAIL IS JUDGED AS ITSELF. A capture whose memory file a
// taken-over VMM has replaced with a link, or with something that is not a file, is
// refused, and nothing is published.
func TestACaptureRefusesWhatTheVMMLeftThatIsNotAFile(t *testing.T) {
	t.Parallel()

	// A FILE THE TEST'S OWN ACCOUNT OWNS, so a link to it is refused for being a link
	// and not because the owner check happened to catch it.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("not the guest's memory"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tamper := range map[string]func(t *testing.T, root string){
		"a symlink": func(t *testing.T, root string) {
			t.Helper()

			mem := filepath.Join(root, snapshotMemoryFile)
			if err := os.Remove(mem); err != nil {
				t.Error(err)
			}

			if err := os.Symlink(elsewhere, mem); err != nil {
				t.Error(err)
			}
		},
		"a FIFO, which would copy as an empty memory file": func(t *testing.T, root string) {
			t.Helper()

			mem := filepath.Join(root, snapshotMemoryFile)
			if err := os.Remove(mem); err != nil {
				t.Error(err)
			}

			if err := unix.Mkfifo(mem, 0o600); err != nil {
				t.Error(err)
			}
		},
		"a directory": func(t *testing.T, root string) {
			t.Helper()

			mem := filepath.Join(root, snapshotMemoryFile)
			if err := os.Remove(mem); err != nil {
				t.Error(err)
			}

			if err := os.Mkdir(mem, 0o700); err != nil {
				t.Error(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			_, vmm := h.launch(t)

			vmm.mu.Lock()
			vmm.onCreate = func(root string) { tamper(t, root) }
			vmm.mu.Unlock()

			if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err == nil {
				t.Fatal("Capture published what the VMM left in place of its memory")
			}

			if _, err := os.Lstat(filepath.Join(h.p.snapshotsDir(), "gold-1")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused capture was published: %v", err)
			}

			assertNoStaging(t, h)
		})
	}
}

// A FILE THE VMM'S ACCOUNT DID NOT WRITE IS NOT ITS CAPTURE. The real check: the
// owner must be the uid the microVM was given, and never root or nobody.
func TestOnlyTheJailsOwnAccountCountsAsWhatItsVMMWrote(t *testing.T) {
	t.Parallel()

	res := resources{UID: 900003, GID: 900003}

	for owner, want := range map[uint32]bool{900003: true, 900004: false, 0: false} {
		if got := jailWritten(unix.Stat_t{Uid: owner}, res); got != want {
			t.Errorf("a file owned by %d counts as written by uid 900003's VMM = %v, want %v", owner, got, want)
		}
	}

	if jailWritten(unix.Stat_t{Uid: 0}, resources{}) {
		t.Error("a jail with no uid recorded counts a root-owned file as its VMM's")
	}
}

// THE KERNEL A CAPTURE RECORDS IS THE FILE BILLET LINKED, and the jail's account owns
// the directory it is in. A kernel replaced by a link is refused before the guest is
// paused.
func TestACaptureRefusesAJailKernelThatIsNotBillets(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	kernel := filepath.Join(h.p.jailFor(theInstance).root(), guestKernel)

	for name, replace := range map[string]func() error{
		"a symlink":   func() error { return os.Symlink(h.p.cfg.KernelImage, kernel) },
		"a directory": func() error { return os.Mkdir(kernel, 0o700) },
	} {
		if err := os.RemoveAll(kernel); err != nil {
			t.Fatal(err)
		}

		if err := replace(); err != nil {
			t.Fatal(err)
		}

		if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err == nil {
			t.Errorf("Capture recorded %s in place of the jail's kernel", name)
		}

		if state := vmm.currentState(); state == statePaused {
			t.Errorf("a capture refused for %s paused the guest", name)
		}
	}
}

// A JAIL ANOTHER VMM BUILD STARTED IS NOT CAPTURED: no restore here could load it.
func TestACaptureRefusesAJailOfAnotherVMMBuild(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)
	h.p.execName = "firecracker-v1.17.0"

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err == nil {
		t.Fatal("Capture took a jail no restore here could load")
	}

	if state := vmm.currentState(); state == statePaused {
		t.Error("a refused capture paused the guest")
	}
}

// assertNoStaging holds a capture to leaving no staging directory in the store.
func assertNoStaging(t *testing.T, h *harness) {
	t.Helper()

	entries, err := os.ReadDir(h.p.snapshotsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging-") {
			t.Errorf("a capture left %s in the snapshot store", entry.Name())
		}
	}
}

// ONLY A REGULAR FILE BILLET'S OWN ACCOUNT OWNS COUNTS AS A KERNEL BILLET LINKED.
func TestOnlyARegularFileBilletOwnsCountsAsTheKernelItLinked(t *testing.T) {
	t.Parallel()

	me := uint32(os.Geteuid())

	for name, tc := range map[string]struct {
		st   unix.Stat_t
		want bool
	}{
		"billet's regular file":        {unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: me}, true},
		"a regular file of another":    {unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: me + 1}, false},
		"billet's directory":           {unix.Stat_t{Mode: unix.S_IFDIR | 0o755, Uid: me}, false},
		"billet's symlink, unfollowed": {unix.Stat_t{Mode: unix.S_IFLNK | 0o777, Uid: me}, false},
	} {
		if got := linkedByBillet(tc.st); got != tc.want {
			t.Errorf("%s counts as billet's kernel = %v, want %v", name, got, tc.want)
		}
	}
}
