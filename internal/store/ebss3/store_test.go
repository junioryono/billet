package ebss3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	storecontract "github.com/junioryono/billet/internal/store"
)

type fakeBlocks struct {
	nextVolume    int
	nextSnapshot  int
	createdFrom   map[string]string
	deletedVolume []string
	deletedSnap   []string
	snapshots     map[string]snapshotInfo
	available     []volumeInfo
}

func newFakeBlocks() *fakeBlocks {
	return &fakeBlocks{createdFrom: map[string]string{}, snapshots: map[string]snapshotInfo{}}
}

func (f *fakeBlocks) CreateVolume(
	_ context.Context, snapshot string, _ int64, _ string,
) (string, error) {
	f.nextVolume++
	id := fmt.Sprintf("vol-%d", f.nextVolume)
	f.createdFrom[id] = snapshot

	return id, nil
}

func (f *fakeBlocks) DeleteVolume(_ context.Context, id string) error {
	f.deletedVolume = append(f.deletedVolume, id)

	return nil
}

func (f *fakeBlocks) CreateSnapshot(
	_ context.Context, volume string, now time.Time, _ string,
) (string, error) {
	if !strings.HasPrefix(volume, "vol-") {
		return "", errors.New("not a volume")
	}
	f.nextSnapshot++
	id := fmt.Sprintf("snap-%d", f.nextSnapshot)
	f.snapshots[id] = snapshotInfo{ID: id, Created: now}

	return id, nil
}

func (f *fakeBlocks) SnapshotExists(_ context.Context, id string) (bool, error) {
	_, ok := f.snapshots[id]

	return ok, nil
}

func (f *fakeBlocks) ListSnapshots(context.Context) ([]snapshotInfo, error) {
	out := make([]snapshotInfo, 0, len(f.snapshots))
	for _, snapshot := range f.snapshots {
		out = append(out, snapshot)
	}

	return out, nil
}

func (f *fakeBlocks) DeleteSnapshot(_ context.Context, id string) error {
	delete(f.snapshots, id)
	f.deletedSnap = append(f.deletedSnap, id)

	return nil
}

func (f *fakeBlocks) ListAvailableVolumes(context.Context) ([]volumeInfo, error) {
	return slices.Clone(f.available), nil
}

type fakeObject struct {
	body []byte
	etag string
}

type fakeObjects struct {
	objects      map[string]fakeObject
	next         int
	putErr       error
	afterPutErr  error
	afterPutHook func()
}

func newFakeObjects() *fakeObjects { return &fakeObjects{objects: map[string]fakeObject{}} }

func (f *fakeObjects) Get(_ context.Context, key string) ([]byte, string, bool, error) {
	object, ok := f.objects[key]
	if !ok {
		return nil, "", false, nil
	}

	return slices.Clone(object.body), object.etag, true, nil
}

func (f *fakeObjects) Put(
	_ context.Context,
	key string,
	body []byte,
	expected string,
) (string, error) {
	if f.putErr != nil {
		return "", f.putErr
	}
	current, exists := f.objects[key]
	if expected == "" && exists || expected != "" && (!exists || current.etag != expected) {
		return "", errObjectConflict
	}
	f.next++
	etag := fmt.Sprintf("etag-%d", f.next)
	f.objects[key] = fakeObject{body: slices.Clone(body), etag: etag}
	if f.afterPutErr != nil {
		err := f.afterPutErr
		f.afterPutErr = nil
		hook := f.afterPutHook
		f.afterPutHook = nil
		if hook != nil {
			hook()
		}

		return "", err
	}

	return etag, nil
}

func (f *fakeObjects) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	return keys, nil
}

func testStore(now *time.Time) (*Store, *fakeBlocks, *fakeObjects) {
	blocks := newFakeBlocks()
	objects := newFakeObjects()
	s := newStore(config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a",
		Bucket: "billet-cache-example", Prefix: "deployments/example/home",
	}, "example/home", blocks, objects, withNow(func() time.Time { return *now }))

	return s, blocks, objects
}

