package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tapClaim names the one tap claim a harness holds.
func tapClaim(h *harness) (string, error) {
	dir := h.p.claimsDir("taps")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		return entry.Name(), nil
	}

	return "", os.ErrNotExist
}

// markedUnmade reports whether the harness's lease has marked a tap unmade.
func markedUnmade(h *harness, tap string) bool {
	raw, err := os.ReadFile(filepath.Join(h.p.unmadeDir(), tap))

	return err == nil && strings.TrimSpace(string(raw)) == theInstance
}

// THE CLAIM SAYS MADE BEFORE THE DEVICE IS. A claim marked after `ip tuntap add`
// would, after a crash between the two, say unmade over a device billet created,
// and every teardown would hand its name back with the device still attached.
func TestATapClaimIsMarkedMadeBeforeTheDeviceIsCreated(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	var unmadeAtClone, unmadeAtCreate, created bool

	h.disk.onClone = func() {
		name, err := tapClaim(h)
		unmadeAtClone = err == nil && markedUnmade(h, name)
	}
	h.refuse = func(bin string, args []string) error {
		if bin == ipBinary && len(args) > 0 && args[0] == "tuntap" {
			created = true
			unmadeAtCreate = markedUnmade(h, args[3])
		}

		return nil
	}

	h.launch(t)

	if !unmadeAtClone {
		t.Error("while the root disk was cloned the tap claim was not marked unmade")
	}

	if !created {
		t.Fatal("the launch never created its tap")
	}

	if unmadeAtCreate {
		t.Error("the tap was created while its claim was still marked unmade")
	}
}

// A CLAIM THAT OUTLIVES ITS JAIL STILL SAYS NO DEVICE WAS MADE. A launch that failed
// before its tap, and could not give the name back, leaves a claim naming a jail that
// is gone; the cleanup that finds it releases the name and deletes nothing.
func TestAnUnmadeTapWhoseClaimOutlivesItsJailIsReleasedAndNotDeleted(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this uses to make the release fail")
	}

	h := newHarness(t)
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

	name, err := tapClaim(h)
	if err != nil {
		t.Fatalf("the tap claim did not outlive the failed release: %v", err)
	}

	if !markedUnmade(h, name) {
		t.Fatalf("the surviving claim on %s is not marked unmade", name)
	}

	if err := os.Chmod(taps, 0o700); err != nil {
		t.Fatal(err)
	}

	h.disk.onDiscard = nil

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("the cleanup of the orphaned claim: %v", err)
	}

	assertNoTapDeleted(t, h)

	if _, err := os.Stat(filepath.Join(taps, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphaned claim on %s was not released: %v", name, err)
	}
}

// A NAME THE KERNEL ALREADY HAS IS NOT BILLET'S DEVICE, and its claim goes back to
// saying so. A launch refused that way whose discard also fails keeps its jail, and
// the later Destroy must not delete the device that refused it.
func TestATapNameTheKernelRefusedIsNeverDeletedByALaterDestroy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.refuse = func(bin string, args []string) error {
		if bin == ipBinary && len(args) > 0 && args[0] == "tuntap" {
			return errors.New("ioctl(TUNSETIFF): Device or resource busy; RTNETLINK answers: File exists")
		}

		return nil
	}
	h.disk.discardErr = errors.New("rbd: cluster unavailable")

	_, err := h.p.Launch(t.Context(), aSpec())
	if !errors.Is(err, errTapTaken) {
		t.Fatalf("Launch = %v, want errTapTaken", err)
	}

	name, err := tapClaim(h)
	if err != nil {
		t.Fatalf("the retained jail's tap claim: %v", err)
	}

	if !markedUnmade(h, name) {
		t.Errorf("the claim on %s, a device billet did not make, is not marked unmade", name)
	}

	h.disk.discardErr = nil

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("the retried Destroy: %v", err)
	}

	for _, run := range h.commands() {
		if run.bin == ipBinary && slices.Contains(run.args, "del") {
			t.Errorf("the retried Destroy deleted a device another holder had: %v", run.args)
		}
	}
}

