package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

type cacheFake struct {
	calls      [][]string
	images     map[string]bool
	snapshots  map[string]bool
	metadata   map[string]map[string]string
	mappings   map[string]string
	lockCookie string
	locker     string
	nextDevice int
	failMeta   string
}

func newCacheFake() *cacheFake {
	return &cacheFake{
		images:    map[string]bool{},
		snapshots: map[string]bool{},
		metadata:  map[string]map[string]string{},
		mappings:  map[string]string{},
		locker:    "client.1234",
	}
}

func (f *cacheFake) run(ctx context.Context, _ string, args []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.calls = append(f.calls, slices.Clone(args))

	if i := slices.Index(args, "lock"); i >= 0 {
		return f.lock(args[i+1:])
	}

	if i := slices.Index(args, "image-meta"); i >= 0 {
		return f.imageMeta(args[i+1:])
	}

	if i := slices.Index(args, "device"); i >= 0 {
		return f.device(args[i+1:])
	}

	if i := slices.Index(args, "snap"); i >= 0 {
		return f.snap(args[i+1:])
	}

	for _, verb := range []string{"create", "clone", "info", "rm", "ls"} {
		if i := slices.Index(args, verb); i >= 0 {
			return f.image(verb, args[i+1:], args)
		}
	}

	return nil, fmt.Errorf("fake does not understand %v", args)
}

func (f *cacheFake) lock(args []string) ([]byte, error) {
	switch args[0] {
	case "add":
		if f.lockCookie != "" {
			return nil, errors.New("exit status 16")
		}

		f.lockCookie = args[2]

		return nil, nil
	case "ls":
		if f.lockCookie == "" {
			return []byte("[]"), nil
		}

		return json.Marshal([]lockEntry{{ID: f.lockCookie, Locker: f.locker}})
	case "rm":
		if args[2] != f.lockCookie || args[3] != f.locker {
			return nil, errors.New("wrong lock owner")
		}

		f.lockCookie = ""

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown lock verb %q", args[0])
	}
}

func (f *cacheFake) imageMeta(args []string) ([]byte, error) {
	verb, image := args[0], args[1]
	if f.metadata[image] == nil {
		f.metadata[image] = map[string]string{}
	}

	switch verb {
	case "set":
		if args[2] == f.failMeta {
			return nil, errors.New("injected metadata failure")
		}

		f.metadata[image][args[2]] = args[3]

		return nil, nil
	case "get":
		value, ok := f.metadata[image][args[2]]
		if !ok {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		return []byte(value + "\n"), nil
	case "remove":
		delete(f.metadata[image], args[2])

		return nil, nil
	case "list":
		var lines []string
		for key, value := range f.metadata[image] {
			lines = append(lines, key+" "+value)
		}

		slices.Sort(lines)

		return []byte(strings.Join(lines, "\n")), nil
	default:
		return nil, fmt.Errorf("unknown image-meta verb %q", verb)
	}
}

func (f *cacheFake) device(args []string) ([]byte, error) {
	switch args[0] {
	case "map":
		image := args[1]
		if !f.images[image] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		device := fmt.Sprintf("/dev/rbd%d", f.nextDevice)
		f.nextDevice++
		f.mappings[device] = image

		return []byte(device + "\n"), nil
	case "list":
		type mapping struct {
			Pool   string `json:"pool"`
			Name   string `json:"name"`
			Device string `json:"device"`
		}

		var out []mapping
		for device, image := range f.mappings {
			pool, name, _ := strings.Cut(image, "/")
			out = append(out, mapping{Pool: pool, Name: name, Device: device})
		}

		if out == nil {
			out = []mapping{}
		}

		return json.Marshal(out)
	case "unmap":
		delete(f.mappings, args[1])

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown device verb %q", args[0])
	}
}

func (f *cacheFake) snap(args []string) ([]byte, error) {
	switch args[0] {
	case "create":
		f.snapshots[args[1]] = true

		return nil, nil
	case "rm":
		delete(f.snapshots, args[1])

		return nil, nil
	case "purge":
		prefix := args[1] + "@"
		for snapshot := range f.snapshots {
			if strings.HasPrefix(snapshot, prefix) {
				delete(f.snapshots, snapshot)
			}
		}

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown snap verb %q", args[0])
	}
}

func (f *cacheFake) image(verb string, tail, all []string) ([]byte, error) {
	switch verb {
	case "create":
		f.images[tail[0]] = true

		return nil, nil
	case "clone":
		if !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		f.images[tail[1]] = true

		return nil, nil
	case "info":
		if !f.images[tail[0]] && !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		return []byte(`{"size":1073741824}`), nil
	case "rm":
		delete(f.images, tail[0])

		return nil, nil
	case "ls":
		pool := valueAfter(all, "-p")
		var names []string
		for image := range f.images {
			imagePool, name, _ := strings.Cut(image, "/")
			if imagePool == pool {
				names = append(names, name)
			}
		}

		slices.Sort(names)

		return json.Marshal(names)
	default:
		return nil, fmt.Errorf("unknown image verb %q", verb)
	}
}

func valueAfter(args []string, key string) string {
	for i := range len(args) - 1 {
		if args[i] == key {
			return args[i+1]
		}
	}

	return ""
}

func cacheClient(t *testing.T, f *cacheFake) *Client {
	t.Helper()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run), withFilesystemVerifier(func(context.Context, string) (storecontract.Filesystem, error) {
			return storecontract.Filesystem{
				Type: "ext4", UUID: "dcab7af5-4ae7-4cc1-8ddb-1db18956c389", Clean: true,
			}, nil
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

func TestACachePointerPublishesOnlyACompleteImmutableCandidate(t *testing.T) {
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
	if err := c.publishCASAt(t.Context(), "acme/api/npm", "", candidate, lease, fence, now); err != nil {
		t.Fatalf("an ambiguously acknowledged PublishCAS could not be retried: %v", err)
	}

	clone, err := c.Clone(t.Context(), "acme/api/npm", "")
	if err != nil {
		t.Fatalf("Clone current: %v", err)
	}

	if clone.Generation != candidate.Generation {
		t.Errorf("clone generation = %q, want %q", clone.Generation, candidate.Generation)
	}

	if clone.Device == "" || clone.Lease.ID == "" {
		t.Errorf("clone is not a mapped, eviction-protected volume: %+v", clone)
	}

	if f.images[volume.Handle] {
		t.Error("the writable source clone survived after its immutable candidate was made")
	}
}

func TestPublishCASRefusesAStaleExpectedGenerationAndFence(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()
	key := "acme/api/go"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	lease, fence, err := c.acquireWriterAt(t.Context(), key, "lease-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}

	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := c.publishCASAt(t.Context(), key, "generation-that-was-never-current",
		candidate, lease, fence, now); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("stale expected generation returned %v, want ErrConflict", err)
	}

	if err := c.publishCASAt(t.Context(), key, "", candidate, lease, fence+1, now); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("unissued fence returned %v, want ErrConflict", err)
	}

	if _, err := c.Clone(t.Context(), key, ""); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("a refused publication changed the pointer: %v", err)
	}
}

func TestAnUntrustedCallerCanDiscardWithoutPublishing(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)

	volume, err := c.Create(t.Context(), "acme/api/pr", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, err := c.Clone(t.Context(), volume.Key, ""); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("discarding a PR write published something: %v", err)
	}
}

