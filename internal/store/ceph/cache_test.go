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
	parents    map[string]string
	depths     map[string]int
	trash      map[string]string
	metadata   map[string]map[string]string
	mappings   map[string]string
	lockCookie string
	locker     string
	nextDevice int
	failMeta   string
	failRemove bool
}

type cacheExitError struct {
	code    int
	message string
}

func (e cacheExitError) Error() string { return e.message }
func (e cacheExitError) ExitCode() int { return e.code }

func newCacheFake() *cacheFake {
	return &cacheFake{
		images:    map[string]bool{},
		snapshots: map[string]bool{},
		parents:   map[string]string{},
		depths:    map[string]int{},
		trash:     map[string]string{},
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

	if i := slices.Index(args, "trash"); i >= 0 {
		return f.trashCommand(args[i+1:])
	}

	for _, verb := range []string{"create", "clone", "cp", "info", "rm", "ls"} {
		if i := slices.Index(args, verb); i >= 0 {
			return f.image(verb, args[i+1:], args)
		}
	}

	return nil, fmt.Errorf("fake does not understand %v", args)
}

func (f *cacheFake) trashCommand(args []string) ([]byte, error) {
	switch args[0] {
	case "mv":
		image := args[1]
		if !f.images[image] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		_, name, _ := strings.Cut(image, "/")
		id := "id-" + name
		f.trash[id] = name
		delete(f.images, image)

		return nil, nil
	case "list":
		type entry struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}

		entries := make([]entry, 0, len(f.trash))
		for id, name := range f.trash {
			entries = append(entries, entry{ID: id, Name: name})
		}
		slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.ID, b.ID) })

		return json.Marshal(entries)
	case "rm":
		_, id, _ := strings.Cut(args[1], "/")
		name, ok := f.trash[id]
		if !ok {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		image := "billet-cache/" + name
		for _, parent := range f.parents {
			if strings.HasPrefix(parent, image+"@") {
				return nil, fmt.Errorf("exit status 39: %w", cacheExitError{
					code: 39,
					message: "rbd: image has snapshots - these must be deleted with 'rbd snap purge' " +
						"before the image can be removed",
				})
			}
		}

		delete(f.trash, id)
		delete(f.parents, image)
		delete(f.depths, image)

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown trash verb %q", args[0])
	}
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
		f.depths[tail[0]] = 0

		return nil, nil
	case "clone":
		if !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		parent := strings.SplitN(tail[0], "@", 2)[0]
		depth := f.depths[parent] + 1
		if depth > maxCacheCloneDepth {
			return nil, fmt.Errorf("fake: clone depth %d exceeds %d", depth, maxCacheCloneDepth)
		}

		f.images[tail[1]] = true
		f.parents[tail[1]] = tail[0]
		f.depths[tail[1]] = depth

		return nil, nil
	case "cp":
		if !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		f.images[tail[1]] = true
		f.depths[tail[1]] = 0

		return nil, nil
	case "info":
		if !f.images[tail[0]] && !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		return []byte(`{"size":1073741824}`), nil
	case "rm":
		if f.failRemove {
			return nil, errors.New("injected remove failure")
		}
		for _, parent := range f.parents {
			if strings.HasPrefix(parent, tail[0]+"@") {
				return nil, fmt.Errorf("exit status 39: %w", cacheExitError{
					code:    39,
					message: "rbd: image has snapshots with linked clones",
				})
			}
		}

		delete(f.images, tail[0])
		delete(f.parents, tail[0])
		delete(f.depths, tail[0])

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

