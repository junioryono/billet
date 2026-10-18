package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// deletedDevices is every device name billet asked `ip` to delete.
func deletedDevices(h *harness) []string {
	var names []string

	for _, run := range h.commands() {
		if run.bin == ipBinary && slices.Contains(run.args, "del") {
			names = append(names, run.args[len(run.args)-1])
		}
	}

	return names
}

// claimExists reports whether a claim file is still there.
func claimExists(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("look for the claim %s: %v", path, err)
	}

	return err == nil
}

// A TEARDOWN DELETES ONLY A DEVICE THE KERNEL SAYS IS BILLET'S. After a launch has
// made bt-0, the device under that name is swapped for each thing a host could have
// there, and Destroy is judged on what it deleted and what it kept claimed.
func TestDestroyDeletesOnlyADeviceTheKernelSaysIsBillets(t *testing.T) {
	t.Parallel()

	const (
		ours  = 900000 // the uid the launch claims: the harness range starts here
		spare = 900005 // in the range, claimed by no one
	)

	for _, tc := range []struct {
		name      string
		owner     int // -2: no device at all
		deleted   bool
		keepTap   bool
		claimGone bool
	}{
		{name: "the tap this launch made", owner: ours, deleted: true},
		{name: "no device", owner: -2},
		{name: "a device that is not a tap", owner: -1, keepTap: true},
		{name: "a tap owned outside the jail range", owner: 1000, keepTap: true},
		{name: "a tap owned by a jail uid no one holds", owner: spare, deleted: true},
		{name: "a tap owned just past the jail range", owner: ours + 8, keepTap: true},
		{name: "no claim on the name any more", owner: 1000, claimGone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.launch(t)

			if tc.claimGone {
				if err := os.Remove(filepath.Join(h.p.claimsDir("taps"), "bt-0")); err != nil {
					t.Fatal(err)
				}
			}

			h.mu.Lock()
			delete(h.devices, "bt-0")
			if tc.owner != -2 {
				h.devices["bt-0"] = tc.owner
			}
			h.mu.Unlock()

			if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
				t.Fatalf("Destroy: %v", err)
			}

			if got := slices.Contains(deletedDevices(h), "bt-0"); got != tc.deleted {
				t.Errorf("Destroy deleted bt-0 = %v, want %v", got, tc.deleted)
			}

			if got := claimExists(t, filepath.Join(h.p.claimsDir("taps"), "bt-0")); got != tc.keepTap {
				t.Errorf("the name bt-0 kept claimed = %v, want %v", got, tc.keepTap)
			}

			if claimExists(t, filepath.Join(h.p.claimsDir("uids"), strconv.Itoa(ours))) {
				t.Errorf("uid %d was kept claimed after its microVM was destroyed", ours)
			}
		})
	}
}

// BILLET'S OWN DEVICE THAT WOULD NOT GO IS FINISHED BY THE NEXT TEARDOWN. Destroy
// keeps the name and gives the uid back, as it always has; orphan cleanup, once the
// jail is gone, still proves the tap is billet's from an owner no lease holds, keeps
// the name while the device stays, and deletes it once the kernel lets it go.
func TestATapThatWouldNotGoIsFinishedByTheNextTeardown(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)
	h.refuse = func(bin string, args []string) error {
		if bin == ipBinary && slices.Contains(args, "del") {
			return errors.New("RTNETLINK answers: Device or resource busy")
		}

		return nil
	}

	tap := filepath.Join(h.p.claimsDir("taps"), "bt-0")

	if _, err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("Destroy succeeded although the tap would not go")
	}

	if !claimExists(t, tap) {
		t.Error("the name of a device still on the host went back to the pool")
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("the orphan cleanup succeeded although the tap would not go")
	}

	if !claimExists(t, tap) {
		t.Error("orphan cleanup gave back the name of a device still on the host")
	}

	h.refuse = nil

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("the retried Destroy: %v", err)
	}

	if h.hasDevice("bt-0") || claimExists(t, tap) {
		t.Error("the retried Destroy did not delete billet's tap and give its name back")
	}
}

