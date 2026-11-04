package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// objectStore is a site store that keeps whole objects, over the volumes of
// fakeCacheStore, whose counters then say that no volume was touched.
type objectStore struct {
	fakeCacheStore

	mu       sync.Mutex
	archives map[string]objectArchive
	staged   map[string]map[string][]byte
	blobs    map[string]map[storecontract.BlobRef][]byte
	uses     map[string][]storecontract.BlobRef
	aborted  []string
	// commitErr fails the next commits, and putErr every PutBlob.
	commitErr error
	putErr    error
	puts      []storecontract.BlobRef
	listings  int
	reads     int
}

type objectArchive struct {
	body   []byte
	upload string
}

func newObjectStore() *objectStore {
	return &objectStore{
		archives: map[string]objectArchive{}, staged: map[string]map[string][]byte{},
		blobs: map[string]map[storecontract.BlobRef][]byte{}, uses: map[string][]storecontract.BlobRef{},
	}
}

func (s *objectStore) ArchiveExists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.archives[key]

	return ok, nil
}

func (s *objectStore) MatchArchive(
	_ context.Context, exact string, restorePrefixes []string,
) (storecontract.Archive, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if archive, ok := s.archives[exact]; ok {
		return storecontract.Archive{Key: exact, Size: int64(len(archive.body)), Modified: time.Unix(1000, 0)}, nil
	}
	for _, prefix := range restorePrefixes {
		for key, archive := range s.archives {
			if strings.HasPrefix(key, prefix) {
				return storecontract.Archive{Key: key, Size: int64(len(archive.body)), Modified: time.Unix(1000, 0)}, nil
			}
		}
	}

	return storecontract.Archive{}, storecontract.ErrMiss
}

func (s *objectStore) StageArchive(
	_ context.Context, upload, block string, body io.Reader, size int64,
) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("a block's length was not what it declared")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staged[upload] == nil {
		s.staged[upload] = map[string][]byte{}
	}
	s.staged[upload][block] = data

	return nil
}

func (s *objectStore) CommitArchive(
	_ context.Context, key, upload string, blocks []string, size int64,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitErr != nil {
		err := s.commitErr
		s.commitErr = nil

		return false, err
	}
	if existing, ok := s.archives[key]; ok {
		return existing.upload == upload, nil
	}
	var body []byte
	for _, block := range blocks {
		part, ok := s.staged[upload][block]
		if !ok {
			return false, storecontract.ErrArchiveSize
		}
		body = append(body, part...)
	}
	if int64(len(body)) != size {
		return false, storecontract.ErrArchiveSize
	}
	s.archives[key] = objectArchive{body: body, upload: upload}
	delete(s.staged, upload)

	return true, nil
}

func (s *objectStore) AbortArchive(_ context.Context, upload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.staged, upload)
	s.aborted = append(s.aborted, upload)

	return nil
}

func (s *objectStore) ReadArchive(
	_ context.Context, archive storecontract.Archive, start, length int64,
) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.archives[archive.Key]
	if !ok {
		return nil, storecontract.ErrMiss
	}

	return io.NopCloser(bytes.NewReader(stored.body[start : start+length])), nil
}

func (s *objectStore) ListBlobs(_ context.Context, key string) (map[storecontract.BlobRef]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings++
	refs := make(map[storecontract.BlobRef]struct{})
	for ref := range s.blobs[key] {
		refs[ref] = struct{}{}
	}

	return refs, nil
}

func (s *objectStore) ReadBlob(
	_ context.Context, key string, ref storecontract.BlobRef,
) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	body, ok := s.blobs[key][ref]
	if !ok {
		return nil, storecontract.ErrMiss
	}

	return io.NopCloser(bytes.NewReader(body)), nil
}