func TestCacheMatchUsesExactThenNewestWithinTheFirstRestorePrefix(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	pointers := []cachePointer{
		{Key: "scope/npm-linux-old", Generation: "g-old", PublishedAt: now.Add(-time.Hour)},
		{Key: "scope/npm-linux-new", Generation: "g-new", PublishedAt: now},
		{Key: "scope/npm-other", Generation: "g-other", PublishedAt: now.Add(time.Hour)},
	}
	f.metadata[c.cacheIndex()] = map[string]string{}
	for _, pointer := range pointers {
		encoded, err := json.Marshal(pointer)
		if err != nil {
			t.Fatalf("marshal pointer: %v", err)
		}
		f.metadata[c.cacheIndex()][pointerKey(pointer.Key)] = string(encoded)
	}

	key, generation, err := c.Match(t.Context(), "scope/missing",
		[]string{"scope/npm-linux-", "scope/npm-"})
	if err != nil || key != "scope/npm-linux-new" || generation != "g-new" {
		t.Fatalf("restore match = %q %q, %v", key, generation, err)
	}
	key, generation, err = c.Match(t.Context(), "scope/npm-linux-old", []string{"scope/npm-"})
	if err != nil || key != "scope/npm-linux-old" || generation != "g-old" {
		t.Fatalf("exact match = %q %q, %v", key, generation, err)
	}
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

func TestDiscardResolvesTheCurrentMappingInsteadOfTrustingAStoredDevice(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	const (
		handle        = "billet-cache/cache-v-lease-17"
		currentDevice = "/dev/rbd8"
		reusedDevice  = "/dev/rbd7"
	)
	f.images[handle] = true
	f.mappings[currentDevice] = handle
	f.mappings[reusedDevice] = "billet-cache/cache-v-another-job"

	volume := storecontract.Volume{Handle: handle, Device: reusedDevice}
	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, ok := f.mappings[currentDevice]; ok {
		t.Fatal("Discard left the volume's current mapping in place")
	}
	if got := f.mappings[reusedDevice]; got != "billet-cache/cache-v-another-job" {
		t.Fatalf("Discard touched a reused device: mapping = %q", got)
	}
	if f.images[handle] {
		t.Fatal("Discard left the cache image in place")
	}

	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("idempotent Discard: %v", err)
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

func TestAGenerationRecordFailureLeavesThePreviousPointerUntouched(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()
	key := "acme/api/cargo"

	firstVolume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	firstLease, firstFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-1", time.Minute, now,
	)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	first, err := c.snapshotAt(t.Context(), firstVolume, now)
	if err != nil {
		t.Fatalf("Snapshot first: %v", err)
	}
	if err := c.publishCASAt(
		t.Context(), key, "", first, firstLease, firstFence, now,
	); err != nil {
		t.Fatalf("PublishCAS first: %v", err)
	}

	secondVolume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone first: %v", err)
	}
	secondNow := now.Add(time.Second)
	secondLease, secondFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-2", time.Minute, secondNow,
	)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}
	second, err := c.snapshotAt(t.Context(), secondVolume, secondNow)
	if err != nil {
		t.Fatalf("Snapshot second: %v", err)
	}

	f.failMeta = generationKey(key, second.Generation)
	if err := c.publishCASAt(
		t.Context(), key, first.Generation, second, secondLease, secondFence, secondNow,
	); err == nil {
		t.Fatal("PublishCAS succeeded although the generation record write failed")
	}

	current, err := c.Current(t.Context(), key)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != first.Generation {
		t.Fatalf("current generation = %q, want previous %q", current, first.Generation)
	}
	if !f.images[second.Handle] {
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

func TestALiveClonePreservesItsOldPointerThroughPublication(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	key := "acme/api/long-publication"

	firstVolume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	firstLease, firstFence, err := c.acquireWriterAt(t.Context(), key, "first", time.Hour, old)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	first, err := c.snapshotAt(t.Context(), firstVolume, old)
	if err != nil {
		t.Fatalf("Snapshot first: %v", err)
	}
	if err := c.publishCASAt(t.Context(), key, "", first, firstLease, firstFence, old); err != nil {
		t.Fatalf("PublishCAS first: %v", err)
	}

	writable, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone first: %v", err)
	}
	var pointer cachePointer
	if err := json.Unmarshal([]byte(f.metadata[c.cacheIndex()][pointerKey(key)]), &pointer); err != nil {
		t.Fatalf("decode pointer: %v", err)
	}
	pointer.UsedAt = old
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("encode old pointer: %v", err)
	}
	f.metadata[c.cacheIndex()][pointerKey(key)] = string(encoded)

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict while clone is live: %v", err)
	}
	if current, err := c.Current(t.Context(), key); err != nil || current != first.Generation {
		t.Fatalf("Current after eviction = %q, %v; want %q", current, err, first.Generation)
	}

	secondLease, secondFence, err := c.acquireWriterAt(t.Context(), key, "second", time.Hour, now)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}
	second, err := c.snapshotAt(t.Context(), writable, now)
	if err != nil {
		t.Fatalf("Snapshot second: %v", err)
	}
	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict between snapshot and publication: %v", err)
	}
	if current, err := c.Current(t.Context(), key); err != nil || current != first.Generation {
		t.Fatalf("Current during publication handoff = %q, %v; want %q", current, err, first.Generation)
	}
	if err := c.publishCASAt(
		t.Context(), key, first.Generation, second, secondLease, secondFence, now,
	); err != nil {
		t.Fatalf("PublishCAS second after eviction: %v", err)
	}
}

