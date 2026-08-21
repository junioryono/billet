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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

type fakeCacheStore struct {
	current            string
	created            int
	cloned             int
	snapshots          int
	published          int
	discarded          int
	discardFailures    int
	renewed            int
	renewedUntil       time.Time
	publishErr         error
	publishConflicts   int
	conflictCurrent    string
	advanceOnSnapshot  bool
	publishExpected    []string
	writerAcquisitions int
	validWriterIDs     map[string]string
	validFences        map[string]storecontract.FencingToken
	publishedWriterIDs []string
	publishedFences    []storecontract.FencingToken
	keys               []string
	createdSizes       []int64
	snapshotVolumes    []storecontract.Volume
}

func TestCacheWriterAuthorityOutlivesTheWholeCommitBudget(t *testing.T) {
	t.Parallel()

	if cacheCleanupMargin <= 0 || cacheWorkLimit+cacheCleanupMargin != cacheHandlerLimit {
		t.Fatalf("cache handler budget %s does not reserve cleanup margin %s",
			cacheHandlerLimit, cacheCleanupMargin)
	}
	if cacheWriterTTL <= cacheHandlerLimit {
		t.Fatalf("writer lease %s does not cover the %s handler budget", cacheWriterTTL, cacheHandlerLimit)
	}
}

func TestCacheSessionLockWaitHonoursTheCommitContext(t *testing.T) {
	t.Parallel()

	session := &cacheSession{}
	session.mu.Lock()
	t.Cleanup(session.mu.Unlock)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := lockCacheSession(ctx, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("lockCacheSession returned %v, want context cancellation", err)
	}
}

func (f *fakeCacheStore) Current(context.Context, string) (string, error) {
	if f.current == "" {
		return "", storecontract.ErrMiss
	}

	return f.current, nil
}

func (f *fakeCacheStore) Match(
	_ context.Context,
	exact string,
	_ []string,
) (string, string, error) {
	if f.current == "" {
		return "", "", storecontract.ErrMiss
	}

	return exact, f.current, nil
}

func (f *fakeCacheStore) Create(_ context.Context, key string, size int64) (storecontract.Volume, error) {
	f.created++
	f.keys = append(f.keys, key)
	f.createdSizes = append(f.createdSizes, size)

	return storecontract.Volume{Key: key, Handle: "working", Device: "/dev/rbd7"}, nil
}

func (f *fakeCacheStore) Clone(_ context.Context, key, generation string) (storecontract.Volume, error) {
	f.keys = append(f.keys, key)
	if f.current == "" {
		return storecontract.Volume{}, storecontract.ErrMiss
	}

	f.cloned++
	if generation == "" {
		generation = f.current
	}

	return storecontract.Volume{
		Key: key, Generation: generation, Handle: "clone", Device: "/dev/rbd8",
		Lease: storecontract.ActiveLease{ID: "active", Expires: time.Now().Add(time.Hour)},
	}, nil
}

func (f *fakeCacheStore) RenewActive(
	_ context.Context, _ storecontract.Volume, until time.Time,
) error {
	f.renewed++
	f.renewedUntil = until

	return nil
}

func (f *fakeCacheStore) AcquireWriter(
	_ context.Context, key, _ string, ttl time.Duration,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	f.writerAcquisitions++
	writerID := fmt.Sprintf("writer-%d", f.writerAcquisitions)
	fence := storecontract.FencingToken(f.writerAcquisitions)
	if f.validWriterIDs == nil {
		f.validWriterIDs = make(map[string]string)
	}
	f.validWriterIDs[key] = writerID
	if f.validFences == nil {
		f.validFences = make(map[string]storecontract.FencingToken)
	}
	f.validFences[key] = fence

	return storecontract.WriterLease{Key: key, ID: writerID, Expires: time.Now().Add(ttl)}, fence, nil
}