// A FAILED LAUNCH WHOSE TAP WOULD NOT GO KEEPS ITS NAME, as Destroy does.
func TestAFailedLaunchWhoseTapWouldNotGoKeepsItsName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.jailerErr = errors.New("jailer refused")
	h.refuse = func(bin string, args []string) error {
		if bin == ipBinary && slices.Contains(args, "del") {
			return errors.New("RTNETLINK answers: Device or resource busy")
		}

		return nil
	}

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil {
		t.Fatal("Launch succeeded although the jailer failed")
	}

	if !h.hasDevice("bt-0") {
		t.Fatal("the launch never made its tap, so this tests nothing")
	}

	if !claimExists(t, filepath.Join(h.p.claimsDir("taps"), "bt-0")) {
		t.Error("the unwind gave back the name of a device still on the host")
	}
}

// A CLAIM THAT OUTLIVES ITS JAIL STILL DELETES ONLY BILLET'S DEVICE. A launch that
// failed before its tap, and could not give the name back, leaves a claim naming a
// jail that is gone; the cleanup that finds it leaves the device under the name.
func TestAnOrphanedTapClaimDeletesNoDeviceBilletDidNotMake(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this uses to make the release fail")
	}

	h := newHarness(t)
	h.putDevice("bt-0", -1)

	taps := h.p.claimsDir("taps")
	h.disk.cloneErr = errors.New("rbd: cluster unavailable")
	h.disk.onDiscard = func() {
		if err := os.Chmod(taps, 0o500); err != nil {
			t.Errorf("make the tap claims unreleasable: %v", err)
		}
	}

	t.Cleanup(func() {
		if err := os.Chmod(taps, 0o700); err != nil {
			t.Errorf("restore the mode of %s: %v", taps, err)
		}
	})

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil {
		t.Fatal("Launch succeeded although the clone failed")
	}

	if _, err := os.Stat(h.p.jailFor(theInstance).dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the jail outlived a discard that succeeded: %v", err)
	}

	if !claimExists(t, filepath.Join(taps, "bt-0")) {
		t.Fatal("the tap claim did not outlive the failed release, so this tests nothing")
	}

	if err := os.Chmod(taps, 0o700); err != nil {
		t.Fatal(err)
	}

	h.disk.onDiscard = nil

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("the cleanup of the orphaned claim: %v", err)
	}

	assertNoTapDeleted(t, h)

	if !h.hasDevice("bt-0") {
		t.Error("orphan cleanup removed a device billet did not make")
	}
}

// A NAME THE KERNEL ALREADY HAS IS NOT BILLET'S DEVICE. A launch refused that way
// whose discard also fails keeps its jail, and the later Destroy must not delete the
// device that refused it.
func TestATapNameTheKernelRefusedIsNeverDeletedByALaterDestroy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.putDevice("bt-0", 1000)
	h.disk.discardErr = errors.New("rbd: cluster unavailable")

	_, err := h.p.Launch(t.Context(), aSpec())
	if !errors.Is(err, errTapTaken) {
		t.Fatalf("Launch = %v, want errTapTaken", err)
	}

	h.disk.discardErr = nil

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("the retried Destroy: %v", err)
	}

	assertNoTapDeleted(t, h)

	if !h.hasDevice("bt-0") {
		t.Error("the retried Destroy removed the device that refused the launch")
	}
}

// COULD NOT TELL IS NOT PERMISSION. Whether it is the claim or the kernel that cannot
// be read, Destroy deletes nothing, says so, and keeps the name for a later teardown
// to judge.
func TestDestroyDeletesNoTapItCannotJudge(t *testing.T) {
	t.Parallel()

	t.Run("an unreadable claim", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its mode")
		}

		h := newHarness(t)
		h.launch(t)

		claim := filepath.Join(h.p.claimsDir("taps"), "bt-0")
		if err := os.Chmod(claim, 0o000); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := os.Chmod(claim, 0o600); err != nil {
				t.Errorf("restore the mode of %s: %v", claim, err)
			}
		})

		assertCouldNotTell(t, h)

		// AND AGAIN THROUGH ORPHAN CLEANUP, now the jail is gone: an inventory with a
		// claim it cannot read is incomplete, and nothing is released from it.
		assertCouldNotTell(t, h)
	})

	t.Run("an unreadable claim on the uid that owns the tap", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its mode")
		}

		h := newHarness(t)
		h.launch(t)

		// OUTSIDE THE RANGE NOW, so only this jail's own claim can prove the tap.
		h.p.cfg.JailUIDMin = 910000

		claim := filepath.Join(h.p.claimsDir("uids"), "900000")
		if err := os.Chmod(claim, 0o000); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := os.Chmod(claim, 0o600); err != nil {
				t.Errorf("restore the mode of %s: %v", claim, err)
			}
		})

		assertCouldNotTell(t, h)
	})

	t.Run("a kernel that cannot answer", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.launch(t)

		h.mu.Lock()
		h.ownerErr = errors.New("read /sys/class/net/bt-0/owner: input/output error")
		h.mu.Unlock()

		assertCouldNotTell(t, h)
	})
}

