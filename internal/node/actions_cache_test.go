package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

type fakeActionsVolumeManager struct {
	published []byte
	mountNew  func() error
	trimmed   int
	trimErr   error
	trimCheck func(string) error
}

type syncCloseRecorder struct {
	syncs  int
	closes int
}

func (r *syncCloseRecorder) Sync() error {
	r.syncs++

	return nil
}

func (r *syncCloseRecorder) Close() error {
	r.closes++

	return nil
}

type actionsPolicyFunc func(context.Context, string, string) (bool, error)

type cancelAfterRead struct {
	cancel context.CancelFunc
	read   bool
}

func (r *cancelAfterRead) Read(body []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	r.cancel()
	body[0] = 'x'

	return 1, nil
}

func (f actionsPolicyFunc) ActionsCacheAllowed(
	ctx context.Context,
	owner, repository string,
) (bool, error) {
	return f(ctx, owner, repository)
}

func (f *fakeActionsVolumeManager) MountNew(_ context.Context, _, target string) error {
	if f.mountNew != nil {
		if err := f.mountNew(); err != nil {
			return err
		}
	}

	return os.MkdirAll(target, 0o700)
}

func (f *fakeActionsVolumeManager) MountReadOnly(_ context.Context, _, target string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(target, "archive"), f.published, 0o600)
}

func (f *fakeActionsVolumeManager) Unmount(_ context.Context, target string) error {
	if body, err := os.ReadFile(filepath.Join(target, "archive")); err == nil {
		f.published = body
	}

	return nil
}

func (f *fakeActionsVolumeManager) Trim(_ context.Context, target string) error {
	f.trimmed++
	if f.trimCheck != nil {
		if err := f.trimCheck(target); err != nil {
			return err
		}
	}

	return f.trimErr
}

func testActionsService(
	t *testing.T,
) (*CacheService, *fakeCacheStore, *cacheSession, *fakeActionsVolumeManager) {
	t.Helper()

	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	volumes := &fakeActionsVolumeManager{}
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
	service.mu.Lock()
	session := service.byToken[credentials.Token]
	service.mu.Unlock()

	return service, storage, session, volumes
}

func actionsRequestForTest(
	t *testing.T,
	method, target string,
	body string,
) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if strings.HasPrefix(target, "https://"+actionsResultsHost+"/twirp/") {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", actionsUserAgent+"test")
	}

	return req
}

func responseJSON(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()

	var value map[string]any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	return value
}

