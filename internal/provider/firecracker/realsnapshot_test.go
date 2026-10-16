package firecracker

import (
	"context"
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
	if out, err := exec.CommandContext(t.Context(), "rbd",
		env.disk.args("snap", "create", captured)...).CombinedOutput(); err != nil {
		t.Fatalf("snapshot the golden's disk: %v: %s", err, out)
	}

	t.Cleanup(func() {
		if out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "rbd",
			env.disk.args("snap", "rm", captured)...).CombinedOutput(); err != nil {
			t.Errorf("remove the golden's disk snapshot %s: %v: %s", captured, err, out)
		}
	})

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

	// AND THE VMM LOGGED NOTHING THAT SAYS IT FAILED.
	if raw, err := os.ReadFile(filepath.Join(fj.root(), vmmLog)); err == nil {
		for _, bad := range []string{"panic", "Error"} {
			if strings.Contains(string(raw), bad) {
				t.Errorf("the restored VMM's log contains %q:\n%s", bad, bounded(string(raw)))
			}
		}
	}
}
