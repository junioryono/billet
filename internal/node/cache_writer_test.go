package node

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// A WRITER LEASE WHOSE PUBLICATION FAILS IS GIVEN BACK. Left standing, it holds
// the key for its whole lifetime (fifteen minutes of cold cache for every job
// at the site after one failed snapshot), because only a successful PublishCAS
// or expiry ever consumed one.

func attachSticky(t *testing.T, service *CacheService, token, key string, publication string) {
	t.Helper()

	body := map[string]any{"key": key, "size_bytes": int64(10 << 30)}
	if publication != "" {
		body["publication"] = publication
	}
	attached := cacheRequest(t, service, token, "/v1/volumes", body)
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}
}

func TestASnapshotFailureGivesTheWriterLeaseBack(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	storage.snapshotErr = errors.New("CreateSnapshot: UnknownParameter")
	attachSticky(t, service, token, "acme/api/npm", "")

	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK || !bytes.Contains(committed.Body.Bytes(), []byte(`"published":false`)) {
		t.Fatalf("commit = %d %s, want a non-fatal miss", committed.Code, committed.Body.String())
	}
	if !slices.Equal(storage.released, []string{"writer-1"}) {
		t.Fatalf("released writers = %v, want the lease the failed snapshot held", storage.released)
	}
	if storage.published != 0 || len(storage.writers) != 0 {
		t.Fatalf("published=%d writers=%v after a failed snapshot, want nothing published and the key free",
			storage.published, storage.writers)
	}
}

func TestAPublishFailureGivesTheWriterLeaseBack(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	storage.publishErr = errors.New("cluster unavailable")
	attachSticky(t, service, token, "acme/api/npm", "")

	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK || !bytes.Contains(committed.Body.Bytes(), []byte(`"published":false`)) {
		t.Fatalf("commit = %d %s, want a non-fatal miss", committed.Code, committed.Body.String())
	}
	if !slices.Equal(storage.released, []string{"writer-1"}) || len(storage.writers) != 0 {
		t.Fatalf("released=%v writers=%v, want the lease the failed publication held given back",
			storage.released, storage.writers)
	}
}

// The CAS consumes the writer, so a publication leaves nothing to release and
// the call has to be precise about when a publication is over.
func TestAPublishedGenerationLeavesNoWriterToRelease(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	attachSticky(t, service, token, "acme/api/npm", "")

	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK || !bytes.Contains(committed.Body.Bytes(), []byte(`"published":true`)) {
		t.Fatalf("commit = %d %s, want a publication", committed.Code, committed.Body.String())
	}
	if len(storage.released) != 0 {
		t.Fatalf("released writers = %v after a successful publication", storage.released)
	}
	if len(storage.writers) != 0 {
		t.Fatalf("the store still records a writer after the publication consumed it: %v", storage.writers)
	}
}

func TestAFailedDockerSettlementGivesTheWriterLeaseBack(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{snapshotErr: errors.New("CreateSnapshot: UnknownParameter")}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token := credentials.Token
	if response := cacheRequest(t, service, token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"}); response.Code != http.StatusCreated {
		t.Fatalf("Docker store status = %d: %s", response.Code, response.Body.String())
	}

	proof := map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
	}
	settled := make(chan error, 1)
	go func() { settled <- service.SettleDocker(t.Context(), "billet-one", true) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := cacheRequest(t, service, token, "/v1/docker-store/ready", proof)
		if ready.Code == http.StatusOK {
			break
		}
		if ready.Code != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("Docker store readiness status = %d: %s", ready.Code, ready.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	err = <-settled
	if err == nil || !strings.Contains(err.Error(), "snapshot Docker image store") {
		t.Fatalf("SettleDocker = %v, want the snapshot failure", err)
	}
	if !slices.Equal(storage.released, []string{"writer-1"}) || len(storage.writers) != 0 {
		t.Fatalf("released=%v writers=%v, want the lease the failed settlement held given back",
			storage.released, storage.writers)
	}
}

// actionsUploadForTest reserves and uploads one Actions cache entry so a
// finalize can be driven against it. It returns the finalize request body.
func actionsUploadForTest(t *testing.T, service *CacheService, session *cacheSession, key string) string {
	t.Helper()

	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"`+key+`","version":"v1"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}
	upload := actionsRequestForTest(t, http.MethodPut, uploadURL, "archive-body")
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, _, err = service.actionsResponse(upload, session)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	response.Body.Close()

	return `{"key":"` + key + `","version":"v1","size_bytes":"12"}`
}

