package node

import (
	"bytes"
	"context"
	"encoding/json"
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
	"sync/atomic"
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
	// commitGate, when set, holds every commit until it is closed, and commits counts them.
	commitGate chan struct{}
	commits    int
	// putHold, when set, holds every PutBlob until it is closed or its context ends,
	// saying so on putHeld. holdOnly, when set, is the one key it holds.
	putHold  chan struct{}
	putHeld  chan struct{}
	holdOnly string
	// putFailures fails that many PutBlobs before any of them is held.
	putFailures int
	// listErr fails every listing.
	listErr error
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
	s.commits++
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
	if s.listErr != nil {
		return nil, s.listErr
	}
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
	ctx context.Context, key string, ref storecontract.BlobRef, body io.Reader, size int64,
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
	together, arrived, hold, held := s.putTogether, s.putArrived, s.putHold, s.putHeld
	if s.putFailures > 0 {
		s.putFailures--
		s.mu.Unlock()

		return errors.New("s3 went away for a moment")
	}
	if s.holdOnly != "" && s.holdOnly != string(data) {
		hold = nil
	}
	s.mu.Unlock()
	if hold != nil {
		held <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if together > 0 {
		arrived <- struct{}{}
		guard := time.NewTimer(30 * time.Second)
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

	var release sync.Once
	open := func() { release.Do(func() { close(gate) }) }
	defer open()
	finished := finalizeInBackground(t, service, session, `{"key":"slow","version":"v1","size_bytes":"4"}`)

	// HELD UNTIL THE RECEIPT IS DURABLE, then given up for the commit.
	guard, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	for {
		if err := lockCacheSession(guard, session); err != nil {
			t.Fatalf("the session stayed held through the commit: %v", err)
		}
		_, recorded := session.receipts[actionsReceiptID("slow", "v1")]
		session.mu.Unlock()
		if recorded {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waited, stopWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopWait()
	if err := lockCacheSession(waited, session); err != nil {
		t.Fatalf("the session is held during the commit: %v", err)
	}
	session.mu.Unlock()
	open()
	if got := finished.wait(t); got["ok"] != true {
		t.Fatalf("the finalization = %v", got)
	}
}

// finalization is one finalization running beside a test.
type finalization struct {
	done chan struct{}
	body map[string]any
	err  error
}

// wait bounds waiting for the answer, so a hung commit is a failure and not a hung package.
func (f *finalization) wait(t *testing.T) map[string]any {
	t.Helper()
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case <-f.done:
	case <-guard.C:
		t.Fatal("the finalization did not answer")
	}
	if f.err != nil {
		t.Fatalf("the finalization: %v", f.err)
	}

	return f.body
}

// finalizeInBackground runs a finalization off the test's goroutine, which is the only
// one that may fail the test.
func finalizeInBackground(t *testing.T, service *CacheService, session *cacheSession, body string) *finalization {
	t.Helper()
	request := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+actionsFinalizePath, body)
	running := &finalization{done: make(chan struct{})}
	go func() {
		defer close(running.done)
		response, _, err := service.actionsResponse(request, session)
		if err != nil {
			running.err = err

			return
		}
		defer response.Body.Close()
		running.err = json.NewDecoder(response.Body).Decode(&running.body)
	}()
	t.Cleanup(func() {
		guard := time.NewTimer(30 * time.Second)
		defer guard.Stop()
		select {
		case <-running.done:
		case <-guard.C:
			t.Error("a finalization outlived its test")
		}
	})

	return running
}

// THE FIRST TO LAND DELETES THE BLOCKS A SECOND IS ABOUT TO LIST, which would then report
// nothing published over an answer that said it was. So a client's retry beside its own
// finalization waits, and reads the first one's answer.
func TestTwoFinalizationsOfOneSaveCommitOnce(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "twice", []string{"body"}, []string{"a"})
	gate := make(chan struct{})
	storage.mu.Lock()
	storage.commitGate = gate
	storage.mu.Unlock()
	const body = `{"key":"twice","version":"v1","size_bytes":"4"}`
	first := finalizeInBackground(t, service, session, body)

	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	for {
		storage.mu.Lock()
		started := storage.commits
		storage.mu.Unlock()
		if started == 1 {
			break
		}
		select {
		case <-guard.C:
			close(gate)
			t.Fatal("the first commit never started")
		case <-time.After(time.Millisecond):
		}
	}
	second := finalizeInBackground(t, service, session, body)
	// TIME FOR THE SECOND TO REACH THE LOCK, which can only make this test weaker if it
	// is not enough, never wrong.
	time.Sleep(50 * time.Millisecond)
	close(gate)

	if got := first.wait(t); got["ok"] != true {
		t.Fatalf("the first finalization = %v", got)
	}
	if got := second.wait(t); got["ok"] != true {
		t.Fatalf("the second finalization = %v", got)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.commits != 1 {
		t.Fatalf("%d commits of one save", storage.commits)
	}
}

// THE JOB'S END MAY LAND IN THE COMMIT: the session then has no record to raise, and the
// client that is still waiting gets an answer rather than an error about the record.
func TestASessionFinishedDuringACommitStillAnswers(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	session := objectActionsSession(t, service, "billet-actions")
	stagedUpload(t, service, session, "late", []string{"body"}, []string{"a"})
	gate := make(chan struct{})
	storage.mu.Lock()
	storage.commitGate = gate
	storage.mu.Unlock()
	finished := finalizeInBackground(t, service, session, `{"key":"late","version":"v1","size_bytes":"4"}`)
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	for {
		storage.mu.Lock()
		started := storage.commits
		storage.mu.Unlock()
		if started == 1 {
			break
		}
		select {
		case <-guard.C:
			close(gate)
			t.Fatal("the commit never started")
		case <-time.After(time.Millisecond):
		}
	}

	closeAndRetry(t, service, "billet-actions")
	if service.sessionOf("billet-actions") != nil {
		close(gate)
		t.Fatal("the session did not finish")
	}
	close(gate)
	finished.wait(t)

	records, err := os.ReadDir(service.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("a finished session's record came back: %v", records)
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

// A YES IS A PROMISE THE ENTRY CAN BE READ: a build told an action's output is there may
// go on without it. So one this session could not read is not there, and a read that finds
// one gone takes it out of the listing.
func TestAnEntryIsOnlySaidToBeThereIfItCanBeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		make func(t *testing.T, service *CacheService, storage *objectStore, handle *casHandle, digest string)
	}{
		{"past this session's limit", func(_ *testing.T, _ *CacheService, _ *objectStore, handle *casHandle, _ string) {
			handle.limit = 1
		}},
		{"on a disk too full to fill the read cache", func(_ *testing.T, service *CacheService, _ *objectStore, _ *casHandle, _ string) {
			service.casFill = -1
		}},
		{"a read found it gone", func(t *testing.T, _ *CacheService, storage *objectStore, handle *casHandle, digest string) {
			t.Helper()
			storage.mu.Lock()
			clear(storage.blobs)
			storage.mu.Unlock()
			if _, err := handle.Open(reapi.TableCAS, digest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Open of an entry gone = %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			handle, err := service.openCASHandle(t.Context(), service.sessionOf(provider.InstanceName("asker")), config.CacheGo, false)
			if err != nil {
				t.Fatalf("openCASHandle: %v", err)
			}
			defer handle.Close()
			if size, err := handle.Size(reapi.TableCAS, digestOf(body)); err != nil || size != int64(len(body)) {
				t.Fatalf("before = %d, %v", size, err)
			}
			tc.make(t, service, storage, handle, digestOf(body))
			if _, err := handle.Size(reapi.TableCAS, digestOf(body)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Size = %v, want it not there", err)
			}
		})
	}
}

// AND ONE ALREADY IN THE READ CACHE NEEDS NOTHING FROM THE DISK.
func TestAFullDiskDoesNotHideWhatTheReadCacheHolds(t *testing.T) {
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
	if got := casRequest(t, service, reader, http.MethodGet, "/v1/cas/go/cas/"+digestOf(body), ""); got.Code != http.StatusOK {
		t.Fatalf("GET = %d", got.Code)
	}
	service.casFill = -1
	handle, err := service.openCASHandle(t.Context(), service.sessionOf(provider.InstanceName("reader")), config.CacheGo, false)
	if err != nil {
		t.Fatalf("openCASHandle: %v", err)
	}
	defer handle.Close()
	if size, err := handle.Size(reapi.TableCAS, digestOf(body)); err != nil || size != int64(len(body)) {
		t.Fatalf("Size = %d, %v", size, err)
	}
}

// A JOB THAT ENDS WHILE ANOTHER UPLOADS STARTS THEN, not when the longest upload ahead of it
// is done: its window is already counting.
func TestASessionThatClosesDuringAPassIsTakenByIt(t *testing.T) {
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
	pass := make(chan error, 1)
	go func() { pass <- service.RetryClosed(t.Context()) }()

	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	for {
		storage.mu.Lock()
		arrived := len(storage.putArrived)
		storage.mu.Unlock()
		if arrived == 1 {
			break
		}
		select {
		case <-guard.C:
			t.Fatal("the first upload never started")
		case <-time.After(time.Millisecond):
		}
	}
	finish(t, service, provider.InstanceName("second"))

	select {
	case err := <-pass:
		if err != nil {
			t.Fatalf("RetryClosed: %v", err)
		}
	case <-guard.C:
		t.Fatal("the pass did not finish")
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 2 {
		t.Fatalf("uploaded %v", storage.puts)
	}
}

// waitHeld returns once an upload has reached the store and is being held.
func waitHeld(t *testing.T, held chan struct{}) {
	t.Helper()
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case <-held:
	case <-guard.C:
		t.Fatal("no upload reached the store")
	}
}

// waitPuts returns once the store has taken that many entries.
func waitPuts(t *testing.T, storage *objectStore, want int) {
	t.Helper()
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	for {
		storage.mu.Lock()
		got := len(storage.puts)
		storage.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-guard.C:
			t.Fatalf("the store took %d entries, want %d", got, want)
		case <-time.After(time.Millisecond):
		}
	}
}

// A FAILED UPLOAD IS TRIED AGAIN WHILE ANOTHER JOB'S RUNS, not when it is done: that one can
// last until this one's window has gone, and what the job wrote with it. Having finished,
// it has nothing to report for the failure before.
func TestAFailedUploadIsTriedAgainDuringThePass(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	service.closedSessionRetry = 5 * time.Millisecond
	logged := &lockedBuffer{}
	service.log = slog.New(slog.NewTextHandler(logged, nil))
	long, short := objectCAS(t, service, "long"), objectCAS(t, service, "short")
	if code := putObject(t, service, long, "the long upload"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, provider.InstanceName("long"))
	hold, held := make(chan struct{}), make(chan struct{}, 1)
	storage.mu.Lock()
	storage.putHold, storage.putHeld, storage.holdOnly = hold, held, "the long upload"
	storage.mu.Unlock()
	var release sync.Once
	open := func() { release.Do(func() { close(hold) }) }
	defer open()
	pass := make(chan error, 1)
	go func() { pass <- service.RetryClosed(t.Context()) }()
	waitHeld(t, held)

	if code := putObject(t, service, short, "the short upload"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	storage.mu.Lock()
	storage.putFailures = 1
	storage.mu.Unlock()
	finish(t, service, provider.InstanceName("short"))

	waitPuts(t, storage, 1)
	open()
	if err := <-pass; err != nil {
		t.Fatalf("RetryClosed = %v; what failed and then finished has nothing to report", err)
	}
	// BUT THE LOG WAS TOLD, because a pass keeps only each session's last word.
	if said := logged.String(); !strings.Contains(said, "could not yet upload") ||
		!strings.Contains(said, "s3 went away for a moment") {
		t.Fatalf("the log says:\n%s", said)
	}
}

// A PASS HAS TO RETURN, because renewal runs after it on the same loop. Past its bound it
// takes no more, and what closed meanwhile is signalled for the next.
func TestAPassStopsTakingSessionsAtItsBound(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	now := time.Now()
	var clock sync.Mutex
	service.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()

		return now
	}
	first, second := objectCAS(t, service, "first"), objectCAS(t, service, "second")
	for _, token := range []string{first, second} {
		if code := putObject(t, service, token, "from "+token); code != http.StatusOK {
			t.Fatalf("PUT = %d", code)
		}
	}
	finish(t, service, provider.InstanceName("first"))
	hold, held := make(chan struct{}), make(chan struct{}, 2)
	storage.mu.Lock()
	storage.putHold, storage.putHeld = hold, held
	storage.mu.Unlock()
	pass := make(chan error, 1)
	go func() { pass <- service.RetryClosed(t.Context()) }()
	waitHeld(t, held)

	clock.Lock()
	now = now.Add(closedSessionAdmit)
	clock.Unlock()
	finish(t, service, provider.InstanceName("second"))
	// The window of the first upload still has to outlast the clock's move, so what is
	// held is released by the test, and the bound is all that is being asked about.
	time.Sleep(50 * time.Millisecond)
	close(hold)

	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case err := <-pass:
		if err != nil {
			t.Fatalf("RetryClosed: %v", err)
		}
	case <-guard.C:
		t.Fatal("the pass did not return")
	}
	if service.sessionOf(provider.InstanceName("second")) == nil {
		t.Fatal("a session that closed past the bound was taken by the pass")
	}
	select {
	case <-service.ClosedSessions():
	default:
		t.Fatal("the next pass was not told about the session left for it")
	}
}

// A CLIENT THAT HANGS UP DURING THE LISTING SAYS NOTHING OF THE STORE, so the next read lists
// again, where a failure of the store itself is left a minute before it is asked again.
func TestAListingACancelledClientCutShortIsNotHeldAgainstTheSession(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	objectCAS(t, service, "asker")
	session := service.sessionOf(provider.InstanceName("asker"))
	ask := func(ctx context.Context) error {
		t.Helper()
		handle, err := service.openCASHandle(t.Context(), session, config.CacheGo, false)
		if err != nil {
			t.Fatalf("openCASHandle: %v", err)
		}
		defer handle.Close()
		handle.ctx = ctx
		_, err = handle.Size(reapi.TableCAS, digestOf("anything"))

		return err
	}
	storage.mu.Lock()
	storage.listErr = context.Canceled
	storage.mu.Unlock()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ask(cancelled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Size = %v", err)
	}
	storage.mu.Lock()
	storage.listErr = nil
	storage.mu.Unlock()
	if err := ask(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Size = %v", err)
	}
	storage.mu.Lock()
	listings := storage.listings
	storage.listErr = errors.New("s3 went away")
	storage.mu.Unlock()
	if listings != 1 {
		t.Fatalf("%d listings; the read after a client hung up did not list again", listings)
	}

	other := objectCAS(t, service, "other")
	if other == "" {
		t.Fatal("no session")
	}
	second := service.sessionOf(provider.InstanceName("other"))
	handle, err := service.openCASHandle(t.Context(), second, config.CacheGo, false)
	if err != nil {
		t.Fatalf("openCASHandle: %v", err)
	}
	defer handle.Close()
	if _, err := handle.Size(reapi.TableCAS, digestOf("anything")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Size = %v", err)
	}
	handle.hv.index.mu.Lock()
	defer handle.hv.index.mu.Unlock()
	if handle.hv.index.retry.IsZero() {
		t.Fatal("a store that failed was asked again at once")
	}
}

// A READ THAT FINDS AN ENTRY GONE TAKES IT OUT OF THE LISTING WHILE OTHERS READ IT, and a map
// read beside that write ends the process, with every guest's cache and renewal on it.
func TestAnEntryForgottenBesideReadersIsNotARace(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	token := objectCAS(t, service, scopedLease)
	bodies := make([]string, 64)
	for i := range bodies {
		bodies[i] = "entry " + strconv.Itoa(i)
		if code := putObject(t, service, token, bodies[i]); code != http.StatusOK {
			t.Fatalf("PUT = %d", code)
		}
	}
	finish(t, service, provider.InstanceName(scopedLease))
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatal(err)
	}

	objectCAS(t, service, "asker")
	handle, err := service.openCASHandle(t.Context(), service.sessionOf(provider.InstanceName("asker")), config.CacheGo, false)
	if err != nil {
		t.Fatalf("openCASHandle: %v", err)
	}
	defer handle.Close()
	if _, err := handle.Size(reapi.TableCAS, digestOf(bodies[0])); err != nil {
		t.Fatalf("Size: %v", err)
	}
	storage.mu.Lock()
	clear(storage.blobs)
	storage.mu.Unlock()

	var readers sync.WaitGroup
	for _, body := range bodies {
		readers.Go(func() {
			if file, err := handle.Open(reapi.TableCAS, digestOf(body)); err == nil {
				file.Close()
			}
		})
		readers.Go(func() {
			for range 64 {
				if _, err := handle.Size(reapi.TableCAS, digestOf(bodies[0])); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Size: %v", err)
				}
			}
		})
	}
	readers.Wait()
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