func (f *fakeCacheStore) Snapshot(
	_ context.Context, volume storecontract.Volume,
) (storecontract.Candidate, error) {
	f.snapshots++
	f.snapshotVolumes = append(f.snapshotVolumes, volume)
	if f.advanceOnSnapshot {
		f.current = "concurrent"
	}

	return storecontract.Candidate{
		Key: volume.Key, Generation: "next", Handle: "candidate",
		Filesystem: storecontract.Filesystem{Type: "ext4", UUID: "uuid", Clean: true},
	}, nil
}

func (f *fakeCacheStore) PublishCAS(
	_ context.Context,
	key, expected string,
	candidate storecontract.Candidate,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
) error {
	f.published++
	f.publishExpected = append(f.publishExpected, expected)
	f.publishedWriterIDs = append(f.publishedWriterIDs, lease.ID)
	f.publishedFences = append(f.publishedFences, fence)
	if lease.ID != f.validWriterIDs[key] || fence != f.validFences[key] {
		return errors.New("stale writer authority")
	}
	if f.publishConflicts > 0 {
		f.publishConflicts--
		if f.conflictCurrent != "" {
			f.current = f.conflictCurrent
		}

		return storecontract.ErrConflict
	}
	if f.publishErr == nil && expected != f.current {
		return storecontract.ErrConflict
	}
	if f.publishErr == nil {
		f.current = candidate.Generation
	}

	return f.publishErr
}

func (f *fakeCacheStore) Discard(context.Context, storecontract.Volume) error {
	f.discarded++
	if f.discardFailures > 0 {
		f.discardFailures--

		return errors.New("temporary discard failure")
	}

	return nil
}

func (f *fakeCacheStore) Evict(context.Context, time.Duration) error { return nil }

type fakeVolumeAttacher struct {
	attached  []int
	detached  []int
	attachErr error
	detachErr error
}

func (f *fakeVolumeAttacher) AttachVolume(_ context.Context, _ string, slot int, _ string) error {
	f.attached = append(f.attached, slot)

	return f.attachErr
}

func (f *fakeVolumeAttacher) DetachVolume(_ context.Context, _ string, slot int, _ string) error {
	f.detached = append(f.detached, slot)

	return f.detachErr
}

func testCacheService(t *testing.T, trust provider.TrustClass) (*CacheService, *fakeCacheStore, string) {
	t.Helper()

	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	credentials, err := service.Prepare("billet-one", trust)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	return service, storage, credentials.Token
}

func cacheRequest(
	t *testing.T,
	service *CacheService,
	token, path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()

	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		input = bytes.NewReader(encoded)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, input)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, req)

	return response
}

func TestATrustedGuestPublishesItsDetachedVolume(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)

	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "fs-123", "clean": true},
	})
	if committed.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", committed.Code, committed.Body.String())
	}

	if storage.created != 1 || storage.snapshots != 1 || storage.published != 1 {
		t.Errorf("create/snapshot/publish = %d/%d/%d", storage.created, storage.snapshots,
			storage.published)
	}
	if len(storage.snapshotVolumes) != 1 || storage.snapshotVolumes[0].Filesystem !=
		(storecontract.Filesystem{Type: "ext4", UUID: "fs-123", Clean: true}) {
		t.Errorf("snapshot filesystem proof = %+v", storage.snapshotVolumes)
	}
}

func TestAnUntrustedGuestReadsTheBaselineAndDiscardsItsWrite(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustUntrusted)
	storage.current = "trusted-generation"

	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK {
		t.Fatalf("discard status = %d: %s", committed.Code, committed.Body.String())
	}

	if storage.cloned != 1 || storage.discarded != 1 || storage.published != 0 {
		t.Errorf("clone/discard/publish = %d/%d/%d", storage.cloned, storage.discarded,
			storage.published)
	}
}

func TestACommitFailureIsAWarningAndNeverFailsTheJob(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	storage.publishErr = errors.New("cluster unavailable")

	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d", attached.Code)
	}

	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK {
		t.Fatalf("a cache failure escaped as HTTP %d", committed.Code)
	}

	if !bytes.Contains(committed.Body.Bytes(), []byte(`"published":false`)) {
		t.Errorf("commit did not report its non-fatal miss: %s", committed.Body.String())
	}
}

