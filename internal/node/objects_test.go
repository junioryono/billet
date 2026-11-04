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
	"github.com/junioryono/billet/internal/node/reapi"
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
	// useIDs is the id of each record, in the order they were written.
	useIDs  []string
	aborted []string
	// commitErr fails the next commits, abortErr every abort, and putErr every PutBlob.
	commitErr error
	abortErr  error
	putErr    error
	// commitGate, when set, holds every commit until it is closed.
	commitGate chan struct{}
	// putTogether, when set, holds each PutBlob until that many are in flight.
	putTogether int
	putArrived  chan struct{}
	puts        []storecontract.BlobRef
	listings    int
	reads       int
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
	gate := s.commitGate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
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
	if s.abortErr != nil {
		return s.abortErr
	}
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

func (s *objectStore) ListBlobs(_ context.Context, key string) (map[storecontract.BlobRef]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings++
	refs := make(map[storecontract.BlobRef]int64)
	for ref, body := range s.blobs[key] {
		refs[ref] = int64(len(body))
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
	_ context.Context, key string, ref storecontract.BlobRef, body io.Reader, size int64,
) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	// S3 STORES WHAT THE LENGTH SAYS, whatever the reader held.
	if int64(len(data)) != size {
		return errors.New("an entry's length was not what it declared")
	}
	s.mu.Lock()
	together, arrived := s.putTogether, s.putArrived
	s.mu.Unlock()
	if together > 0 {
		arrived <- struct{}{}
		guard := time.NewTimer(10 * time.Second)
		defer guard.Stop()
		for {
			s.mu.Lock()
			reached := len(arrived) >= together
			s.mu.Unlock()
			if reached {
				break
			}
			select {
			case <-guard.C:
				return errors.New("no other upload ran beside this one")
			case <-time.After(time.Millisecond):
			}
		}
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
	_ context.Context, key, id string, used []storecontract.BlobRef,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uses[key] = append(s.uses[key], used...)
	s.useIDs = append(s.useIDs, id)

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

// jsonString is a non-empty string field of an answer.
func jsonString(t *testing.T, answer map[string]any, name string) string {
	t.Helper()
	value, ok := answer[name].(string)
	if !ok || value == "" {
		t.Fatalf("the answer's %s is not a string: %v", name, answer)
	}

	return value
}

// blobLeg uploads one block, with the length Azure's client always sends.
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
	uploadURL := jsonString(t, created, "signed_upload_url")
	if created["ok"] != true {
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
	downloadURL := jsonString(t, found, "signed_download_url")
	if found["ok"] != true || found["matched_key"] != "linux-npm-abc" {
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
	uploadURL := jsonString(t, created, "signed_upload_url")

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
	uploadURL := jsonString(t, created, "signed_upload_url")

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
	// A RETRY WILL NOT CHANGE THE CLIENT'S ACCOUNT, so its blocks are not kept for one.
	if len(storage.aborted) != 1 || len(storage.staged) != 0 {
		t.Fatalf("aborted %v, staged %v", storage.aborted, storage.staged)
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
	if response, _, err := finalize(); err == nil || !strings.Contains(err.Error(), "s3 went away") {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("a failed commit answered %v", err)
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
	if response, _, err := service.actionsResponse(request, session); err == nil ||
		!strings.Contains(err.Error(), "s3 went away") {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("a failed commit answered %v", err)
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
	if response, _, err := service.actionsResponse(request, session); err == nil ||
		!strings.Contains(err.Error(), "too many") {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("past the ceiling = %v", err)
	}
	// A LOOKUP COUNTS THE SAME WAY, or one job's mode=max restore is held to what a
	// volume costs.
	lookup := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsDownloadPath,
		`{"key":"past-the-ceiling","restore_keys":[],"version":"v1"}`)
	if response, _, err := service.actionsResponse(lookup, session); err == nil ||
		!strings.Contains(err.Error(), "too many") {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("a lookup past the ceiling = %v", err)
	}
	session.mu.Lock()
	for i := range actionsObjectArchives {
		delete(session.actions, "filler"+strconv.Itoa(i))
	}
	session.mu.Unlock()
	if got := actionsCall(t, service, session, actionsDownloadPath,
		`{"key":"absent","restore_keys":[],"version":"v1"}`); got["ok"] != false {
		t.Fatalf("a lookup past a volume store's ceiling = %v", got)
	}
}

// A RESTORE KEY FINDS THE ARCHIVE UNDER IT, under the job's own scope and with the
// client's own name back.
func TestARestoreKeyFindsAnArchiveOnAStoreOfObjects(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "linux-npm-abc", []string{"body"}, []string{"a"})
	if got := actionsCall(t, service, session, actionsFinalizePath,
		`{"key":"linux-npm-abc","version":"v1","size_bytes":"4"}`); got["ok"] != true {
		t.Fatalf("FinalizeCacheEntryUpload = %v", got)
	}
	found := actionsCall(t, service, session, actionsDownloadPath,
		`{"key":"linux-npm-new","restore_keys":["linux-npm-"],"version":"v1"}`)
	if found["ok"] != true || found["matched_key"] != "linux-npm-abc" {
		t.Fatalf("a restore key's lookup = %v", found)
	}
}

// A COMMIT CAN TAKE MINUTES, AND THE REST OF THE JOB'S CACHE CALLS MUST NOT QUEUE BEHIND
// IT: BuildKit saves its layers side by side.
func TestACommitDoesNotHoldTheSession(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "slow", []string{"body"}, []string{"a"})
	gate := make(chan struct{})
	storage.mu.Lock()
	storage.commitGate = gate
	storage.mu.Unlock()

	finished := make(chan map[string]any, 1)
	go func() {
		finished <- actionsCall(t, service, session, actionsFinalizePath,
			`{"key":"slow","version":"v1","size_bytes":"4"}`)
	}()

	// HELD UNTIL THE RECEIPT IS DURABLE, then given up for the commit.
	guard, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	for {
		if err := lockCacheSession(guard, session); err != nil {
			close(gate)
			t.Fatalf("the session stayed held through the commit: %v", err)
		}
		_, recorded := session.receipts[actionsReceiptID("slow", "v1")]
		session.mu.Unlock()
		if recorded {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waited, stopWait := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopWait()
	err := lockCacheSession(waited, session)
	if err == nil {
		session.mu.Unlock()
	}
	close(gate)
	if err != nil {
		t.Fatalf("the session is held during the commit: %v", err)
	}
	if got := <-finished; got["ok"] != true {
		t.Fatalf("the finalization = %v", got)
	}
}

// A SAVE THAT NEVER FINISHED COMMITTING LEFT ITS BLOCKS, and its archive is by then a
// receipt, so the session's end has to look there too.
func TestAFailedCommitsBlocksAreReleasedWhenTheJobEnds(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "failed", []string{"body"}, []string{"a"})
	storage.mu.Lock()
	storage.commitErr = errors.New("s3 went away")
	storage.mu.Unlock()
	request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath,
		`{"key":"failed","version":"v1","size_bytes":"4"}`)
	if response, _, err := service.actionsResponse(request, session); err == nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal("a failed commit answered success")
	}

	closeAndRetry(t, service, "billet-actions")
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.aborted) != 1 || len(storage.staged) != 0 {
		t.Fatalf("aborted %v, staged %v", storage.aborted, storage.staged)
	}
}

// EVICTION TAKES STAGED BLOCKS ANYWAY, so one abort that fails is no reason to keep a
// finished job's session open: a node upgraded before its IAM would retry them for ever.
func TestAnAbortThatFailsDoesNotHoldTheSessionOpen(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "abandoned", []string{"body"}, []string{"a"})
	storage.mu.Lock()
	storage.abortErr = errors.New("AccessDenied")
	storage.mu.Unlock()

	closeAndRetry(t, service, "billet-actions")
	if service.sessionOf("billet-actions") != nil {
		t.Fatal("a failed abort kept the session open")
	}
}