func TestACloudGenerationPublishesByCASAndClonesFromItsSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)
	volume, err := s.Create(t.Context(), "acme/api/npm", 10<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	volume.Filesystem = storecontract.Filesystem{Type: "ext4", UUID: "fs-1", Clean: true}
	lease, fence, err := s.AcquireWriter(t.Context(), volume.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	candidate, err := s.Snapshot(t.Context(), volume)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	state, _, err := s.load(t.Context(), volume.Key)
	if err != nil {
		t.Fatalf("load state after Snapshot: %v", err)
	}
	if len(state.Active) != 0 {
		t.Fatalf("Snapshot kept consumed writable-volume custody: %+v", state.Active)
	}
	if !slices.Contains(blocks.deletedVolume, volume.Handle) {
		t.Fatal("snapshot did not consume its writable EBS volume")
	}
	if err := s.PublishCAS(t.Context(), volume.Key, "", candidate, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	if current, err := s.Current(t.Context(), volume.Key); err != nil || current != candidate.Generation {
		t.Fatalf("Current = %q, %v; want %q", current, err, candidate.Generation)
	}

	clone, err := s.Clone(t.Context(), volume.Key, "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if blocks.createdFrom[clone.Handle] != candidate.Handle || clone.Lease.ID == "" {
		t.Fatalf("clone = %+v, created from %q", clone, blocks.createdFrom[clone.Handle])
	}
	now = now.Add(time.Minute)
	if err := s.RenewActive(t.Context(), clone, now.Add(time.Hour)); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}
	if err := s.Discard(t.Context(), clone); err != nil {
		t.Fatalf("Discard: %v", err)
	}
}

func TestCloudStateIsIsolatedByDeploymentAndSiteOwner(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	first, blocks, objects := testStore(&now)
	if _, err := first.Create(t.Context(), "acme/api/npm", 1<<30); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second := newStore(first.cfg, "another-deployment/home", blocks, objects,
		withNow(func() time.Time { return now }))
	if first.stateKey("acme/api/npm") == second.stateKey("acme/api/npm") {
		t.Fatal("two owners share one S3 state object")
	}
	if _, err := second.Current(t.Context(), "acme/api/npm"); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("another owner observed the first owner's cache state: %v", err)
	}
}

func TestUnnamespacedCloudStateIsNeverRead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s, _, objects := testStore(&now)
	unnamespaced := emptyState("acme/api/npm")
	unnamespaced.Pointer = "another-owner-generation"
	unnamespaced.Generations[unnamespaced.Pointer] = generationState{
		Handle: "snap-old", UsedAt: now,
	}
	body, err := json.Marshal(unnamespaced)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	digest := sha256.Sum256([]byte(unnamespaced.Key))
	oldKey := s.cfg.Prefix + "/state/" + hex.EncodeToString(digest[:]) + ".json"
	if _, err := objects.Put(t.Context(), oldKey, body, ""); err != nil {
		t.Fatalf("seed unnamespaced state: %v", err)
	}
	if got, err := s.Current(t.Context(), unnamespaced.Key); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("Current = %q, %v; want an isolated cache miss", got, err)
	}
	if _, _, found, err := objects.Get(t.Context(), s.stateKey(unnamespaced.Key)); err != nil || found {
		t.Fatalf("unnamespaced state was copied into this owner: found=%t err=%v", found, err)
	}
}

func TestACloudSnapshotRequiresGuestFilesystemProof(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)
	volume, err := s.Create(t.Context(), "acme/api/npm", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Snapshot(t.Context(), volume); err == nil {
		t.Fatal("Snapshot accepted a remotely detached volume with no filesystem proof")
	}
	if blocks.nextSnapshot != 0 {
		t.Fatal("an unverified filesystem reached CreateSnapshot")
	}
}

