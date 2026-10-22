package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

		for published, file := range map[string]string{
			snapshotStateFile: captureStateFile, snapshotMemoryFile: captureMemoryFile,
		} {
			var st unix.Stat_t
			if err := unix.Lstat(filepath.Join(root, file), &st); err != nil {
				t.Errorf("stat what the VMM wrote: %v", err)
			}

			written[published] = st
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
	if err != nil || string(raw) != "written by the VMM at /"+captureMemoryFile {
		t.Errorf("the published memory reads %q (%v), want what the VMM wrote", raw, err)
	}

	for _, file := range []string{captureStateFile, captureMemoryFile} {
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

	host, err := h.p.hostCompatibility(guest)
	host.VCPUs, host.Memory = aSpec().VCPU, int64(aSpec().Memory)
	host.Image, host.Disk = aSpec().Image, "golden-gold-1@"+fakeGeneration

	if err != nil || record != host {
		t.Errorf("the published record = %+v (%v), want this host's %+v", record, err, host)
	}

	// THE SYSTEM DISK, FLUSHED AND PUBLISHED AS THE GOLDEN'S, from the device and
	// the clone the launch recorded.
	h.mu.Lock()
	flushed := append([]string(nil), h.flushed...)
	h.mu.Unlock()

	if !slices.Equal(flushed, []string{h.disk.device}) {
		t.Errorf("the capture flushed %v, want the root disk's device %s", flushed, h.disk.device)
	}

	if published, withdrawn, _ := h.disk.goldenCalls(); !slices.Equal(published,
		[]string{theInstance + " -> golden-gold-1@" + fakeGeneration}) || len(withdrawn) != 0 {
		t.Errorf("the disk was published %v and withdrawn %v, want the builder's clone published once",
			published, withdrawn)
	}

	// AND IT RESTORES, as another lease, onto a clone of that disk.
	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy the golden: %v", err)
	}

	if _, _, err := h.restoreWith(t, "gold-1"); err != nil {
		t.Errorf("a published capture would not restore: %v", err)
	}

	if _, _, cloned := h.disk.goldenCalls(); len(cloned) != 1 ||
		!strings.HasPrefix(cloned[0], "golden-gold-1@"+fakeGeneration+" -> ") {
		t.Errorf("the restore cloned %v, want the golden's system disk", cloned)
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

			mem := filepath.Join(root, captureMemoryFile)
			if err := os.Remove(mem); err != nil {
				t.Error(err)
			}

			if err := os.Symlink(elsewhere, mem); err != nil {
				t.Error(err)
			}
		},
		"a FIFO, which would copy as an empty memory file": func(t *testing.T, root string) {
			t.Helper()

			mem := filepath.Join(root, captureMemoryFile)
			if err := os.Remove(mem); err != nil {
				t.Error(err)
			}

			if err := unix.Mkfifo(mem, 0o600); err != nil {
				t.Error(err)
			}
		},
		"a directory": func(t *testing.T, root string) {
			t.Helper()

			mem := filepath.Join(root, captureMemoryFile)
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

// A RESTORED MICROVM CAN BE CAPTURED AGAIN, and its own snapshot is untouched. A
// restored jail holds the snapshot it came from as billet's read-only links, so a
// capture that asked the VMM to write those names would be refused after the pause.
func TestARestoredMicroVMCanBeCapturedWithoutTouchingItsSnapshot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)

	before, err := os.ReadFile(filepath.Join(dir, snapshotMemoryFile))
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := h.restoreWith(t, "gold-1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if err := h.p.Capture(t.Context(), theInstance, "gold-2"); err != nil {
		t.Fatalf("Capture of a restored microVM: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, snapshotMemoryFile))
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("capturing a fork changed the snapshot it was restored from: %q (%v)", after, err)
	}

	if err := checkPublished(filepath.Join(h.p.snapshotsDir(), "gold-2", snapshotMemoryFile)); err != nil {
		t.Errorf("the fork's own capture: %v", err)
	}
}

// A VMM RUNNING A BUILD OTHER THAN THE ONE AT ITS PATH IS NOT CAPTURED: its snapshot
// would be labelled with a Firecracker it was not taken by.
func TestACaptureRefusesAVMMRunningAReplacedBuild(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	older := filepath.Join(t.TempDir(), "firecracker-v1.16.1")
	if err := os.WriteFile(older, []byte("the build this VMM was started from"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.p.runningVMM = func(context.Context, jail, resources) (*os.File, error) { return os.Open(older) }

	err := h.p.Capture(t.Context(), theInstance, "gold-1")
	if !errors.Is(err, errSnapshotIncompatible) {
		t.Fatalf("Capture = %v, want errSnapshotIncompatible", err)
	}

	if state := vmm.currentState(); state == statePaused {
		t.Error("a refused capture paused the guest")
	}
}

// WHAT A VMM WRITES IS HELD TO THE SIZE BILLET GAVE ITS GUEST. A memory file larger
// than the guest's memory, or a state file larger than any state, is refused before a
// byte of it is copied.
func TestACaptureRefusesFilesLargerThanItsGuestCouldWrite(t *testing.T) {
	t.Parallel()

	for file, size := range map[string]int64{
		captureMemoryFile: int64(aSpec().Memory) + 1,
		captureStateFile:  maxCapturedState + 1,
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			_, vmm := h.launch(t)

			vmm.mu.Lock()
			vmm.onCreate = func(root string) {
				// SPARSE, so the test writes nothing.
				if err := os.Truncate(filepath.Join(root, file), size); err != nil {
					t.Error(err)
				}
			}
			vmm.mu.Unlock()

			err := h.p.Capture(t.Context(), theInstance, "gold-1")
			if err == nil || !strings.Contains(err.Error(), "more than") {
				t.Fatalf("Capture = %v, want a refusal for its size", err)
			}

			if _, err := os.Lstat(filepath.Join(h.p.snapshotsDir(), "gold-1")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("an oversized capture was published: %v", err)
			}

			assertNoStaging(t, h)
		})
	}
}

// A JAIL WITH NO RECORD OF ITS GUEST'S SIZE IS NOT CAPTURED, because nothing would
// bound what its VMM left: refused before the pause.
func TestACaptureRefusesAJailWithNoRecordedShape(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	if err := os.Remove(h.p.jailFor(theInstance).shapeFile()); err != nil {
		t.Fatal(err)
	}

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err == nil {
		t.Fatal("Capture took a microVM with no recorded size")
	}

	if state := vmm.currentState(); state == statePaused {
		t.Error("a refused capture paused the guest")
	}
}

// A COPY STOPS WHEN ITS CALLER DOES, between chunks, rather than running to the end
// of a file billet did not size.
func TestACaptureCopyStopsWhenCanceled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)
	if err := os.WriteFile(filepath.Join(j.root(), captureMemoryFile), []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := unix.Open(j.root(), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)

	staging, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(staging)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := resourcesOf(j)
	if err != nil {
		t.Fatal(err)
	}

	err = h.p.copyOut(ctx, root, staging, captureMemoryFile, snapshotMemoryFile, 1<<20, j, res)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("copyOut with a canceled context = %v, want it canceled", err)
	}
}

