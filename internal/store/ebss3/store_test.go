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
	nextVolume   int
	nextSnapshot int
	// snapshotTokens records the request token of every CreateSnapshot, in order.
	snapshotTokens []string
	createdFrom    map[string]string
	deletedVolume  []string
	deletedSnap    []string
	snapshots      map[string]snapshotInfo
	available      []volumeInfo
	// notOwned ids refuse deletion the way the real API does for a resource
	// carrying another store's ownership tags.
	notOwned map[string]bool
	// deleteErr ids fail deletion with a NON-ownership error (a transport or
	// authorization failure), which eviction must never swallow.
	deleteErr map[string]bool
}

// errFakeDelete is a stand-in for a real (non-ownership) delete failure.
var errFakeDelete = errors.New("ebs-s3: delete failed for a non-ownership reason")

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
	if f.deleteErr[id] {
		return errFakeDelete
	}
	if f.notOwned[id] {
		return fmt.Errorf("ebs-s3: refusing to delete volume %s: %w", id, errNotOwned)
	}
	f.deletedVolume = append(f.deletedVolume, id)

	return nil
}

func (f *fakeBlocks) CreateSnapshot(
	_ context.Context, volume string, now time.Time, token string,
) (string, error) {
	if !strings.HasPrefix(volume, "vol-") {
		return "", errors.New("not a volume")
	}
	f.snapshotTokens = append(f.snapshotTokens, token)
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
	if f.deleteErr[id] {
		return errFakeDelete
	}
	if f.notOwned[id] {
		return fmt.Errorf("ebs-s3: refusing to delete snapshot %s: %w", id, errNotOwned)
	}
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
	lastGetKey   string
	getErr       error
	putErr       error
	afterPutErr  error
	afterPutHook func()
	deleteErr    error
	deleted      []string
}

func newFakeObjects() *fakeObjects { return &fakeObjects{objects: map[string]fakeObject{}} }

func (f *fakeObjects) Get(_ context.Context, key string) ([]byte, string, bool, error) {
	f.lastGetKey = key
	if f.getErr != nil {
		return nil, "", false, f.getErr
	}

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

func (f *fakeObjects) Delete(_ context.Context, key string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.objects, key)
	f.deleted = append(f.deleted, key)

	return nil
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

// THE SNAPSHOT TOKEN NAMES THE OPERATION, NOT THE ATTEMPT. EC2's CreateSnapshot
// has no ClientToken, so at-most-once rests on ebsAPI looking the token up as a
// tag — which only works if an attempt that starts later, in this process or a
// restarted one, derives the SAME token for the same key and volume. A clock in
// the derivation is what the first version had, and it made every retry a
// stranger to its predecessor's snapshot.
func TestASnapshotTokenIsTheSameAcrossClocksAndDiffersAcrossVolumes(t *testing.T) {
	t.Parallel()

	snapshotWith := func(t *testing.T, now time.Time, key, volume string) string {
		t.Helper()
		s, blocks, _ := testStore(&now)
		if _, err := s.Create(t.Context(), key, 10<<30); err != nil {
			t.Fatalf("Create: %v", err)
		}
		v := storecontract.Volume{
			Key: key, Handle: volume, Device: volume,
			Filesystem: storecontract.Filesystem{Type: "ext4", UUID: "fs-1", Clean: true},
		}
		if _, err := s.Snapshot(t.Context(), v); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if len(blocks.snapshotTokens) != 1 {
			t.Fatalf("CreateSnapshot was called %d times", len(blocks.snapshotTokens))
		}

		return blocks.snapshotTokens[0]
	}

	// A day apart, so a token carrying seconds — or a date — would differ.
	first := snapshotWith(t, time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), "acme/api/npm", "vol-1")
	later := snapshotWith(t, time.Date(2026, 8, 17, 12, 0, 0, 1, time.UTC), "acme/api/npm", "vol-1")
	if first != later {
		t.Fatalf("the token changed with the clock: %s != %s", first, later)
	}
	otherVolume := snapshotWith(t, time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), "acme/api/npm", "vol-2")
	if otherVolume == first {
		t.Fatal("two volumes derived one token")
	}
	otherKey := snapshotWith(t, time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), "acme/api/pip", "vol-1")
	if otherKey == first {
		t.Fatal("two keys derived one token for one volume")
	}
	if first == "" || len(first) != 64 {
		t.Fatalf("token = %q, want a sha256 hex digest", first)
	}
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

