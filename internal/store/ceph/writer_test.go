package ceph

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

func recordedWriter(t *testing.T, f *cacheFake, c *Client, key string) (cacheWriter, bool) {
	t.Helper()

	value, ok := f.metadata[c.cacheIndex()][writerKey(key)]
	if !ok {
		return cacheWriter{}, false
	}
	var writer cacheWriter
	if err := json.Unmarshal([]byte(value), &writer); err != nil {
		t.Fatalf("the recorded writer is not json: %v", err)
	}

	return writer, true
}

// A RELEASED WRITER FREES THE KEY WITHOUT MOVING THE FENCE, under the index
// lock — a lock-free removal could interleave with an acquisition's read.
func TestAReleasedWriterLetsTheNextWriterInWithoutWaitingOutTheLease(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	const key = "acme/api/npm"
	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, firstFence, err := c.acquireWriterAt(t.Context(), key, "home-a", 15*time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	if _, _, err := c.acquireWriterAt(t.Context(), key, "home-b", 15*time.Minute, now); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("a second writer got in while the first held the key: %v", err)
	}

	if err := c.releaseWriterAt(t.Context(), first, firstFence, now); err != nil {
		t.Fatalf("ReleaseWriter: %v", err)
	}
	if locked, ran := f.removeLocked[writerKey(key)]; !ran || !locked {
		t.Fatalf("the writer record was removed ran=%t under the index lock=%t; want removal under the lock",
			ran, locked)
	}
	if writer, ok := recordedWriter(t, f, c, key); ok {
		t.Fatalf("the writer record survived the release: %+v", writer)
	}
	if got := f.metadata[c.cacheIndex()][fenceKey(key)]; got != strconv.FormatUint(uint64(firstFence), 10) {
		t.Fatalf("fence after release = %q, want %d untouched", got, firstFence)
	}

	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := c.publishCASAt(t.Context(), key, "", candidate, first, firstFence, now); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("a released lease published: %v", err)
	}
	second, secondFence, err := c.acquireWriterAt(t.Context(), key, "home-b", 15*time.Minute, now)
	if err != nil {
		t.Fatalf("the next writer still had to wait out a released lease: %v", err)
	}
	if secondFence != firstFence+1 {
		t.Fatalf("fence after release = %d, want %d: the fence must move on, never back",
			secondFence, firstFence+1)
	}
	if err := c.publishCASAt(t.Context(), key, "", candidate, second, secondFence, now); err != nil {
		t.Fatalf("PublishCAS with the writer acquired after the release: %v", err)
	}
}

func TestReleasingAWriterLeavesANewerWriterAndTheFenceStanding(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	const key = "acme/api/npm"
	first, firstFence, err := c.acquireWriterAt(t.Context(), key, "job-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	later := now.Add(2 * time.Minute)
	second, secondFence, err := c.acquireWriterAt(t.Context(), key, "job-2", time.Minute, later)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}

	wrongExpiry := second
	wrongExpiry.Expires = second.Expires.Add(time.Second)
	for name, release := range map[string]func() error{
		"an expired predecessor": func() error {
			return c.releaseWriterAt(t.Context(), first, firstFence, later)
		},
		"the right identity under the wrong fence": func() error {
			return c.releaseWriterAt(t.Context(), second, secondFence+1, later)
		},
		"the right identity under the wrong expiry": func() error {
			return c.releaseWriterAt(t.Context(), wrongExpiry, secondFence, later)
		},
	} {
		if err := release(); err != nil {
			t.Fatalf("releasing %s returned %v, want a silent no-op", name, err)
		}
		writer, ok := recordedWriter(t, f, c, key)
		if !ok || writer.Lease.ID != second.ID {
			t.Fatalf("releasing %s cleared the live writer: %+v", name, writer)
		}
		if got := f.metadata[c.cacheIndex()][fenceKey(key)]; got != strconv.FormatUint(uint64(secondFence), 10) {
			t.Fatalf("releasing %s moved the fence to %q", name, got)
		}
	}
}

// cacheIndexState is the cache's own metadata on the index image: pointer,
// generation, active-clone and writer records, with the lock heartbeat left out.
func cacheIndexState(f *cacheFake, c *Client) map[string]string {
	state := map[string]string{}
	for key, value := range f.metadata[c.cacheIndex()] {
		if strings.HasPrefix(key, cacheMetaPrefix) {
			state[key] = value
		}
	}

	return state
}

// A release touches the writer record and nothing else: not the pointer, not a
// generation record, not an active-clone lease, not the fence.
func TestAReleaseTouchesNothingButTheWriterRecord(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	const key = "acme/api/npm"
	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, firstFence, err := c.acquireWriterAt(t.Context(), key, "job-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := c.publishCASAt(t.Context(), key, "", candidate, first, firstFence, now); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	if _, err := c.Clone(t.Context(), key, ""); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	later := now.Add(2 * time.Minute)
	next, nextFence, err := c.acquireWriterAt(t.Context(), key, "job-2", time.Minute, later)
	if err != nil {
		t.Fatalf("AcquireWriter next: %v", err)
	}

	before := cacheIndexState(f, c)
	if _, ok := before[pointerKey(key)]; !ok {
		t.Fatal("the fixture has no pointer to protect")
	}
	if _, ok := before[generationKey(key, candidate.Generation)]; !ok {
		t.Fatal("the fixture has no generation record to protect")
	}
	if !slices.ContainsFunc(slices.Collect(maps.Keys(before)), func(k string) bool {
		return strings.HasPrefix(k, cacheMetaPrefix+"active.")
	}) {
		t.Fatal("the fixture has no active-clone lease to protect")
	}
	if err := c.releaseWriterAt(t.Context(), next, nextFence, later); err != nil {
		t.Fatalf("ReleaseWriter: %v", err)
	}
	delete(before, writerKey(key))
	if after := cacheIndexState(f, c); !maps.Equal(before, after) {
		t.Fatalf("a release changed more than the writer record:\nbefore minus writer: %v\nafter: %v",
			before, after)
	}
}

func TestAReleaseNeedsTheLeaseThatWasIssued(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	const key = "acme/api/npm"
	lease, fence, err := c.acquireWriterAt(t.Context(), key, "job-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if err := c.releaseWriterAt(t.Context(), storecontract.WriterLease{Key: key, Expires: lease.Expires}, fence, now); err == nil {
		t.Fatal("a release without a lease identity was accepted")
	}
	if err := c.releaseWriterAt(t.Context(), lease, 0, now); err == nil {
		t.Fatal("a release under fence zero was accepted")
	}
	if writer, ok := recordedWriter(t, f, c, key); !ok || writer.Lease.ID != lease.ID {
		t.Fatalf("a refused release cleared the writer: %+v", writer)
	}
}

func TestAHeldWriterRefusalNamesTheHolderAndItsExpiry(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	const key = "acme/api/npm"
	first, _, err := c.acquireWriterAt(t.Context(), key, "home-a", 15*time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	_, _, err = c.acquireWriterAt(t.Context(), key, "home-b", 15*time.Minute, now)
	held, ok := errors.AsType[*storecontract.WriterHeldError](err)
	if !ok {
		t.Fatalf("a live writer was refused with %v, want a WriterHeldError", err)
	}
	if held.Key != key || held.Holder != "home-a" || !held.Expires.Equal(first.Expires) {
		t.Fatalf("held = %+v, want the first writer's holder and expiry %s", held, first.Expires)
	}
	if !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("a held writer is no longer a conflict: %v", err)
	}
}