func TestTheKillSwitchStillSendsAnObjectStoresCallsToGitHub(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) { return false, nil }))

	request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsCreatePath,
		`{"key":"blocked","version":"v1"}`)
	response, handled, err := service.actionsResponse(request, session)
	if response != nil {
		defer response.Body.Close()
	}
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
func objectCAS(t *testing.T, service *CacheService, lease string) string {
	t.Helper()
	credentials, err := service.PrepareScoped(provider.InstanceName(lease), CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: lease, Epoch: 1, Cache: goBazelCache(),
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
	token := objectCAS(t, service, scopedLease)
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		kinds, err := os.ReadDir(filepath.Join(casRoot(service), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(kinds) != 0 {
			t.Fatalf("a finished session left %v on the node's disk", kinds)
		}
	}
}

func TestAJobThatNeverCompletedPublishesNothing(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	instance := provider.InstanceName(scopedLease)
	token := objectCAS(t, service, scopedLease)
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

// WHAT A JOB READ IS WHAT KEEPS IT FROM EVICTION, whether or not the job's own writes were
// published; what it wrote and threw away keeps nothing. Each job has a record of its own,
// or the second would replace the first's and what only the first read would go.
func TestAJobsRecordNamesWhatItReadAndWhatItPublished(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	published := "published by the first job"
	token := objectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, published); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, provider.InstanceName(scopedLease))
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatal(err)
	}

	reader := objectCAS(t, service, "reader")
	if got := casRequest(t, service, reader, http.MethodGet, "/v1/cas/go/cas/"+digestOf(published), ""); got.Code != http.StatusOK {
		t.Fatalf("GET = %d", got.Code)
	}
	discarded := "written and never published"
	if code := putObject(t, service, reader, discarded); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	closeAndRetry(t, service, provider.InstanceName("reader"))

	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.useIDs) != 2 || storage.useIDs[0] == storage.useIDs[1] {
		t.Fatalf("record ids %v, want one per job", storage.useIDs)
	}
	var reads int
	for _, refs := range storage.uses {
		for _, ref := range refs {
			switch ref.Digest {
			case digestOf(published):
				reads++
			case digestOf(discarded):
				t.Fatalf("a discarded write is recorded as used: %v", refs)
			}
		}
	}
	if reads != 2 {
		t.Fatalf("the entry is named %d times, want by its writer and its reader", reads)
	}
}