func (s *objectStore) PutBlob(
	_ context.Context, key string, ref storecontract.BlobRef, body io.Reader, _ int64,
) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return s.putErr
	}
	if s.blobs[key] == nil {
		s.blobs[key] = map[storecontract.BlobRef][]byte{}
	}
	if _, exists := s.blobs[key][ref]; !exists {
		s.blobs[key][ref] = data
	}
	s.puts = append(s.puts, ref)

	return nil
}

func (s *objectStore) RecordBlobUse(
	_ context.Context, key, _ string, used []storecontract.BlobRef,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uses[key] = append(s.uses[key], used...)

	return nil
}

func objectService(t *testing.T) (*CacheService, *objectStore, *deviceVolumes) {
	t.Helper()
	storage := newObjectStore()
	service, err := NewCacheService("https://10.0.0.1:9443", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	t.Cleanup(service.git.close)
	service.git.api = "http://127.0.0.1:1"
	volumes := newDeviceVolumes(t)
	service.actionIO = volumes
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))

	return service, storage, volumes
}

func objectActionsSession(t *testing.T, service *CacheService, instance string) *cacheSession {
	t.Helper()
	credentials, err := service.PrepareScoped(instance, CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()

	return service.byToken[credentials.Token]
}

func actionsCall(
	t *testing.T, service *CacheService, session *cacheSession, path, body string,
) map[string]any {
	t.Helper()
	request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+path, body)
	response, handled, err := service.actionsResponse(request, session)
	if err != nil || !handled {
		t.Fatalf("%s handled=%t error=%v", path, handled, err)
	}

	return responseJSON(t, response)
}

// putBlock uploads one block, with the length Azure's client always sends.
func blobLeg(
	t *testing.T, service *CacheService, session *cacheSession, target, body string,
) *http.Response {
	t.Helper()
	request := actionsRequestForTest(t, http.MethodPut, target, body)
	request.ContentLength = int64(len(body))
	response, handled, err := service.actionsResponse(request, session)
	if err != nil || !handled {
		t.Fatalf("blob leg handled=%t error=%v", handled, err)
	}

	return response
}

func stagedUpload(
	t *testing.T, service *CacheService, session *cacheSession, key string, blocks []string, order []string,
) {
	t.Helper()
	created := actionsCall(t, service, session, actionsCreatePath, `{"key":"`+key+`","version":"v1"}`)
	uploadURL, _ := created["signed_upload_url"].(string)
	if created["ok"] != true || uploadURL == "" {
		t.Fatalf("CreateCacheEntry = %v", created)
	}
	parsed, err := url.Parse(uploadURL)
	if err != nil {
		t.Fatal(err)
	}
	list := "<BlockList>"
	for i, body := range blocks {
		query := parsed.Query()
		query.Set("comp", "block")
		query.Set("blockid", order[i])
		parsed.RawQuery = query.Encode()
		response := blobLeg(t, service, session, parsed.String(), body)
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("block %s = %d", order[i], response.StatusCode)
		}
		response.Body.Close()
	}
	for _, id := range slices.Sorted(slices.Values(order)) {
		list += "<Latest>" + id + "</Latest>"
	}
	list += "</BlockList>"
	query := parsed.Query()
	query.Set("comp", "blocklist")
	query.Del("blockid")
	parsed.RawQuery = query.Encode()
	response := blobLeg(t, service, session, parsed.String(), list)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("block list = %d", response.StatusCode)
	}
	response.Body.Close()
}