func TestAPointerWriteFailureLeavesThePreviousPointerUntouched(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()
	key := "acme/api/cargo"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	lease, fence, err := c.acquireWriterAt(t.Context(), key, "lease-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}

	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	f.failMeta = pointerKey(key)
	if err := c.publishCASAt(t.Context(), key, "", candidate, lease, fence, now); err == nil {
		t.Fatal("PublishCAS succeeded although the pointer write failed")
	}

	if _, err := c.Clone(t.Context(), key, ""); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("the failed pointer became visible: %v", err)
	}

	if !f.images[candidate.Handle] {
		t.Error("the failed publication did not leave an orphan candidate for GC")
	}
}

func TestEvictionPreservesAnOldGenerationWhileACloneLeaseIsLive(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	handle := fmt.Sprintf("billet-cache/cache-g-%d-abcdef", now.Add(-8*24*time.Hour).Unix())
	f.images[handle] = true
	f.metadata[handle] = map[string]string{
		cacheMetaPrefix + "used_at": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	active := cacheActive{
		Key: "acme/api/npm", Handle: handle, Generation: "g1", Expires: now.Add(time.Hour),
	}
	encoded, err := json.Marshal(active)
	if err != nil {
		t.Fatalf("marshal active lease: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{activeKey("lease-1"): string(encoded)}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict with a live clone: %v", err)
	}

	if !f.images[handle] {
		t.Fatal("eviction removed a generation a live clone lease still names")
	}

	active.Expires = now.Add(-time.Hour)
	encoded, err = json.Marshal(active)
	if err != nil {
		t.Fatalf("marshal expired lease: %v", err)
	}
	f.metadata[c.cacheIndex()][activeKey("lease-1")] = string(encoded)

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict after the clone expired: %v", err)
	}

	if f.images[handle] {
		t.Fatal("eviction kept an inactive generation beyond its retention window")
	}
}

func TestEvictionExpiresAnInactiveCurrentPointer(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	handle := fmt.Sprintf("billet-cache/cache-g-%d-abcdef", now.Add(-8*24*time.Hour).Unix())
	f.images[handle] = true
	pointer := cachePointer{
		Generation: "g1", Handle: handle, UsedAt: now.Add(-8 * 24 * time.Hour),
		RetentionHours: 7 * 24,
	}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{
		pointerKey("acme/api/npm"):          string(encoded),
		generationKey("acme/api/npm", "g1"): string(encoded),
	}
	f.metadata[handle] = map[string]string{
		cacheMetaPrefix + "used_at": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano),
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if f.images[handle] {
		t.Fatal("eviction kept an inactive current generation forever")
	}
	if _, present := f.metadata[c.cacheIndex()][pointerKey("acme/api/npm")]; present {
		t.Fatal("eviction left a pointer to the generation it removed")
	}
	if _, present := f.metadata[c.cacheIndex()][generationKey("acme/api/npm", "g1")]; present {
		t.Fatal("eviction left generation metadata for the image it removed")
	}
}

func TestCloningRefreshesTheCurrentPointersLastUse(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	handle := fmt.Sprintf("billet-cache/cache-g-%d-abcdef", old.Unix())
	f.images[handle] = true
	f.snapshots[handle+"@g1"] = true
	pointer := cachePointer{Generation: "g1", Handle: handle, UsedAt: old, RetentionHours: 7 * 24}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey("acme/api/npm"): string(encoded)}
	f.metadata[handle] = map[string]string{cacheMetaPrefix + "used_at": old.Format(time.RFC3339Nano)}

	volume, err := c.Clone(t.Context(), "acme/api/npm", "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	var refreshed cachePointer
	if err := json.Unmarshal([]byte(f.metadata[c.cacheIndex()][pointerKey("acme/api/npm")]),
		&refreshed); err != nil {
		t.Fatalf("decode refreshed pointer: %v", err)
	}
	if !refreshed.UsedAt.After(old) {
		t.Errorf("pointer use stayed at %s", refreshed.UsedAt)
	}
}