// ASKING WHETHER AN ENTRY IS THERE COSTS NO REQUEST, and is a use: a build that finds it
// goes on to depend on it.
func TestAnEntrysSizeComesFromTheListing(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	body := "published object"
	token := objectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, body); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, provider.InstanceName(scopedLease))
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatal(err)
	}

	objectCAS(t, service, "asker")
	session := service.sessionOf(provider.InstanceName("asker"))
	handle, err := service.openCASHandle(t.Context(), session, config.CacheGo, false)
	if err != nil {
		t.Fatalf("openCASHandle: %v", err)
	}
	defer handle.Close()
	size, err := handle.Size(reapi.TableCAS, digestOf(body))
	if err != nil || size != int64(len(body)) {
		t.Fatalf("Size = %d, %v", size, err)
	}
	if _, err := handle.Size(reapi.TableCAS, digestOf("absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an absent entry's size = %v", err)
	}
	storage.mu.Lock()
	reads := storage.reads
	storage.mu.Unlock()
	if reads != 0 {
		t.Fatalf("asking about an entry read it %d times", reads)
	}
	if !handle.hv.hit.Load() {
		t.Fatal("finding an entry did not count as a hit")
	}
	handle.hv.reads.mu.Lock()
	defer handle.hv.reads.mu.Unlock()
	if len(handle.hv.reads.refs) != 1 {
		t.Fatalf("the entry found is not recorded as used: %v", handle.hv.reads.refs)
	}
}

// THE REAPER'S AGE IS THE LAST USE, so an entry a build keeps reading is not the first to
// go.
func TestReadingACachedEntryMarksItUsed(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	body := "published object"
	token := objectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, body); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, provider.InstanceName(scopedLease))
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatal(err)
	}

	reader := objectCAS(t, service, "reader")
	read := func() {
		t.Helper()
		if got := casRequest(t, service, reader, http.MethodGet, "/v1/cas/go/cas/"+digestOf(body), ""); got.Code != http.StatusOK {
			t.Fatalf("GET = %d", got.Code)
		}
	}
	read()
	later := time.Now().Add(time.Hour).Truncate(time.Second)
	service.now = func() time.Time { return later }
	read()

	session := service.sessionOf(provider.InstanceName("reader"))
	info, err := os.Stat(service.casCachePath(session.hosts[config.CacheGo].Volume.Key, reapi.TableCAS, digestOf(body)))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(later) {
		t.Fatalf("the cached entry was last used at %s, want %s", info.ModTime(), later)
	}
}

