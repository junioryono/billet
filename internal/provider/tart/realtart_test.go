package tart

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// A real VM, created, listed, run and destroyed through real tart.
//
// The stub tests assert what billet SAYS; these assert that what it says
// matches what tart DOES — the firecracker backend's two launch-killing defects
// survived every unit test and died on the first real run, which is the whole
// argument for this file. The VM is created empty (`tart create --linux`, no
// OS), which needs no image download: the guest boots, finds nothing to run,
// and halts itself, which is enough to drive every state the provider reads.
//
// TART_HOME is pointed at a per-test directory so nothing here can see or touch
// an operator's own VMs.

// realHome isolates both sides — the provider's marker paths and the tart
// binary's store — in one per-test directory.
func realHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("TART_HOME", home)

	return home
}

// createEmptyVM makes an OS-less VM the way Launch's clone would place one,
// marker included.
func createEmptyVM(t *testing.T, p *Provider, name string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(),
		"tart", "create", name, "--linux", "--disk-size", "1").CombinedOutput()
	if err != nil {
		t.Fatalf("tart create: %v: %s", err, out)
	}

	t.Cleanup(func() {
		// Belt and braces: a failed test must not leak a VM. On the happy path the
		// VM is already destroyed and this answers "does not exist", which is not a
		// leak — anything else left something behind and is worth a line.
		out, err := exec.CommandContext(context.WithoutCancel(t.Context()),
			"tart", "delete", name).CombinedOutput()
		if err != nil && !isMissingVM(fmt.Errorf("%w: %s", err, out)) {
			t.Logf("cleanup could not delete %s: %v: %s", name, err, out)
		}
	})

	if err := p.writeOwner(name); err != nil {
		t.Fatalf("writeOwner: %v", err)
	}
}

func TestRealTartLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	home := realHome(t)

	p, err := New(testOwner, WithHome(home))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const name = "billet-realtart"

	createEmptyVM(t, p, name)

	// The real `tart list --format json` must parse, pass the directory
	// cross-check, and report the VM as this deployment's.
	inst, ok, err := p.Find(t.Context(), name)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if !ok {
		t.Fatal("a created, marked VM was not found")
	}

	if inst.Running {
		t.Error("a created VM must report stopped before it runs")
	}

	// The detached run boots the guest; with no OS it halts itself, so the
	// observable contract is that tart accepted the run and the state machine
	// travels back to stopped rather than wedging.
	if err := p.startDetached(name); err != nil {
		t.Fatalf("startDetached: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)

	for {
		inst, ok, err = p.Find(t.Context(), name)
		if err != nil {
			t.Fatalf("Find while settling: %v", err)
		}

		if !ok {
			t.Fatal("the VM vanished from inventory while its guest ran")
		}

		if !inst.Running {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the empty guest never halted; the state never returned to stopped")
		}

		time.Sleep(200 * time.Millisecond)
	}

	// Destroy against the real thing: stop answers "is not running" for the
	// already-halted guest, the state is proved through the real list, and the
	// delete is confirmed.
	got, err := p.Destroy(t.Context(), name)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if got != provider.TeardownStopped {
		t.Errorf("Teardown = %v, want stopped", got)
	}

	if _, ok, err := p.Find(t.Context(), name); err != nil || ok {
		t.Errorf("Find after destroy = %v, %v; want absent", ok, err)
	}

	// Idempotent against the real store: a second destroy of the same name is
	// success, not an error.
	if got, err := p.Destroy(t.Context(), name); err != nil || got != provider.TeardownStopped {
		t.Errorf("second Destroy = %v, %v; want stopped, nil", got, err)
	}
}

// The two error phrasings billet matches are tart's own, and this is the test
// that notices when a tart release rewords them: isNotRunning and isMissingVM
// are string matches, and a phrasing drift turns an idempotent teardown into a
// stuck one.
func TestRealTartErrorPhrasingsStillMatch(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	home := realHome(t)

	p, err := New(testOwner, WithHome(home))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const name = "billet-realphrase"

	createEmptyVM(t, p, name)

	// Stopping a VM that is not running must be recognisable as such.
	if _, err := p.run(t.Context(), "stop", name); err == nil {
		t.Fatal("stopping a stopped VM succeeded; the phrasing test needs its error")
	} else if !isNotRunning(err) {
		t.Errorf("isNotRunning missed tart's real phrasing: %v", err)
	}

	if _, err := p.run(t.Context(), "delete", name); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Deleting or stopping what does not exist must be recognisable as such.
	if _, err := p.run(t.Context(), "delete", name); err == nil {
		t.Fatal("deleting a missing VM succeeded; the phrasing test needs its error")
	} else if !isMissingVM(err) {
		t.Errorf("isMissingVM missed tart's real phrasing: %v", err)
	}
}

// The digest-resolution phrasings billet matches are tart's own. `tart fqn`
// answers from the local OCI cache, so an unpulled reference errors instantly
// and without network — which is what makes this testable offline.
func TestRealTartImageResolutionPhrasingsStillMatch(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	realHome(t)

	p, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// An unpulled remote reference must be refused by NAME, which depends on
	// recognising VMStorageOCI's exact phrasing — that is what this test is
	// really pinning, against the real binary rather than the stub.
	_, err = p.resolveImage(t.Context(), "ghcr.io/example/never-pulled:latest")
	if err == nil || !strings.Contains(err.Error(), "ghcr.io/example/never-pulled:latest is not pulled") {
		t.Errorf("resolveImage on an unpulled ref = %v, want it refused by name", err)
	}

	// A local VM name passes through unchanged.
	got, err := p.resolveImage(t.Context(), "some-local-image")
	if err != nil || got != "some-local-image" {
		t.Errorf("resolveImage(local) = %q, %v; want the name back", got, err)
	}
}

// The stub's JSON is billet's own guess; the real `tart list` output is the
// fact. A field rename in a tart release must fail here, not in production
// reconciliation.
func TestRealTartListShapeStillMatches(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	home := realHome(t)

	p, err := New(testOwner, WithHome(home))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const name = "billet-realshape"

	createEmptyVM(t, p, name)

	vms, err := p.list(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	found := false

	for _, vm := range vms {
		if vm.Name != name {
			continue
		}

		found = true

		if !strings.EqualFold(vm.Source, "local") {
			t.Errorf("Source = %q, want local", vm.Source)
		}

		if !strings.EqualFold(vm.State, "stopped") {
			t.Errorf("State = %q, want stopped for a never-run VM", vm.State)
		}

		if vm.Running {
			t.Error("Running = true for a never-run VM")
		}
	}

	if !found {
		t.Fatalf("the created VM is missing from the parsed list: %+v", vms)
	}
}

// TWO BILLET DEPLOYMENTS SHARING ONE MAC MUST NOT SEE EACH OTHER'S GUESTS, and
// this asserts it against a REAL tart store rather than the stub's.
//
// The stub tests already cover the ownership logic. What they cannot cover is
// the thing that makes this dangerous on a Mac: both deployments run the same
// binary against the same per-user store, so `tart list` genuinely returns the
// other one's VM. Nothing filters it out at the tart layer — the marker file is
// the ONLY thing standing between two deployments, and reconciliation acts on
// what List returns by freeing capacity and destroying compute.
//
// A Mac is where this stops being hypothetical: one machine is expensive, so
// running a personal deployment beside a team one is an ordinary thing to do.
func TestRealTartKeepsTwoDeploymentsApart(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	realHome(t)

	mine, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The SAME store, a different deployment identity — which is exactly the
	// shape of two billets on one Mac.
	theirs, err := New(foreignOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const name = "billet-realtwodeploy"

	// Created and marked as THEIRS.
	createEmptyVM(t, theirs, name)

	// tart itself sees it, which is what makes the marker load-bearing rather
	// than decorative: if this listing were empty the test would prove nothing.
	raw, err := mine.list(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var visible bool

	for _, vm := range raw {
		if vm.Name == name {
			visible = true
		}
	}

	if !visible {
		t.Fatalf("tart does not list %s at all, so nothing below is a real test", name)
	}

	// INVENTORY: not mine, so not in mine.
	instances, err := mine.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, inst := range instances {
		if inst.Name == name {
			t.Fatalf("List returned another deployment's VM; reconciliation would free "+
				"a lease that deployment still holds and then destroy its guest: %+v", inst)
		}
	}

	// LOOKUP: Find must not adopt it either. A launch that adopted a foreign VM
	// would hand somebody else's guest a fresh registration.
	if _, ok, err := mine.Find(t.Context(), name); err != nil || ok {
		t.Errorf("Find(%s) = ok %v, err %v; want it not found, because it is not ours",
			name, ok, err)
	}

	// TEARDOWN: the destructive one. Refused, and the VM still there afterwards.
	if _, err := mine.Destroy(t.Context(), name); err == nil {
		t.Error("Destroy accepted another deployment's VM")
	}

	after, err := theirs.List(t.Context())
	if err != nil {
		t.Fatalf("List as the owner: %v", err)
	}

	var survived bool

	for _, inst := range after {
		if inst.Name == name {
			survived = true
		}
	}

	if !survived {
		t.Error("the other deployment's VM is gone from its OWN inventory after we " +
			"touched it, which is the failure this test exists for")
	}
}

// BILLET'S LOCK LIVES INSIDE TART'S STORE, AND TART MUST NOT NOTICE.
//
// The store lock is a file billet writes into a directory tart owns, so the
// question is not whether flock works — that has its own tests — but whether
// putting it there changes what tart does. Measured when the mechanism was
// built, and kept here so a tart release that starts enumerating TART_HOME
// fails this rather than a teardown in production.
//
// The prune is the second half. A lock file whose PATH can be unlinked by
// ordinary traffic is not a lock: unlinking does not release the flock, but it
// detaches the name from the locked inode, so the next process creates a fresh
// file there and both run. That is the failure that ruled out a cache directory
// for the deployment lock one package over, so tart's most aggressive prune is
// run against a store holding the lock.
func TestRealTartIgnoresBilletsStoreLock(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	home := realHome(t)

	p, err := New(testOwner, WithHome(home))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// TAKEN AND HELD ACROSS EVERYTHING BELOW. An earlier version released it
	// here and then claimed the prune ran "against a store holding the lock",
	// which it did not — the sentence was true of the file and false of the
	// lock, and a review caught the gap between them.
	unlock, err := p.lockStore(t.Context(), "prove tart ignores this")
	if err != nil {
		t.Fatalf("lockStore against a real store: %v", err)
	}

	held := true

	defer func() {
		if held {
			unlock()
		}
	}()

	if _, err := os.Stat(p.storeLockPath()); err != nil {
		t.Fatalf("the store lock was not created at %s: %v", p.storeLockPath(), err)
	}

	const name = "billet-reallock"

	createEmptyVM(t, p, name)

	// tart's own list must still parse, still cross-check against the store, and
	// still report the VM: the cross-check enumerates TART_HOME/vms, which is one
	// directory along from where the lock sits.
	inst, ok, err := p.Find(t.Context(), name)
	if err != nil {
		t.Fatalf("Find with the lock file present: %v", err)
	}

	if !ok {
		t.Fatal("a real VM was not found while billet's lock file sat in the store")
	}

	if inst.Name != name {
		t.Errorf("Find returned %q, want %q", inst.Name, name)
	}

	// tart's own prune, in the most aggressive form its flags allow, against both
	// of the entry kinds it knows about.
	for _, entries := range []string{"caches", "vms"} {
		out, err := exec.CommandContext(t.Context(),
			"tart", "prune", "--entries", entries, "--older-than", "0").CombinedOutput()
		if err != nil {
			t.Fatalf("tart prune --entries %s: %v: %s", entries, err, out)
		}
	}

	if _, err := os.Stat(p.storeLockPath()); err != nil {
		t.Fatalf("tart's own prune removed billet's store lock at %s (%v); the next billet "+
			"would create a fresh file there and two of them would mutate names at once",
			p.storeLockPath(), err)
	}

	// AND THE LOCK ITSELF SURVIVED, not merely its file. Released only now, so
	// everything above ran against a store this process genuinely held.
	unlock()

	held = false

	again, err := p.lockStore(t.Context(), "take it after a prune")
	if err != nil {
		t.Fatalf("the store lock was unusable after a prune: %v", err)
	}

	again()
}