// A CLAIM THAT CANNOT BE READ IS NOT PERMISSION. Destroy deletes no device it cannot
// tell is billet's, says so, and keeps the name for a later teardown to judge.
func TestDestroyDeletesNoTapWhoseClaimItCannotRead(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}

	h := newHarness(t)
	h.launch(t)

	name, err := tapClaim(h)
	if err != nil {
		t.Fatalf("the tap claim: %v", err)
	}

	claim := filepath.Join(h.p.claimsDir("taps"), name)
	if err := os.Chmod(claim, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(claim, 0o600); err != nil {
			t.Errorf("restore the mode of %s: %v", claim, err)
		}
	})

	_, err = h.p.Destroy(t.Context(), theInstance)
	if err == nil || !strings.Contains(err.Error(), "could not tell") {
		t.Fatalf("Destroy = %v, want it to say it could not tell", err)
	}

	for _, run := range h.commands() {
		if run.bin == ipBinary && slices.Contains(run.args, "del") {
			t.Errorf("Destroy deleted a device whose claim it could not read: %v", run.args)
		}
	}

	if _, err := os.Lstat(claim); err != nil {
		t.Errorf("Destroy gave back a name it could not judge: %v", err)
	}
}

// A MARK THAT CANNOT BE READ IS NOT PERMISSION EITHER. A claim whose unmade mark
// cannot be read may or may not have a device under it, so Destroy deletes nothing,
// says it could not tell, and keeps the name.
func TestDestroyDeletesNoTapWhoseUnmadeMarkItCannotRead(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}

	h := newHarness(t)
	h.disk.cloneErr = errors.New("rbd: cluster unavailable")
	h.disk.discardErr = errors.New("rbd: cluster unavailable")

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil {
		t.Fatal("Launch succeeded although the clone failed")
	}

	name, err := tapClaim(h)
	if err != nil {
		t.Fatalf("the retained jail's tap claim: %v", err)
	}

	mark := filepath.Join(h.p.unmadeDir(), name)
	if err := os.Chmod(mark, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(mark, 0o600); err != nil {
			t.Errorf("restore the mode of %s: %v", mark, err)
		}
	})

	h.disk.cloneErr, h.disk.discardErr = nil, nil

	_, err = h.p.Destroy(t.Context(), theInstance)
	if err == nil || !strings.Contains(err.Error(), "could not tell") {
		t.Fatalf("Destroy = %v, want it to say it could not tell", err)
	}

	assertNoTapDeleted(t, h)

	if _, err := os.Lstat(filepath.Join(h.p.claimsDir("taps"), name)); err != nil {
		t.Errorf("Destroy gave back a name it could not judge: %v", err)
	}
}

// A NAME THAT CANNOT BE MARKED IS NOT HELD. A claim handed out without its mark would
// say a device may exist under it, so the claim is given back and the claim fails.
func TestATapThatCannotBeMarkedUnmadeIsGivenBack(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// A FILE WHERE THE MARKS' DIRECTORY GOES, which refuses root as well.
	if err := os.MkdirAll(filepath.Dir(h.p.unmadeDir()), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(h.p.unmadeDir(), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	j := h.p.jailFor(theInstance)
	if err := h.p.claim(j); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := h.p.claimResources(j); err == nil {
		t.Fatal("claimResources handed out a tap it could not mark unmade")
	}

	if name, err := tapClaim(h); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the unmarked claim on %s was kept (%v)", name, err)
	}
}

// ANOTHER JAIL'S MARK SPARES NOTHING OF THIS ONE. A mark left on a name by a lease
// that held it before is not this lease's statement about its own device: Destroy
// deletes the device this launch made, and leaves the other lease's mark alone.
func TestAnotherJailsUnmadeMarkNeitherSparesNorIsCleared(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	name, err := tapClaim(h)
	if err != nil {
		t.Fatalf("the tap claim: %v", err)
	}

	other := otherLeaseOnAnotherStripe(t, theInstance)
	mark := filepath.Join(h.p.unmadeDir(), name)

	if err := os.WriteFile(mark, []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	deleted := false

	for _, run := range h.commands() {
		if run.bin == ipBinary && slices.Contains(run.args, "del") && slices.Contains(run.args, name) {
			deleted = true
		}
	}

	if !deleted {
		t.Errorf("Destroy left %s, the device its launch made, because another lease had marked the name", name)
	}

	raw, err := os.ReadFile(mark)
	if err != nil || strings.TrimSpace(string(raw)) != other {
		t.Errorf("the other lease's mark on %s = %q (%v), want it kept", name, raw, err)
	}
}

// A CLAIM THAT IS NOT THIS JAIL'S IS NEVER MARKED BY IT, either way.
func TestATapClaimIsMarkedOnlyByTheJailThatHoldsIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := h.p.claimsDir("taps")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	other := otherLeaseOnAnotherStripe(t, theInstance)
	if err := os.WriteFile(filepath.Join(dir, "bt-5"), []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.p.markTap(h.p.jailFor(theInstance), "bt-5", false); err == nil {
		t.Error("a lease marked another lease's tap claim")
	}

	if _, err := os.Stat(filepath.Join(h.p.unmadeDir(), "bt-5")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused mark was written anyway: %v", err)
	}
}