// finalizeForTest drives one finalize and returns its status.
func finalizeForTest(t *testing.T, service *CacheService, session *cacheSession, body string) int {
	t.Helper()

	response, handled := service.actions.respond(actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath, body), session)
	response.Body.Close()
	if !handled {
		t.Fatal("a finalize was passed to GitHub rather than answered locally")
	}

	return response.StatusCode
}

// namedActionsService is testActionsService over a root the test names, so it
// can be reopened from disk.
func namedActionsService(
	t *testing.T,
	root string,
	storage *fakeCacheStore,
	volumes *fakeActionsVolumeManager,
) (*CacheService, string) {
	t.Helper()

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.actionIO = volumes
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	credentials, err := service.PrepareScoped("billet-actions", CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service, credentials.Token
}

func sessionForToken(t *testing.T, service *CacheService, token string) *cacheSession {
	t.Helper()

	service.mu.Lock()
	session := service.byToken[token]
	service.mu.Unlock()
	if session == nil {
		t.Fatal("the cache session was not recovered")

		// Unreachable; see internal/node/reap_test.go for why it is written.
		return nil
	}

	return session
}

func receiptOf(t *testing.T, session *cacheSession, key string) *actionsReceipt {
	t.Helper()

	session.mu.Lock()
	defer session.mu.Unlock()
	receipt := session.receipts[actionsReceiptID(key, "v1")]
	if receipt == nil {
		t.Fatalf("no receipt for %q", key)

		// Unreachable; see internal/node/reap_test.go for why it is written.
		return nil
	}
	copied := *receipt

	return &copied
}

// The Actions finalize holds its lease in the frame until the receipt is
// durable, and in the receipt after that — a retried finalize reuses it. So a
// failure before the receipt releases, and a failure after it does not, and
// "after it" means the receipt is ON DISK: a receipt only in memory is a
// crash away from a writer nothing durable can publish with or give back.
func TestAnActionsFinalizeReleasesItsWriterOnlyUntilTheReceiptIsDurable(t *testing.T) {
	t.Parallel()

	t.Run("an unmount failure before the receipt releases", func(t *testing.T) {
		t.Parallel()

		service, storage, session, volumes := testActionsService(t)
		body := actionsUploadForTest(t, service, session, "release")
		volumes.unmountErr = errors.New("umount: target is busy")
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway {
			t.Fatalf("finalize status=%d, want a local failure", status)
		}
		if !slices.Equal(storage.released, []string{"writer-1"}) || storage.snapshots != 0 ||
			len(storage.writers) != 0 {
			t.Fatalf("released=%v snapshots=%d writers=%v, want the lease back and no snapshot",
				storage.released, storage.snapshots, storage.writers)
		}
	})

	t.Run("a publish failure after the receipt keeps the lease in the durable receipt", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		storage := &fakeCacheStore{}
		service, token := namedActionsService(t, root, storage, &fakeActionsVolumeManager{})
		session := sessionForToken(t, service, token)
		body := actionsUploadForTest(t, service, session, "release")
		storage.publishErr = errors.New("cluster unavailable")
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway {
			t.Fatalf("finalize status=%d, want a local failure", status)
		}
		if len(storage.released) != 0 {
			t.Fatalf("released=%v, but the durable receipt is what holds this lease", storage.released)
		}

		// Reopened from disk: the receipt, and the lease in it, survive a restart.
		restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
			storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("restart NewCacheService: %v", err)
		}
		receipt := receiptOf(t, sessionForToken(t, restarted, token), "release")
		if receipt.Complete || receipt.Lease.ID != "writer-1" {
			t.Fatalf("recovered receipt = %+v, want writer-1 retained, incomplete", receipt)
		}
		if live, ok := storage.writers[receipt.StoreKey]; !ok || live.id != "writer-1" {
			t.Fatalf("store writers = %v, want writer-1 still held for the retry", storage.writers)
		}
	})
}

