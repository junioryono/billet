package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
// the snapshots directory) with the golden's own system disk published beside it,
// and restores a second lease from it onto a clone of that disk.
//
// It skips wherever requireRealHost does: everywhere but a prepared host.
func TestRealFirecrackerRestoresACapturedMicroVM(t *testing.T) {
	env := requireRealHost(t)

	golden, err := New(selftestDeployment, env.cfg, env.disk, WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	goldenName := provider.InstanceName(env.lease)
	name := "selftest-" + env.lease[:12]

	// THE GOLDEN'S SYSTEM DISK GOES LAST: it is the builder's clone, renamed, so the
	// builder's mapping and every fork's clone of it have to be gone first. Cleanups
	// run in reverse, so this is registered before the builder's Destroy.
	t.Cleanup(func() {
		if err := env.disk.removeGolden(context.WithoutCancel(t.Context()), goldenDiskPrefix+name); err != nil {
			t.Errorf("remove the golden's system disk: %v", err)
		}
	})

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

	// PUBLISHED AS A NODE WILL PUBLISH IT, by Capture: copied out of the jail into
	// the store, owned by billet, read-only, with the host it was taken on.
	published := filepath.Join(golden.snapshotsDir(), name)

	// THE STORE AND ITS PUBLISH LOCK STAY. Every publisher on this node shares them,
	// and removing the lock file, even while holding it, would let a later publisher
	// lock a new inode beside one already waiting on the old. Only this test's own
	// snapshots are removed.
	t.Cleanup(func() {
		if err := os.RemoveAll(published); err != nil {
			t.Errorf("remove the published snapshot: %v", err)
		}
	})

	captureStarted := time.Now()

	if err := golden.Capture(t.Context(), goldenName, name); err != nil {
		t.Fatalf("Capture the golden: %v", err)
	}

	t.Logf("captured and published a 1 GiB guest in %s", time.Since(captureStarted).Round(time.Millisecond))

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile, compatibilityFile} {
		if err := checkPublished(filepath.Join(published, file)); err != nil {
			t.Errorf("the published %s: %v", file, err)
		}
	}

	// THE DISK THE MEMORY WAS TAKEN WITH, published by the capture itself: the
	// builder's clone, flushed while the guest was paused and renamed to the
	// golden's name, which the record names for every restore.
	var record compatibility
	if raw, err := os.ReadFile(filepath.Join(published, compatibilityFile)); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("the record a real capture wrote: %v", err)
	}

	if !strings.HasPrefix(record.Disk, goldenDiskPrefix+name+"@g") || record.Image != env.image {
		t.Fatalf("the capture recorded disk %q from %q, want the golden's own disk from %q",
			record.Disk, record.Image, env.image)
	}

	// THE SAME PROVIDER RESTORES: its restore clones the record's disk, wherever the
	// image pool is.
	forks := golden
	forkName := provider.InstanceName(freshLease(t))
	forkSnapshot := "selftest-fork-" + env.lease[:8]

	// AFTER THE FORK, for the reason the golden's disk goes after its builder: the
	// fork's capture publishes the fork's clone too.
	t.Cleanup(func() {
		if err := env.disk.removeGolden(context.WithoutCancel(t.Context()), goldenDiskPrefix+forkSnapshot); err != nil {
			t.Errorf("remove the fork's system disk: %v", err)
		}
	})

	t.Cleanup(func() {
		if _, err := forks.Destroy(context.WithoutCancel(t.Context()), forkName); err != nil {
			t.Errorf("cleanup the fork: %v", err)
		}
	})

	inst, err := forks.Restore(t.Context(), provider.Spec{
		Name: forkName, Image: env.image, VCPU: 2, Memory: 1 * config.GiB,
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

	// AND THE FORK CAN BE CAPTURED IN TURN, which a capture writing over the
	// read-only links its own restore placed could not, while the snapshot it came
	// from stays the file it was.
	t.Cleanup(func() {
		if err := os.RemoveAll(filepath.Join(golden.snapshotsDir(), forkSnapshot)); err != nil {
			t.Errorf("remove the fork's snapshot: %v", err)
		}
	})

	if err := forks.Capture(t.Context(), forkName, forkSnapshot); err != nil {
		t.Errorf("Capture of a restored fork: %v", err)
	} else if err := samePublished(filepath.Join(published, snapshotMemoryFile),
		filepath.Join(fj.root(), snapshotMemoryFile)); err != nil {
		t.Errorf("capturing the fork changed the snapshot it was restored from: %v", err)
	}

	// AND A SNAPSHOT TAKEN ON ANOTHER HOST KERNEL IS REFUSED BEFORE THE LOAD, by the
	// real provider against the record a real capture wrote.
	incompatible := filepath.Join(golden.snapshotsDir(), "selftest-elsewhere-"+env.lease[:8])
	if err := os.Mkdir(incompatible, 0o755); err != nil {
		t.Fatalf("make the incompatible directory: %v", err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(incompatible); err != nil {
			t.Errorf("remove the incompatible directory: %v", err)
		}
	})

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		if err := os.Link(filepath.Join(published, file), filepath.Join(incompatible, file)); err != nil {
			t.Fatalf("link %s into the incompatible directory: %v", file, err)
		}
	}

	stagedRecord, err := os.ReadFile(filepath.Join(published, compatibilityFile))
	if err != nil {
		t.Fatal(err)
	}

	var elsewhere compatibility
	if err := json.Unmarshal(stagedRecord, &elsewhere); err != nil {
		t.Fatalf("the record a real capture wrote: %v", err)
	}

	elsewhere.HostKernel = "5.10.0-elsewhere #1 SMP"

	if raw, err := json.Marshal(elsewhere); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(incompatible, compatibilityFile), raw, 0o444); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(filepath.Join(incompatible, compatibilityFile), 0o444); err != nil {
		t.Fatal(err)
	}

	elsewhereName := provider.InstanceName(freshLease(t))

	t.Cleanup(func() {
		if _, err := forks.Destroy(context.WithoutCancel(t.Context()), elsewhereName); err != nil {
			t.Errorf("cleanup the restore that should have been refused: %v", err)
		}
	})

	_, err = forks.Restore(t.Context(), provider.Spec{
		Name: elsewhereName, Image: env.image, VCPU: 2, Memory: 1 * config.GiB,
		Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}, filepath.Base(incompatible))
	if !errors.Is(err, errSnapshotIncompatible) || !strings.Contains(err.Error(), "host kernel") {
		t.Errorf("Restore of a snapshot taken on another host kernel = %v, want a refusal naming it", err)
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
		Name: refusedName, Image: env.image, VCPU: 2, Memory: 1 * config.GiB,
		Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}, filepath.Base(foreign))
	if !errors.Is(err, errSnapshotNotPublished) || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("Restore from a directory a jail-range uid owns = %v, want refusal naming the owner", err)
	}
}