func TestLastWriteWinsRebasesABuilderCandidateAfterAConcurrentCommit(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	storage.current = "baseline"
	storage.advanceOnSnapshot = true
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/buildkit", "size_bytes": int64(10 << 30),
		"publication": publicationLWW,
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", committed.Code, committed.Body.String())
	}
	if got := storage.publishExpected; len(got) != 2 || got[0] != "baseline" || got[1] != "concurrent" {
		t.Errorf("publication expectations = %v, want baseline then concurrent", got)
	}
}

func TestDockerImageStoreIsArchitectureScopedAndReservesSlotZero(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	attacher := &fakeVolumeAttacher{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token := credentials.Token

	docker := cacheRequest(t, service, token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"})
	if docker.Code != http.StatusCreated {
		t.Fatalf("Docker store status = %d: %s", docker.Code, docker.Body.String())
	}
	var mounted struct {
		Slot   int    `json:"slot"`
		Device string `json:"device"`
		Cold   bool   `json:"cold"`
	}
	if err := json.Unmarshal(docker.Body.Bytes(), &mounted); err != nil {
		t.Fatalf("decode Docker store response: %v", err)
	}
	if mounted.Slot != 0 || mounted.Device != "/dev/vdb" || !mounted.Cold {
		t.Fatalf("Docker store attachment = %+v, want cold slot 0 at /dev/vdb", mounted)
	}
	if len(storage.keys) != 2 || storage.keys[0] != "test-deployment/docker-images/amd64" ||
		storage.keys[1] != storage.keys[0] {
		t.Fatalf("Docker store keys = %v", storage.keys)
	}
	if len(storage.createdSizes) != 1 || storage.createdSizes[0] != cacheVolumeLimit {
		t.Fatalf("Docker store sizes = %v, want %d", storage.createdSizes, cacheVolumeLimit)
	}
	if len(attacher.attached) != 1 || attacher.attached[0] != 0 {
		t.Fatalf("Docker store attached slots = %v, want [0]", attacher.attached)
	}

	sticky := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(1 << 30),
	})
	if sticky.Code != http.StatusCreated {
		t.Fatalf("sticky disk status = %d: %s", sticky.Code, sticky.Body.String())
	}
	if len(attacher.attached) != 2 || attacher.attached[1] != 1 {
		t.Fatalf("attachment slots = %v, want Docker in 0 and sticky disk in 1", attacher.attached)
	}

	storage.advanceOnSnapshot = true
	direct := cacheRequest(t, service, token, "/v1/volumes/0/commit", map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
	})
	if direct.Code != http.StatusForbidden {
		t.Fatalf("guest Docker store commit status = %d, want 403: %s",
			direct.Code, direct.Body.String())
	}
	proof := map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
	}
	if early := cacheRequest(t, service, token, "/v1/docker-store/ready", proof); early.Code != http.StatusConflict {
		t.Fatalf("early helper Docker readiness status = %d, want 409: %s",
			early.Code, early.Body.String())
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
	if err := <-settled; err != nil {
		t.Fatalf("SettleDocker: %v", err)
	}
	if got := storage.publishExpected; len(got) != 2 || got[0] != "" || got[1] != "concurrent" {
		t.Errorf("Docker publication expectations = %v, want an LWW rebase", got)
	}
	if len(attacher.detached) != 1 || attacher.detached[0] != 0 {
		t.Errorf("Docker detached slots = %v, want [0]", attacher.detached)
	}
}

func TestFailedJobLeavesTheDockerStoreAttachedUntilComputeIsGone(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	attacher := &fakeVolumeAttacher{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, attacher, slog.New(slog.DiscardHandler))
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
	if response := cacheRequest(t, service, token, "/v1/docker-store/ready", map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
	}); response.Code != http.StatusConflict {
		t.Fatalf("failed job's early Docker readiness status = %d, want 409: %s",
			response.Code, response.Body.String())
	}

	if err := service.SettleDocker(t.Context(), "billet-one", false); err != nil {
		t.Fatalf("SettleDocker failed result: %v", err)
	}
	if len(attacher.detached) != 0 || storage.discarded != 0 || storage.published != 0 {
		t.Fatalf("failed result detached/discarded/published = %v/%d/%d before compute stopped",
			attacher.detached, storage.discarded, storage.published)
	}
	if err := service.Cleanup(t.Context(), "billet-one"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if storage.discarded != 1 {
		t.Errorf("cleanup discarded %d Docker stores, want 1", storage.discarded)
	}
}