func TestActionsCacheRoundTripPublishesAndServesRanges(t *testing.T) {
	t.Parallel()

	service, storage, session, volumes := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"linux-npm-abc","version":"v1"}`)
	response, handled, err := service.actionsResponse(create, session)
	if err != nil || !handled {
		t.Fatalf("CreateCacheEntry handled=%t error=%v", handled, err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if created["ok"] != true || !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}
	if response.Header.Get(actionsLocalHeader) != "local" {
		t.Fatalf("CreateCacheEntry did not identify the local conformance path")
	}
	if strings.Contains(uploadURL, session.token) || strings.Contains(storage.keys[0], session.token) {
		t.Fatal("the signed URL or durable key exposes the guest session capability")
	}
	if !strings.Contains(storage.keys[0], "actions-cache/") {
		t.Fatalf("store key is not generic and scoped: %q", storage.keys[0])
	}
	if len(storage.createdSizes) != 1 || storage.createdSizes[0] != actionsVolumeSize ||
		actionsVolumeSize <= actionsArchiveLimit {
		t.Fatalf("archive backing sizes = %v; want %d bytes with ext4 headroom above %d",
			storage.createdSizes, actionsVolumeSize, actionsArchiveLimit)
	}

	upload := actionsRequestForTest(t, http.MethodPut, uploadURL, "archive-body")
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, handled, err = service.actionsResponse(upload, session)
	if err != nil || !handled || response.StatusCode != http.StatusCreated {
		t.Fatalf("blob upload handled=%t status=%v error=%v", handled, response, err)
	}
	response.Body.Close()

	finalize := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"linux-npm-abc","version":"v1","size_bytes":"12"}`)
	response, handled, err = service.actionsResponse(finalize, session)
	if err != nil || !handled {
		t.Fatalf("FinalizeCacheEntryUpload handled=%t error=%v", handled, err)
	}
	finalized := responseJSON(t, response)
	if finalized["ok"] != true || storage.snapshots != 1 || storage.published != 1 ||
		volumes.trimmed != 1 ||
		string(volumes.published) != "archive-body" {
		t.Fatalf("finalization=%v snapshots=%d published=%d trims=%d archive=%q",
			finalized, storage.snapshots, storage.published, volumes.trimmed, volumes.published)
	}
	entryIDText, ok := finalized["entry_id"].(string)
	entryID, parseErr := strconv.ParseInt(entryIDText, 10, 64)
	if !ok || parseErr != nil || entryID <= 0 {
		t.Fatalf("finalization entry_id = %v, error=%v; want a positive protobuf int64",
			finalized["entry_id"], parseErr)
	}

	lookup := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsDownloadPath,
		`{"key":"linux-npm-abc","restore_keys":["linux-npm-"],"version":"v1"}`)
	response, handled, err = service.actionsResponse(lookup, session)
	if err != nil || !handled {
		t.Fatalf("GetCacheEntryDownloadURL handled=%t error=%v", handled, err)
	}
	found := responseJSON(t, response)
	downloadURL, ok := found["signed_download_url"].(string)
	if found["ok"] != true || found["matched_key"] != "linux-npm-abc" || !ok || downloadURL == "" {
		t.Fatalf("GetCacheEntryDownloadURL response = %v", found)
	}

	download := actionsRequestForTest(t, http.MethodGet, downloadURL, "")
	download.Header.Set("X-Ms-Range", "bytes=2-8")
	response, handled, err = service.actionsResponse(download, session)
	if err != nil || !handled || response.StatusCode != http.StatusPartialContent {
		t.Fatalf("range download handled=%t status=%v error=%v", handled, response, err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "chive-b" || response.Header.Get("Content-Range") != "bytes 2-8/12" {
		t.Fatalf("range body=%q header=%q error=%v", body, response.Header.Get("Content-Range"), err)
	}
}

func TestActionsDownloadRefusesConflictingAzureAndHTTPRanges(t *testing.T) {
	t.Parallel()

	service, storage, session, volumes := testActionsService(t)
	storage.current = "g1"
	volumes.published = []byte("archive-body")
	lookup := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsDownloadPath,
		`{"key":"ranges","restore_keys":[],"version":"v1"}`)
	response, handled, err := service.actionsResponse(lookup, session)
	if err != nil || !handled {
		t.Fatalf("GetCacheEntryDownloadURL handled=%t error=%v", handled, err)
	}
	found := responseJSON(t, response)
	downloadURL, ok := found["signed_download_url"].(string)
	if !ok || downloadURL == "" {
		t.Fatalf("GetCacheEntryDownloadURL response = %v", found)
	}
	download := actionsRequestForTest(t, http.MethodGet, downloadURL, "")
	download.Header.Set("Range", "bytes=0-3")
	download.Header.Set("X-Ms-Range", "bytes=4-7")
	response, handled, err = service.actionsResponse(download, session)
	if err != nil || !handled || response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("conflicting ranges handled=%t status=%v error=%v", handled, response, err)
	}
	response.Body.Close()
}