func TestEvictionReclaimsTheRetiredWriterAfterItsGeneration(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	key := "acme/api/npm"

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
	if len(f.trash) != 1 {
		t.Fatalf("retired writer count = %d, want 1", len(f.trash))
	}
	if err := c.publishCASAt(t.Context(), key, "", candidate, lease, fence, now); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict with a live candidate: %v", err)
	}
	if !f.images[candidate.Handle] || len(f.trash) != 1 {
		t.Fatal("eviction removed a current generation or its retired copy-on-write parent")
	}

	old := now.Add(-8 * 24 * time.Hour)
	var pointer cachePointer
	if err := json.Unmarshal([]byte(f.metadata[c.cacheIndex()][pointerKey(key)]), &pointer); err != nil {
		t.Fatalf("decode current pointer: %v", err)
	}
	pointer.UsedAt = old
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("encode expired pointer: %v", err)
	}
	f.metadata[c.cacheIndex()][pointerKey(key)] = string(encoded)
	f.metadata[c.cacheIndex()][generationKey(key, candidate.Generation)] = string(encoded)
	f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = old.Format(time.RFC3339Nano)

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if f.images[candidate.Handle] {
		t.Fatal("eviction kept the expired generation")
	}
	if len(f.trash) != 0 {
		t.Fatalf("eviction left %d retired writers after their children were removed", len(f.trash))
	}
}

func TestEvictionEventuallyReclaimsAMultiGenerationCloneChain(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	key := "acme/api/npm-chain"

	firstVolume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	firstLease, firstFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-1", time.Minute, now,
	)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	first, err := c.snapshotAt(t.Context(), firstVolume, now)
	if err != nil {
		t.Fatalf("Snapshot first: %v", err)
	}
	if err := c.publishCASAt(
		t.Context(), key, "", first, firstLease, firstFence, now,
	); err != nil {
		t.Fatalf("PublishCAS first: %v", err)
	}

	secondVolume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone first: %v", err)
	}
	secondNow := now.Add(time.Second)
	secondLease, secondFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-2", time.Minute, secondNow,
	)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}
	second, err := c.snapshotAt(t.Context(), secondVolume, secondNow)
	if err != nil {
		t.Fatalf("Snapshot second: %v", err)
	}
	if err := c.publishCASAt(
		t.Context(), key, first.Generation, second, secondLease, secondFence, secondNow,
	); err != nil {
		t.Fatalf("PublishCAS second: %v", err)
	}

	for _, candidate := range []storecontract.Candidate{first, second} {
		f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = old.Format(time.RFC3339Nano)
	}
	for metadataKey, value := range f.metadata[c.cacheIndex()] {
		if !strings.HasPrefix(metadataKey, cacheMetaPrefix+"pointer.") &&
			!strings.HasPrefix(metadataKey, cacheMetaPrefix+"generation.") {
			continue
		}

		var pointer cachePointer
		if err := json.Unmarshal([]byte(value), &pointer); err != nil {
			t.Fatalf("decode %s: %v", metadataKey, err)
		}
		pointer.UsedAt = old
		encoded, err := json.Marshal(pointer)
		if err != nil {
			t.Fatalf("encode %s: %v", metadataKey, err)
		}
		f.metadata[c.cacheIndex()][metadataKey] = string(encoded)
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	for image := range f.images {
		if strings.Contains(image, "/cache-g-") || strings.Contains(image, "/cache-v-") {
			t.Fatalf("eviction left a cache image after the clone chain expired: %s", image)
		}
	}
	if len(f.trash) != 0 {
		t.Fatalf("eviction left retired writers after the clone chain expired: %v", f.trash)
	}
	if _, ok := f.metadata[c.cacheIndex()][generationKey(key, first.Generation)]; ok {
		t.Fatal("eviction left metadata for the reclaimed parent generation")
	}
}

