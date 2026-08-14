package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// NOTHING IS DESTROYED UNTIL THE VMM IS PROVABLY STOPPED.
//
// This is the failure the inventory exists to prevent, reached through teardown.
// The jail holds the pid file, which is the ONLY handle for ever stopping this
// microVM, and List reports a lease only while its jail is there. Remove the jail
// while the VMM is still running — or while billet merely could not TELL whether it
// is — and the guest becomes a job nothing can find, nothing can kill, and whose
// capacity is handed back to be sold to somebody else.
func TestTeardownStopsBeforeItDestroysAnything(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)

	// A pid file billet cannot read as a pid: "could not tell", which must stop
	// teardown exactly as a VMM that refuses to die does.
	if err := os.WriteFile(j.pidFile(), []byte("not a number"), 0o600); err != nil {
		t.Fatalf("stage a corrupt pid file: %v", err)
	}

	if err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("Destroy proceeded although it could not tell whether the vmm was running")
	}

	// THE THREE ASSERTIONS THAT MAKE THIS TEST ABOUT THE CODE. Without them it
	// passes against a teardown that reports the error and demolishes everything
	// anyway, which is what it did.
	if _, err := os.Stat(j.dir()); err != nil {
		t.Errorf("the jail was removed although the vmm was not stopped, so nothing can ever "+
			"stop it now and List no longer reports the lease: %v", err)
	}

	if got := h.disk.discards(); len(got) != 0 {
		t.Errorf("the root disk was discarded out from under a possibly-live guest: %v", got)
	}

	for _, r := range h.commands() {
		if len(r.args) >= 2 && r.args[0] == "link" && r.args[1] == "del" {
			t.Error("the guest's network was removed although the vmm was not stopped")
		}
	}
}

// AND THE LEASE IS STILL IN THE INVENTORY AFTERWARDS, which is the consequence the
// assertions above are really about.
func TestAMicroVMThatCouldNotBeStoppedStaysInTheInventory(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	if err := os.WriteFile(h.p.jailFor(theInstance).pidFile(), []byte("nonsense"), 0o600); err != nil {
		t.Fatalf("stage a corrupt pid file: %v", err)
	}

	//nolint:errcheck // the refusal itself is asserted by the test above
	_ = h.p.Destroy(t.Context(), theInstance)

	found, err := h.p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(found) != 1 || found[0].Name != theInstance {
		t.Errorf("a microVM billet could not stop has disappeared from the inventory, so its "+
			"capacity will be resold: %+v", found)
	}
}

// LIST REPORTS AN ORDINARY, OWNED, RUNNING MICROVM.
//
// The other List cases are all about what it must NOT report — another deployment's,
// an unreadable marker, a fresh host. Without this one, a List that dropped every
// jail whose marker matches passes all of them: the headline failure mode of this
// backend, untested in the ordinary case.
func TestListReportsAnOwnedRunningMicroVM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	found, err := h.p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("List reported %d microVMs, want the one that was just launched: %+v",
			len(found), found)
	}

	if found[0].Name != theInstance || found[0].ID != theInstance {
		t.Errorf("List reported %+v", found[0])
	}

	if !found[0].Running {
		t.Error("List reported a running microVM as not running, which makes the caller " +
			"destroy it")
	}
}

// A BINARY UPGRADE MUST NOT EMPTY THE INVENTORY.
//
// The jailer names its chroot after the RESOLVED --exec-file, and the reference host
// installs firecracker as a versioned filename behind a stable symlink so a version
// can be bumped and rolled back. Retarget the symlink, restart billet — which is
// documented, and which billet promises leaves running jobs running — and every
// guest started before the bump sits under the OLD directory. A List that read only
// the current one would return an inventory that is empty, short and error-free, and
// the control plane frees the capacity of every lease absent from it.
func TestAMicroVMSurvivesTheBinaryBeingUpgradedUnderIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	// The upgrade: a new versioned binary, the stable symlink retargeted, billet
	// restarted. The jail from before the bump stays where the old jailer put it.
	dir := filepath.Dir(h.p.cfg.BinaryPath)
	next := filepath.Join(dir, "firecracker-v1.17.0")

	if err := os.WriteFile(next, []byte("#!/bin/true\n"), 0o600); err != nil {
		t.Fatalf("stage the new binary: %v", err)
	}

	if err := os.Remove(h.p.cfg.BinaryPath); err != nil {
		t.Fatalf("clear the symlink: %v", err)
	}

	if err := os.Symlink(next, h.p.cfg.BinaryPath); err != nil {
		t.Fatalf("retarget the symlink: %v", err)
	}

	upgraded, err := New("deployment-a", h.p.cfg, h.disk,
		withRunner(h.record), withJailUser(1000, 1000))
	if err != nil {
		t.Fatalf("New after the upgrade: %v", err)
	}

	if upgraded.execName != "firecracker-v1.17.0" {
		t.Fatalf("the upgraded provider resolved %q, so this case stages nothing",
			upgraded.execName)
	}

	found, err := upgraded.List(t.Context())
	if err != nil {
		t.Fatalf("List after the upgrade: %v", err)
	}

	if len(found) != 1 || found[0].Name != theInstance {
		t.Errorf("a microVM started before a binary upgrade vanished from the inventory, so "+
			"its capacity is handed back while the guest runs: %+v", found)
	}

	// AND IT IS STILL FINDABLE AND STILL DESTROYABLE, which is the half that stops
	// the guest becoming permanently unkillable.
	if _, ok, err := upgraded.Find(t.Context(), theInstance); err != nil || !ok {
		t.Errorf("Find after the upgrade: found=%v err=%v", ok, err)
	}

	if err := upgraded.Destroy(t.Context(), theInstance); err != nil {
		t.Errorf("Destroy after the upgrade: %v", err)
	}

	if _, err := os.Stat(h.p.jailFor(theInstance).dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pre-upgrade jail survived its destroy: %v", err)
	}
}