func TestActionsCacheAcceptsAzureStagedBlocksInDeclaredOrder(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"blocks","version":"v2"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}
	parsed, err := url.Parse(uploadURL)
	if err != nil {
		t.Fatalf("parse upload URL: %v", err)
	}
	for blockID, body := range map[string]string{"YmxvY2stMQ==": "one", "YmxvY2stMg==": "two"} {
		query := parsed.Query()
		query.Set("comp", "block")
		query.Set("blockid", blockID)
		parsed.RawQuery = query.Encode()
		request := actionsRequestForTest(t, http.MethodPut, parsed.String(), body)
		response, _, err := service.actionsResponse(request, session)
		if err != nil || response.StatusCode != http.StatusCreated {
			t.Fatalf("upload block %s: status=%v error=%v", blockID, response, err)
		}
		response.Body.Close()
	}
	query := parsed.Query()
	query.Set("comp", "blocklist")
	query.Del("blockid")
	parsed.RawQuery = query.Encode()
	request := actionsRequestForTest(t, http.MethodPut, parsed.String(),
		`<?xml version="1.0" encoding="utf-8"?><BlockList><Uncommitted>YmxvY2stMg==</Uncommitted><Latest>YmxvY2stMQ==</Latest></BlockList>`)
	response, _, err = service.actionsResponse(request, session)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("commit block list: status=%v error=%v", response, err)
	}
	response.Body.Close()

	session.mu.Lock()
	var archive *actionsArchive
	for _, candidate := range session.actions {
		archive = candidate
	}
	session.mu.Unlock()
	body, err := os.ReadFile(service.actionsArchivePath(session, archive))
	if err != nil || !bytes.Equal(body, []byte("twoone")) {
		t.Fatalf("assembled archive = %q, error=%v", body, err)
	}
}

func TestActionsStagedUploadBackingCoversTheArchiveBoundary(t *testing.T) {
	t.Parallel()

	stagedAndAssembled := 2 * actionsArchiveLimit
	if actionsVolumeSize-stagedAndAssembled < 1<<30 {
		t.Fatalf("Actions volume = %d bytes; a boundary staged upload needs %d bytes plus filesystem headroom",
			actionsVolumeSize, stagedAndAssembled)
	}
}

func TestActionsCopyStopsAfterItsContextIsCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	reader := &cancelAfterRead{cancel: cancel}
	_, err := copyActionsData(ctx, io.Discard, reader, actionsArchiveLimit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy error = %v; want context cancellation", err)
	}
}

func TestCanceledActionsUploadClosesWithoutSyncing(t *testing.T) {
	t.Parallel()

	file := &syncCloseRecorder{}
	err := finishActionsFile(t.Context(), file, context.Canceled)
	if !errors.Is(err, context.Canceled) || file.syncs != 0 || file.closes != 1 {
		t.Fatalf("finish error=%v syncs=%d closes=%d; want canceled, 0, 1",
			err, file.syncs, file.closes)
	}
}

func TestActionsBlobLockStopsAfterItsContextIsCanceled(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"locked","version":"v1"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}

	session.mu.Lock()
	var archive *actionsArchive
	for _, candidate := range session.actions {
		archive = candidate
	}
	session.mu.Unlock()
	archive.mu.Lock()
	locked := true
	defer func() {
		if locked {
			archive.mu.Unlock()
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL,
		strings.NewReader("body"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := service.serveActionsBlob(request, session)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ctx.Err()) {
			t.Fatalf("serve error = %v; want %v", err, ctx.Err())
		}
	case <-time.After(200 * time.Millisecond):
		archive.mu.Unlock()
		locked = false
		<-done
		t.Fatal("blob request remained blocked on an archive lock after cancellation")
	}
}

func TestActionsCleanupStopsAfterItsContextIsCanceled(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"cleanup-locked","version":"v1"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	response.Body.Close()
	session.mu.Lock()
	var archive *actionsArchive
	for _, candidate := range session.actions {
		archive = candidate
	}
	session.mu.Unlock()
	archive.mu.Lock()
	defer archive.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := service.cleanupSession(ctx, session, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup error = %v; want context deadline while the archive is busy", err)
	}
}