func TestACloudPointerRejectsAStaleGenerationAndWriter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s, _, _ := testStore(&now)
	one := snapshotCandidate(t, s)
	two := snapshotCandidate(t, s)

	leaseOne, fenceOne, err := s.AcquireWriter(t.Context(), one.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter one: %v", err)
	}
	if err := s.PublishCAS(t.Context(), one.Key, "", one, leaseOne, fenceOne); err != nil {
		t.Fatalf("publish one: %v", err)
	}
	leaseTwo, fenceTwo, err := s.AcquireWriter(t.Context(), one.Key, "job-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter two: %v", err)
	}
	if err := s.PublishCAS(t.Context(), one.Key, "", two, leaseTwo, fenceTwo); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("stale expected generation error = %v", err)
	}

	now = now.Add(2 * time.Minute)
	leaseThree, fenceThree, err := s.AcquireWriter(t.Context(), one.Key, "job-3", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter three: %v", err)
	}
	if err := s.PublishCAS(t.Context(), one.Key, one.Generation, two, leaseTwo, fenceTwo); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("stale writer error = %v", err)
	}
	if fenceThree <= fenceTwo || leaseThree.ID == leaseTwo.ID {
		t.Fatalf("new writer = %+v fence %d; old = %+v fence %d",
			leaseThree, fenceThree, leaseTwo, fenceTwo)
	}
}

func TestCloudEvictionPreservesAnActivelyClonedGeneration(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)
	candidate := snapshotCandidate(t, s)
	lease, fence, err := s.AcquireWriter(t.Context(), candidate.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if err := s.PublishCAS(t.Context(), candidate.Key, "", candidate, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	clone, err := s.Clone(t.Context(), candidate.Key, "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := s.RenewActive(t.Context(), clone, now.Add(9*24*time.Hour)); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}
	clone.Lease.Expires = now.Add(9 * 24 * time.Hour)

	now = now.Add(8 * 24 * time.Hour)
	if err := s.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict active: %v", err)
	}
	if !slices.ContainsFunc(blocks.List(), func(id string) bool { return id == candidate.Handle }) {
		t.Fatal("eviction deleted a generation with a live active-clone lease")
	}

	now = clone.Lease.Expires.Add(time.Second)
	if err := s.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict expired: %v", err)
	}
	if !slices.Contains(blocks.deletedSnap, candidate.Handle) {
		t.Fatal("eviction kept an inactive expired generation")
	}
}

func TestCloudEvictionPreservesAColdVolumeInActiveCustody(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)
	volume, err := s.Create(t.Context(), "acme/api/npm", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blocks.available = []volumeInfo{{ID: volume.Handle, Created: now}}

	now = now.Add(8 * 24 * time.Hour)
	if err := s.RenewActive(t.Context(), volume, now.Add(time.Hour)); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}
	if err := s.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if slices.Contains(blocks.deletedVolume, volume.Handle) {
		t.Fatal("eviction deleted a cold writable volume still held in active custody")
	}
}

func TestACloudPointerSurvivesALostConditionalWriteResponse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s, _, objects := testStore(&now)
	candidate := snapshotCandidate(t, s)
	lease, fence, err := s.AcquireWriter(t.Context(), candidate.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	objects.afterPutErr = fmt.Errorf("%w: connection vanished after S3 committed the object",
		errObjectAmbiguous)
	if err := s.PublishCAS(t.Context(), candidate.Key, "", candidate, lease, fence); err != nil {
		t.Fatalf("an ambiguously acknowledged publication was not recovered: %v", err)
	}
	if current, err := s.Current(t.Context(), candidate.Key); err != nil || current != candidate.Generation {
		t.Fatalf("Current = %q, %v; want the atomically published generation", current, err)
	}

	restarted := newStore(s.cfg, s.owner, s.blocks, objects, withNow(func() time.Time { return now }))
	if err := restarted.PublishCAS(t.Context(), candidate.Key, "", candidate, lease, fence); err != nil {
		t.Fatalf("retry after process restart did not recognise the committed publication: %v", err)
	}
}