// ON A STORE OF WHOLE OBJECTS THE THREE CALLS ARE THE SAME AND NOTHING IS MOUNTED: a
// client sees what it sees on Firecracker, and no volume is created, cloned or
// snapshotted for an archive. This is what ADR-023 measured the other shape against.
func TestAnActionsArchiveOnAStoreOfObjectsTouchesNoVolume(t *testing.T) {
	t.Parallel()
	service, storage, volumes := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")

	stagedUpload(t, service, session, "linux-npm-abc", []string{"two", "one"}, []string{"b", "a"})
	finalized := actionsCall(t, service, session, actionsFinalizePath,
		`{"key":"linux-npm-abc","version":"v1","size_bytes":"6"}`)
	if finalized["ok"] != true || finalized["entry_id"] == nil {
		t.Fatalf("FinalizeCacheEntryUpload = %v", finalized)
	}

	found := actionsCall(t, service, session, actionsDownloadPath,
		`{"key":"linux-npm-abc","restore_keys":[],"version":"v1"}`)
	downloadURL, _ := found["signed_download_url"].(string)
	if found["ok"] != true || found["matched_key"] != "linux-npm-abc" || downloadURL == "" {
		t.Fatalf("GetCacheEntryDownloadURL = %v", found)
	}

	// THE LIST'S ORDER, not the order the blocks arrived in.
	for _, read := range []struct {
		rangeHeader, body string
		status            int
	}{
		{"", "onetwo", http.StatusOK},
		{"bytes=2-4", "etw", http.StatusPartialContent},
	} {
		request := actionsRequestForTest(t, http.MethodGet, downloadURL, "")
		if read.rangeHeader != "" {
			request.Header.Set("Range", read.rangeHeader)
		}
		response, handled, err := service.actionsResponse(request, session)
		if err != nil || !handled || response.StatusCode != read.status {
			t.Fatalf("download %q handled=%t response=%v error=%v", read.rangeHeader, handled, response, err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(body) != read.body {
			t.Fatalf("download %q = %q, %v; want %q", read.rangeHeader, body, err, read.body)
		}
		if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(read.body)) {
			t.Fatalf("Content-Length = %q", got)
		}
	}

	if storage.created != 0 || storage.cloned != 0 || storage.snapshots != 0 || len(volumes.targets) != 0 {
		t.Fatalf("an archive touched volumes: created %d, cloned %d, snapshots %d, mounts %v",
			storage.created, storage.cloned, storage.snapshots, volumes.targets)
	}
}

func TestASingleShotUploadIsAnArchiveOfOneBlock(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	created := actionsCall(t, service, session, actionsCreatePath, `{"key":"small","version":"v1"}`)
	uploadURL, _ := created["signed_upload_url"].(string)

	request := actionsRequestForTest(t, http.MethodPut, uploadURL, "archive-body")
	request.ContentLength = 12
	request.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	response, handled, err := service.actionsResponse(request, session)
	if err != nil || !handled || response.StatusCode != http.StatusCreated {
		t.Fatalf("upload handled=%t response=%v error=%v", handled, response, err)
	}
	response.Body.Close()
	finalized := actionsCall(t, service, session, actionsFinalizePath, `{"key":"small","version":"v1","size_bytes":"12"}`)
	if finalized["ok"] != true {
		t.Fatalf("FinalizeCacheEntryUpload = %v", finalized)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	var bodies []string
	for _, archive := range storage.archives {
		bodies = append(bodies, string(archive.body))
	}
	if !slices.Equal(bodies, []string{"archive-body"}) {
		t.Fatalf("the store holds %q", bodies)
	}
}

// S3 will not take a body of unknown length, and spooling one to the node's disk is
// what this shape exists to avoid. Azure's client always says.
func TestABlockOfUnknownLengthIsRefusedRatherThanSpooled(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	created := actionsCall(t, service, session, actionsCreatePath, `{"key":"unknown","version":"v1"}`)
	uploadURL, _ := created["signed_upload_url"].(string)

	request := actionsRequestForTest(t, http.MethodPut, uploadURL+"&comp=block&blockid=YQ==", "body")
	request.ContentLength = -1
	response, handled, err := service.actionsResponse(request, session)
	if err != nil || !handled || response.StatusCode != http.StatusLengthRequired {
		t.Fatalf("handled=%t response=%v error=%v; want 411", handled, response, err)
	}
	response.Body.Close()
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.staged) != 0 {
		t.Fatal("a block of unknown length was staged")
	}
}

func TestASaveThatLostToAnotherJobIsToldSo(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	first := objectActionsSession(t, service, "billet-first")
	second := objectActionsSession(t, service, "billet-second")
	stagedUpload(t, service, first, "shared", []string{"mine"}, []string{"a"})
	stagedUpload(t, service, second, "shared", []string{"mine"}, []string{"a"})

	if got := actionsCall(t, service, first, actionsFinalizePath,
		`{"key":"shared","version":"v1","size_bytes":"4"}`); got["ok"] != true {
		t.Fatalf("the first finalize = %v", got)
	}
	got := actionsCall(t, service, second, actionsFinalizePath,
		`{"key":"shared","version":"v1","size_bytes":"4"}`)
	if got["ok"] != false {
		t.Fatalf("the second finalize = %v; it lost", got)
	}
}

func TestAFinalizationThatDoesNotAddUpPublishesNothing(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "wrong", []string{"body"}, []string{"a"})

	got := actionsCall(t, service, session, actionsFinalizePath,
		`{"key":"wrong","version":"v1","size_bytes":"5"}`)
	if got["ok"] != false {
		t.Fatalf("FinalizeCacheEntryUpload = %v", got)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.archives) != 0 {
		t.Fatalf("published %v", storage.archives)
	}
}