// A JAIL LEFT BY AN EARLIER RUN IS REFUSED AND LEFT ALONE.
//
// Every other launch failure unwinds what the launch MADE. This one is a refusal to
// touch what was already there, so tearing it down would destroy the state of a
// microVM this launch never created — while the error still told the operator it had
// been left in place.
func TestARefusedRelaunchDoesNotDestroyTheJailItRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)

	before := h.disk.discards()

	_, err := h.p.Launch(t.Context(), aSpec())
	if err == nil {
		t.Fatal("Launch reused a jail the jailer would have refused")
	}

	if !errors.Is(err, ErrJailExists) {
		t.Errorf("the refusal is not the one that says a jail is in the way: %v", err)
	}

	if _, statErr := os.Stat(j.dir()); statErr != nil {
		t.Errorf("the refused launch destroyed the previous microVM's jail: %v", statErr)
	}

	if got := h.disk.discards(); len(got) != len(before) {
		t.Errorf("the refused launch discarded the previous microVM's root disk: %v", got)
	}
}

// ANOTHER DEPLOYMENT'S MICROVM IS NOT THIS ONE'S TO DESTROY, checked at the layer
// that ACTS rather than only at the one that reports. Find already refuses to report
// one; without the same check here, Destroy would stop it, unlink it and discard its
// disk on the strength of a name.
func TestDestroyRefusesAnotherDeploymentsMicroVM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	stranger, err := New("deployment-b", h.p.cfg, &fakeDisk{device: "/dev/rbd1"},
		withJailUser(1000, 1000))
	if err != nil {
		t.Fatalf("New for the second deployment: %v", err)
	}

	if err := stranger.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("a second billet on this machine destroyed the first's microVM")
	}

	if _, err := os.Stat(h.p.jailFor(theInstance).dir()); err != nil {
		t.Errorf("the first deployment's jail was removed by the second: %v", err)
	}
}

