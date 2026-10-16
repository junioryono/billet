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
		taken = 900006 // in the range, claimed below by another lease
	)

	for _, tc := range []struct {
		name               string
		owner              int // -2: no device at all
		deleted            bool
		keepTap, keepUID   bool
		claimTakenByOthers bool
		claimGone          bool
	}{
		{name: "the tap this launch made", owner: ours, deleted: true},
		{name: "no device", owner: -2},
		{name: "a device that is not a tap", owner: -1, keepTap: true},
		{name: "a tap owned outside the jail range", owner: 1000, keepTap: true},
		{name: "a tap owned by a jail uid no one holds", owner: spare, deleted: true},
		{name: "a tap owned by another lease's uid", owner: taken, keepTap: true, claimTakenByOthers: true},
		{name: "a tap owned just past the jail range", owner: ours + 8, keepTap: true},
		{name: "no claim on the name any more", owner: 1000, claimGone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.launch(t)

			if tc.claimTakenByOthers {
				other := otherLeaseOnAnotherStripe(t, theInstance)
				claim := filepath.Join(h.p.claimsDir("uids"), strconv.Itoa(taken))

				if err := os.WriteFile(claim, []byte(other+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

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

			uid := filepath.Join(h.p.claimsDir("uids"), strconv.Itoa(ours))
			if got := claimExists(t, uid); got != tc.keepUID {
				t.Errorf("uid %d kept claimed = %v, want %v", ours, got, tc.keepUID)
			}
		})
	}
}

// BILLET'S OWN DEVICE THAT WOULD NOT GO KEEPS ITS UID AS WELL AS ITS NAME. The uid
// owns the tap still attached to the bridge, so a VMM given it could open that tap,
// and a later teardown needs it to prove the device is billet's.
func TestADeviceThatWouldNotGoKeepsItsUIDAndItsName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)
	h.refuse = func(bin string, args []string) error {
		if bin == ipBinary && slices.Contains(args, "del") {
			return errors.New("RTNETLINK answers: Device or resource busy")
		}

		return nil
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("Destroy succeeded although the tap would not go")
	}

	if !claimExists(t, filepath.Join(h.p.claimsDir("taps"), "bt-0")) {
		t.Error("the name of a device still on the host went back to the pool")
	}

	if !claimExists(t, filepath.Join(h.p.claimsDir("uids"), "900000")) {
		t.Error("the uid owning a device still on the host went back to the pool")
	}

	// ORPHAN CLEANUP KEEPS BOTH TOO, now the jail is gone and only the claims say
	// whose the device is.
	if _, err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("the orphan cleanup succeeded although the tap would not go")
	}

	for _, claim := range []string{
		filepath.Join(h.p.claimsDir("taps"), "bt-0"),
		filepath.Join(h.p.claimsDir("uids"), "900000"),
	} {
		if !claimExists(t, claim) {
			t.Errorf("orphan cleanup gave back %s while its device is still on the host", claim)
		}
	}

	// AND THE NEXT TEARDOWN FINISHES IT.
	h.refuse = nil

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("the retried Destroy: %v", err)
	}

	if h.hasDevice("bt-0") {
		t.Error("the retried Destroy left billet's device on the host")
	}

	for _, claim := range []string{
		filepath.Join(h.p.claimsDir("taps"), "bt-0"),
		filepath.Join(h.p.claimsDir("uids"), "900000"),
	} {
		if claimExists(t, claim) {
			t.Errorf("the retried Destroy kept %s", claim)
		}
	}
}

// A FAILED LAUNCH WHOSE TAP WOULD NOT GO KEEPS ITS NAME AND ITS UID, as Destroy does.
func TestAFailedLaunchWhoseTapWouldNotGoKeepsItsUIDAndItsName(t *testing.T) {
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

	for _, claim := range []string{
		filepath.Join(h.p.claimsDir("taps"), "bt-0"),
		filepath.Join(h.p.claimsDir("uids"), "900000"),
	} {
		if !claimExists(t, claim) {
			t.Errorf("the unwind gave back %s while its device is still on the host", claim)
		}
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
// be read, Destroy deletes nothing, says so, and keeps both claims for a later
// teardown to judge.
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

	for _, claim := range []string{
		filepath.Join(h.p.claimsDir("taps"), "bt-0"),
		filepath.Join(h.p.claimsDir("uids"), "900000"),
	} {
		if !claimExists(t, claim) {
			t.Errorf("Destroy gave back %s, which it could not judge", claim)
		}
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