func TestLineageCompactionLetsEvictionReclaimHistoryBehindAnActiveCache(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	key := "acme/api/active-chain"
	var previous string
	var generations []storecontract.Candidate

	for i := range 5 {
		var volume storecontract.Volume
		var err error
		if i == 0 {
			volume, err = c.Create(t.Context(), key, 1<<30)
		} else {
			volume, err = c.Clone(t.Context(), key, "")
		}
		if err != nil {
			t.Fatalf("generation %d volume: %v", i+1, err)
		}

		commitTime := now.Add(time.Duration(i) * time.Second)
		lease, fence, err := c.acquireWriterAt(
			t.Context(), key, fmt.Sprintf("lease-%d", i+1), time.Minute, commitTime,
		)
		if err != nil {
			t.Fatalf("generation %d AcquireWriter: %v", i+1, err)
		}
		candidate, err := c.snapshotAt(t.Context(), volume, commitTime)
		if err != nil {
			t.Fatalf("generation %d Snapshot: %v", i+1, err)
		}
		if err := c.publishCASAt(
			t.Context(), key, previous, candidate, lease, fence, commitTime,
		); err != nil {
			t.Fatalf("generation %d PublishCAS: %v", i+1, err)
		}
		generations = append(generations, candidate)
		previous = candidate.Generation
	}

	current := generations[len(generations)-1]
	if parent := f.parents[current.Handle]; parent != "" {
		t.Fatalf("the compacted current generation still has parent %q", parent)
	}
	if got := f.metadata[current.Handle][cacheMetaPrefix+"lineage"]; got != "0" {
		t.Fatalf("compacted lineage depth = %q, want 0", got)
	}
	cpCalls := 0
	for _, call := range f.calls {
		if slices.Contains(call, "cp") {
			cpCalls++
		}
	}
	if cpCalls != 1 {
		t.Fatalf("independent copy count = %d, want one bounded compaction", cpCalls)
	}

	for _, candidate := range generations[:len(generations)-1] {
		f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = old.Format(time.RFC3339Nano)
	}
	for metadataKey, value := range f.metadata[c.cacheIndex()] {
		if !strings.HasPrefix(metadataKey, cacheMetaPrefix+"generation.") {
			continue
		}

		var pointer cachePointer
		if err := json.Unmarshal([]byte(value), &pointer); err != nil {
			t.Fatalf("decode %s: %v", metadataKey, err)
		}
		if pointer.Generation == current.Generation {
			continue
		}
		pointer.UsedAt = old
		encoded, err := json.Marshal(pointer)
		if err != nil {
			t.Fatalf("encode %s: %v", metadataKey, err)
		}
		f.metadata[c.cacheIndex()][metadataKey] = string(encoded)
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if !f.images[current.Handle] {
		t.Fatal("eviction removed the active current generation")
	}
	for _, candidate := range generations[:len(generations)-1] {
		if f.images[candidate.Handle] {
			t.Fatalf("eviction kept expired ancestor %s behind an independent current generation",
				candidate.Handle)
		}
	}
	if len(f.trash) != 0 {
		t.Fatalf("eviction left retired writers behind the active cache: %v", f.trash)
	}
}

func TestCloningALegacyGenerationDoesNotCrossTheDepthLimit(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	key := "acme/api/legacy-depth"
	source := "billet-cache/cache-g-1-legacy"
	pointer := cachePointer{
		Generation: "g1", Handle: source, UsedAt: time.Now().UTC(), RetentionHours: 7 * 24,
	}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.images[source] = true
	f.snapshots[source+"@g1"] = true
	f.depths[source] = maxCacheCloneDepth
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey(key): string(encoded)}

	volume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone legacy generation: %v", err)
	}
	if parent := f.parents[volume.Handle]; parent != "" {
		t.Fatalf("legacy writable clone still has parent %q", parent)
	}
	if got := f.depths[volume.Handle]; got != 0 {
		t.Fatalf("legacy writable clone depth = %d, want independent depth 0", got)
	}
	if got := f.metadata[volume.Handle][cacheMetaPrefix+"lineage"]; got != "0" {
		t.Fatalf("legacy writable lineage metadata = %q, want 0", got)
	}
	cpCalls := 0
	for _, call := range f.calls {
		if slices.Contains(call, "cp") {
			cpCalls++
		}
	}
	if cpCalls != 1 {
		t.Fatalf("legacy independent copy count = %d, want 1", cpCalls)
	}
}