func TestPendingActionsArchiveCustodySurvivesANodeRestart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.actionIO = &fakeActionsVolumeManager{}
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	credentials, err := service.PrepareScoped("billet-restart", CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	service.mu.Lock()
	session := service.byToken[credentials.Token]
	service.mu.Unlock()
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"pending","version":"v1"}`)
	response, handled, err := service.actionsResponse(create, session)
	if err != nil || !handled {
		t.Fatalf("CreateCacheEntry handled=%t error=%v", handled, err)
	}
	response.Body.Close()

	restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("restart NewCacheService: %v", err)
	}
	restarted.actionIO = &fakeActionsVolumeManager{}
	if err := restarted.ReconcileInventory(t.Context(), nil); err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}
	if storage.discarded != 1 {
		t.Fatalf("discarded archive volumes = %d, want 1", storage.discarded)
	}
	entries, err := os.ReadDir(filepath.Join(root, cacheSessionDirectory))
	if err != nil {
		t.Fatalf("read custody directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("restart cleanup left custody records %v", entries)
	}
}

func TestActionsArchiveCustodyIsDurableBeforeTheHostMount(t *testing.T) {
	t.Parallel()

	service, _, session, volumes := testActionsService(t)
	volumes.mountNew = func() error {
		body, err := os.ReadFile(filepath.Join(service.stateDir, session.token+".json"))
		if err != nil {
			return fmt.Errorf("read custody before mount: %w", err)
		}
		var record durableCacheSession
		if err := json.Unmarshal(body, &record); err != nil {
			return fmt.Errorf("decode custody before mount: %w", err)
		}
		if len(record.Actions) != 1 {
			return fmt.Errorf("custody before mount has %d Actions archives", len(record.Actions))
		}

		return nil
	}
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath,
		`{"key":"durable-before-mount","version":"v1"}`)
	response, handled, err := service.actionsResponse(create, session)
	if err != nil || !handled {
		t.Fatalf("CreateCacheEntry handled=%t error=%v", handled, err)
	}
	response.Body.Close()
}

func TestActionsCacheKeysCannotCrossRepositoryOrWorkflowScopes(t *testing.T) {
	t.Parallel()

	service := &CacheService{namespace: "deployment/site"}
	base := &cacheSession{
		owner: "acme", repository: "api",
		workflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	}
	baseKey := service.actionsStoreKey(base, "linux-npm", "v1")
	for name, other := range map[string]*cacheSession{
		"repository": {
			owner: "acme", repository: "web",
			workflowRef: "acme/web/.github/workflows/ci.yml@refs/heads/main",
		},
		"workflow ref": {
			owner: "acme", repository: "api",
			workflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/release",
		},
	} {
		if otherKey := service.actionsStoreKey(other, "linux-npm", "v1"); otherKey == baseKey {
			t.Errorf("%s scope produced the same durable key %q", name, baseKey)
		}
	}
}

func TestActionsCacheKillSwitchAppliesToAnAlreadyIssuedUploadURL(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"disabled","version":"v1"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return false, nil
	}))
	upstreamRequests := 0
	service.actions.upstream = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamRequests++

		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r,
		}, nil
	})
	upload := actionsRequestForTest(t, http.MethodPut, uploadURL, "must-not-be-local")
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, err = service.actions.roundTrip(upload, session)
	if err != nil {
		t.Fatalf("disabled upload: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || upstreamRequests != 0 {
		t.Fatalf("disabled reserved upload status=%d upstream=%d; want local failure",
			response.StatusCode, upstreamRequests)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for _, archive := range session.actions {
		if _, err := os.Stat(service.actionsArchivePath(session, archive)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disabled upload wrote a local archive: %v", err)
		}
	}
}

func TestActionsFinalizeFailureNeverFallsThroughToGitHub(t *testing.T) {
	t.Parallel()

	service, _, session, volumes := testActionsService(t)
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"finalize-local","version":"v1"}`)
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
	volumes.trimErr = errors.New("trim failed")
	upstreamRequests := 0
	service.actions.upstream = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamRequests++

		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r,
		}, nil
	})
	finalize := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"finalize-local","version":"v1","size_bytes":"12"}`)
	response, err = service.actions.roundTrip(finalize, session)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || upstreamRequests != 0 || volumes.trimmed != 1 {
		t.Fatalf("failed finalization status=%d upstream=%d trims=%d; want local failure",
			response.StatusCode, upstreamRequests, volumes.trimmed)
	}
}

func TestActionsFinalizeRechecksThePointerAfterAConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		conflictCurrent  string
		wantOK           bool
		wantCurrent      string
		wantPublishes    int
		wantAcquisitions int
		wantWriterIDs    []string
		wantFences       []storecontract.FencingToken
	}{
		{
			name: "retry after an absent pointer", wantOK: true, wantCurrent: "next",
			wantPublishes: 2, wantAcquisitions: 2,
			wantWriterIDs: []string{"writer-1", "writer-2"},
			wantFences:    []storecontract.FencingToken{1, 2},
		},
		{
			name: "recognize an already-published candidate", conflictCurrent: "next",
			wantOK: true, wantCurrent: "next", wantPublishes: 1, wantAcquisitions: 1,
			wantWriterIDs: []string{"writer-1"}, wantFences: []storecontract.FencingToken{1},
		},
		{
			name: "reject a different published generation", conflictCurrent: "other",
			wantCurrent: "other", wantPublishes: 1, wantAcquisitions: 1,
			wantWriterIDs: []string{"writer-1"}, wantFences: []storecontract.FencingToken{1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			service, storage, session, _ := testActionsService(t)
			create := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsCreatePath,
				`{"key":"conflict","version":"v1"}`)
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
			storage.publishConflicts = 1
			storage.conflictCurrent = test.conflictCurrent
			finalize := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsFinalizePath,
				`{"key":"conflict","version":"v1","size_bytes":"12"}`)
			response, _, err = service.actionsResponse(finalize, session)
			if err != nil {
				t.Fatalf("FinalizeCacheEntryUpload: %v", err)
			}
			finalized := responseJSON(t, response)
			if finalized["ok"] != test.wantOK || storage.published != test.wantPublishes ||
				storage.current != test.wantCurrent ||
				storage.writerAcquisitions != test.wantAcquisitions ||
				!slices.Equal(storage.publishedWriterIDs, test.wantWriterIDs) ||
				!slices.Equal(storage.publishedFences, test.wantFences) {
				t.Fatalf("finalization=%v published=%d current=%q acquisitions=%d writers=%v fences=%v",
					finalized, storage.published, storage.current, storage.writerAcquisitions,
					storage.publishedWriterIDs, storage.publishedFences)
			}
		})
	}
}

func TestActionsFinalizePersistsFreshAuthorityAcrossRestartAfterRepeatedConflicts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storage := &fakeCacheStore{publishConflicts: 2}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.actionIO = &fakeActionsVolumeManager{}
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	credentials, err := service.PrepareScoped("billet-conflicts", CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	service.mu.Lock()
	session := service.byToken[credentials.Token]
	service.mu.Unlock()
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath,
		`{"key":"repeated-conflict","version":"v1"}`)
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
	finalizeBody := `{"key":"repeated-conflict","version":"v1","size_bytes":"12"}`
	finalize := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath, finalizeBody)
	response, handled, err := service.actionsResponse(finalize, session)
	if response != nil {
		response.Body.Close()
	}
	if !handled || !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("first finalize handled=%t error=%v, want local conflict", handled, err)
	}
	if storage.writerAcquisitions != 3 ||
		!slices.Equal(storage.publishedWriterIDs, []string{"writer-1", "writer-2"}) ||
		!slices.Equal(storage.publishedFences, []storecontract.FencingToken{1, 2}) {
		t.Fatalf("first finalize acquisitions=%d writers=%v fences=%v",
			storage.writerAcquisitions, storage.publishedWriterIDs, storage.publishedFences)
	}

	restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("restart NewCacheService: %v", err)
	}
	restarted.actionIO = &fakeActionsVolumeManager{}
	restarted.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	restarted.mu.Lock()
	restartedSession := restarted.byToken[credentials.Token]
	restarted.mu.Unlock()
	if restartedSession == nil {
		t.Fatal("restart did not recover the cache session")
	}
	restartedSession.mu.Lock()
	receipt := restartedSession.receipts[actionsReceiptID("repeated-conflict", "v1")]
	if receipt == nil {
		restartedSession.mu.Unlock()
		t.Fatal("restart did not recover the finalization receipt")
	}
	complete, writerID, fence := receipt.Complete, receipt.Lease.ID, receipt.Fence
	restartedSession.mu.Unlock()
	if complete || writerID != "writer-3" || fence != 3 {
		t.Fatalf("recovered receipt complete=%t writer=%q fence=%d",
			complete, writerID, fence)
	}

	retry := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath, finalizeBody)
	response, handled, err = restarted.actionsResponse(retry, restartedSession)
	if err != nil || !handled {
		t.Fatalf("retry finalize handled=%t error=%v", handled, err)
	}
	retried := responseJSON(t, response)
	if retried["ok"] != true || storage.current != "next" ||
		storage.writerAcquisitions != 3 ||
		!slices.Equal(storage.publishedWriterIDs,
			[]string{"writer-1", "writer-2", "writer-3"}) ||
		!slices.Equal(storage.publishedFences, []storecontract.FencingToken{1, 2, 3}) {
		t.Fatalf("retry=%v current=%q acquisitions=%d writers=%v fences=%v",
			retried, storage.current, storage.writerAcquisitions,
			storage.publishedWriterIDs, storage.publishedFences)
	}
}

func TestActionsFinalizeReceiptSurvivesRestartAndPolicyOutage(t *testing.T) {
	t.Parallel()

	for _, initialFailure := range []bool{false, true} {
		name := "response lost after success"
		if initialFailure {
			name = "CAS failure"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			storage := &fakeCacheStore{}
			service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment",
				root, storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("NewCacheService: %v", err)
			}
			service.actionIO = &fakeActionsVolumeManager{}
			service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
				return true, nil
			}))
			credentials, err := service.PrepareScoped("billet-receipt", CacheSessionScope{
				Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
				WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
			})
			if err != nil {
				t.Fatalf("PrepareScoped: %v", err)
			}
			service.mu.Lock()
			session := service.byToken[credentials.Token]
			service.mu.Unlock()
			create := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsCreatePath,
				`{"key":"durable-receipt","version":"v1"}`)
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
			if initialFailure {
				storage.publishErr = errors.New("publication unavailable")
			}
			finalizeBody := `{"key":"durable-receipt","version":"v1","size_bytes":"12"}`
			finalize := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsFinalizePath, finalizeBody)
			response, err = service.actions.roundTrip(finalize, session)
			if err != nil {
				t.Fatalf("initial finalize: %v", err)
			}
			initialStatus := response.StatusCode
			response.Body.Close()
			if initialFailure && initialStatus != http.StatusBadGateway ||
				!initialFailure && initialStatus != http.StatusOK {
				t.Fatalf("initial finalize status = %d", initialStatus)
			}
			if initialFailure {
				duplicate := actionsRequestForTest(t, http.MethodPost,
					"https://"+actionsResultsHost+actionsCreatePath,
					`{"key":"durable-receipt","version":"v1"}`)
				response, _, err = service.actionsResponse(duplicate, session)
				if err != nil {
					t.Fatalf("duplicate CreateCacheEntry: %v", err)
				}
				duplicated := responseJSON(t, response)
				if duplicated["ok"] != false || storage.created != 1 {
					t.Fatalf("duplicate reservation=%v created=%d", duplicated, storage.created)
				}
			}

			storage.publishErr = nil
			restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment",
				root, storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("restart NewCacheService: %v", err)
			}
			restarted.actionIO = &fakeActionsVolumeManager{}
			restarted.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
				return false, errors.New("policy unavailable")
			}))
			restarted.mu.Lock()
			restartedSession := restarted.byToken[credentials.Token]
			restarted.mu.Unlock()
			upstreamRequests := 0
			restarted.actions.upstream = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				upstreamRequests++

				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r,
				}, nil
			})
			retry := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsFinalizePath, finalizeBody)
			response, err = restarted.actions.roundTrip(retry, restartedSession)
			if err != nil {
				t.Fatalf("retry finalize: %v", err)
			}
			if initialFailure {
				if response.StatusCode != http.StatusBadGateway || upstreamRequests != 0 ||
					storage.current != "" {
					t.Fatalf("policy-outage retry status=%d upstream=%d current=%q",
						response.StatusCode, upstreamRequests, storage.current)
				}
				response.Body.Close()
				restarted.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
					return true, nil
				}))
				retry = actionsRequestForTest(t, http.MethodPost,
					"https://"+actionsResultsHost+actionsFinalizePath, finalizeBody)
				response, err = restarted.actions.roundTrip(retry, restartedSession)
				if err != nil {
					if response != nil {
						response.Body.Close()
					}
					t.Fatalf("allowed retry finalize: %v", err)
				}
			}
			retried := responseJSON(t, response)
			if retried["ok"] != true || upstreamRequests != 0 || storage.current != "next" {
				t.Fatalf("retry=%v upstream=%d current=%q", retried, upstreamRequests, storage.current)
			}
		})
	}
}

func TestActionsFinalizeRemovesInterruptedStagingAfterRestart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.actionIO = &fakeActionsVolumeManager{}
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	credentials, err := service.PrepareScoped("billet-staging", CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	service.mu.Lock()
	session := service.byToken[credentials.Token]
	service.mu.Unlock()
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"staging","version":"v1"}`)
	response, _, err := service.actionsResponse(create, session)
	if err != nil {
		t.Fatalf("CreateCacheEntry: %v", err)
	}
	created := responseJSON(t, response)
	uploadURL, ok := created["signed_upload_url"].(string)
	if !ok || uploadURL == "" {
		t.Fatalf("CreateCacheEntry response = %v", created)
	}
	session.mu.Lock()
	var archive *actionsArchive
	for _, candidate := range session.actions {
		archive = candidate
	}
	session.mu.Unlock()
	for _, name := range []string{".upload-interrupted", ".blocks-interrupted"} {
		if err := os.WriteFile(filepath.Join(service.actionsStagingPath(session, archive), name),
			[]byte("orphaned blocks"), 0o600); err != nil {
			t.Fatalf("write interrupted staging file: %v", err)
		}
	}
	upload := actionsRequestForTest(t, http.MethodPut, uploadURL, "archive-body")
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, _, err = service.actionsResponse(upload, session)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	response.Body.Close()

	restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("restart NewCacheService: %v", err)
	}
	volumes := &fakeActionsVolumeManager{trimCheck: func(target string) error {
		if _, err := os.Stat(filepath.Join(target, "staging")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("staging still exists at trim: %w", err)
		}

		return nil
	}}
	restarted.actionIO = volumes
	restarted.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))
	restarted.mu.Lock()
	restartedSession := restarted.byToken[credentials.Token]
	restarted.mu.Unlock()
	finalize := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"staging","version":"v1","size_bytes":"12"}`)
	response, _, err = restarted.actionsResponse(finalize, restartedSession)
	if err != nil {
		t.Fatalf("finalize after restart: %v", err)
	}
	finalized := responseJSON(t, response)
	if finalized["ok"] != true || volumes.trimmed != 1 {
		t.Fatalf("finalize=%v trims=%d", finalized, volumes.trimmed)
	}
}

func TestActionsCacheArchiveCountIsBoundedBeforeStorageAllocation(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	session.mu.Lock()
	for index := range actionsArchiveCount {
		id := fmt.Sprintf("%064x", index)
		session.actions[id] = &actionsArchive{ID: id}
	}
	session.mu.Unlock()
	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"overflow","version":"v1"}`)
	response, handled, err := service.actionsResponse(create, session)
	if response != nil {
		response.Body.Close()
	}
	if response != nil || !handled || err == nil {
		t.Fatalf("overflow response=%v handled=%t error=%v", response, handled, err)
	}
	if len(storage.keys) != 0 {
		t.Fatalf("archive overflow allocated storage keys %v", storage.keys)
	}
}

