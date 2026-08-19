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
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

type fakeActionsVolumeManager struct {
	published []byte
	mountNew  func() error
}

type actionsPolicyFunc func(context.Context, string, string) (bool, error)

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
		string(volumes.published) != "archive-body" {
		t.Fatalf("finalization=%v snapshots=%d published=%d archive=%q",
			finalized, storage.snapshots, storage.published, volumes.published)
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
	upload := actionsRequestForTest(t, http.MethodPut, uploadURL, "must-not-be-local")
	upload.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, handled, err := service.actionsResponse(upload, session)
	if response != nil {
		response.Body.Close()
	}
	if response != nil || handled || err != nil {
		t.Fatalf("disabled upload response=%v handled=%t error=%v; want upstream passthrough",
			response, handled, err)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for _, archive := range session.actions {
		if _, err := os.Stat(service.actionsArchivePath(session, archive)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disabled upload wrote a local archive: %v", err)
		}
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