// uploadInProgress starts a publication whose one upload is held, and returns when it has
// reached the store, so whatever the test does next happens during it.
func uploadInProgress(t *testing.T, service *CacheService, storage *objectStore, hold chan struct{}) chan error {
	t.Helper()
	instance := provider.InstanceName(scopedLease)
	token := scopedObjectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, "object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, instance)
	held := make(chan struct{}, 1)
	storage.mu.Lock()
	storage.putHold, storage.putHeld = hold, held
	storage.mu.Unlock()

	session := service.sessionOf(instance)
	root := service.casMountPath(session, config.CacheGo)
	result := make(chan error, 1)
	go func() {
		result <- service.publishBlobs(t.Context(), session, session.hosts[config.CacheGo], root, service.writtenBlobs(root))
	}()
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case <-held:
	case <-guard.C:
		t.Fatal("the upload never reached the store")
	}

	return result
}

// AN UPLOAD LASTS AS LONG AS THE JOB'S OUTPUT IS LARGE, and disabling a cache has to stop
// one already running, however few entries it is: the upload itself is what takes the time.
func TestTheKillSwitchStopsAnUploadInProgress(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	policy := &ledgerPolicy{}
	service.SetCachePolicy(policy)
	service.casPolicyEvery = 5 * time.Millisecond
	hold := make(chan struct{})
	defer close(hold)
	result := uploadInProgress(t, service, storage, hold)

	policy.blocked.Store(true)
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case err := <-result:
		if !errors.Is(err, errPublishDisabled) {
			t.Fatalf("publishBlobs = %v, want the kill switch's stop", err)
		}
	case <-guard.C:
		t.Fatal("the kill switch did not stop the upload")
	}
}

