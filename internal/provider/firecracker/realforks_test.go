package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// ONE GOLDEN, RESTORED AS MANY FORKS AT ONCE (#508, item 4).
//
// The other real tests restore one fork; this restores batches of 1, 5 and 10 from
// one capture concurrently and asserts what each fork must have alone: it runs, in a
// jail of its own with its own uid and tap, from the published memory file (the same
// read-only inode, never chowned), on a clone of its own whose parent is the golden's
// generation. It logs how long each batch took, beside the 2026-10-16 measurements in
// docs/reference/records/workspace-snapshots.md. The load order, the empty data store
// after a load and the refused fingerprint are the fake VMM's to assert, and
// TestRealFirecrackerRestoresACapturedMicroVM refuses a real snapshot of another host.
//
// It skips wherever requireRealHost does: everywhere but a prepared host.
func TestRealFirecrackerRestoresAGoldenAsManyForks(t *testing.T) {
	env := requireRealHost(t)

	p, err := New(selftestDeployment, env.cfg, env.disk, WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	builder := provider.InstanceName(env.lease)
	snapshot := "selftest-forks-" + env.lease[:12]
	published := filepath.Join(p.snapshotsDir(), snapshot)

	// THE GOLDEN'S DISK GOES LAST, after the builder and every fork (cleanups run in
	// reverse), because each holds a mapping or a clone of it.
	t.Cleanup(func() {
		if err := env.disk.removeGolden(context.WithoutCancel(t.Context()), goldenDiskPrefix+env.lease); err != nil {
			t.Errorf("remove the golden's system disk: %v", err)
		}
	})

	t.Cleanup(func() {
		if _, err := p.Destroy(context.WithoutCancel(t.Context()), builder); err != nil {
			t.Errorf("cleanup the builder: %v", err)
		}
	})

	t.Cleanup(func() {
		if err := os.RemoveAll(published); err != nil {
			t.Errorf("remove the published snapshot: %v", err)
		}
	})

	spec := func(name string) provider.Spec {
		return provider.Spec{
			Name: name, Image: env.image, VCPU: 2, Memory: 1 * config.GiB,
			Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
			JITConfig: "not-a-real-registration",
		}
	}

	if _, err := p.Launch(t.Context(), spec(builder)); err != nil {
		t.Fatalf("Launch the builder: %v", err)
	}

	// LONG ENOUGH FOR THE GUEST KERNEL TO FINISH BOOTING, for the reason the single
	// restore test waits.
	time.Sleep(5 * time.Second)

	evidence, err := p.Capture(t.Context(), builder, snapshot)
	if err != nil {
		t.Fatalf("Capture the golden: %v", err)
	}

	_, generation, _ := strings.Cut(evidence.Disk, "@")

	for _, n := range []int{1, 5, 10} {
		t.Run(fmt.Sprintf("%d forks", n), func(t *testing.T) {
			names := make([]string, n)
			for i := range names {
				names[i] = provider.InstanceName(freshLease(t))
			}

			// REGISTERED BEFORE THE RESTORES: a fork that started is destroyed even when
			// the batch fails, and a Destroy of one that never started is a no-op.
			t.Cleanup(func() {
				for _, name := range names {
					if _, err := p.Destroy(context.WithoutCancel(t.Context()), name); err != nil {
						t.Errorf("cleanup the fork %s: %v", name, err)
					}
				}
			})

			errs := make([]error, n)
			took := make([]time.Duration, n)

			var wg sync.WaitGroup

			started := time.Now()

			for i, name := range names {
				wg.Add(1)

				go func() {
					defer wg.Done()

					began := time.Now()
					_, errs[i] = p.Restore(t.Context(), spec(name), snapshot, evidence)
					took[i] = time.Since(began)
				}()
			}

			wg.Wait()

			batch := time.Since(started)

			for i, err := range errs {
				if err != nil {
					t.Errorf("Restore of fork %d (%s): %v", i, names[i], err)
				}
			}

			if t.Failed() {
				return
			}

			sorted := slices.Clone(took)
			slices.Sort(sorted)
			t.Logf("restored %d forks concurrently in %s; each from request to running: fastest %s, median %s, "+
				"slowest %s", n, batch.Round(time.Millisecond), sorted[0].Round(time.Millisecond),
				sorted[n/2].Round(time.Millisecond), sorted[n-1].Round(time.Millisecond))

			// STILL RUNNING A MOMENT LATER, which a guest that crashed on its VMGenID
			// interrupt, or a memory file a VMM could not map, would not be.
			time.Sleep(3 * time.Second)

			uids := map[int]string{}
			taps := map[string]string{}

			for _, name := range names {
				j := p.jailFor(name)

				info, err := p.apiFor(j.socket()).info(t.Context())
				if err != nil {
					t.Errorf("ask %s its state: %v", name, err)

					continue
				}

				if info.State != stateRunning || info.ID != name {
					t.Errorf("the VMM of %s reports %s in state %s, want it running", name, info.ID, info.State)
				}

				// THE PUBLISHED MEMORY, the one read-only inode every fork maps.
				if err := samePublished(filepath.Join(published, snapshotMemoryFile),
					filepath.Join(j.root(), snapshotMemoryFile)); err != nil {
					t.Errorf("the memory file of %s: %v", name, err)
				}

				res, err := resourcesOf(j)
				if err != nil {
					t.Errorf("read what %s claimed: %v", name, err)

					continue
				}

				if other, taken := uids[res.UID]; taken {
					t.Errorf("%s and %s run as one uid, %d", name, other, res.UID)
				}

				if other, taken := taps[res.Tap]; taken {
					t.Errorf("%s and %s share the tap %s", name, other, res.Tap)
				}

				uids[res.UID], taps[res.Tap] = name, name

				// A CLONE OF ITS OWN, whose parent is the golden's generation.
				parent, err := rbdParent(t.Context(), env.disk, name)
				if err != nil {
					t.Errorf("read the parent of %s's disk: %v", name, err)
				} else if parent != goldenDiskPrefix+env.lease+"@"+generation {
					t.Errorf("the disk of %s is a clone of %s, want the golden's %s@%s", name, parent,
						goldenDiskPrefix+env.lease, generation)
				}
			}
		})
	}
}

// rbdParent is the image@snapshot a root disk in the cache pool was cloned from.
func rbdParent(ctx context.Context, d rbdDisk, name string) (string, error) {
	out, err := exec.CommandContext(ctx, "rbd", d.args("info", "--format", "json", d.cache+"/"+name)...).Output()
	if err != nil {
		return "", err
	}

	var info struct {
		Parent struct {
			Pool     string `json:"pool"`
			Image    string `json:"image"`
			Snapshot string `json:"snapshot"`
		} `json:"parent"`
	}

	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("decode rbd info: %w", err)
	}

	if info.Parent.Pool != d.cache {
		return "", fmt.Errorf("its parent is in pool %q, not the cache pool %s", info.Parent.Pool, d.cache)
	}

	return info.Parent.Image + "@" + info.Parent.Snapshot, nil
}