// After a conflict the receipt re-acquires — and its own lease may still be
// live when the conflict was S3 contention rather than a newer writer, in
// which case the acquisition is refused by this receipt's own holder for the
// rest of the lease. The live lease is given back first, exactly, and the
// fake refuses an acquisition against a live writer just as the stores do, so
// the second acquisition succeeding is what proves the release was exact.
func TestAnActionsReceiptReleasesItsLiveLeaseBeforeAcquiringAnother(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	body := actionsUploadForTest(t, service, session, "contended")
	storage.publishConflicts = 1
	response, _, err := service.actionsResponse(actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath, body), session)
	if err != nil {
		t.Fatalf("FinalizeCacheEntryUpload: %v", err)
	}
	if finalized := responseJSON(t, response); finalized["ok"] != true {
		t.Fatalf("finalization = %v, want ok after the retry", finalized)
	}
	want := []string{"acquire writer-1", "release writer-1", "acquire writer-2"}
	if !slices.Equal(storage.writerEvents, want) {
		t.Fatalf("writer events = %v, want %v", storage.writerEvents, want)
	}
	if len(storage.writers) != 0 {
		t.Fatalf("store writers = %v after the publication, want none", storage.writers)
	}
}

// A receipt completed from the pointer the store shows never uses its lease
// again, so it gives it back: a pointer written whose writer removal then
// failed would otherwise hold the key until expiry. The release comes BEFORE
// the verdict is recorded, because a recorded verdict returns early forever
// after and nothing would release the lease then.
func TestACompletedReceiptGivesItsWriterBack(t *testing.T) {
	t.Parallel()

	t.Run("the verdict is recorded after the release", func(t *testing.T) {
		t.Parallel()

		service, storage, session, _ := testActionsService(t)
		body := actionsUploadForTest(t, service, session, "completed")
		storage.publishErr = errors.New("cluster unavailable")
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway ||
			len(storage.released) != 0 {
			t.Fatalf("first finalize status=%d released=%v, want a kept lease", status, storage.released)
		}

		// The pointer now shows this receipt's candidate — a publication whose
		// response was lost, or whose writer removal failed after the pointer moved.
		storage.publishErr = nil
		storage.current = "next"
		response, _, err := service.actionsResponse(actionsRequestForTest(t, http.MethodPost,
			"https://"+actionsResultsHost+actionsFinalizePath, body), session)
		if err != nil {
			t.Fatalf("retry finalize: %v", err)
		}
		if finalized := responseJSON(t, response); finalized["ok"] != true {
			t.Fatalf("finalization = %v, want ok from the pointer the store shows", finalized)
		}
		if !slices.Equal(storage.released, []string{"writer-1"}) || storage.published != 1 ||
			len(storage.writers) != 0 {
			t.Fatalf("released=%v published=%d writers=%v, want the completed receipt's lease given back without a publish",
				storage.released, storage.published, storage.writers)
		}
	})

	t.Run("the release does not wait for a verdict that fails to record", func(t *testing.T) {
		t.Parallel()

		service, storage, session, _ := testActionsService(t)
		body := actionsUploadForTest(t, service, session, "unrecorded-verdict")
		storage.publishErr = errors.New("cluster unavailable")
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway {
			t.Fatalf("first finalize status=%d, want a kept lease", status)
		}
		storage.publishErr = nil
		storage.current = "next"
		if err := os.RemoveAll(service.stateDir); err != nil {
			t.Fatalf("remove cache state directory: %v", err)
		}
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway {
			t.Fatalf("retry status=%d, want the failed custody write reported", status)
		}
		if !slices.Equal(storage.released, []string{"writer-1"}) || len(storage.writers) != 0 {
			t.Fatalf("released=%v writers=%v, want the lease given back ahead of the verdict",
				storage.released, storage.writers)
		}
	})
}

