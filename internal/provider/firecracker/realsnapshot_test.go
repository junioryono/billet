package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A REAL MICROVM, CAPTURED AND RESTORED THROUGH THE REAL JAILER.
//
// The fake VMM asserts what billet SAYS to Firecracker; this asserts that what it
// says restores a guest. It launches a golden from the test image, lets it run,
// captures it, publishes the capture the way a node will (root-owned, 0444, under
// the snapshots directory), snapshots the golden's own disk so the fork's disk is
// the one the memory was taken with, and restores a second lease from it.
//
// It skips wherever requireRealHost does: everywhere but a prepared host.
func TestRealFirecrackerRestoresACapturedMicroVM(t *testing.T) {
	env := requireRealHost(t)

	golden, err := New(selftestDeployment, env.cfg, env.disk, WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	goldenName := provider.InstanceName(env.lease)

	t.Cleanup(func() {
		if _, err := golden.Destroy(context.WithoutCancel(t.Context()), goldenName); err != nil {
			t.Errorf("cleanup the golden: %v", err)
		}
	})

	if _, err := golden.Launch(t.Context(), provider.Spec{
		Name: goldenName, Image: env.image, VCPU: 2, Memory: 1 * config.GiB,
		Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}); err != nil {
		t.Fatalf("Launch the golden: %v", err)
	}

	// LONG ENOUGH FOR THE GUEST KERNEL TO FINISH BOOTING. Firecracker's own
	// guidance is to snapshot only after boot, because VMGenID's interrupt on
	// restore can crash a kernel that has not set up its handling yet.
	time.Sleep(5 * time.Second)

	gj := golden.jailFor(goldenName)

	if err := capture(t.Context(), golden.apiFor(gj.socket()),
		snapshotFiles{State: "/" + snapshotStateFile, Memory: "/" + snapshotMemoryFile}); err != nil {
		t.Fatalf("capture the golden: %v", err)
	}

	// PUBLISHED AS A NODE WILL PUBLISH IT: out of the jail, owned by billet, read-only.
	name := "selftest-" + env.lease[:12]
	published := filepath.Join(golden.snapshotsDir(), name)

	if err := os.MkdirAll(published, 0o755); err != nil {
		t.Fatalf("make the snapshot directory: %v", err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(published); err != nil {
			t.Errorf("remove the published snapshot: %v", err)
		}
	})

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		dst := filepath.Join(published, file)
		if err := os.Rename(filepath.Join(gj.root(), file), dst); err != nil {
			t.Fatalf("publish %s: %v", file, err)
		}

		if err := os.Chown(dst, 0, 0); err != nil {
			t.Fatalf("own %s: %v", file, err)
		}

		if err := os.Chmod(dst, 0o444); err != nil {
			t.Fatalf("make %s read-only: %v", file, err)
		}
	}

	// THE DISK THE MEMORY WAS TAKEN WITH, not a fresh golden: a restored kernel's
	// page cache describes this disk, and another one under it is corruption.
	captured := env.disk.cache + "/" + goldenName + "@cap"

	// REGISTERED BEFORE THE CREATE, because a create cancelled after Ceph made the
	// snapshot reports failure and would otherwise leave it, which then stops the
	// golden's disk being removed. Only a confirmed absence counts as nothing to do.
	t.Cleanup(func() {
		out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "rbd",
			env.disk.args("snap", "rm", captured)...).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such file or directory") {
			t.Errorf("remove the golden's disk snapshot %s: %v: %s", captured, err, out)
		}
	})

	if out, err := exec.CommandContext(t.Context(), "rbd",
		env.disk.args("snap", "create", captured)...).CombinedOutput(); err != nil {
		t.Fatalf("snapshot the golden's disk: %v: %s", err, out)
	}

	// A SECOND PROVIDER WHOSE "IMAGE POOL" IS THE CACHE POOL, so the clone it makes
	// for the fork is of the captured disk. Same deployment, same host state.
	forks, err := New(selftestDeployment, env.cfg,
		rbdDisk{images: env.disk.cache, cache: env.disk.cache, id: env.disk.id},
		WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New for the fork: %v", err)
	}

	forkName := provider.InstanceName(freshLease(t))

	t.Cleanup(func() {
		if _, err := forks.Destroy(context.WithoutCancel(t.Context()), forkName); err != nil {
			t.Errorf("cleanup the fork: %v", err)
		}
	})

	inst, err := forks.Restore(t.Context(), provider.Spec{
		Name: forkName, Image: goldenName + "@cap", VCPU: 2, Memory: 1 * config.GiB,
		Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}, name)
	if err != nil {
		t.Fatalf("Restore against real firecracker: %v", err)
	}

	if !inst.Running {
		t.Error("Restore reported an instance that is not running")
	}

	// STILL RUNNING A MOMENT LATER, which a guest that crashed on its VMGenID
	// interrupt, or a memory file the VMM could not map, would not be.
	time.Sleep(3 * time.Second)

	fj := forks.jailFor(forkName)

	info, err := forks.apiFor(fj.socket()).info(t.Context())
	if err != nil {
		t.Fatalf("ask the restored VMM its state: %v", err)
	}

	if info.State != stateRunning || info.ID != forkName {
		t.Errorf("the restored VMM reports %s in state %s, want %s running", info.ID, info.State, forkName)
	}

	// THE MEMORY IN THE JAIL IS THE PUBLISHED FILE, never chowned to the jail.
	if err := samePublished(filepath.Join(published, snapshotMemoryFile),
		filepath.Join(fj.root(), snapshotMemoryFile)); err != nil {
		t.Errorf("the fork's memory file: %v", err)
	}

	// AND A SNAPSHOT DIRECTORY ANOTHER ACCOUNT OWNS IS NOT RESTORED FROM. Only root
	// can make one, which is why this half lives here: the owner of a directory can
	// rename any file in it, so a jail-range uid owning one could swap the memory a
	// restore links.
	foreign := filepath.Join(golden.snapshotsDir(), "selftest-foreign-"+env.lease[:8])
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatalf("make the foreign directory: %v", err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(foreign); err != nil {
			t.Errorf("remove the foreign directory: %v", err)
		}
	})

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		if err := os.Link(filepath.Join(published, file), filepath.Join(foreign, file)); err != nil {
			t.Fatalf("link %s into the foreign directory: %v", file, err)
		}
	}

	if err := os.Chown(foreign, env.cfg.JailUIDMin+env.cfg.JailUIDCount-1, env.cfg.JailUIDMin+env.cfg.JailUIDCount-1); err != nil {
		t.Fatalf("hand the directory to a jail-range uid: %v", err)
	}

	refusedName := provider.InstanceName(freshLease(t))

	// REGISTERED BEFORE THE CALL, because the refusal is what is under test: a
	// broken check restores this lease, and its clone would then hold the golden's
	// disk snapshot and strand the golden too (which a mutation of the owner check
	// did, on the reference node).
	t.Cleanup(func() {
		if _, err := forks.Destroy(context.WithoutCancel(t.Context()), refusedName); err != nil {
			t.Errorf("cleanup the restore that should have been refused: %v", err)
		}
	})

	_, err = forks.Restore(t.Context(), provider.Spec{
		Name: refusedName, Image: goldenName + "@cap", VCPU: 2, Memory: 1 * config.GiB,
		Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}, filepath.Base(foreign))
	if !errors.Is(err, errSnapshotNotPublished) || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("Restore from a directory a jail-range uid owns = %v, want refusal naming the owner", err)
	}
}
