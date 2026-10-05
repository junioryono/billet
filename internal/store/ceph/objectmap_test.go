package ceph

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A CACHE VOLUME AND EVERY CLONE OF ONE CARRY OBJECT-MAP, AND NOTHING THAT CHANGES
// ONE CAN BLOCKLIST THE HOST. Created with `layering` alone, a discarded volume's
// deletion removed every object its size could hold: 237 s for a 100 GiB volume
// with 100 MiB written, against 3.5 s with object-map, and the node's trash purge
// kept both OSDs saturated for hours (2026-10-05). Object-map brings
// exclusive-lock, which the kernel holds while a volume is mapped, so every
// command that changes one must not blocklist the kernel client if it breaks a
// lock an interrupted unmap left; the cache index's advisory lock keeps its
// blocklist, which is its fence.
func TestCacheVolumesCarryObjectMapAndNeverBlocklistTheHost(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()

	volume, err := c.Create(t.Context(), "acme/api/npm", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lease, fence, err := c.acquireWriterAt(t.Context(), "acme/api/npm", "lease-17", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := c.publishCASAt(t.Context(), "acme/api/npm", "", candidate, lease, fence, now); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	clone, err := c.Clone(t.Context(), "acme/api/npm", "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := c.Discard(t.Context(), clone); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	featured := func(call []string) bool {
		i := slices.Index(call, "--image-feature")

		return i >= 0 && i+1 < len(call) && call[i+1] == cacheImageFeatures
	}
	keepsClient := func(call []string) bool {
		i := slices.Index(call, "--rbd_blocklist_on_break_lock")

		return i >= 0 && i+1 < len(call) && call[i+1] == "false"
	}
	has := func(call []string, verbs ...string) bool {
		at := slices.Index(call, verbs[0])

		return at >= 0 && at+len(verbs) <= len(call) && slices.Equal(call[at:at+len(verbs)], verbs)
	}

	var created, cloned, changed int
	for _, call := range f.calls {
		switch {
		// THE PUBLISH LOCK IS `layering` ALONE ON PURPOSE: Ceph documents
		// exclusive-lock as incompatible with the advisory locks taken on it.
		case has(call, "create") && slices.ContainsFunc(call, func(arg string) bool {
			return strings.HasSuffix(arg, "/"+LockImageName)
		}):
		case has(call, "create") && !has(call, "snap", "create"):
			created++
			if !featured(call) {
				t.Errorf("a cache volume was created without object-map: %v", call)
			}
		case has(call, "clone"), has(call, "cp"):
			cloned++
			if !featured(call) {
				t.Errorf("a cache volume was cloned without object-map: %v", call)
			}
		case has(call, "snap", "create"), has(call, "snap", "rm"), has(call, "snap", "purge"),
			has(call, "trash", "mv"), has(call, "image-meta", "set"), has(call, "image-meta", "remove"),
			has(call, "rm") && !has(call, "lock", "rm") && !has(call, "trash", "rm"):
			changed++
			if !keepsClient(call) {
				t.Errorf("a command that changes a cache image could blocklist the host: %v", call)
			}
		case has(call, "lock", "rm"):
			if keepsClient(call) {
				t.Errorf("the cache index's advisory lock lost its blocklist fence: %v", call)
			}
		}
	}

	// THE FLOW REACHED EVERY KIND, or the checks above judged nothing.
	if created == 0 || cloned == 0 || changed == 0 {
		t.Fatalf("the flow ran %d creates, %d clones and %d changes; it must exercise each",
			created, cloned, changed)
	}
}
