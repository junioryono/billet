package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

type fakeCacheStore struct {
	current           string
	created           int
	cloned            int
	snapshots         int
	published         int
	discarded         int
	discardFailures   int
	renewed           int
	renewedUntil      time.Time
	publishErr        error
	advanceOnSnapshot bool
	publishExpected   []string
	keys              []string
	snapshotVolumes   []storecontract.Volume
}

func (f *fakeCacheStore) Current(context.Context, string) (string, error) {
	if f.current == "" {
		return "", storecontract.ErrMiss
	}

	return f.current, nil
}

func (f *fakeCacheStore) Create(_ context.Context, key string, _ int64) (storecontract.Volume, error) {
	f.created++
	f.keys = append(f.keys, key)

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
	return storecontract.WriterLease{Key: key, ID: "writer", Expires: time.Now().Add(ttl)}, 1, nil
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
	_, expected string,
	candidate storecontract.Candidate,
	_ storecontract.WriterLease,
	_ storecontract.FencingToken,
) error {
	f.published++
	f.publishExpected = append(f.publishExpected, expected)
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

	token, err := service.Prepare("billet-one", trust)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	return service, storage, token
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
	if err := runner.Launch(t.Context(), lease, nodeapi.TierSpecOf(tier), Job{
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

func TestFirecrackerBootsWithAnArchitectureScopedDockerImageStore(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderFirecracker}
	storage := &fakeCacheStore{}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(), storage,
		&fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
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
	if err := runner.Launch(t.Context(), lease, nodeapi.TierSpecOf(tier), Job{
		RequestID: lease.RequestID, Event: "push",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(p.launched) != 1 || len(p.launched[0].Volumes) != 1 {
		t.Fatalf("launch volumes = %+v", p.launched)
	}
	volume := p.launched[0].Volumes[0]
	if volume.Device != "/dev/rbd7" || volume.Path != "/var/lib/docker" {
		t.Errorf("Docker image store mount = %+v", volume)
	}
	if storage.created != 1 {
		t.Errorf("created %d cold Docker image stores, want 1", storage.created)
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
	token, err := first.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	attached := cacheRequest(t, first, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
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
	token, err := first.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
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
	token, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	path := filepath.Join(stateDir, cacheSessionDirectory, token+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat custody file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("cache custody mode = %o, want 600", got)
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
	token, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
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