// A SWITCH THAT CANNOT BE READ MID-UPLOAD IS NOT ONE THAT REFUSED: a blip in the control
// plane must not discard what a job wrote while its window is open.
func TestAKillSwitchThatCannotBeReadDoesNotStopAnUpload(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	asked := make(chan struct{}, 1)
	var away atomic.Bool
	service.SetCachePolicy(cachePolicyFunc(func(context.Context, config.CacheKind, string, string) (bool, error) {
		if !away.Load() {
			return true, nil
		}
		select {
		case asked <- struct{}{}:
		default:
		}

		return false, errors.New("the plane is away")
	}))
	service.casPolicyEvery = 5 * time.Millisecond
	hold := make(chan struct{})
	result := uploadInProgress(t, service, storage, hold)
	away.Store(true)

	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case <-asked:
	case <-guard.C:
		t.Fatal("the switch was never asked during the upload")
	}
	close(hold)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("publishBlobs = %v", err)
		}
	case <-guard.C:
		t.Fatal("the upload did not finish")
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 1 {
		t.Fatalf("uploaded %v", storage.puts)
	}
}

// AND ONE THAT CANNOT BE READ BEFORE IT STARTS HOLDS IT FOR THE NEXT PASS, rather than
// abandoning it for good.
func TestAKillSwitchThatCannotBeReadHoldsAPublication(t *testing.T) {
	t.Parallel()
	service, storage, _ := objectService(t)
	var readable atomic.Bool
	service.SetCachePolicy(cachePolicyFunc(func(context.Context, config.CacheKind, string, string) (bool, error) {
		if !readable.Load() {
			return false, errors.New("the plane is away")
		}

		return true, nil
	}))
	instance := provider.InstanceName(scopedLease)
	readable.Store(true)
	token := scopedObjectCAS(t, service, scopedLease)
	if code := putObject(t, service, token, "object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, instance)
	readable.Store(false)

	if err := service.RetryClosed(t.Context()); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("RetryClosed = %v, want the publication held", err)
	}
	if service.sessionOf(instance) == nil {
		t.Fatal("the session was forgotten with its writes unpublished")
	}

	readable.Store(true)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.puts) != 1 {
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

// WRITERS OF ONE ENTRY SIDE BY SIDE COUNT WHAT IS THERE, whichever of them installed last: a
// count below it lets a job past its size, and one above it refuses what fits.
func TestWritersOfOneEntryCountWhatIsThere(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	token := objectCAS(t, service, scopedLease)
	instance := provider.InstanceName(scopedLease)
	if code := putObject(t, service, token, "first"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	handle, err := service.openCASHandle(t.Context(), service.sessionOf(instance), config.CacheGo, true)
	if err != nil {
		t.Fatalf("openCASHandle: %v", err)
	}
	defer handle.Close()

	root := service.casMountPath(service.sessionOf(instance), config.CacheGo)
	// MANY ROUNDS, because one interleaving in many is what miscounts.
	for round := range 200 {
		digest := digestOf("an action " + strconv.Itoa(round))
		failures := make(chan error, 16)
		var writers sync.WaitGroup
		for i := range cap(failures) {
			writers.Go(func() {
				result := strings.Repeat("r", 1+i*64)
				failures <- handle.Put(t.Context(), reapi.TableAC, digest, strings.NewReader(result))
			})
		}
		writers.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if got, want := handle.hv.written.Load(), service.writtenBytes(root); got != want {
			t.Fatalf("round %d: %d bytes counted, %d in the directory", round, got, want)
		}
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

// WHAT A CRASH LEFT BESIDE THE ENTRIES IS NOT WHAT THE JOB SPENT: no upload will ever take it.
func TestWhatACrashLeftIsNotCountedAgainstTheJob(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "entry"), []byte("counted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".held-1"), []byte("not counted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := service.writtenBytes(root); got != int64(len("counted")) {
		t.Fatalf("writtenBytes = %d", got)
	}
}

// A FILL STILL WRITING IS NOT AN ENTRY: reaped, its rename fails, and a read that was going to
// succeed costs a miss and a warning.
func TestTheReadCacheLeavesWhatAFillIsWriting(t *testing.T) {
	t.Parallel()
	service, _, _ := objectService(t)
	root := filepath.Join(service.rootState, casCacheDirectory, "key", "cas", "aa")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	filling := filepath.Join(root, ".put-123")
	if err := os.WriteFile(filling, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	crashed := filepath.Join(root, ".put-456")
	if err := os.WriteFile(crashed, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-casRequestLife - time.Minute)
	if err := os.Chtimes(crashed, time.Time{}, old); err != nil {
		t.Fatal(err)
	}
	if err := reapCASEntries(t.Context(), filepath.Join(service.rootState, casCacheDirectory), 1<<20); err != nil {
		t.Fatalf("reapCASEntries: %v", err)
	}
	if _, err := os.Stat(filling); err != nil {
		t.Fatalf("a fill in progress was reaped: %v", err)
	}
	// OLDER THAN ANY TRANSFER CAN LAST, it is what a crash left, and nothing else takes it.
	if _, err := os.Stat(crashed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("what a crash left = %v, want it gone", err)
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