// assertCouldNotTell runs Destroy and holds it to the could-not-tell answer.
func assertCouldNotTell(t *testing.T, h *harness) {
	t.Helper()

	_, err := h.p.Destroy(t.Context(), theInstance)
	if err == nil || !strings.Contains(err.Error(), "could not tell") {
		t.Fatalf("Destroy = %v, want it to say it could not tell", err)
	}

	assertNoTapDeleted(t, h)

	if !claimExists(t, filepath.Join(h.p.claimsDir("taps"), "bt-0")) {
		t.Error("Destroy gave back the name of a device it could not judge")
	}
}

// THE KERNEL'S ANSWER, READ AS THE KERNEL WRITES IT: a tap's owner file holds the
// uid; a device that is not a tap has none; an absent device is no device; and a name
// billet never spells is refused before anything is read.
func TestSysTapOwnerReadsTheKernelsAnswer(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	for name, owner := range map[string]string{"bt-1": "900001\n", "bt-4": "not a number\n"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(root, name, "owner"), []byte(owner), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "bt-2"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		tap     string
		owner   int
		present bool
		fails   bool
	}{
		{tap: "bt-1", owner: 900001, present: true},
		{tap: "bt-2", owner: -1, present: true},
		{tap: "bt-3"},
		{tap: "bt-4", fails: true},
		{tap: "bt-07", fails: true},
		{tap: "../bt-1", fails: true},
	} {
		owner, present, err := sysTapOwnerIn(root, tc.tap)

		if (err != nil) != tc.fails {
			t.Errorf("sysTapOwnerIn(%q) error = %v, want failure %v", tc.tap, err, tc.fails)

			continue
		}

		if !tc.fails && (owner != tc.owner || present != tc.present) {
			t.Errorf("sysTapOwnerIn(%q) = %d, %v, want %d, %v", tc.tap, owner, present, tc.owner, tc.present)
		}
	}
}

// A RANGE CHANGED UNDER A RUNNING MICROVM DOES NOT STRAND ITS TAP. The uid it was
// given is no longer in the configured range, but its claim still names this jail.
func TestATapOutlivesAChangeOfTheJailUIDRange(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)
	h.p.cfg.JailUIDMin = 910000

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if h.hasDevice("bt-0") {
		t.Error("Destroy left the tap its launch made because the range moved")
	}
}

// AND ANOTHER LEASE'S CLAIM ON AN OUT-OF-RANGE UID PROVES NOTHING FOR THIS ONE. Outside
// the range only this jail's own claim makes a tap billet's to delete.
func TestAnOutOfRangeOwnerClaimedByAnotherLeaseIsLeft(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)
	h.p.cfg.JailUIDMin = 910000

	other := otherLeaseOnAnotherStripe(t, theInstance)
	if err := os.WriteFile(filepath.Join(h.p.claimsDir("uids"), "900006"), []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.putDevice("bt-0", 900006)

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if !h.hasDevice("bt-0") {
		t.Error("Destroy deleted a device owned outside the range by a uid another lease holds")
	}
}

// A TAP OWNED BY A UID ANOTHER MICROVM HOLDS NOW IS STILL BILLET'S TO DELETE. The
// teardown that kept its name gave the uid back and a launch took it; that launch's
// own tap is under its own name, so the one under this name is nobody's live device.
// Treating it as foreign would strand it, and treating it as could-not-tell would
// fail recovery until the other lease let the uid go.
func TestATapOwnedByAUIDAnotherLeaseHoldsIsStillDeleted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	other := otherLeaseOnAnotherStripe(t, theInstance)
	if err := os.WriteFile(filepath.Join(h.p.claimsDir("uids"), "900006"), []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.putDevice("bt-0", 900006)

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if h.hasDevice("bt-0") || claimExists(t, filepath.Join(h.p.claimsDir("taps"), "bt-0")) {
		t.Error("Destroy left billet's tap, or its name, because another lease holds the uid that owns it")
	}

	if !claimExists(t, filepath.Join(h.p.claimsDir("uids"), "900006")) {
		t.Error("Destroy released another lease's uid")
	}
}