func TestDockerImageStoreRefusesAnUnusableArchitecture(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	for _, architecture := range []string{"", "amd64/other", "amd64\nother", "amd 64", "-amd64"} {
		response := cacheRequest(t, service, token, "/v1/docker-store",
			map[string]any{"architecture": architecture})
		if response.Code != http.StatusBadRequest {
			t.Errorf("architecture %q returned HTTP %d, want 400", architecture, response.Code)
		}
	}
	if storage.created != 0 || storage.cloned != 0 {
		t.Errorf("invalid architectures created/cloned %d/%d stores", storage.created, storage.cloned)
	}
}

func TestOrdinaryCacheRequestsCannotClaimTheDockerImageStoreNamespace(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	response := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "docker-images/amd64", "size_bytes": int64(1 << 30),
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("reserved Docker key returned HTTP %d: %s", response.Code, response.Body.String())
	}
	if storage.created != 0 || storage.cloned != 0 {
		t.Errorf("reserved Docker key created/cloned %d/%d stores", storage.created, storage.cloned)
	}
}

func TestCacheKeysAreNamespacedByDeployment(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}
	if len(storage.keys) != 2 || storage.keys[0] != "test-deployment/acme/api/npm" ||
		storage.keys[1] != storage.keys[0] {
		t.Errorf("store keys = %v", storage.keys)
	}
}

func TestARejectedGuestMountCanDiscardItsVolume(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d", attached.Code)
	}

	discarded := cacheRequest(t, service, token, "/v1/volumes/0/discard", nil)
	if discarded.Code != http.StatusOK {
		t.Fatalf("discard status = %d: %s", discarded.Code, discarded.Body.String())
	}
	if storage.discarded != 1 {
		t.Errorf("discarded %d volumes, want 1", storage.discarded)
	}
}

func TestACacheTokenCanActOnlyForItsOwnSession(t *testing.T) {
	t.Parallel()

	service, _, _ := testCacheService(t, provider.TrustTrusted)
	response := cacheRequest(t, service, "not-the-token", "/v1/volumes",
		map[string]any{"key": "acme/api/npm", "size_bytes": int64(10 << 30)})

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", response.Code)
	}
}

func TestAJobCannotAttachASixthVolume(t *testing.T) {
	t.Parallel()

	service, _, token := testCacheService(t, provider.TrustTrusted)
	for slot := range provider.MaxVolumes {
		response := cacheRequest(t, service, token, "/v1/volumes",
			map[string]any{"key": "key-" + string(rune('a'+slot)), "size_bytes": int64(1 << 30)})
		if response.Code != http.StatusCreated {
			t.Fatalf("volume %d status = %d", slot, response.Code)
		}
	}

	response := cacheRequest(t, service, token, "/v1/volumes",
		map[string]any{"key": "sixth", "size_bytes": int64(1 << 30)})
	if response.Code != http.StatusConflict {
		t.Fatalf("sixth volume status = %d", response.Code)
	}
}

func TestMountedGenerationsStayProtectedFromEviction(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	storage.current = "baseline"
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(1 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}
	until := time.Now().Add(7 * time.Hour)
	if err := service.RenewActive(t.Context(), until); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}
	if storage.renewed != 1 || !storage.renewedUntil.Equal(until) {
		t.Fatalf("renewals = %d until %s, want one until %s",
			storage.renewed, storage.renewedUntil, until)
	}
}