func TestAnEmptyActionsArchiveRefusesAByteRange(t *testing.T) {
	t.Parallel()

	if _, _, _, err := actionsByteRange("bytes=0-", 0); err == nil {
		t.Fatal("an empty archive accepted a byte range with no satisfiable byte")
	}
}

func TestOnlyTheThreeExactCacheServiceMethodsAreLocal(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	policyCalls := 0
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		policyCalls++

		return false, errors.New("policy must not be queried")
	}))
	for _, target := range []string{
		"https://" + actionsResultsHost + "/twirp/github.actions.results.api.v1.CacheService/DeleteCacheEntry",
		"https://" + actionsResultsHost + actionsCreatePath + "/extra",
		"https://" + actionsResultsHost + "/twirp/github.actions.results.api.v1.CacheService/%43reateCacheEntry",
		"https://" + actionsResultsHost + "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact",
	} {
		request := actionsRequestForTest(t, http.MethodPost, target, `{}`)
		response, handled, err := service.actionsResponse(request, session)
		if response != nil {
			response.Body.Close()
		}
		if handled || err != nil {
			t.Errorf("%s was handled locally (handled=%t error=%v)", target, handled, err)
		}
	}
	if policyCalls != 0 {
		t.Fatalf("non-cache requests made %d cache policy calls", policyCalls)
	}
}