// A DISK PAST THE LINE IS NOT WORTH A REQUEST whose answer would be thrown away.
func TestAFullDiskCostsNoRequest(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	body := "published object"
	token := objectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, body); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, provider.InstanceName(scopedLease))
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatal(err)
	}
	service.casFill = -1

	reader := objectCAS(t, service, "reader")
	if got := casRequest(t, service, reader, http.MethodGet, "/v1/cas/go/cas/"+digestOf(body), ""); got.Code != http.StatusNotFound {
		t.Fatalf("GET on a full disk = %d", got.Code)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.reads != 0 {
		t.Fatalf("a full disk still cost %d reads", storage.reads)
	}
}

// ONE JOB'S UPLOAD MUST NOT SPEND THE NEXT JOB'S WINDOW: a window counts from the job's
// completion, whatever the sessions ahead of it are doing.
func TestClosedSessionsUploadSideBySide(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	first := objectCAS(t, service, "first")
	second := objectCAS(t, service, "second")
	for _, token := range []string{first, second} {
		if code := putObject(t, service, token, "from "+token); code != http.StatusOK {
			t.Fatalf("PUT = %d", code)
		}
	}
	storage.mu.Lock()
	storage.putTogether, storage.putArrived = 2, make(chan struct{}, 2)
	storage.mu.Unlock()
	finish(t, service, provider.InstanceName("first"))
	finish(t, service, provider.InstanceName("second"))

	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 2 {
		t.Fatalf("uploaded %v", storage.puts)
	}
}

// scopedObjectCAS opens a session the kill switch can name.
func scopedObjectCAS(t *testing.T, service *CacheService, lease string) string {
	t.Helper()
	spec := goBazelCache()
	spec.Owner, spec.Repository = "acme", "api"
	credentials, err := service.PrepareScoped(provider.InstanceName(lease), CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: lease, Epoch: 1, Cache: spec,
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return credentials.Token
}

// A PUBLICATION IS ABANDONED ON THE KILL SWITCH AND ON ITS WINDOW, as a volume's is, and
// the session then ends with nothing uploaded.
func TestAPublicationIsAbandonedOnTheKillSwitchAndItsWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		before func(service *CacheService, policy *ledgerPolicy, now *time.Time)
	}{
		{"the kill switch", func(_ *CacheService, policy *ledgerPolicy, _ *time.Time) { policy.blocked.Store(true) }},
		{"its window", func(_ *CacheService, _ *ledgerPolicy, now *time.Time) { *now = now.Add(publishWindow) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service, storage, _ := objectService(t)
			policy := &ledgerPolicy{}
			service.SetCachePolicy(policy)
			now := time.Now()
			service.now = func() time.Time { return now }
			instance := provider.InstanceName(scopedLease)
			token := scopedObjectCAS(t, service, scopedLease)
			if code := putObject(t, service, token, "object"); code != http.StatusOK {
				t.Fatalf("PUT = %d", code)
			}
			finish(t, service, instance)
			tc.before(service, policy, &now)

			if err := service.RetryClosed(t.Context()); err != nil {
				t.Fatalf("RetryClosed: %v", err)
			}
			storage.mu.Lock()
			defer storage.mu.Unlock()
			if len(storage.puts) != 0 {
				t.Fatalf("uploaded %v", storage.puts)
			}
			if service.sessionOf(instance) != nil {
				t.Fatal("an abandoned publication kept its session open")
			}
		})
	}
}