func TestACloudPublicationReceiptSurvivesANewerPointer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s, _, objects := testStore(&now)
	first := snapshotCandidate(t, s)
	second := snapshotCandidate(t, s)
	lease, fence, err := s.AcquireWriter(t.Context(), first.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	var supersedeErr error
	objects.afterPutErr = fmt.Errorf("%w: connection vanished after S3 committed the object",
		errObjectAmbiguous)
	objects.afterPutHook = func() {
		secondLease, secondFence, acquireErr := s.AcquireWriter(
			t.Context(), second.Key, "job-2", time.Minute,
		)
		if acquireErr != nil {
			supersedeErr = acquireErr

			return
		}
		supersedeErr = s.PublishCAS(t.Context(), second.Key, first.Generation,
			second, secondLease, secondFence)
	}
	if err := s.PublishCAS(t.Context(), first.Key, "", first, lease, fence); err != nil {
		t.Fatalf("the superseded publication receipt was not recovered: %v", err)
	}
	if supersedeErr != nil {
		t.Fatalf("publish newer generation: %v", supersedeErr)
	}
	if current, err := s.Current(t.Context(), first.Key); err != nil || current != second.Generation {
		t.Fatalf("Current = %q, %v; want newer generation %q", current, err, second.Generation)
	}
}

func TestACloudWriterLeaseSurvivesALostConditionalWriteResponse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s, _, objects := testStore(&now)
	objects.afterPutErr = fmt.Errorf("%w: connection vanished after S3 committed the object",
		errObjectAmbiguous)
	lease, fence, err := s.AcquireWriter(t.Context(), "acme/api/npm", "job-1", time.Minute)
	if err != nil {
		t.Fatalf("an ambiguously acknowledged writer lease was not recovered: %v", err)
	}
	if lease.ID == "" || fence == 0 {
		t.Fatalf("writer receipt = %+v fence %d", lease, fence)
	}
}

func TestCloudCacheMatchUsesExactThenNewestWithinTheFirstRestorePrefix(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	s, _, objects := testStore(&now)
	states := []keyState{
		{Key: "scope/npm-linux-old", Pointer: "g-old", Generations: map[string]generationState{
			"g-old": {Handle: "snap-old", PublishedAt: now.Add(-time.Hour)},
		}},
		{Key: "scope/npm-linux-new", Pointer: "g-new", Generations: map[string]generationState{
			"g-new": {Handle: "snap-new", PublishedAt: now},
		}},
		{Key: "scope/npm-other", Pointer: "g-other", Generations: map[string]generationState{
			"g-other": {Handle: "snap-other", PublishedAt: now.Add(time.Hour)},
		}},
	}
	for _, state := range states {
		body, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal state: %v", err)
		}
		if _, err := objects.Put(t.Context(), s.stateKey(state.Key), body, ""); err != nil {
			t.Fatalf("put state: %v", err)
		}
	}

	key, generation, err := s.Match(t.Context(), "scope/missing",
		[]string{"scope/npm-linux-", "scope/npm-"})
	if err != nil || key != "scope/npm-linux-new" || generation != "g-new" {
		t.Fatalf("restore match = %q %q, %v", key, generation, err)
	}
	key, generation, err = s.Match(t.Context(), "scope/npm-linux-old", []string{"scope/npm-"})
	if err != nil || key != "scope/npm-linux-old" || generation != "g-old" {
		t.Fatalf("exact match = %q %q, %v", key, generation, err)
	}
}

func (f *fakeBlocks) List() []string {
	out := make([]string, 0, len(f.snapshots))
	for id := range f.snapshots {
		out = append(out, id)
	}

	return out
}

func snapshotCandidate(t *testing.T, s *Store) storecontract.Candidate {
	t.Helper()
	const key = "acme/api/npm"
	volume, err := s.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create candidate volume: %v", err)
	}
	volume.Filesystem = storecontract.Filesystem{Type: "ext4", UUID: volume.Handle, Clean: true}
	candidate, err := s.Snapshot(t.Context(), volume)
	if err != nil {
		t.Fatalf("Snapshot candidate: %v", err)
	}

	return candidate
}