// A fresh lease counts only once the receipt carrying it is durable. One the
// receipt could not record is released again, or a crash would leave the store
// holding a writer nothing durable can publish with or give back.
func TestAReceiptReleasesAFreshLeaseItCouldNotRecord(t *testing.T) {
	t.Parallel()

	t.Run("re-acquiring after a conflict", func(t *testing.T) {
		t.Parallel()

		service, storage, session, _ := testActionsService(t)
		body := actionsUploadForTest(t, service, session, "unrecorded")
		storage.publishConflicts = 1
		storage.publishHook = func() {
			if err := os.RemoveAll(service.stateDir); err != nil {
				t.Errorf("remove cache state directory: %v", err)
			}
		}
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway {
			t.Fatalf("finalize status=%d, want a local failure", status)
		}
		want := []string{"acquire writer-1", "release writer-1", "acquire writer-2", "release writer-2"}
		if !slices.Equal(storage.writerEvents, want) {
			t.Fatalf("writer events = %v, want %v", storage.writerEvents, want)
		}
		if len(storage.writers) != 0 {
			t.Fatalf("store writers = %v, want the unrecorded lease given back", storage.writers)
		}
		if receipt := receiptOf(t, session, "unrecorded"); receipt.Lease.ID != "writer-1" {
			t.Fatalf("receipt = %+v, want the recorded lease restored in memory", receipt)
		}
	})

	t.Run("re-acquiring for an expired lease", func(t *testing.T) {
		t.Parallel()

		service, storage, session, _ := testActionsService(t)
		// One clock for the node and the store, started a day AHEAD of the wall
		// clock: the first lease is issued, twenty minutes then pass on this
		// clock alone, and its fifteen-minute lifetime is over. Ahead of the
		// wall clock so that a receipt consulting time.Now instead of the
		// service clock still sees the lease as live and tries to publish with
		// it, which the assertion on publishedWriterIDs below refuses.
		clock := time.Now().Add(24 * time.Hour)
		service.now = func() time.Time { return clock }
		storage.clock = service.now
		body := actionsUploadForTest(t, service, session, "expired")
		storage.publishErr = errors.New("cluster unavailable")
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway {
			t.Fatalf("first finalize status=%d, want a kept receipt", status)
		}
		clock = clock.Add(20 * time.Minute)
		if receipt := receiptOf(t, session, "expired"); !receipt.Lease.Expires.Before(clock) {
			t.Fatalf("the fixture's lease is not expired: %s", receipt.Lease.Expires)
		}
		if err := os.RemoveAll(service.stateDir); err != nil {
			t.Fatalf("remove cache state directory: %v", err)
		}

		storage.publishErr = nil
		if status := finalizeForTest(t, service, session, body); status != http.StatusBadGateway {
			t.Fatalf("retry status=%d, want a local failure", status)
		}
		// The expired lease is released anyway — whether it is live is the
		// store's question — and so is the fresh one the receipt could not record.
		want := []string{"acquire writer-1", "release writer-1", "acquire writer-2", "release writer-2"}
		if !slices.Equal(storage.writerEvents, want) {
			t.Fatalf("writer events = %v, want %v", storage.writerEvents, want)
		}
		if !slices.Equal(storage.publishedWriterIDs, []string{"writer-1"}) {
			t.Fatalf("published with %v, want no publication attempted with the expired writer-1 on the retry",
				storage.publishedWriterIDs)
		}
		for i, ctxErr := range storage.releaseCtxErrs {
			if ctxErr != nil || !storage.releaseDeadlines[i] {
				t.Fatalf("release %d ran on a context that was %v with deadline=%t", i, ctxErr,
					storage.releaseDeadlines[i])
			}
		}
	})
}

// The receipt's release, like the commit's, runs on a context that does not
// inherit the request's cancellation.
func TestAReceiptsReleaseOutlivesACancelledFinalize(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	body := actionsUploadForTest(t, service, session, "cancelled")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	storage.publishConflicts = 1
	storage.publishHook = cancel
	finalize := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath, body).WithContext(ctx)
	response, _ := service.actions.respond(finalize, session)
	response.Body.Close()
	if !slices.Equal(storage.released, []string{"writer-1"}) {
		t.Fatalf("released = %v, want the conflicted lease given back", storage.released)
	}
	if storage.releaseCtxErrs[0] != nil || !storage.releaseDeadlines[0] {
		t.Fatalf("the release ran on a context that was %v with deadline=%t",
			storage.releaseCtxErrs[0], storage.releaseDeadlines[0])
	}
}