// AN UPLOAD LASTS AS LONG AS THE JOB'S OUTPUT IS LARGE, and disabling a cache has to stop
// one already running.
func TestTheKillSwitchStopsAnUploadInProgress(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	policy := &ledgerPolicy{}
	service.SetCachePolicy(policy)
	instance := provider.InstanceName(scopedLease)
	token := scopedObjectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, "object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, instance)

	// EACH ASK OF THE CLOCK IS LATER, so the one after the publication began finds the
	// answer old, and the first question has already been told yes.
	session := service.sessionOf(instance)
	hv := session.hosts[config.CacheGo]
	now := time.Now()
	service.now = func() time.Time {
		now = now.Add(casPolicyAge)

		return now
	}
	policy.blocked.Store(true)
	err := service.publishBlobs(t.Context(), session, hv, service.casMountPath(session, config.CacheGo),
		service.writtenBlobs(service.casMountPath(session, config.CacheGo)))
	if !errors.Is(err, errPublishDisabled) {
		t.Fatalf("publishBlobs = %v, want the kill switch's stop", err)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 0 {
		t.Fatalf("uploaded %v", storage.puts)
	}
}

// WHAT A JOB WROTE REACHES NO OTHER JOB UNTIL IT IS PUBLISHED, which is the whole of
// ADR-013 for this cache: an unauthorised job's entry must not be readable through
// the read cache by the next.
func TestOneJobsUnpublishedWritesReachNoOtherJob(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	writer := objectCAS(t, service, "writer")
	reader := objectCAS(t, service, "reader")
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
	token := objectCAS(t, service, scopedLease)
	body := "published object"
	if code := putObject(t, service, token, body); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, first)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatal(err)
	}

	second := objectCAS(t, service, "second")
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
	token := objectCAS(t, service, "reader")
	digest := digestOf("what it should be")
	key := service.cacheKeyFor(service.sessionOf(provider.InstanceName("reader")), config.CacheGo, "", "")
	ref := storecontract.BlobRef{Table: storecontract.BlobTableCAS, Digest: digest}
	storage.mu.Lock()
	storage.blobs[key] = map[storecontract.BlobRef][]byte{ref: []byte("something else")}
	storage.mu.Unlock()

	if got := casRequest(t, service, token, http.MethodGet, "/v1/cas/go/cas/"+digest, ""); got.Code != http.StatusNotFound {
		t.Fatalf("GET = %d %q; a corrupt entry was served", got.Code, got.Body.String())
	}
	// THE STORE WAS ASKED, so it is the bytes that were refused and not the digest.
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.reads != 1 {
		t.Fatalf("the store was read %d times", storage.reads)
	}
}

