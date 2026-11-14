package node

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// errLostWrites is how a store or a host filesystem reports writes that never
// reached a volume.
var errLostWrites = fmt.Errorf("%w: /dev/rbd4: input/output error", storecontract.ErrWritesLost)

// A STICKY DISK WHOSE DETACH LOST WRITES IS NEVER PUBLISHED, NOR ON A COMMIT ASKED
// AGAIN. The kernel tells the one fsync that its write-back failed, so the second
// commit's detach is told nothing; only the mark the first one left refuses it.
func TestAStickyDiskWhoseDetachLostWritesIsNeverPublished(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	attacher, ok := service.attacher.(*fakeVolumeAttacher)
	if !ok {
		t.Fatalf("the service's attacher is %T", service.attacher)
	}

	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d", attached.Code)
	}

	attacher.detachErr = fmt.Errorf("firecracker: cache slot 1 is detached: %w", provider.ErrWritesLost)
	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK || !bytes.Contains(committed.Body.Bytes(), []byte(`"published":false`)) ||
		!strings.Contains(committed.Body.String(), "lost") {
		t.Fatalf("a commit whose detach lost writes answered %d %s, want an unpublished commit that says why",
			committed.Code, committed.Body.String())
	}

	attacher.detachErr = nil
	detaches := len(attacher.detached)
	again := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if !bytes.Contains(again.Body.Bytes(), []byte(`"published":false`)) ||
		!strings.Contains(again.Body.String(), "lost") {
		t.Errorf("the commit asked again answered %s, want it unpublished for its lost writes", again.Body.String())
	}
	if len(attacher.detached) != detaches {
		t.Error("the commit asked again detached the volume again, as if its writes had landed")
	}
	if storage.snapshots != 0 || storage.published != 0 {
		t.Errorf("a volume whose writes were lost was snapshotted %d time(s) and published %d",
			storage.snapshots, storage.published)
	}
}

// A STICKY DISK WHOSE SNAPSHOT LOST WRITES IS NEVER PUBLISHED, NOR ON A COMMIT ASKED
// AGAIN, whose snapshot would be told nothing.
func TestAStickyDiskWhoseSnapshotLostWritesIsNeverPublished(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d", attached.Code)
	}

	storage.snapshotErr = errLostWrites
	if committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil); !bytes.Contains(
		committed.Body.Bytes(), []byte(`"published":false`)) {
		t.Fatalf("a commit whose snapshot lost writes answered %s", committed.Body.String())
	}

	storage.snapshotErr = nil
	cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if storage.snapshots != 1 || storage.published != 0 {
		t.Errorf("after lost writes the volume was snapshotted %d time(s) and published %d; want the one "+
			"snapshot that lost them and no publication", storage.snapshots, storage.published)
	}
}

// AN ACTIONS ARCHIVE WHOSE WRITES WERE LOST IS NEVER PUBLISHED, NOR BY A FINALIZE
// ASKED AGAIN: lost before its unmount, its filesystem's syncfs says so; lost
// below the filesystem, its snapshot does. Either says it once.
func TestAnActionsArchiveWhoseWritesWereLostIsNeverPublished(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		lose      func(*fakeCacheStore, *fakeActionsVolumeManager)
		recover   func(*fakeCacheStore, *fakeActionsVolumeManager)
		snapshots int
	}{
		"its filesystem's write-back": {
			lose:    func(_ *fakeCacheStore, v *fakeActionsVolumeManager) { v.writesLost = errLostWrites },
			recover: func(_ *fakeCacheStore, v *fakeActionsVolumeManager) { v.writesLost = nil },
		},
		"its snapshot": {
			lose:      func(s *fakeCacheStore, _ *fakeActionsVolumeManager) { s.snapshotErr = errLostWrites },
			recover:   func(s *fakeCacheStore, _ *fakeActionsVolumeManager) { s.snapshotErr = nil },
			snapshots: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, storage, session, volumes := testActionsService(t)
			create := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsCreatePath, `{"key":"lost-writes","version":"v1"}`)
			response, _, err := service.actionsResponse(create, session)
			if err != nil {
				t.Fatalf("CreateCacheEntry: %v", err)
			}
			uploadURL, ok := responseJSON(t, response)["signed_upload_url"].(string)
			if !ok || uploadURL == "" {
				t.Fatal("CreateCacheEntry gave no upload URL")
			}
			upload := actionsRequestForTest(t, http.MethodPut, uploadURL, "archive-body")
			upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
			response, _, err = service.actionsResponse(upload, session)
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			response.Body.Close()

			finalize := func() int {
				request := actionsRequestForTest(t, http.MethodPost,
					"https://"+actionsResultsHost+actionsFinalizePath,
					`{"key":"lost-writes","version":"v1","size_bytes":"12"}`)
				response, handled := service.actions.respond(request, session)
				if !handled {
					t.Fatal("a finalization was passed to GitHub")
				}
				response.Body.Close()

				return response.StatusCode
			}

			tc.lose(storage, volumes)
			if status := finalize(); status == http.StatusOK {
				t.Fatal("a finalization whose writes were lost succeeded")
			}
			if len(volumes.synced) == 0 {
				t.Fatal("the finalization never asked whether the archive's writes reached its volume")
			}

			tc.recover(storage, volumes)
			if status := finalize(); status == http.StatusOK {
				t.Error("a finalization asked again, after the kernel had said it once, succeeded")
			}
			if storage.published != 0 || storage.snapshots != tc.snapshots {
				t.Errorf("an archive whose writes were lost was snapshotted %d time(s) and published %d; "+
					"want %d and none", storage.snapshots, storage.published, tc.snapshots)
			}

			session.mu.Lock()
			defer session.mu.Unlock()
			for _, archive := range session.actions {
				if archive.CacheKey == "lost-writes" && !archive.WritesLost {
					t.Error("the archive does not record that its writes were lost")
				}
			}
		})
	}
}