// A STAGING DIRECTORY A KILLED PUBLISH LEFT IS SWEPT BY THE NEXT ONE.
func TestACaptureSweepsStagingAKilledPublishLeft(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	left := filepath.Join(h.p.snapshotsDir(), stagingPrefix+"0123456789abcdef")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(left, snapshotMemoryFile), []byte("half a copy"), 0o400); err != nil {
		t.Fatal(err)
	}

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	assertNoStaging(t, h)
}

// DURABLE IN THE RIGHT ORDER: every published file is read-only by the time it is
// synced, and the directory above the store is synced, since a first capture creates
// the store itself.
func TestACapturePublishesDurably(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		unready []string
		synced  []string
	)

	h := newHarness(t, withDurability(
		func(f *os.File) error {
			info, err := f.Stat()
			if err == nil && strings.Contains(f.Name(), "snapshot") && info.Mode().Perm() != 0o444 {
				mu.Lock()
				unready = append(unready, f.Name())
				mu.Unlock()
			}

			return f.Sync()
		},
		func(dir string) error {
			mu.Lock()
			synced = append(synced, dir)
			mu.Unlock()

			return syncDirectory(dir)
		}))
	h.launch(t)

	// ONLY WHAT THE CAPTURE SYNCS: a launch syncs directories of its own.
	mu.Lock()
	synced = nil
	mu.Unlock()

	if err := h.p.Capture(t.Context(), theInstance, "gold-1"); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(unready) > 0 {
		t.Errorf("synced before it was read-only: %v", unready)
	}

	if !slices.Contains(synced, filepath.Dir(h.p.snapshotsDir())) {
		t.Errorf("the directory holding the store was never synced; synced %v", synced)
	}
}

// A CAPTURE QUEUED BEHIND ANOTHER PUBLISH CAN BE ABANDONED, and it waits for the
// store before it pauses its guest, so giving up leaves the guest running.
func TestACaptureWaitingForTheStoreCanBeAbandonedWithItsGuestRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	held, err := h.p.openStore(t.Context())
	if err != nil {
		t.Fatalf("hold the store: %v", err)
	}
	defer held.close()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err = h.p.Capture(ctx, theInstance, "gold-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Capture behind a held store = %v, want it to give up with its context", err)
	}

	if state := vmm.currentState(); state == statePaused {
		t.Error("a capture that never got the store paused its guest")
	}
}

// A PID FILE THE JAIL TURNED INTO A FIFO IS NOT WAITED ON. A plain read of one with no
// writer never returns, and billet would hold this lease's locks while it waited.
func TestAPIDFileThatIsAFIFOIsRefusedWithoutWaiting(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)
	if err := os.Remove(j.pidFile()); err != nil {
		t.Fatal(err)
	}

	if err := unix.Mkfifo(j.pidFile(), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := h.p.vmmPID(j)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO in place of the pid file was read as a pid")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO in place of the pid file did not return")
	}
}

// A PID FILE LONGER THAN ANY PID IS REFUSED, not parsed by its first bytes: padding
// after a stranger's pid would otherwise read as that pid.
func TestAPIDFileLongerThanAnyPIDIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)
	padded := "1" + strings.Repeat(" ", maxPIDFile) + "garbage"

	if err := os.WriteFile(j.pidFile(), []byte(padded), 0o600); err != nil {
		t.Fatal(err)
	}

	if pid, err := h.p.vmmPID(j); err == nil {
		t.Errorf("a padded pid file read as pid %d", pid)
	}
}
