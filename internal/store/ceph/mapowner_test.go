package ceph

import (
	"context"
	"slices"
	"testing"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

// CREATE AND CLONE RECORD THE NODE THAT MAPS A VOLUME BEFORE THEY MAP IT, so no
// mapping of this node's exists without its proof; and a client that names no
// node writes nothing.
func TestCreateAndCloneRecordWhichNodeMapsAVolumeBeforeMappingIt(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run), withFilesystemVerifier(func(context.Context, string) (storecontract.Filesystem, error) {
			return storecontract.Filesystem{
				Type: "ext4", UUID: "dcab7af5-4ae7-4cc1-8ddb-1db18956c389", Clean: true,
			}, nil
		}), WithCacheSessions(f.readSessions), WithMapOwner(rigOwner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recordedBeforeMap := func(handle string) {
		t.Helper()

		if got := f.metadata[handle][mapperKey]; got != rigOwner {
			t.Errorf("%s records %q as the node that maps it, want %q", handle, got, rigOwner)
		}

		marked, mapped := -1, -1
		for i, call := range f.calls {
			switch {
			case slices.Contains(call, "image-meta") && slices.Contains(call, "set") &&
				slices.Contains(call, handle) && slices.Contains(call, mapperKey) && marked < 0:
				marked = i
			case slices.Contains(call, "map") && slices.Contains(call, handle) && mapped < 0:
				mapped = i
			}
		}
		if marked < 0 || mapped < 0 || marked > mapped {
			t.Errorf("%s: the owner written at call %d and the map at %d; want the owner first", handle, marked, mapped)
		}
	}

	now := time.Now()

	volume, err := c.Create(t.Context(), "acme/api/npm", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	recordedBeforeMap(volume.Handle)

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

	// A CLONE COPIES ITS PARENT'S METADATA, which names the node that published
	// the generation: the clone writes its own over whatever it inherits.
	if f.metadata[candidate.Handle] == nil {
		f.metadata[candidate.Handle] = map[string]string{}
	}
	f.metadata[candidate.Handle][mapperKey] = "deployment-b/node-1"

	clone, err := c.Clone(t.Context(), "acme/api/npm", "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	recordedBeforeMap(clone.Handle)

	// AND WITHOUT AN OWNER, NOTHING IS WRITTEN.
	plain := newCacheFake()
	bare, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(plain.run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	unowned, err := bare.Create(t.Context(), "acme/api/npm", 1<<30)
	if err != nil {
		t.Fatalf("Create without an owner: %v", err)
	}
	if _, ok := plain.metadata[unowned.Handle][mapperKey]; ok {
		t.Errorf("a client that names no node recorded one on %s", unowned.Handle)
	}
}