// A CONTENT-ADDRESSED CACHE WHOSE WRITES WERE LOST IS NEVER PUBLISHED: its own
// volume, asked before its unmount, or the merge the node copied into, asked before
// that one's. The first is abandoned in its journal before the unmount, so a pass
// after, whose syncfs is told nothing, publishes nothing either.
func TestAContentAddressedCacheWhoseWritesWereLostIsNeverPublished(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		moved      bool
		lostSuffix string
	}{
		"its own volume":         {lostSuffix: "/go"},
		"the merge it copied to": {moved: true, lostSuffix: "/go.merge"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{}
			service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
			ours := "our object"
			if got := casRequest(t, service, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf(ours), ours); got.Code != http.StatusOK {
				t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
			}
			if tc.moved {
				storage.current = "theirs"
			}
			volumes.lostSuffix = tc.lostSuffix

			if err := service.SettleCompleted(t.Context(), instance, true, dispatch.CacheAuthority{}); err != nil {
				t.Fatalf("SettleCompleted: %v", err)
			}
			if err := service.Close(t.Context(), instance); err != nil {
				t.Fatalf("Close: %v", err)
			}
			// EITHER PASS MAY ANSWER AN ERROR, for a discard the fake leaves to the next;
			// what matters here is only what was published.
			if err := service.RetryClosed(t.Context()); err != nil {
				t.Logf("the first pass: %v", err)
			}
			volumes.mu.Lock()
			volumes.lostSuffix = ""
			asked := slices.ContainsFunc(volumes.synced, func(target string) bool {
				return strings.HasSuffix(target, tc.lostSuffix)
			})
			volumes.mu.Unlock()
			if err := service.RetryClosed(t.Context()); err != nil {
				t.Logf("the second pass: %v", err)
			}

			if !asked {
				t.Fatalf("nothing asked whether the writes to %s reached the volume", tc.lostSuffix)
			}
			if storage.published != 0 {
				t.Errorf("a cache whose writes were lost was published %d time(s)", storage.published)
			}
			if !tc.moved && storage.snapshots != 0 {
				t.Errorf("a volume whose writes were lost was snapshotted %d time(s)", storage.snapshots)
			}
		})
	}
}

// attachedSticky attaches a sticky disk in slot 0 of a default-branch session and
// answers its session and attachment.
func attachedSticky(t *testing.T) (*CacheService, *fakeCacheStore, *cacheSession, *cacheAttachment) {
	t.Helper()

	storage := &fakeCacheStore{}
	service, token, _ := scopedService(t, provider.TrustTrusted, defaultBranchCache(), nil, storage)
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "npm", "size_bytes": int64(10 << 30), "architecture": "amd64",
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d", attached.Code)
	}
	service.mu.Lock()
	session := service.byToken[token]
	service.mu.Unlock()
	session.mu.Lock()
	attachment := session.slots[0]
	session.mu.Unlock()
	if attachment == nil {
		t.Fatal("no attachment in slot 0")
	}

	return service, storage, session, attachment
}