// A STORE FAILURE AT THE COMMIT LEAVES THE RECEIPT, and the client's retry finishes
// it: the upload is billet's by then, so it can never fall through to GitHub.
func TestACommitThatFailedIsFinishedByTheRetriedFinalization(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "retry", []string{"body"}, []string{"a"})

	storage.mu.Lock()
	storage.commitErr = errors.New("s3 went away")
	storage.mu.Unlock()
	finalize := func() (*http.Response, bool, error) {
		request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
			`{"key":"retry","version":"v1","size_bytes":"4"}`)

		return service.actionsResponse(request, session)
	}
	if _, _, err := finalize(); err == nil {
		t.Fatal("a failed commit answered success")
	}
	response, handled, err := finalize()
	if err != nil || !handled {
		t.Fatalf("the retried finalization handled=%t error=%v", handled, err)
	}
	if got := responseJSON(t, response); got["ok"] != true {
		t.Fatalf("the retried finalization = %v", got)
	}
}

func TestAnObjectReceiptSurvivesARestart(t *testing.T) {
	t.Parallel()
	storage := newObjectStore()
	root := t.TempDir()
	service, err := NewCacheService("https://10.0.0.1:9443", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.git.close)
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) { return true, nil }))
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "restart", []string{"body"}, []string{"a"})
	storage.mu.Lock()
	storage.commitErr = errors.New("s3 went away")
	storage.mu.Unlock()
	request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"restart","version":"v1","size_bytes":"4"}`)
	if _, _, err := service.actionsResponse(request, session); err == nil {
		t.Fatal("a failed commit answered success")
	}

	restarted, err := NewCacheService("https://10.0.0.1:9443", "test-deployment", root,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("a restart could not read what the first process recorded: %v", err)
	}
	t.Cleanup(restarted.git.close)
	restarted.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) { return true, nil }))
	restarted.mu.Lock()
	recovered := restarted.byToken[session.token]
	restarted.mu.Unlock()
	if recovered == nil {
		t.Fatal("the session did not survive the restart")
	}
	if got := actionsCall(t, restarted, recovered, actionsFinalizePath,
		`{"key":"restart","version":"v1","size_bytes":"4"}`); got["ok"] != true {
		t.Fatalf("the finalization after the restart = %v", got)
	}
}

func TestARecoveredObjectArchiveIsNotMountedAgain(t *testing.T) {
	t.Parallel()
	service, _, volumes := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "kept", []string{"body"}, []string{"a"})

	service.RestoreMounts(t.Context())
	if len(volumes.targets) != 0 {
		t.Fatalf("recovery mounted %v", volumes.targets)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for _, archive := range session.actions {
		if archive.lost {
			t.Fatal("an archive with nothing to mount was marked lost")
		}
	}
}

func TestAnUploadTheJobNeverFinalizedIsAbortedWhenItEnds(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "abandoned", []string{"body"}, []string{"a"})

	if err := service.Close(t.Context(), "billet-actions"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.aborted) != 1 || len(storage.staged) != 0 {
		t.Fatalf("aborted %v, staged %v", storage.aborted, storage.staged)
	}
}

// THE LIMIT ON A STORE OF VOLUMES IS WHAT A VOLUME COSTS. BuildKit's `mode=max` writes
// one archive per layer, and on a store of objects each costs one entry of a record.
func TestAnObjectStoreTakesMoreArchivesThanAVolumeStoreCould(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	for i := range actionsArchiveCount + 8 {
		created := actionsCall(t, service, session, actionsCreatePath,
			`{"key":"layer-`+strconv.Itoa(i)+`","version":"v1"}`)
		if created["ok"] != true {
			t.Fatalf("reservation %d = %v", i, created)
		}
	}
	session.mu.Lock()
	for i := range actionsObjectArchives {
		session.actions["filler"+strconv.Itoa(i)] = &actionsArchive{}
	}
	session.mu.Unlock()
	request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsCreatePath,
		`{"key":"past-the-ceiling","version":"v1"}`)
	if _, _, err := service.actionsResponse(request, session); err == nil ||
		!strings.Contains(err.Error(), "too many") {
		t.Fatalf("past the ceiling = %v", err)
	}
	session.mu.Lock()
	for i := range actionsObjectArchives {
		delete(session.actions, "filler"+strconv.Itoa(i))
	}
	session.mu.Unlock()
}

func TestTheKillSwitchStillSendsAnObjectStoresCallsToGitHub(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) { return false, nil }))

	request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsCreatePath,
		`{"key":"blocked","version":"v1"}`)
	_, handled, err := service.actionsResponse(request, session)
	if err != nil || handled {
		t.Fatalf("handled=%t error=%v; a disabled cache goes to GitHub", handled, err)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.staged) != 0 || len(session.actions) != 0 {
		t.Fatal("a disabled cache reserved something")
	}
}

// objectCAS opens a session for a lease, named as a real instance of it is.
func objectCAS(
	t *testing.T, service *CacheService, lease string, trust provider.TrustClass,
) string {
	t.Helper()
	credentials, err := service.PrepareScoped(provider.InstanceName(lease), CacheSessionScope{
		Trust: trust, LeaseID: lease, Epoch: 1, Cache: goBazelCache(),
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return credentials.Token
}

func closeAndRetry(t *testing.T, service *CacheService, instance string) {
	t.Helper()
	if err := service.Close(t.Context(), instance); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
}

func casRoot(service *CacheService) string { return filepath.Join(service.rootState, "cas-volumes") }

func TestACompletedJobsEntriesAreUploadedAndNothingIsMounted(t *testing.T) {
	t.Parallel()
	service, storage, volumes := objectService(t)
	instance := provider.InstanceName(scopedLease)
	token := objectCAS(t, service, scopedLease, provider.TrustTrusted)
	body := "go object"
	if code := putObject(t, service, token, body); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if code := casRequest(t, service, token, http.MethodPut, "/v1/cas/go/ac/"+digestOf("action"), "result").Code; code != http.StatusOK {
		t.Fatalf("PUT ac = %d", code)
	}
	if got := casRequest(t, service, token, http.MethodGet, "/v1/cas/go/cas/"+digestOf(body), ""); got.Code != http.StatusOK || got.Body.String() != body {
		t.Fatalf("GET own write = %d %q", got.Code, got.Body.String())
	}
	storage.mu.Lock()
	uploaded := len(storage.puts)
	storage.mu.Unlock()
	if uploaded != 0 {
		t.Fatalf("%d entries went up before the job's completion authorised them", uploaded)
	}

	finish(t, service, instance)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}

	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 2 {
		t.Fatalf("uploaded %v", storage.puts)
	}
	if storage.created != 0 || storage.cloned != 0 || storage.snapshots != 0 || len(volumes.targets) != 0 {
		t.Fatalf("a content-addressed cache touched volumes: %d created, %d cloned, %d snapshots, mounts %v",
			storage.created, storage.cloned, storage.snapshots, volumes.targets)
	}
	for key := range storage.uses {
		if len(storage.uses[key]) != 2 {
			t.Fatalf("the use record names %v", storage.uses[key])
		}
	}
	if len(storage.uses) != 1 {
		t.Fatalf("use records: %v", storage.uses)
	}
	entries, err := os.ReadDir(casRoot(service))
	if err == nil {
		for _, entry := range entries {
			kinds, _ := os.ReadDir(filepath.Join(casRoot(service), entry.Name()))
			if len(kinds) != 0 {
				t.Fatalf("a finished session left %v on the node's disk", kinds)
			}
		}
	}
}

func TestAJobThatNeverCompletedPublishesNothing(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	instance := provider.InstanceName(scopedLease)
	token := objectCAS(t, service, scopedLease, provider.TrustTrusted)
	if code := putObject(t, service, token, "unauthorised"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	closeAndRetry(t, service, instance)
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 0 {
		t.Fatalf("uploaded %v with no completion to authorise it", storage.puts)
	}
}

// WHAT A JOB WROTE REACHES NO OTHER JOB UNTIL IT IS PUBLISHED, which is the whole of
// ADR-013 for this cache: an unauthorised job's entry must not be readable through
// the read cache by the next.
func TestOneJobsUnpublishedWritesReachNoOtherJob(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	writer := objectCAS(t, service, "writer", provider.TrustTrusted)
	reader := objectCAS(t, service, "reader", provider.TrustTrusted)
	body := "unpublished"
	if code := putObject(t, service, writer, body); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if got := casRequest(t, service, reader, http.MethodGet, "/v1/cas/go/cas/"+digestOf(body), ""); got.Code != http.StatusNotFound {
		t.Fatalf("another job read it: %d %q", got.Code, got.Body.String())
	}
}

func TestAPublishedEntryIsReadThroughTheStoreOnceThenFromTheNodesDisk(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	first := provider.InstanceName(scopedLease)
	token := objectCAS(t, service, scopedLease, provider.TrustTrusted)
	body := "published object"
	if code := putObject(t, service, token, body); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, first)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatal(err)
	}

	second := objectCAS(t, service, "second", provider.TrustTrusted)
	for range 3 {
		got := casRequest(t, service, second, http.MethodGet, "/v1/cas/go/cas/"+digestOf(body), "")
		if got.Code != http.StatusOK || got.Body.String() != body {
			t.Fatalf("GET = %d %q", got.Code, got.Body.String())
		}
	}
	storage.mu.Lock()
	reads := storage.reads
	listings := storage.listings
	storage.mu.Unlock()
	if reads != 1 {
		t.Fatalf("the store was read %d times for one entry", reads)
	}
	if listings != 1 {
		t.Fatalf("the store was listed %d times for one job", listings)
	}

	// A MISS COSTS NO REQUEST: the listing already said.
	if got := casRequest(t, service, second, http.MethodGet, "/v1/cas/go/cas/"+digestOf("absent"), ""); got.Code != http.StatusNotFound {
		t.Fatalf("GET absent = %d", got.Code)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.reads != 1 {
		t.Fatalf("a miss cost %d reads", storage.reads-1)
	}
}

// AN OBJECT WHOSE BYTES ARE NOT ITS NAME IS A MISS, whatever the store says it holds:
// a build given it would use wrong output.
func TestAStoredEntryWhoseBytesAreNotItsDigestIsAMiss(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	token := objectCAS(t, service, "reader", provider.TrustTrusted)
	digest := digestOf("what it should be")
	key := service.cacheKeyFor(service.sessionOf(provider.InstanceName("reader")), config.CacheGo, "", "")
	ref := storecontract.BlobRef{Table: storecontract.BlobTableCAS, Digest: digest}
	storage.mu.Lock()
	storage.blobs[key] = map[storecontract.BlobRef][]byte{ref: []byte("something else")}
	storage.mu.Unlock()

	if got := casRequest(t, service, token, http.MethodGet, "/v1/cas/go/cas/"+digest, ""); got.Code != http.StatusNotFound {
		t.Fatalf("GET = %d %q; a corrupt entry was served", got.Code, got.Body.String())
	}
}

// AN UPLOAD THAT STOPPED PARTWAY LEFT ONLY WHOLE ENTRIES, so it is done again rather
// than abandoned, and the session stays open until it has.
func TestAFailedUploadIsRetriedAndHoldsTheSessionOpen(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	instance := provider.InstanceName(scopedLease)
	token := objectCAS(t, service, scopedLease, provider.TrustTrusted)
	if code := putObject(t, service, token, "object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	storage.mu.Lock()
	storage.putErr = errors.New("s3 went away")
	storage.mu.Unlock()
	finish(t, service, instance)
	if err := service.RetryClosed(t.Context()); err == nil {
		t.Fatal("a failed upload reported the session finished")
	}
	if service.sessionOf(instance) == nil {
		t.Fatal("the session was forgotten with its writes unpublished")
	}

	storage.mu.Lock()
	storage.putErr = nil
	storage.mu.Unlock()
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 1 {
		t.Fatalf("uploaded %v", storage.puts)
	}
}

func TestAJobCanAddNoMoreThanItsTiersSize(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	enabled := true
	spec := config.Tier{Cache: &config.TierCache{
		Go: &config.GoCache{Enabled: &enabled, MaxSize: 16},
	}}.EffectiveCache()
	credentials, err := service.PrepareScoped(provider.InstanceName(scopedLease), CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: scopedLease, Epoch: 1, Cache: &spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := putObject(t, service, credentials.Token, "sixteen bytes ok"); code != http.StatusOK {
		t.Fatalf("the first PUT = %d", code)
	}
	if code := putObject(t, service, credentials.Token, "one more"); code != http.StatusInsufficientStorage {
		t.Fatalf("past the size = %d, want 507", code)
	}
}

func TestTheReadCacheIsReapedOldestFirstOnlyPastItsFill(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	root := filepath.Join(service.rootState, casCacheDirectory, "key", "cas", "aa")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	old, recent := filepath.Join(root, "old"), filepath.Join(root, "recent")
	for _, path := range []string{old, recent} {
		if err := os.WriteFile(path, []byte("entry"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, time.Time{}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// BELOW ITS FILL, NOTHING GOES, whatever the disk under this test holds.
	service.casCacheFill = 1.1
	if err := service.ReapCASCache(t.Context()); err != nil {
		t.Fatalf("ReapCASCache: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("an entry was reaped below the fill: %v", err)
	}

	// PAST IT, THE LEAST RECENTLY USED GO FIRST, and only as many as make the room.
	if err := reapCASEntries(t.Context(), filepath.Join(service.rootState, casCacheDirectory), 1); err != nil {
		t.Fatalf("reapCASEntries: %v", err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the older entry = %v, want it gone", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("the recent entry was reaped too: %v", err)
	}

	// NOTHING THERE IS NOT A FAILURE: the node has served no such cache yet.
	empty, _, _ := objectService(t)
	if err := empty.ReapCASCache(t.Context()); err != nil {
		t.Fatalf("ReapCASCache on no cache: %v", err)
	}
}