// A VMM THAT IGNORES SIGTERM IS KILLED.
//
// The escalation had no coverage at all: the stand-in used elsewhere is `sleep`,
// which dies on SIGTERM, so the SIGKILL path was never reached. A guest that will
// not go is holding a mapped block device open while its capacity stays charged.
func TestAVMMThatIgnoresSIGTERMIsKilled(t *testing.T) {
	requireProc(t)

	h := newHarness(t)

	var pid int

	h.onJailer = func(id string) {
		h.serveVMM(t, id)

		// TRAPS SIGTERM AND CARRIES ON, which is what a wedged VMM does.
		pid = standIn(t, id, true)

		if err := os.WriteFile(h.p.jailFor(id).pidFile(),
			[]byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Errorf("write the pid file: %v", err)
		}
	}

	if _, err := h.p.Launch(t.Context(), aSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if pid == 0 {
		t.Fatal("no stand-in vmm was started")
	}

	if err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	owns, err := pidIsVMM(pid, theInstance)
	if err != nil {
		t.Fatalf("pidIsVMM after Destroy: %v", err)
	}

	if owns {
		t.Error("a vmm that ignores SIGTERM survived Destroy, so it is still holding its " +
			"root disk open while the capacity has been handed back")
	}
}

// A PID BILLET CANNOT VERIFY IS NEVER SIGNALLED, not even when the VMM has already
// failed to stop.
//
// The escalation runs on "SIGTERM did not work", and that failure arrives for two
// unlike reasons: the VMM outlived its grace, or billet could not TELL whether the
// pid is still the VMM. Killing on the second is the act process.go refuses by name
// — sending a signal, as root, to a number nothing ties to this jail, on a machine
// where the kernel has long since reused it.
func TestAPidBilletCannotVerifyIsNeverKilled(t *testing.T) {
	requireProc(t)

	h := newHarness(t, withPidOwner(func(int, string) (bool, error) {
		// The shape a permission error on /proc takes: not "gone", not "still
		// ours", but "billet does not know".
		return false, errors.New("cannot tell whether this pid is still the microVM")
	}))

	var pid int

	h.onJailer = func(id string) {
		h.serveVMM(t, id)

		// Traps SIGTERM, so the only thing that could end it is a SIGKILL — which
		// is exactly what must NOT be sent.
		pid = standIn(t, id, true)

		if err := os.WriteFile(h.p.jailFor(id).pidFile(),
			[]byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Errorf("write the pid file: %v", err)
		}
	}

	if _, err := h.p.Launch(t.Context(), aSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("Destroy reported success although it could not verify the vmm's pid")
	}

	// THE PROCESS IS STILL THERE. Checked against /proc rather than through the
	// seam this test replaced, or it would be asserting its own stub.
	raw, err := os.ReadFile(procCmdline(pid))
	if err != nil {
		t.Fatalf("billet killed a pid it could not verify: %v", err)
	}

	if !strings.Contains(string(raw), theInstance) {
		t.Errorf("the stand-in vmm is gone or was replaced: %q", raw)
	}
}

// THE GUEST KERNEL IS NOT HANDED TO THE ACCOUNT THE VMM RUNS AS.
//
// It is placed as a HARD LINK, so it is the same inode as the operator's only copy
// on the host — chowning the name inside the jail chowns that file. Every VMM on the
// machine runs as this account, and the owner of a file may chmod and write it, so
// one VMM escape would rewrite the kernel every later microVM boots. Nothing needs
// to leave the chroot for that: the link is inside it.
func TestTheGuestKernelIsNotGivenToTheJailedAccount(t *testing.T) {
	t.Parallel()

	var chowned []string

	h := newHarness(t, withPrivileged(
		func(path, _ string, _, _ int) error {
			return os.WriteFile(path, []byte("device node"), 0o600)
		},
		func(root string, _, _ int) error {
			chowned = append(chowned, root)

			return nil
		},
	))

	h.launch(t)

	j := h.p.jailFor(theInstance)

	for _, root := range chowned {
		// The kernel and the owner marker both live ABOVE root/: the kernel because
		// giving away a hard link gives away the inode, and the marker because it is
		// the fact List and Find trust when they decide whose microVM this is.
		if root == j.dir() {
			t.Errorf("the whole jail was given to the jailed account, which hands it the "+
				"operator's kernel inode and the owner marker: %s", root)
		}
	}

	if len(chowned) != 1 || chowned[0] != j.root() {
		t.Errorf("the chroot was not the thing given to the jailed account: %v", chowned)
	}
}

// A CRASH BETWEEN CLONING AND CLAIMING WOULD LEAVE A DISK NOTHING CAN ATTRIBUTE, so
// the jail is claimed FIRST. This asserts the ordering by the only observable it
// has: when the clone fails, the jail already existed and was cleaned up.
func TestTheJailIsClaimedBeforeTheDiskIsCloned(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	var existedAtCloneTime bool

	j := h.p.jailFor(theInstance)
	h.disk.onClone = func() {
		_, err := os.Stat(j.dir())
		existedAtCloneTime = err == nil
	}

	h.disk.cloneErr = errors.New("the cluster refused")

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil {
		t.Fatal("Launch continued without a root disk")
	}

	if !existedAtCloneTime {
		t.Error("the root disk was cloned before the jail existed, so a crash in between " +
			"leaves a mapped device and a pool image that nothing on this host can name")
	}

	// AND THE JAIL DOES NOT SURVIVE A CLONE THAT FAILED, or the lease could never
	// be launched again.
	if _, err := os.Stat(j.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the jail outlived a failed clone: %v", err)
	}
}

// TWO CONCURRENT LAUNCHES OF ONE LEASE: ONE WINS.
//
// A stat-then-mkdir pair lets both pass the check, because MkdirAll tolerates an
// existing directory — and the race then surfaces later and messily, inside the
// jailer, as a refusal about /dev/net/tun.
func TestOnlyOneLaunchCanClaimALease(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	j := h.p.jailFor(theInstance)

	if err := h.p.claim(j); err != nil {
		t.Fatalf("the first claim: %v", err)
	}

	err := h.p.claim(j)
	if err == nil {
		t.Fatal("two launches both claimed one lease")
	}

	if !errors.Is(err, ErrJailExists) {
		t.Errorf("the second claim failed for the wrong reason: %v", err)
	}

	if !strings.Contains(err.Error(), j.dir()) {
		t.Errorf("the error does not name what is in the way: %v", err)
	}
}