// EVICTION SKIPS A RESOURCE THIS STORE DOES NOT OWN rather than aborting the whole
// sweep on it. The listings are scoped by the cache-owner tag, but a delete also
// checks the deployment-owner tag, so a resource from another deployment sharing
// this cache owner is listed and then refused — and one such resource must not
// strand all the genuine garbage behind it.
func TestCloudEvictionSkipsResourcesItDoesNotOwn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)

	old := now
	blocks.snapshots = map[string]snapshotInfo{
		"snap-owned":   {ID: "snap-owned", Created: old},
		"snap-foreign": {ID: "snap-foreign", Created: old},
	}
	blocks.available = []volumeInfo{
		{ID: "vol-owned", Created: old},
		{ID: "vol-foreign", Created: old},
	}
	blocks.notOwned = map[string]bool{"snap-foreign": true, "vol-foreign": true}

	// Age everything past the retention window so the orphan sweeps consider it.
	now = now.Add(8 * 24 * time.Hour)
	if err := s.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict aborted on a foreign resource instead of skipping it: %v", err)
	}

	if !slices.Contains(blocks.deletedSnap, "snap-owned") {
		t.Error("eviction did not delete the store's own orphan snapshot")
	}
	if slices.Contains(blocks.deletedSnap, "snap-foreign") {
		t.Error("eviction deleted a snapshot owned by another store")
	}
	if !slices.Contains(blocks.deletedVolume, "vol-owned") {
		t.Error("eviction did not delete the store's own orphan volume")
	}
	if slices.Contains(blocks.deletedVolume, "vol-foreign") {
		t.Error("eviction deleted a volume owned by another store")
	}
}

// A STATE-OWNED RESOURCE THAT CANNOT BE DELETED ABORTS EVICTION rather than being
// skipped. The remove list comes from this store's own state, whose record is
// dropped before the delete — skipping a not-owned refusal there would strand the
// snapshot with nothing pointing at it. So the first loop stays fatal on any error,
// unlike the orphan sweeps.
func TestCloudEvictionAbortsOnAStateOwnedResourceItCannotDelete(t *testing.T) {
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

	// No clone/renew: the published generation is unprotected, so eviction moves it
	// to the remove list — but its ownership tags now read as foreign.
	now = now.Add(8 * 24 * time.Hour)
	blocks.notOwned = map[string]bool{candidate.Handle: true}

	if err := s.Evict(t.Context(), 7*24*time.Hour); !errors.Is(err, errNotOwned) {
		t.Fatalf("eviction did not abort with the ownership refusal from loop 1: got %v", err)
	}
	if slices.Contains(blocks.deletedSnap, candidate.Handle) {
		t.Fatal("a refused delete was recorded as done")
	}
}

// A NON-OWNERSHIP DELETE FAILURE (transport, throttling, authorization) must abort
// eviction, never be swallowed as a skip — only the errNotOwned sentinel is a skip.
func TestCloudEvictionAbortsOnANonOwnershipDeleteFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)

	old := now
	blocks.available = []volumeInfo{{ID: "vol-flaky", Created: old}}
	blocks.deleteErr = map[string]bool{"vol-flaky": true}

	now = now.Add(8 * 24 * time.Hour)
	err := s.Evict(t.Context(), 7*24*time.Hour)
	if !errors.Is(err, errFakeDelete) {
		t.Fatalf("a non-ownership delete failure was not surfaced: got %v", err)
	}
}

// PURGE REMOVES EVERYTHING THIS STORE OWNS — snapshots, available volumes, and the
// S3 state index — regardless of age, for a full deployment teardown.
func TestCloudPurgeRemovesEverythingOwned(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, objects := testStore(&now)

	candidate := snapshotCandidate(t, s)
	lease, fence, err := s.AcquireWriter(t.Context(), candidate.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if err := s.PublishCAS(t.Context(), candidate.Key, "", candidate, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	blocks.available = []volumeInfo{{ID: "vol-orphan", Created: now}}

	report, err := s.Purge(t.Context())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if report.Snapshots < 1 || report.Volumes != 1 || report.StateObjects < 1 {
		t.Errorf("purge report = %+v; want >=1 snapshot, 1 volume, >=1 state object", report)
	}
	if !slices.Contains(blocks.deletedSnap, candidate.Handle) {
		t.Error("purge did not delete the published generation's snapshot")
	}
	if !slices.Contains(blocks.deletedVolume, "vol-orphan") {
		t.Error("purge did not delete the owned available volume")
	}
	if len(objects.objects) != 0 {
		t.Errorf("purge left %d S3 state objects behind", len(objects.objects))
	}
}

// PURGE REFUSES WHILE A CLONE IS LIVE, because that means billet still believes a
// job holds the cache — the operator must stop the node first.
func TestCloudPurgeRefusesWhileACloneIsLive(t *testing.T) {
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
	if err := s.RenewActive(t.Context(), clone, now.Add(time.Hour)); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}

	_, err = s.Purge(t.Context())
	if err == nil || !strings.Contains(err.Error(), "active clone lease") {
		t.Fatalf("Purge did not refuse a live clone: %v", err)
	}
	if len(blocks.deletedSnap) != 0 {
		t.Error("purge deleted resources despite refusing")
	}
}

// PURGE SKIPS A FOREIGN RESOURCE (one this store does not own) rather than deleting
// it, counting it separately.
func TestCloudPurgeSkipsForeignResources(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)
	blocks.snapshots = map[string]snapshotInfo{
		"snap-owned":   {ID: "snap-owned"},
		"snap-foreign": {ID: "snap-foreign"},
	}
	blocks.notOwned = map[string]bool{"snap-foreign": true}

	report, err := s.Purge(t.Context())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if report.Snapshots != 1 || report.SkippedForeign != 1 {
		t.Errorf("report = %+v; want 1 owned snapshot deleted, 1 foreign skipped", report)
	}
	if slices.Contains(blocks.deletedSnap, "snap-foreign") {
		t.Error("purge deleted a foreign snapshot")
	}
}