func TestRunnerBindsCacheAccessToTheComputeLifetime(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}
	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(), storage,
		&fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	a, host := newAllocatorWithHost(t)
	runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
	lease := assignedLease(t, a)
	if err := runner.Launch(t.Context(), lease, dockerSpec(), Job{
		RequestID: 11, Event: "push",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	spec := p.launched[0]
	if spec.CacheEndpoint != service.Endpoint() || spec.CacheToken == "" {
		t.Fatalf("guest cache credentials = endpoint %q token present %t",
			spec.CacheEndpoint, spec.CacheToken != "")
	}
	if spec.BuildKitCacheMountLimit != config.DefaultBuildKitCacheMountLimit {
		t.Errorf("guest BuildKit mount limit = %s, want %s", spec.BuildKitCacheMountLimit,
			config.DefaultBuildKitCacheMountLimit)
	}

	attached := cacheRequest(t, service, spec.CacheToken, "/v1/volumes",
		map[string]any{"key": "acme/api/npm", "size_bytes": int64(1 << 30)})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	if err := runner.Destroy(t.Context(), 11); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if storage.discarded != 1 {
		t.Errorf("discarded %d attached volumes when compute stopped, want 1", storage.discarded)
	}

	after := cacheRequest(t, service, spec.CacheToken, "/v1/volumes",
		map[string]any{"key": "acme/api/npm", "size_bytes": int64(1 << 30)})
	if after.Code != http.StatusUnauthorized {
		t.Errorf("expired compute token returned HTTP %d, want 401", after.Code)
	}
}

func TestRunnerBindsActionsInterceptionToTheStaticPoolScope(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	a, host := newAllocatorWithHost(t)
	runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
	lease := assignedLease(t, a)
	tier := dockerSpec()
	tier.Intercept = true
	tier.CacheScope = &config.CacheScope{Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"}
	if err := runner.Launch(t.Context(), lease, tier, Job{
		RequestID:   11,
		Event:       "push",
		Owner:       "other",
		Repository:  "unrelated",
		WorkflowRef: "other/unrelated/.github/workflows/ci.yml@refs/heads/main",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	spec := p.launched[0]
	if spec.ActionsProxy == "" || spec.ActionsCAPEM == "" {
		t.Fatalf("guest Actions interception credentials were incomplete: proxy=%q ca=%t",
			spec.ActionsProxy, spec.ActionsCAPEM != "")
	}
	if !strings.Contains(spec.ActionsProxy, spec.CacheToken) {
		t.Error("the proxy URL is not bound to this guest's ephemeral cache capability")
	}
	service.mu.Lock()
	session := service.byToken[spec.CacheToken]
	service.mu.Unlock()
	if session == nil {
		t.Fatal("cache session was not recorded")
	}
	if session.owner != "acme" || session.repository != "api" ||
		session.workflowRef != "acme/api/.github/workflows/ci.yml@refs/heads/main" {
		t.Fatalf("cache session scope = %s/%s %q, want the static pool scope",
			session.owner, session.repository, session.workflowRef)
	}
}

func TestRunnerLeavesUntrustedActionsTrafficEntirelyOnGitHub(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, acceptsAll: true}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	a, host := newAllocatorWithHost(t)
	runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
	lease := assignedLease(t, a)
	tier := dockerSpec()
	tier.Trust = config.WorkloadUntrusted
	tier.Intercept = true
	if err := runner.Launch(t.Context(), lease, tier, Job{
		RequestID:   11,
		Event:       "pull_request",
		Owner:       "acme",
		Repository:  "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/pull/1/merge",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	spec := p.launched[0]
	if spec.ActionsProxy != "" || spec.ActionsCAPEM != "" {
		t.Fatalf("untrusted guest received interception state: proxy=%q ca=%t",
			spec.ActionsProxy, spec.ActionsCAPEM != "")
	}
}

func TestRunnerCarriesTheNodesRegistryMirrorsToItsGuest(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderFirecracker}
	tier := dockerTier()
	tier.Provider = config.ProviderFirecracker
	a, err := alloc.New(openState(t), alloc.Limits{
		MaxVCPU: 32, MaxMemory: 128 * config.GiB,
	}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	const host = "firecracker-host"
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: host, Provider: config.ProviderFirecracker,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	runner := New(a, host, &fakeJIT{setID: 7}, p, nil,
		WithRegistryMirrors(config.RegistryMirrors{
			DockerIO: "https://docker-cache.home.example",
			GHCRIO:   "https://ghcr-cache.home.example",
			QuayIO:   "https://quay-cache.home.example",
		}))
	lease := assignedLease(t, a)
	if err := runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(tier, config.ProviderFirecracker), Job{
			RequestID: lease.RequestID, Event: "push",
		}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := p.launched[0].RegistryMirrors; got.DockerIO != "https://docker-cache.home.example" ||
		got.GHCRIO != "https://ghcr-cache.home.example" ||
		got.QuayIO != "https://quay-cache.home.example" {
		t.Fatalf("provider registry mirrors = %+v", got)
	}
}

// The historical name is retained for the mutation gate. The store is requested
// by the root guest agent after the VM starts, before it starts the runner.
func TestFirecrackerBootsWithAnArchitectureScopedDockerImageStore(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderFirecracker}
	storage := &fakeCacheStore{}
	attacher := &fakeVolumeAttacher{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(), storage,
		attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	tier := dockerTier()
	tier.Provider = config.ProviderFirecracker
	a, err := alloc.New(openState(t), alloc.Limits{
		MaxVCPU: 32, MaxMemory: 128 * config.GiB,
	}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	const host = "firecracker-host"
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: host, Provider: config.ProviderFirecracker,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
	lease := assignedLease(t, a)
	if err := runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(tier, config.ProviderFirecracker), Job{
			RequestID: lease.RequestID, Event: "push",
		}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(p.launched) != 1 || len(p.launched[0].Volumes) != 0 {
		t.Fatalf("launch volumes = %+v", p.launched)
	}
	response := cacheRequest(t, service, p.launched[0].CacheToken, "/v1/docker-store",
		map[string]any{"architecture": "amd64"})
	if response.Code != http.StatusCreated {
		t.Fatalf("guest Docker store request returned HTTP %d: %s", response.Code, response.Body.String())
	}
	if storage.created != 1 || len(attacher.attached) != 1 || attacher.attached[0] != 0 {
		t.Errorf("created %d Docker stores and attached slots %v, want one in slot 0",
			storage.created, attacher.attached)
	}
}

func TestCachePreparationFailureDoesNotFailTheJob(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}
	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	if err := os.RemoveAll(service.stateDir); err != nil {
		t.Fatalf("remove cache state directory: %v", err)
	}

	a, host := newAllocatorWithHost(t)
	runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
	lease := assignedLease(t, a)
	if err := runner.Launch(t.Context(), lease, dockerSpec(), Job{
		RequestID: lease.RequestID, Event: "push",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(p.launched) != 1 {
		t.Fatalf("launched %d jobs, want 1", len(p.launched))
	}
	if p.launched[0].CacheEndpoint != "" || p.launched[0].CacheToken != "" {
		t.Fatalf("cold launch retained cache credentials: %+v", p.launched[0])
	}
}

func TestCleanupDoesNotDiscardUntilTheClosedSessionIsDurable(t *testing.T) {
	service, storage, token := testCacheService(t, provider.TrustTrusted)
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(1 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}
	if err := os.RemoveAll(service.stateDir); err != nil {
		t.Fatalf("remove cache state directory: %v", err)
	}
	if err := service.Cleanup(t.Context(), "billet-one"); err == nil {
		t.Fatal("Cleanup succeeded without durably recording the closed session")
	}
	if storage.discarded != 0 {
		t.Fatalf("discarded %d volumes before the closed session was durable", storage.discarded)
	}
}

func TestCacheCustodySurvivesANodeProcessRestart(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	storage := &fakeCacheStore{}
	attacher := &fakeVolumeAttacher{}
	first, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("first NewCacheService: %v", err)
	}
	credentials, err := first.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token := credentials.Token
	attached := cacheRequest(t, first, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}
	custodyPath := filepath.Join(stateDir, cacheSessionDirectory, token+".json")
	custody, err := os.ReadFile(custodyPath)
	if err != nil {
		t.Fatalf("read cache custody before restart: %v", err)
	}
	if bytes.Contains(custody, []byte(`"ready_token"`)) {
		t.Fatal("cache custody still requires an intra-guest readiness credential")
	}

	second, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("second NewCacheService: %v", err)
	}
	committed := cacheRequest(t, second, token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK {
		t.Fatalf("recovered commit status = %d: %s", committed.Code, committed.Body.String())
	}
	if storage.published != 1 {
		t.Errorf("published %d recovered cache volumes, want 1", storage.published)
	}
}

func TestAnAmbiguousAttachIsDurableBeforeTheProviderCanMapIt(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	storage := &fakeCacheStore{}
	attacher := &fakeVolumeAttacher{
		attachErr: errors.New("attach response was lost"),
		detachErr: errors.New("cannot prove the device was detached"),
	}
	first, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("first NewCacheService: %v", err)
	}
	credentials, err := first.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token := credentials.Token
	attached := cacheRequest(t, first, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusServiceUnavailable {
		t.Fatalf("ambiguous attach status = %d: %s", attached.Code, attached.Body.String())
	}
	if storage.discarded != 0 {
		t.Fatal("an ambiguously attached volume was discarded before detachment was proved")
	}

	restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("restart NewCacheService: %v", err)
	}
	if err := restarted.Cleanup(t.Context(), "billet-one"); err != nil {
		t.Fatalf("Cleanup recovered attachment custody: %v", err)
	}
	if storage.discarded != 1 {
		t.Fatalf("recovered cleanup discarded %d volumes, want 1", storage.discarded)
	}
}

func TestCacheBearerTokensArePersistedOnlyInPrivateFiles(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token := credentials.Token

	path := filepath.Join(stateDir, cacheSessionDirectory, token+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat custody file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("cache custody mode = %o, want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache custody: %v", err)
	}
	if !bytes.Contains(body, []byte(credentials.Token)) {
		t.Fatal("cache custody did not preserve its session credential")
	}
}

func TestAClosedSessionRetriesFailedDiscardsAfterRestart(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	storage := &fakeCacheStore{discardFailures: 1}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token := credentials.Token
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	if err := service.Cleanup(t.Context(), "billet-one"); err == nil {
		t.Fatal("Cleanup succeeded while discard failed")
	}
	closed := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "another", "size_bytes": int64(1 << 30),
	})
	if closed.Code != http.StatusGone {
		t.Fatalf("closed session status = %d, want 410", closed.Code)
	}

	restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("restart NewCacheService: %v", err)
	}
	if err := restarted.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	after := cacheRequest(t, restarted, token, "/v1/volumes", map[string]any{
		"key": "another", "size_bytes": int64(1 << 30),
	})
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("fully discarded session status = %d, want 401", after.Code)
	}
	if storage.discarded != 2 {
		t.Errorf("discard attempts = %d, want 2", storage.discarded)
	}
}

func TestSuccessfulEmptyInventoryClosesPersistedCacheSessions(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-gone", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token := credentials.Token
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	if err := service.ReconcileInventory(t.Context(), nil); err != nil {
		t.Fatalf("ReconcileInventory: %v", err)
	}
	if storage.discarded != 1 {
		t.Fatalf("discarded = %d, want 1", storage.discarded)
	}
	response := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "another", "size_bytes": int64(1 << 30),
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("removed session status = %d, want 401", response.Code)
	}
}

func TestCacheSessionAdmissionIsBoundedBeforeStorage(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
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
	service.byToken[token].admit <- struct{}{}
	defer func() { <-service.byToken[token].admit }()

	response := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "blocked", "size_bytes": int64(1 << 30),
	})
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("admission status = %d, want 429", response.Code)
	}
	if storage.created != 0 || storage.cloned != 0 {
		t.Fatal("storage was reached after admission was refused")
	}
}
