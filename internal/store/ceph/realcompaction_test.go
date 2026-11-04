package ceph

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// A COMPACTION HOLDS WHAT ITS SOURCE HOLDS, AND IT IS THE ONE COMMAND THAT DOES. A
// real chain, on a real cluster: a parent holding data, and an object-map clone of
// it with a little of its own. `rbd cp` of that clone's snapshot exits 0 holding
// the clone's own layer alone, so the first half stands as the measurement behind
// compactCacheImage; if a Ceph release fixes it this fails and the rule can be
// looked at again. Opt-in, because it writes to a pool: BILLET_CEPH_TEST_POOL names
// one, with the host's default ceph.conf and two keyrings: admin's, which the bare
// rbd commands here use, and billet's own user's for the compaction.
func TestACompactionHoldsWhatItsSourceHolds(t *testing.T) {
	pool := os.Getenv("BILLET_CEPH_TEST_POOL")
	if pool == "" {
		t.Skip("BILLET_CEPH_TEST_POOL is unset; this test writes to a real Ceph pool and is opt-in")
	}

	rbd := func(args ...string) string {
		t.Helper()

		out, err := exec.CommandContext(t.Context(), "rbd", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("rbd %v: %v: %s", args, err, out)
		}

		return string(out)
	}
	export := func(spec string) [sha256.Size]byte {
		t.Helper()

		var body bytes.Buffer
		cmd := exec.CommandContext(t.Context(), "rbd", "export", "--no-progress", spec, "-")
		cmd.Stdout = &body
		if err := cmd.Run(); err != nil {
			t.Fatalf("rbd export %s: %v", spec, err)
		}
		if body.Len() == 0 {
			t.Fatalf("rbd export %s answered nothing", spec)
		}

		return sha256.Sum256(body.Bytes())
	}
	write := func(image string, offset int, fill byte) {
		t.Helper()

		device := strings.TrimSpace(rbd("device", "map", image))
		defer rbd("device", "unmap", device)
		f, err := os.OpenFile(device, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", device, err)
		}
		block := bytes.Repeat([]byte{fill}, 1<<20)
		for at := range 8 {
			if _, err := f.WriteAt(block, int64(offset+at*(1<<22))); err != nil {
				t.Fatalf("write %s: %v", device, err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close %s: %v", device, err)
		}
	}

	// NAMED FOR THIS INVOCATION, and removed only once this invocation made them, so a
	// second run against the pool can neither collide with this one nor delete its images.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("read a nonce: %v", err)
	}
	name := func(part string) string { return fmt.Sprintf("%s/billet-test-%x-%s", pool, nonce, part) }
	parent, child, copied, compacted := name("parent"), name("child"), name("copied"), name("compacted")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
		defer cancel()

		// Children first, and every image is tried whatever became of another.
		for _, image := range []string{compacted, copied, child, parent} {
			if err := exec.CommandContext(ctx, "rbd", "info", image).Run(); err != nil {
				continue
			}
			for _, args := range [][]string{{"snap", "purge", image}, {"rm", image}} {
				if out, err := exec.CommandContext(ctx, "rbd", args...).CombinedOutput(); err != nil {
					t.Errorf("could not remove this test's image %s (rbd %v): %v: %s", image, args, err, out)
				}
			}
		}
	})

	features := cacheImageFeatures
	rbd("create", parent, "--size", "256M", "--image-feature", features)
	write(parent, 0, 0xA1)
	rbd("snap", "create", parent+"@s")
	rbd("clone", parent+"@s", child, "--image-feature", features)
	write(child, 1<<25, 0xB2)
	rbd("snap", "create", child+"@s")
	want := export(child + "@s")

	rbd("cp", "--no-progress", child+"@s", copied, "--image-feature", features)
	if export(copied) == want {
		t.Errorf("rbd cp of a snapshot of an object-map clone kept its parent's data; " +
			"this Ceph does not need what compactCacheImage says it does")
	}

	c, err := New(config.CephConfig{
		User: config.DefaultCephUser, CachePool: pool, ImagePool: "billet-test-unused",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.compactCacheImage(t.Context(), child+"@s", compacted); err != nil {
		t.Fatalf("compactCacheImage: %v", err)
	}
	if export(compacted) != want {
		t.Error("the compacted image does not hold what its source holds")
	}
	if info := rbd("info", compacted); strings.Contains(info, "parent:") {
		t.Errorf("the compacted image still has a parent:\n%s", info)
	}
}