// A NON-OWNERSHIP DELETE FAILURE ABORTS PURGE, never swallowed.
func TestCloudPurgeAbortsOnANonOwnershipError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, _ := testStore(&now)
	blocks.available = []volumeInfo{{ID: "vol-flaky"}}
	blocks.deleteErr = map[string]bool{"vol-flaky": true}

	if _, err := s.Purge(t.Context()); !errors.Is(err, errFakeDelete) {
		t.Fatalf("a non-ownership delete failure was not surfaced: %v", err)
	}
}

// PURGE FAILS CLOSED ON A ZERO-EXPIRY ACTIVE LEASE. A truncated active record whose
// expiry is the zero time is UNVERIFIABLE, not expired — purge must refuse, or a
// corrupted index would let it delete a cache a job still holds.
func TestCloudPurgeRefusesUnverifiableActiveLease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, objects := testStore(&now)

	state := emptyState("acme/api/npm")
	state.Active = map[string]activeState{"lease-x": {Volume: "vol-1"}} // Expires is the zero time
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	objects.objects[s.stateKey("acme/api/npm")] = fakeObject{body: body, etag: "e1"}
	blocks.snapshots = map[string]snapshotInfo{"snap-1": {ID: "snap-1"}}

	if _, err := s.Purge(t.Context()); err == nil || !strings.Contains(err.Error(), "may still be live") {
		t.Fatalf("Purge did not refuse a zero-expiry active lease: %v", err)
	}
	if len(blocks.deletedSnap) != 0 {
		t.Error("purge deleted despite an unverifiable active lease")
	}
}

// PURGE REJECTS A STRUCTURALLY INVALID STATE OBJECT rather than reading it as "no
// active leases" and proceeding to delete.
func TestCloudPurgeRejectsMalformedState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, blocks, objects := testStore(&now)

	// Valid JSON, but no key — Evict rejects this shape and so must Purge.
	objects.objects[s.stateKey("acme/api/npm")] = fakeObject{body: []byte("{}"), etag: "e1"}
	blocks.snapshots = map[string]snapshotInfo{"snap-1": {ID: "snap-1"}}

	if _, err := s.Purge(t.Context()); err == nil || !strings.Contains(err.Error(), "not a valid cache state") {
		t.Fatalf("Purge did not reject a malformed state object: %v", err)
	}
	if len(blocks.deletedSnap) != 0 {
		t.Error("purge deleted despite a malformed state object")
	}
}

// A NON-OWNERSHIP FAILURE DELETING THE S3 STATE INDEX aborts purge, surfaced.
func TestCloudPurgeSurfacesAStateDeleteError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s, _, objects := testStore(&now)

	candidate := snapshotCandidate(t, s)
	lease, fence, err := s.AcquireWriter(t.Context(), candidate.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if err := s.PublishCAS(t.Context(), candidate.Key, "", candidate, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	objects.deleteErr = errFakeDelete

	if _, err := s.Purge(t.Context()); !errors.Is(err, errFakeDelete) {
		t.Fatalf("a state-object delete failure was not surfaced: %v", err)
	}
}

// THE ACCESS PROBE'S CONTRACT: a clean miss under the owner's own state
// prefix is a healthy bucket, and any refusal or transport failure surfaces —
// a 403 must not read as "empty cache".
func TestCheckAccess(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s, _, objects := testStore(&now)

	if err := s.CheckAccess(t.Context()); err != nil {
		t.Fatalf("a clean miss failed the probe: %v", err)
	}

	// UNDER THE OWNER'S STATE PREFIX, because that is what the node's policy
	// is scoped to: a probe outside it would 403 under a correctly minimal
	// grant and read as a broken bucket.
	if !strings.HasPrefix(objects.lastGetKey, s.statePrefix()) {
		t.Errorf("the probe read %q, outside the policy-scoped prefix %q",
			objects.lastGetKey, s.statePrefix())
	}

	objects.getErr = errors.New("ebs-s3: S3 GET returned HTTP 403")
	if err := s.CheckAccess(t.Context()); err == nil {
		t.Fatal("a refused read passed the probe")
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("the probe hides the refusal: %v", err)
	}
}