// The release runs on the failure path, so it must not inherit the failure: a
// commit whose context is gone still owes the key back, on a bounded context of
// its own.
func TestTheWriterReleaseOutlivesACancelledCommit(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	attachSticky(t, service, token, "acme/api/npm", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	storage.snapshotHook = cancel
	storage.snapshotErr = errors.New("snapshot lost to cancellation")

	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/volumes/0/commit", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", response.Code, response.Body.String())
	}
	if !slices.Equal(storage.released, []string{"writer-1"}) {
		t.Fatalf("released writers = %v, want the lease the failed snapshot held", storage.released)
	}
	if storage.releaseCtxErrs[0] != nil {
		t.Fatalf("the release arrived on a context already %v", storage.releaseCtxErrs[0])
	}
	if !storage.releaseDeadlines[0] {
		t.Fatal("the release ran unbounded")
	}
}

// THE LAST-WRITE-WINS WAIT BACKS OFF AND SAYS WHAT IT IS WAITING FOR. Before,
// it polled the store every 100ms — ~20 S3 requests a second — and logged
// nothing, so a settlement that lasted minutes read as a hang.

// waitFixture drives the wait on a clock the fake sleep advances, so what the
// waiter thinks is left of a lease is a fact about the fixture and not about
// the machine running the test.
type waitFixture struct {
	service *CacheService
	storage *fakeCacheStore
	token   string
	logs    *bytes.Buffer
	waits   []time.Duration
	clock   time.Time
}

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	w := &waitFixture{
		service: service, storage: storage, token: token, logs: &bytes.Buffer{},
		clock: time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC),
	}
	service.log = slog.New(slog.NewTextHandler(w.logs, nil))
	service.now = func() time.Time { return w.clock }
	service.wait = func(_ context.Context, d time.Duration) error {
		w.waits = append(w.waits, d)
		w.clock = w.clock.Add(d)

		return nil
	}
	storage.clock = func() time.Time { return w.clock }

	return w
}

func (w *waitFixture) commit(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return cacheRequest(t, w.service, w.token, "/v1/volumes/0/commit", nil)
}

func TestALastWriteWinsWaitBacksOffAndNamesTheHolder(t *testing.T) {
	t.Parallel()

	w := newWaitFixture(t)
	expires := w.clock.Add(10 * time.Minute)
	w.storage.heldRefusals, w.storage.heldHolder, w.storage.heldExpires = 3, "i-0288803c710732024", expires
	attachSticky(t, w.service, w.token, "acme/api/buildkit", publicationLWW)

	committed := w.commit(t)
	if committed.Code != http.StatusOK || !bytes.Contains(committed.Body.Bytes(), []byte(`"published":true`)) {
		t.Fatalf("commit = %d %s, want a publication once the holder let go", committed.Code, committed.Body.String())
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	if !slices.Equal(w.waits, want) {
		t.Fatalf("waits = %v, want the doubling schedule %v", w.waits, want)
	}
	log := w.logs.String()
	if strings.Count(log, "waiting for another writer of this cache key") != 1 {
		t.Fatalf("the wait was announced %d times, want once:\n%s",
			strings.Count(log, "waiting for another writer of this cache key"), log)
	}
	if !strings.Contains(log, "held_by=i-0288803c710732024") ||
		!strings.Contains(log, "until="+expires.UTC().Format(time.RFC3339)) ||
		!strings.Contains(log, "remaining=10m0s") {
		t.Fatalf("the announcement does not say who holds the key or until when:\n%s", log)
	}
	if !strings.Contains(log, "acquired the cache writer after waiting") ||
		!strings.Contains(log, "waited=3.5s") {
		t.Fatalf("the end of the wait was not reported with how long it took:\n%s", log)
	}
}

func TestALastWriteWinsWaitStopsDoublingAtTheCap(t *testing.T) {
	t.Parallel()

	w := newWaitFixture(t)
	w.storage.heldRefusals, w.storage.heldHolder, w.storage.heldExpires = 8, "i-abc", w.clock.Add(time.Hour)
	attachSticky(t, w.service, w.token, "acme/api/buildkit", publicationLWW)

	if committed := w.commit(t); committed.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", committed.Code, committed.Body.String())
	}
	want := []time.Duration{
		500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, cacheWriterWaitCap, cacheWriterWaitCap,
	}
	if !slices.Equal(w.waits, want) {
		t.Fatalf("waits = %v, want doubling to the cap and staying there: %v", w.waits, want)
	}
}