func TestBuildKitAndUnknownCacheClientsRemainOnGitHub(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	policyCalls := 0
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		policyCalls++

		return false, errors.New("policy must not be queried")
	}))
	for _, userAgent := range []string{"buildkit/v0.25", "go-actions-cache/1.0", ""} {
		request := actionsRequestForTest(t, http.MethodPost,
			"https://"+actionsResultsHost+actionsCreatePath,
			`{"key":"upstream","version":"v1"}`)
		request.Header.Set("User-Agent", userAgent)
		response, handled, err := service.actionsResponse(request, session)
		if response != nil {
			response.Body.Close()
		}
		if handled || err != nil {
			t.Errorf("user agent %q was handled locally (handled=%t error=%v)",
				userAgent, handled, err)
		}
	}
	if policyCalls != 0 {
		t.Fatalf("non-toolkit requests made %d cache policy calls", policyCalls)
	}
}

func TestActionsCachePolicyFailsOpenWithoutAllocatingStorage(t *testing.T) {
	t.Parallel()

	for name, policy := range map[string]actionsPolicyFunc{
		"blocked": func(context.Context, string, string) (bool, error) {
			return false, nil
		},
		"unavailable": func(context.Context, string, string) (bool, error) {
			return false, errors.New("control plane unavailable")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, storage, session, _ := testActionsService(t)
			service.SetActionsPolicy(policy)
			request := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsCreatePath,
				`{"key":"blocked","version":"v1"}`)
			response, handled, err := service.actionsResponse(request, session)
			if response != nil {
				response.Body.Close()
			}
			if response != nil || handled || err != nil {
				t.Fatalf("policy response=%v handled=%t error=%v; want upstream passthrough",
					response, handled, err)
			}
			if len(storage.keys) != 0 {
				t.Fatalf("blocked request allocated storage keys %v", storage.keys)
			}
		})
	}
}

func TestUntrustedActionsCacheTrafficRemainsOnGitHub(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	session.trust = provider.TrustUntrusted
	request := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath,
		`{"key":"fork","version":"v1"}`)
	response, handled, err := service.actionsResponse(request, session)
	if response != nil {
		response.Body.Close()
	}
	if response != nil || handled || err != nil {
		t.Fatalf("untrusted response=%v handled=%t error=%v; want GitHub passthrough",
			response, handled, err)
	}
	if len(storage.keys) != 0 {
		t.Fatalf("untrusted request allocated storage keys %v", storage.keys)
	}
}