// A DEFERRED PUBLICATION WHOSE SNAPSHOT LOST WRITES IS ABANDONED, NOT RETRIED: the
// next pass's snapshot would be told nothing. One already marked is abandoned
// without a snapshot.
func TestADeferredPublicationWhoseWritesWereLostIsAbandoned(t *testing.T) {
	t.Parallel()

	for name, marked := range map[string]bool{"its snapshot says so": false, "a detach said so": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, storage, session, attachment := attachedSticky(t)
			session.mu.Lock()
			defer session.mu.Unlock()
			attachment.Deferred, attachment.Ready, attachment.WritesLost = true, true, marked
			attachment.Intent = &publishIntent{LeaseID: "lease", JobID: "1", RunID: 1, At: service.now()}
			storage.snapshotErr = errLostWrites

			done, err := service.publishDeferred(t.Context(), session, 0, attachment)
			if err != nil || !done || attachment.Journal == nil || attachment.Journal.Phase != phaseAbandoned ||
				!attachment.WritesLost {
				t.Fatalf("done %v (%v), journal %+v, marked %v; want the publication abandoned and the volume marked",
					done, err, attachment.Journal, attachment.WritesLost)
			}
			storage.snapshotErr = nil
			if done, err := service.publishDeferred(t.Context(), session, 0, attachment); err != nil || !done {
				t.Errorf("the abandoned publication was taken up again: done %v, %v", done, err)
			}
			wantSnapshots := map[bool]int{false: 1, true: 0}[marked]
			if storage.snapshots != wantSnapshots || storage.published != 0 {
				t.Errorf("snapshotted %d time(s) and published %d; want %d and none",
					storage.snapshots, storage.published, wantSnapshots)
			}
		})
	}
}

// A DOCKER IMAGE STORE WHOSE DETACH LOST WRITES IS NEVER PUBLISHED, NOR ASKED
// AGAIN: the second publication neither detaches nor snapshots.
func TestADockerStoreWhoseDetachLostWritesIsNeverPublished(t *testing.T) {
	t.Parallel()

	service, storage, session, attachment := attachedSticky(t)
	attacher, ok := service.attacher.(*fakeVolumeAttacher)
	if !ok {
		t.Fatalf("the service's attacher is %T", service.attacher)
	}
	session.mu.Lock()
	defer session.mu.Unlock()

	attacher.detachErr = fmt.Errorf("detached: %w", provider.ErrWritesLost)
	if err := service.publishDocker(t.Context(), session, attachment); !errors.Is(err, errWritesLost) {
		t.Fatalf("a publication whose detach lost writes answered %v, want errWritesLost", err)
	}
	detaches := len(attacher.detached)
	attacher.detachErr = nil
	if err := service.publishDocker(t.Context(), session, attachment); !errors.Is(err, errWritesLost) {
		t.Errorf("the publication asked again answered %v, want errWritesLost", err)
	}
	if len(attacher.detached) != detaches || storage.snapshots != 0 || storage.published != 0 {
		t.Errorf("asked again, it detached %d more time(s), snapshotted %d and published %d; want nothing",
			len(attacher.detached)-detaches, storage.snapshots, storage.published)
	}
}

// THE FUNNEL REFUSES A MARKED VOLUME ITSELF, whatever its caller asked first: every
// guest-attached publication passes through publishAcquired, and a marked volume
// is neither snapshotted nor published, and its writer goes back.
func TestTheFunnelRefusesAVolumeWhoseWritesWereLost(t *testing.T) {
	t.Parallel()

	service, storage, session, attachment := attachedSticky(t)
	session.mu.Lock()
	defer session.mu.Unlock()
	attachment.WritesLost = true

	lease, fence, err := storage.AcquireWriter(t.Context(), attachment.Volume.Key, "holder", cacheWriterTTL)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if _, _, err := service.publishAcquired(t.Context(), session, 0, attachment, lease, fence); !errors.Is(err, errWritesLost) {
		t.Fatalf("publishAcquired of a marked volume answered %v, want errWritesLost", err)
	}
	if storage.snapshots != 0 || storage.published != 0 || len(storage.released) != 1 {
		t.Errorf("a marked volume was snapshotted %d time(s) and published %d, and %d writer(s) went back; "+
			"want none, none and its one", storage.snapshots, storage.published, len(storage.released))
	}
}

// THE MARK IS WHAT REFUSES, so it must be what reads as lost: a store's and a
// provider's report both, and nothing else.
func TestOnlyALostWriteReadsAsOne(t *testing.T) {
	t.Parallel()

	for err, want := range map[error]bool{
		errLostWrites: true,
		fmt.Errorf("detach: %w", provider.ErrWritesLost): true,
		errors.New("input/output error"):                 false,
		storecontract.ErrDeviceUnknown:                   false,
	} {
		if writesLost(err) != want {
			t.Errorf("writesLost(%v) = %v, want %v", err, !want, want)
		}
	}
}