// AN UPLOAD THAT STOPPED PARTWAY LEFT ONLY WHOLE ENTRIES, so it is done again rather
// than abandoned, and the session stays open until it has.
func TestAFailedUploadIsRetriedAndHoldsTheSessionOpen(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	instance := provider.InstanceName(scopedLease)
	token := objectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, "object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	storage.mu.Lock()
	storage.putErr = errors.New("s3 went away")
	storage.mu.Unlock()
	finish(t, service, instance)
	if err := service.RetryClosed(t.Context()); err == nil || !strings.Contains(err.Error(), "s3 went away") {
		t.Fatalf("a failed upload reported %v", err)
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

// THE SIZE BOUNDS WHAT THE JOB ADDS, NOT ONLY WHETHER IT MAY START ANOTHER: an object past
// what is left is refused whole, and what was refused is not counted.
func TestAnObjectPastWhatIsLeftIsRefusedWhole(t *testing.T) {
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
	if code := putObject(t, service, credentials.Token, "nine byte"); code != http.StatusOK {
		t.Fatalf("the first PUT = %d", code)
	}
	if code := putObject(t, service, credentials.Token, "nine more"); code != http.StatusInsufficientStorage {
		t.Fatalf("an object past what is left = %d, want 507", code)
	}
	if got := casRequest(t, service, credentials.Token, http.MethodGet, "/v1/cas/go/cas/"+digestOf("nine more"), ""); got.Code != http.StatusNotFound {
		t.Fatalf("the refused object is readable: %d", got.Code)
	}
	if code := putObject(t, service, credentials.Token, "seven b"); code != http.StatusOK {
		t.Fatalf("an object that fits = %d", code)
	}
}

// countedReader says how much of it was read.
type countedReader struct {
	body io.Reader
	read int64
}

func (c *countedReader) Read(p []byte) (int, error) {
	count, err := c.body.Read(p)
	c.read += int64(count)

	return count, err
}

// AND IS REFUSED WITHOUT BEING WRITTEN: an object the size of the whole allowance must not
// reach the node's disk first, only to be taken off it, when the job has nearly none left.
func TestAnObjectPastWhatIsLeftIsNotReadToTheEnd(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	enabled := true
	spec := config.Tier{Cache: &config.TierCache{
		Go: &config.GoCache{Enabled: &enabled, MaxSize: 1 << 20},
	}}.EffectiveCache()
	instance := provider.InstanceName(scopedLease)
	credentials, err := service.PrepareScoped(instance, CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: scopedLease, Epoch: 1, Cache: &spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	spent := strings.Repeat("s", 1<<20-16)
	if code := putObject(t, service, credentials.Token, spent); code != http.StatusOK {
		t.Fatalf("the first PUT = %d", code)
	}
	handle, err := service.openCASHandle(t.Context(), service.sessionOf(instance), config.CacheGo, true)
	if err != nil {
		t.Fatalf("openCASHandle: %v", err)
	}
	defer handle.Close()

	large := strings.Repeat("l", 1<<19)
	body := &countedReader{body: strings.NewReader(large)}
	if err := handle.Put(t.Context(), reapi.TableCAS, digestOf(large), body); !errors.Is(err, reapi.ErrFull) {
		t.Fatalf("Put = %v, want the allowance refused", err)
	}
	if body.read > 17 {
		t.Fatalf("read %d bytes of an object with 16 left", body.read)
	}
}

// WRITING THE SAME ENTRY AGAIN ADDS NOTHING: a build retries.
func TestWritingAnEntryAgainSpendsNothing(t *testing.T) {
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
	for range 3 {
		if code := putObject(t, service, credentials.Token, "nine byte"); code != http.StatusOK {
			t.Fatalf("PUT = %d", code)
		}
	}
}

// A RESTART DOES NOT REFILL A JOB'S ALLOWANCE, and what the job wrote is still in its
// directory, which recovery does not try to mount.
func TestARestartKeepsWhatAJobHasSpent(t *testing.T) {
	t.Parallel()
	storage := newObjectStore()
	root := t.TempDir()
	open := func() *CacheService {
		t.Helper()
		service, err := NewCacheService("https://10.0.0.1:9443", "test-deployment", root,
			storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(service.git.close)
		service.actionIO = newDeviceVolumes(t)

		return service
	}
	service := open()
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
		t.Fatalf("PUT = %d", code)
	}

	restarted := open()
	volumes := newDeviceVolumes(t)
	restarted.actionIO = volumes
	restarted.RestoreMounts(t.Context())
	if len(volumes.targets) != 0 {
		t.Fatalf("recovery mounted %v", volumes.targets)
	}
	if code := putObject(t, restarted, credentials.Token, "one more"); code != http.StatusInsufficientStorage {
		t.Fatalf("past the size after a restart = %d, want 507", code)
	}
}

func TestTheReadCacheIsReapedOldestFirstOnlyPastItsFill(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	root := filepath.Join(service.rootState, casCacheDirectory, "key", "cas", "aa")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// NAMED SO THE WALK REACHES THE OLDER SECOND: only its age can put it first.
	old, recent := filepath.Join(root, "z-old"), filepath.Join(root, "a-recent")
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

	// AND PAST THE FILL, THE PASS ITSELF MAKES THE ROOM.
	service.casCacheFill = -1
	if err := service.ReapCASCache(t.Context()); err != nil {
		t.Fatalf("ReapCASCache: %v", err)
	}
	if _, err := os.Stat(recent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the entry past the fill = %v, want it gone", err)
	}

	// NOTHING THERE IS NOT A FAILURE: the node has served no such cache yet.
	empty, _, _ := objectService(t)
	if err := empty.ReapCASCache(t.Context()); err != nil {
		t.Fatalf("ReapCASCache on no cache: %v", err)
	}
}