func TestALastWriteWinsWaitAnnouncesANewHolder(t *testing.T) {
	t.Parallel()

	w := newWaitFixture(t)
	w.storage.heldRefusals = 3
	w.storage.heldHolders = []string{"i-first", "i-first", "i-second"}
	w.storage.heldExpires = w.clock.Add(10 * time.Minute)
	attachSticky(t, w.service, w.token, "acme/api/buildkit", publicationLWW)

	if committed := w.commit(t); committed.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", committed.Code, committed.Body.String())
	}
	if len(w.waits) != 3 {
		t.Fatalf("waits = %v, want three", w.waits)
	}
	log := w.logs.String()
	if strings.Count(log, "waiting for another writer of this cache key") != 2 ||
		!strings.Contains(log, "held_by=i-first") || !strings.Contains(log, "held_by=i-second") {
		t.Fatalf("a changed holder was not announced:\n%s", log)
	}
}

// Each sleep is cut to what is left of the holder's lease AT THAT ATTEMPT, so
// the waiter is back the moment the lease ends. Past the expiry, a store that
// still refuses is running on its own clock, and the schedule carries on.
func TestALastWriteWinsWaitNeverSleepsPastTheHoldersLease(t *testing.T) {
	t.Parallel()

	w := newWaitFixture(t)
	expires := w.clock.Add(1400 * time.Millisecond)
	w.storage.heldRefusals, w.storage.heldHolder, w.storage.heldExpires = 3, "i-abc", expires
	attachSticky(t, w.service, w.token, "acme/api/buildkit", publicationLWW)

	if committed := w.commit(t); committed.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", committed.Code, committed.Body.String())
	}
	// 500ms fits; the scheduled 1s is cut to the 900ms left; the lease has then
	// ended on this clock and the third refusal is polled on the 2s schedule.
	want := []time.Duration{500 * time.Millisecond, 900 * time.Millisecond, 2 * time.Second}
	if !slices.Equal(w.waits, want) {
		t.Fatalf("waits = %v, want %v", w.waits, want)
	}

	// Below the floor, the first sleep itself is cut.
	short := newWaitFixture(t)
	short.storage.heldRefusals, short.storage.heldHolder = 1, "i-abc"
	short.storage.heldExpires = short.clock.Add(200 * time.Millisecond)
	attachSticky(t, short.service, short.token, "acme/api/buildkit", publicationLWW)
	if committed := short.commit(t); committed.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", committed.Code, committed.Body.String())
	}
	if want := []time.Duration{200 * time.Millisecond}; !slices.Equal(short.waits, want) {
		t.Fatalf("waits = %v, want the floor cut to %v", short.waits, want)
	}
}

func TestACASPolicyDoesNotWaitOnAHeldWriter(t *testing.T) {
	t.Parallel()

	w := newWaitFixture(t)
	w.storage.heldRefusals, w.storage.heldHolder, w.storage.heldExpires = 1, "i-abc", w.clock.Add(10*time.Minute)
	attachSticky(t, w.service, w.token, "acme/api/npm", "")

	committed := w.commit(t)
	if committed.Code != http.StatusOK || !bytes.Contains(committed.Body.Bytes(), []byte(`"published":false`)) {
		t.Fatalf("commit = %d %s, want a non-fatal miss", committed.Code, committed.Body.String())
	}
	if len(w.waits) != 0 {
		t.Fatalf("a CAS publication waited %v on a held writer", w.waits)
	}
	if log := w.logs.String(); !strings.Contains(log, "operation=\"acquire writer\"") ||
		!strings.Contains(log, "i-abc") {
		t.Fatalf("the refusal did not name the holder:\n%s", log)
	}
}