func TestCanceledCloneCleanupRemovesThePartialImage(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	handle := "billet-cache/cache-v-1-partial"
	leaseID := "cache-a-1-active"
	f.images[handle] = true
	f.mappings["/dev/rbd9"] = handle
	f.metadata[c.cacheIndex()] = map[string]string{activeKey(leaseID): `{"key":"legacy"}`}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	primary := errors.New("copy canceled")

	err := c.cleanupCloneFailure(ctx, leaseID, handle, primary)
	if !errors.Is(err, primary) {
		t.Fatalf("cleanup error = %v, want the primary failure", err)
	}
	if f.images[handle] {
		t.Fatal("canceled clone cleanup left the partial image")
	}
	if _, ok := f.mappings["/dev/rbd9"]; ok {
		t.Fatal("canceled clone cleanup left the partial image mapped")
	}
	if _, ok := f.metadata[c.cacheIndex()][activeKey(leaseID)]; ok {
		t.Fatal("canceled clone cleanup left the active-clone record")
	}
}

func TestSnapshotRefreshesALongLivedWritableCloneBeforeUnmapping(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	now := time.Now().UTC()
	handle := fmt.Sprintf("billet-cache/cache-v-%d-long-job", old.Unix())
	f.images[handle] = true
	f.depths[handle] = 0
	f.mappings["/dev/rbd7"] = handle
	f.metadata[handle] = map[string]string{
		cacheMetaPrefix + "used_at": old.Format(time.RFC3339Nano),
	}
	volume := storecontract.Volume{
		Key: "acme/api/long-job", Handle: handle, Device: "/dev/rbd7",
	}

	if _, err := c.snapshotAt(t.Context(), volume, now); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	refreshed, err := time.Parse(time.RFC3339Nano, f.metadata[handle][cacheMetaPrefix+"used_at"])
	if err != nil {
		t.Fatalf("parse refreshed use time: %v", err)
	}
	if !refreshed.Equal(now) {
		t.Fatalf("writable use time = %s, want commit time %s", refreshed, now)
	}

	refreshCall := -1
	unmapCall := -1
	for i, call := range f.calls {
		if slices.Contains(call, "image-meta") && slices.Contains(call, handle) &&
			strings.Contains(strings.Join(call, "\x00"), cacheMetaPrefix+"used_at") {
			refreshCall = i
		}
		if slices.Contains(call, "unmap") {
			unmapCall = i
		}
	}
	if refreshCall < 0 || unmapCall < 0 || refreshCall >= unmapCall {
		t.Fatalf("writable refresh call %d did not precede unmap call %d", refreshCall, unmapCall)
	}
}

func TestCleanupFailureDoesNotRemainClassifiedAsAColdMiss(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	key := "acme/api/missing-with-orphan"
	source := "billet-cache/cache-g-1-missing"
	pointer := cachePointer{
		Generation: "g1", Handle: source, UsedAt: time.Now().UTC(), RetentionHours: 7 * 24,
	}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey(key): string(encoded)}
	f.metadata[source] = map[string]string{cacheMetaPrefix + "lineage": "0"}
	f.failRemove = true

	_, err = c.Clone(t.Context(), key, "")
	if err == nil {
		t.Fatal("Clone succeeded after source and cleanup failures")
	}
	if errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("cleanup failure remained classified as a replaceable cache miss: %v", err)
	}
	if !strings.Contains(err.Error(), "injected remove failure") {
		t.Fatalf("cleanup failure was lost: %v", err)
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
