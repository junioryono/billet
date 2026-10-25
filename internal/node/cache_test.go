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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	publishHook        func()
	publishConflicts   int
	conflictCurrent    string
	advanceOnSnapshot  bool
	publishExpected    []string
	writerAcquisitions int
	writers            map[string]fakeWriter
	clock              func() time.Time
	publishedWriterIDs []string
	publishedFences    []storecontract.FencingToken
	keys               []string
	createdSizes       []int64
	// createBudgets and cloneBudgets are the time each Create and each Clone had
	// left before its deadline.
	createBudgets []time.Duration
	cloneBudgets  []time.Duration
	// discardDelay is how long each Discard works, and discardContextErrs what
	// its context said once that work was done: a cleanup whose context ended
	// part-way did not finish.
	discardDelay       time.Duration
	discardContextErrs []error
	snapshotVolumes    []storecontract.Volume
	snapshotErr        error
	snapshotHook       func()
	heldRefusals       int
	heldRefused        int
	heldHolder         string
	heldHolders        []string
	heldExpires        time.Time
	writerEvents       []string
	released           []string
	releaseCtxErrs     []error
	releaseDeadlines   []bool
	// createErr makes a fresh volume unobtainable, the way a read-only pool would.
	createErr error
	// damagedGenerations are the generations GenerationDamaged proves damaged,
	// damageErr what it answers when it could not tell, and damageChecks every
	// generation it was asked about.
	damagedGenerations map[string]bool
	damageErr          error
	damageChecks       []string
	damageKeys         []string
	// discardedVolumes are the volumes Discard was handed, in order.
	discardedVolumes []storecontract.Volume
}

func TestCacheWriterAuthorityOutlivesTheWholeCommitBudget(t *testing.T) {
	t.Parallel()

	const (
		actionCacheClientLimit = 13 * time.Minute
		minimumResponseMargin  = 15 * time.Second
	)

	if cacheCleanupMargin <= 0 || cacheWorkLimit+cacheCleanupMargin != cacheHandlerLimit {
		t.Fatalf("cache handler budget %s does not reserve cleanup margin %s",
			cacheHandlerLimit, cacheCleanupMargin)
	}
	if cacheWriterTTL <= cacheHandlerLimit {
		t.Fatalf("writer lease %s does not cover the %s handler budget", cacheWriterTTL, cacheHandlerLimit)
	}
	if cacheHandlerLimit+minimumResponseMargin > actionCacheClientLimit {
		t.Fatalf("cache handler budget %s leaves less than %s beneath the action's %s deadline",
			cacheHandlerLimit, minimumResponseMargin, actionCacheClientLimit)
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

func (f *fakeCacheStore) Create(ctx context.Context, key string, size int64) (storecontract.Volume, error) {
	if deadline, ok := ctx.Deadline(); ok {
		f.createBudgets = append(f.createBudgets, time.Until(deadline))
	}
	if f.createErr != nil {
		return storecontract.Volume{}, f.createErr
	}
	f.created++
	f.keys = append(f.keys, key)
	f.createdSizes = append(f.createdSizes, size)

	return storecontract.Volume{Key: key, Handle: "working", Device: "/dev/rbd7"}, nil
}

func (f *fakeCacheStore) Clone(ctx context.Context, key, generation string) (storecontract.Volume, error) {
	if deadline, ok := ctx.Deadline(); ok {
		f.cloneBudgets = append(f.cloneBudgets, time.Until(deadline))
	}
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

// fakeWriter is the writer record the fake keeps per key, the same facts the
// real stores keep: identity, fence, expiry and holder.
type fakeWriter struct {
	id      string
	fence   storecontract.FencingToken
	expires time.Time
	holder  string
}

func (f *fakeCacheStore) now() time.Time {
	if f.clock != nil {
		return f.clock()
	}

	return time.Now()
}

// AcquireWriter keeps the real stores' rule: a key with a live writer refuses,
// naming the holder. heldRefusals is a scripted refusal on top of that, for
// tests about the wait itself.
func (f *fakeCacheStore) AcquireWriter(
	_ context.Context, key, holder string, ttl time.Duration,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	if f.heldRefusals > 0 {
		f.heldRefusals--
		scripted := f.heldHolder
		if n := len(f.heldHolders); n > 0 {
			scripted = f.heldHolders[min(f.heldRefused, n-1)]
		}
		f.heldRefused++
		f.writerEvents = append(f.writerEvents, "refuse "+scripted)

		return storecontract.WriterLease{}, 0, &storecontract.WriterHeldError{
			Key: key, Holder: scripted, Expires: f.heldExpires,
		}
	}
	now := f.now()
	if live, ok := f.writers[key]; ok && now.Before(live.expires) {
		f.writerEvents = append(f.writerEvents, "refuse "+live.holder)

		return storecontract.WriterLease{}, 0, &storecontract.WriterHeldError{
			Key: key, Holder: live.holder, Expires: live.expires,
		}
	}
	f.writerAcquisitions++
	writerID := fmt.Sprintf("writer-%d", f.writerAcquisitions)
	f.writerEvents = append(f.writerEvents, "acquire "+writerID)
	fence := storecontract.FencingToken(f.writerAcquisitions)
	if f.writers == nil {
		f.writers = make(map[string]fakeWriter)
	}
	f.writers[key] = fakeWriter{id: writerID, fence: fence, expires: now.Add(ttl), holder: holder}

	return storecontract.WriterLease{Key: key, ID: writerID, Expires: now.Add(ttl)}, fence, nil
}

func (f *fakeCacheStore) Snapshot(
	_ context.Context, volume storecontract.Volume,
) (storecontract.Candidate, error) {
	f.snapshots++
	f.snapshotVolumes = append(f.snapshotVolumes, volume)
	if f.snapshotHook != nil {
		f.snapshotHook()
	}
	if f.snapshotErr != nil {
		return storecontract.Candidate{}, f.snapshotErr
	}
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
	if f.publishHook != nil {
		f.publishHook()
	}
	f.publishExpected = append(f.publishExpected, expected)
	f.publishedWriterIDs = append(f.publishedWriterIDs, lease.ID)
	f.publishedFences = append(f.publishedFences, fence)
	// The real stores check the lease's own validity under their lock before
	// the recorded triple, so an expired writer publishes nothing here either.
	if err := lease.ValidAt(key, f.now()); err != nil {
		return fmt.Errorf("%w: %w", storecontract.ErrConflict, err)
	}
	live, ok := f.writers[key]
	if !ok || lease.ID != live.id || fence != live.fence || !lease.Expires.Equal(live.expires) {
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
		// The same write that advances the pointer consumes the writer.
		f.current = candidate.Generation
		delete(f.writers, key)
	}

	return f.publishErr
}

// ReleaseWriter records the call and what it was given — the lease, whether
// the context it arrived on was still live, and whether it carried a deadline
// — and drops the record exactly as the real stores do: only on a match of
// identity, expiry and fence, so a release that is not exact leaves the key
// held and the next acquisition refused. It judges nothing else.
func (f *fakeCacheStore) ReleaseWriter(
	ctx context.Context, lease storecontract.WriterLease, fence storecontract.FencingToken,
) error {
	f.writerEvents = append(f.writerEvents, "release "+lease.ID)
	f.released = append(f.released, lease.ID)
	f.releaseCtxErrs = append(f.releaseCtxErrs, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	f.releaseDeadlines = append(f.releaseDeadlines, hasDeadline)
	if live, ok := f.writers[lease.Key]; ok && live.id == lease.ID && live.fence == fence &&
		live.expires.Equal(lease.Expires) {
		delete(f.writers, lease.Key)
	}

	return nil
}

func (f *fakeCacheStore) Discard(ctx context.Context, volume storecontract.Volume) error {
	f.discarded++
	f.discardedVolumes = append(f.discardedVolumes, volume)
	time.Sleep(f.discardDelay)
	f.discardContextErrs = append(f.discardContextErrs, ctx.Err())
	if f.discardFailures > 0 {
		f.discardFailures--

		return errors.New("temporary discard failure")
	}

	return nil
}

func (f *fakeCacheStore) Evict(context.Context, time.Duration) error { return nil }

func (f *fakeCacheStore) GenerationDamaged(_ context.Context, key, generation string) (bool, error) {
	f.damageChecks = append(f.damageChecks, generation)
	f.damageKeys = append(f.damageKeys, key)

	return f.damagedGenerations[generation], f.damageErr
}

type fakeVolumeAttacher struct {
	attached  []int
	detached  []int
	attachErr error
	detachErr error
	// attachBudgets is the time each AttachVolume had left before its deadline.
	// blockAttach makes AttachVolume wait for its context to end, and
	// attachDelay makes it take that long before answering attachErr.
	attachBudgets []time.Duration
	blockAttach   bool
	attachDelay   time.Duration
	// onAttach runs inside a successful AttachVolume, which is how a test makes
	// what follows the attach fail.
	onAttach func()
	// detachDelay is how long each DetachVolume works, and detachContextErrs
	// what its context said once that work was done.
	detachDelay       time.Duration
	detachContextErrs []error
}

func (f *fakeVolumeAttacher) AttachVolume(ctx context.Context, _ string, slot int, _ string) error {
	f.attached = append(f.attached, slot)
	if deadline, ok := ctx.Deadline(); ok {
		f.attachBudgets = append(f.attachBudgets, time.Until(deadline))
	}
	if f.blockAttach {
		<-ctx.Done()

		return ctx.Err()
	}
	time.Sleep(f.attachDelay)
	if f.attachErr == nil && f.onAttach != nil {
		f.onAttach()
	}

	return f.attachErr
}

func (f *fakeVolumeAttacher) DetachVolume(ctx context.Context, _ string, slot int, _ string) error {
	f.detached = append(f.detached, slot)
	time.Sleep(f.detachDelay)
	f.detachContextErrs = append(f.detachContextErrs, ctx.Err())

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

	return cacheRequestWithin(t, t.Context(), service, token, path, body)
}

// cacheRequestWithin is cacheRequest on a request context the caller chooses,
// which is how a guest that hangs up is staged.
func cacheRequestWithin(
	t *testing.T,
	ctx context.Context,
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

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, input)
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

// THE DOCKER STORE IS ACQUIRED INSIDE THE GUEST'S TOKEN AND ATTACHED INSIDE THE
// GUEST'S WAIT. The guest asks for it before starting its runner; an image built
// before #320's fix does so on a metadata token that lives five minutes, and a
// store acquired under the twelve-minute cache bound outlived it on 2026-10-03,
// so the runner never started. The attach is bounded by the whole request's
// limit, which the guest waits just past.
func TestTheDockerStoreIsAcquiredAndAttachedWithinItsBounds(t *testing.T) {
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

	if response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"}); response.Code != http.StatusCreated {
		t.Fatalf("Docker store status = %d: %s", response.Code, response.Body.String())
	}

	// THE CLONE TOO, which is where the store waited behind the cache index lock.
	for name, budgets := range map[string][]time.Duration{
		"cloned": storage.cloneBudgets, "created": storage.createBudgets,
	} {
		if len(budgets) != 1 {
			t.Fatalf("the store was %s %d time(s) with a deadline, want 1", name, len(budgets))
		}
		if got := budgets[0]; got > dockerStoreAttachLimit || got < dockerStoreAttachLimit-5*time.Second {
			t.Fatalf("the Docker store was %s with %s left, want about %s", name, got, dockerStoreAttachLimit)
		}
	}
	if len(attacher.attachBudgets) != 1 {
		t.Fatalf("the store was attached %d time(s) with a deadline, want 1", len(attacher.attachBudgets))
	}
	if got := attacher.attachBudgets[0]; got > dockerStoreRequestLimit ||
		got < dockerStoreRequestLimit-5*time.Second {
		t.Fatalf("the Docker store was attached with %s left, want about %s", got, dockerStoreRequestLimit)
	}
}

// AN ATTACH THE REQUEST'S BOUND CUTS SHORT IS A COLD JOB WHOSE VOLUME STAYS IN
// CUSTODY. Nothing proves such an attach failed, EC2 can finish one it accepted,
// so the node answers at once and does not race it with a detach; the session's
// cleanup, once the compute is gone, is what discards the volume.
func TestADockerStoreAttachCutShortKeepsItsVolumeInCustody(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := &fakeCacheStore{}
		attacher := &fakeVolumeAttacher{blockAttach: true}
		service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
			storage, attacher, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService: %v", err)
		}
		credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}

		start := time.Now()
		response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"})
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("Docker store status = %d, want the cold 503: %s", response.Code, response.Body.String())
		}
		if waited := time.Since(start); waited != dockerStoreRequestLimit {
			t.Fatalf("the request answered after %s, want exactly its limit, %s", waited, dockerStoreRequestLimit)
		}
		if len(attacher.detached) != 0 || storage.discarded != 0 {
			t.Fatalf("an attach of unknown outcome was raced: %d detach(es), %d discard(s)",
				len(attacher.detached), storage.discarded)
		}
		if again := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"}); again.Code != http.StatusConflict {
			t.Fatalf("a second request found the slot free (%d), so custody of the first volume was dropped: %s",
				again.Code, again.Body.String())
		}

		// NOTHING WAITS TO SETTLE IT. The guest never mounted the store, so it
		// will never prove it quiesced; a settlement that waited for that held
		// the job's teardown for the whole settle window.
		settling := time.Now()
		if err := service.SettleDocker(t.Context(), "billet-one", true); err != nil {
			t.Fatalf("SettleDocker: %v", err)
		}
		if waited := time.Since(settling); waited != 0 {
			t.Fatalf("the settlement of a store the guest never mounted waited %s", waited)
		}
		if storage.published != 0 {
			t.Fatalf("a store the guest never mounted was published %d time(s)", storage.published)
		}

		if err := service.Cleanup(t.Context(), "billet-one"); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		if storage.discarded != 1 {
			t.Fatalf("the session's cleanup discarded %d volume(s), want the one it kept", storage.discarded)
		}
	})
}

// AN ATTACH THAT FAILED OUTRIGHT IS NO MORE PROOF THAN ONE CUT SHORT. EC2's
// own timeouts and describes can fail after it accepted the attach, so the
// volume stays in custody, the guest is answered at once, and the session's
// cleanup is what discards it.
func TestAFailedDockerStoreAttachKeepsItsVolumeInCustody(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	attacher := &fakeVolumeAttacher{attachErr: errors.New("the VMM refused the drive")}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("Docker store status = %d, want the cold 503: %s", response.Code, response.Body.String())
	}
	if len(attacher.attached) != 1 || len(attacher.detached) != 0 || storage.discarded != 0 {
		t.Fatalf("attached %d, detached %d, discarded %d: want one failed attach and the volume kept",
			len(attacher.attached), len(attacher.detached), storage.discarded)
	}
	if readiness := cacheRequest(t, service, credentials.Token, "/v1/docker-store/ready", map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
	}); readiness.Code != http.StatusNotFound {
		t.Fatalf("readiness for a store the guest never mounted = %d, want 404: %s",
			readiness.Code, readiness.Body.String())
	}

	if err := service.Cleanup(t.Context(), "billet-one"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if storage.discarded != 1 {
		t.Fatalf("the session's cleanup discarded %d volume(s), want the one it kept", storage.discarded)
	}
}

// A STORE WHOSE ATTACH FAILED IS NOT THE GUEST'S TO DISCARD. The guest's
// discard detaches and discards at once, which for an attach EC2 may still
// complete is the race the attach handler stopped running; the session's
// cleanup takes the store once the compute is gone.
func TestAGuestCannotDiscardAStoreThatWasNeverMounted(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	attacher := &fakeVolumeAttacher{attachErr: errors.New("the VMM refused the drive")}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"}); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("Docker store status = %d, want the cold 503: %s", response.Code, response.Body.String())
	}

	discard := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/discard", nil)
	if discard.Code != http.StatusConflict {
		t.Fatalf("the guest's discard of a never-mounted store = %d, want 409: %s", discard.Code, discard.Body.String())
	}
	if len(attacher.detached) != 0 || storage.discarded != 0 {
		t.Fatalf("the guest's discard detached %d and discarded %d; want the store left in custody",
			len(attacher.detached), storage.discarded)
	}
}

// A STORE THE GUEST WAS NOT DURABLY GIVEN IS ONE IT DOES NOT GET. The attach
// succeeded but recording it as mounted failed, so a restart would not know
// the guest has it; the guest is sent cold, and the store stays out of
// settlement and in custody.
func TestAStoreThatCouldNotBeRecordedAsMountedSendsTheGuestCold(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory without write permission, so the session cannot be made to fail to persist")
	}

	storage := &fakeCacheStore{}
	stateDir := t.TempDir()
	sessions := filepath.Join(stateDir, cacheSessionDirectory)
	attacher := &fakeVolumeAttacher{onAttach: func() {
		if err := os.Chmod(sessions, 0o500); err != nil {
			t.Errorf("make the session directory read-only: %v", err)
		}
	}}
	t.Cleanup(func() {
		if err := os.Chmod(sessions, 0o700); err != nil {
			t.Errorf("make the session directory writable again: %v", err)
		}
	})
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		storage, attacher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("Docker store status = %d, want the cold 503 for a store not recorded as mounted: %s",
			response.Code, response.Body.String())
	}
	if len(attacher.attached) != 1 {
		t.Fatalf("attached %d time(s), want the one attach that succeeded", len(attacher.attached))
	}
	if readiness := cacheRequest(t, service, credentials.Token, "/v1/docker-store/ready", map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
	}); readiness.Code != http.StatusNotFound {
		t.Fatalf("readiness for a store the guest was sent cold from = %d, want 404: %s",
			readiness.Code, readiness.Body.String())
	}
	if storage.discarded != 0 {
		t.Fatalf("discarded %d volume(s) before the compute was gone, want the store kept", storage.discarded)
	}
}

// A RESTARTED NODE KEEPS A NEVER-MOUNTED STORE OUT OF SETTLEMENT. The store is
// recorded as never mounted from the moment it is in custody, so the record a
// restart reads says so whatever happened to the attach, and nothing waits for
// a readiness proof a cold guest will never send.
func TestARestartedNodeKeepsANeverMountedStoreOutOfSettlement(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		stateDir := t.TempDir()
		first, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
			&fakeCacheStore{}, &fakeVolumeAttacher{attachErr: errors.New("the VMM refused the drive")},
			slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService: %v", err)
		}
		credentials, err := first.Prepare("billet-one", provider.TrustTrusted)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if response := cacheRequest(t, first, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"}); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("Docker store status = %d, want the cold 503: %s", response.Code, response.Body.String())
		}

		storage := &fakeCacheStore{}
		restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
			storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService after the restart: %v", err)
		}

		settling := time.Now()
		if err := restarted.SettleDocker(t.Context(), "billet-one", true); err != nil {
			t.Fatalf("SettleDocker after the restart: %v", err)
		}
		if waited := time.Since(settling); waited != 0 {
			t.Fatalf("the restarted node waited %s to settle a store the guest never mounted", waited)
		}
		if err := restarted.Cleanup(t.Context(), "billet-one"); err != nil {
			t.Fatalf("Cleanup after the restart: %v", err)
		}
		if storage.discarded != 1 {
			t.Fatalf("the restarted node's cleanup discarded %d volume(s), want the one recorded", storage.discarded)
		}
	})
}

// A SETTLEMENT FOLLOWS THE STORE IT OPENED AND NO OTHER. The guest can discard
// that store and ask for another while settlement waits; the replacement was
// never opened for settlement, so the readiness route refuses it as not
// settling, and waiting on it would hold the job's teardown for the whole
// settle window.
func TestASettlementStopsWaitingWhenItsStoreIsReplaced(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
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
		if response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"}); response.Code != http.StatusCreated {
			t.Fatalf("Docker store status = %d: %s", response.Code, response.Body.String())
		}

		start := time.Now()
		settled := make(chan error, 1)
		go func() { settled <- service.SettleDocker(t.Context(), "billet-one", true) }()
		synctest.Wait()

		if discard := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/discard", nil); discard.Code != http.StatusOK {
			t.Fatalf("the guest's discard of its mounted store = %d: %s", discard.Code, discard.Body.String())
		}
		// THE REPLACEMENT MOUNTS. It is not never-mounted, so only following the
		// store the settlement opened tells the two apart.
		if again := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"}); again.Code != http.StatusCreated {
			t.Fatalf("the replacement's status = %d, want 201: %s", again.Code, again.Body.String())
		}

		if err := <-settled; err != nil {
			t.Fatalf("SettleDocker: %v", err)
		}
		if waited := time.Since(start); waited >= dockerSettleWait {
			t.Fatalf("the settlement waited %s on a store it never opened", waited)
		}
		if storage.published != 0 {
			t.Fatalf("published %d store(s); neither the discarded nor the failed one was the guest's to publish",
				storage.published)
		}
	})
}

// A SETTLEMENT'S WAIT FOR THE SESSION IS INSIDE ITS WINDOW. A replacement
// request holds the session through its acquisition and attach, which can run
// for minutes, and the job's teardown waits on the settlement; a wait for the
// lock on the caller's context outlasted the two minutes the settlement is
// allowed. Two orders: the replacement takes the session while the settlement
// waits, and before the settlement has begun.
func TestASettlementWaitsForTheSessionOnlyWithinItsWindow(t *testing.T) {
	t.Parallel()

	for _, replacementFirst := range []bool{false, true} {
		name := "replacement while settling"
		if replacementFirst {
			name = "replacement before settling"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
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
				if response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
					map[string]any{"architecture": "amd64"}); response.Code != http.StatusCreated {
					t.Fatalf("Docker store status = %d: %s", response.Code, response.Body.String())
				}

				settled := make(chan error, 1)
				var start time.Time
				settle := func() {
					start = time.Now()
					go func() { settled <- service.SettleDocker(t.Context(), "billet-one", true) }()
					synctest.Wait()
				}
				replaced := make(chan int, 1)
				replace := func() {
					if discard := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/discard", nil); discard.Code != http.StatusOK {
						t.Fatalf("the guest's discard of its mounted store = %d: %s", discard.Code, discard.Body.String())
					}
					// THE REPLACEMENT HOLDS THE SESSION FOR THREE MINUTES, past the window.
					attacher.attachDelay = 3 * time.Minute
					go func() {
						replaced <- cacheRequest(t, service, credentials.Token, "/v1/docker-store",
							map[string]any{"architecture": "amd64"}).Code
					}()
					synctest.Wait()
				}
				if replacementFirst {
					replace()
					settle()
				} else {
					settle()
					replace()
				}

				err = <-settled
				if err == nil || err.Error() != "docker image store did not become ready before teardown" {
					t.Fatalf("SettleDocker = %v, want the settle window's own refusal", err)
				}
				if waited := time.Since(start); waited != dockerSettleWait {
					t.Fatalf("the settlement waited %s behind a request holding its session; want exactly its window, %s",
						waited, dockerSettleWait)
				}
				if code := <-replaced; code != http.StatusCreated {
					t.Fatalf("the replacement's status = %d, want 201", code)
				}
			})
		})
	}
}

// A RECORD INSTALLED AS MOUNTED, WHOSE WRITE THEN FAILED, IS WRITTEN BACK AS
// NEVER MOUNTED. The write failed at the directory sync, after its rename, so
// "mounted" was already the record on disk while the guest was sent cold; a
// restart reading it would wait on a readiness proof that cannot come. The
// image observation is already recorded, so nothing else rewrites the session
// and the record a restart reads is the one the failure path left.
func TestAMountedRecordThatCouldNotBeSyncedIsWrittenBackAsNeverMounted(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		stateDir := t.TempDir()
		var failSync atomic.Bool
		attacher := &fakeVolumeAttacher{onAttach: func() { failSync.Store(true) }}
		first, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
			&fakeCacheStore{}, attacher, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService: %v", err)
		}
		first.syncDir = func(path string) error {
			if failSync.CompareAndSwap(true, false) {
				return errors.New("the directory could not be synced")
			}

			return syncDirectory(path)
		}
		credentials, err := first.Prepare("billet-one", provider.TrustTrusted)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		first.byToken[credentials.Token].observed.ImageCache = string(alloc.ImageCacheWarm)

		if response := cacheRequest(t, first, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"}); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("Docker store status = %d, want the cold 503: %s", response.Code, response.Body.String())
		}
		if failSync.Load() {
			t.Fatal("the directory sync was never failed, so this staged nothing")
		}

		restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
			&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService after the restart: %v", err)
		}
		settling := time.Now()
		if err := restarted.SettleDocker(t.Context(), "billet-one", true); err != nil {
			t.Fatalf("SettleDocker after the restart: %v", err)
		}
		if waited := time.Since(settling); waited != 0 {
			t.Fatalf("the restarted node waited %s on a store the guest was sent cold from", waited)
		}
	})
}

// AN ATTACH THAT SUCCEEDS AFTER THE REQUEST'S BOUND IS NOT HANDED OVER. The
// guest gives up twenty seconds later, so a store recorded as mounted then is
// one a cold guest never proves ready; it stays never mounted, in custody.
func TestADockerStoreAttachedAfterTheRequestsBoundIsAnsweredCold(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := &fakeCacheStore{}
		attacher := &fakeVolumeAttacher{attachDelay: dockerStoreRequestLimit + time.Second}
		service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
			storage, attacher, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService: %v", err)
		}
		credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}

		response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"})
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("Docker store status = %d, want the cold 503 for an attach past the bound: %s",
				response.Code, response.Body.String())
		}
		if readiness := cacheRequest(t, service, credentials.Token, "/v1/docker-store/ready", map[string]any{
			"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
		}); readiness.Code != http.StatusNotFound {
			t.Fatalf("readiness for a store attached past the bound = %d, want 404: %s",
				readiness.Code, readiness.Body.String())
		}
		if len(attacher.detached) != 0 || storage.discarded != 0 {
			t.Fatalf("detached %d, discarded %d before the compute was gone; want the store kept",
				len(attacher.detached), storage.discarded)
		}
	})
}

// THE ONE CLEANUP LEFT IN THE HANDLER OUTLIVES THE GUEST HANGING UP. A clone
// the session could not record is discarded before the answer, and the guest
// stops waiting twenty seconds past the request's bound, which ends the
// request's context; a discard cancelled with it leaves a volume nothing
// records.
func TestADockerStoreDiscardOutlivesTheGuestHangingUp(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory without write permission, so the session cannot be made to fail to persist")
	}

	synctest.Test(t, func(t *testing.T) {
		storage := &fakeCacheStore{discardDelay: time.Minute}
		stateDir := t.TempDir()
		service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
			storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewCacheService: %v", err)
		}
		credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		// THE SESSION CAN NO LONGER BE PERSISTED, so the clone is discarded.
		sessions := filepath.Join(stateDir, cacheSessionDirectory)
		if err := os.Chmod(sessions, 0o500); err != nil {
			t.Fatalf("make the session directory read-only: %v", err)
		}
		t.Cleanup(func() {
			// Writable again, or the temporary directory cannot be removed.
			if err := os.Chmod(sessions, 0o700); err != nil {
				t.Errorf("make the session directory writable again: %v", err)
			}
		})

		guest, hangUp := context.WithTimeout(t.Context(), 10*time.Second)
		defer hangUp()
		response := cacheRequestWithin(t, guest, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"})
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("Docker store status = %d, want the cold 503: %s", response.Code, response.Body.String())
		}
		if guest.Err() == nil {
			t.Fatal("the guest had not hung up by the time the discard ended, so this staged nothing")
		}
		if storage.discarded != 1 || storage.discardContextErrs[0] != nil {
			t.Fatalf("discarded %d volume(s) with context errors %v, want 1 finished on a live context",
				storage.discarded, storage.discardContextErrs)
		}
	})
}

// A SESSION HELD BY SOMETHING ELSE COSTS THE GUEST NO MORE THAN THE REQUEST'S
// LIMIT. The lock is the first step toward the answer, and a wait for it on the
// twelve-minute cache bound would outlast the guest's own wait.
func TestADockerStoreRequestWaitsForItsSessionOnlyUntilItsLimit(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
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
		session := service.byToken[credentials.Token]
		session.mu.Lock()
		defer session.mu.Unlock()

		start := time.Now()
		response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"})
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("Docker store status = %d, want the cold 503: %s", response.Code, response.Body.String())
		}
		if waited := time.Since(start); waited != dockerStoreRequestLimit {
			t.Fatalf("the request answered after %s, want exactly its limit, %s", waited, dockerStoreRequestLimit)
		}
		if storage.cloned+storage.created != 0 {
			t.Fatalf("the store was touched (%d clones, %d creates) by a request that never held its session",
				storage.cloned, storage.created)
		}
	})
}

// THE ACQUISITION GETS WHAT IS LEFT OF THE REQUEST, NOT A FRESH THREE MINUTES.
// A request that waited two minutes for its session has two left, and an
// acquisition given its own whole bound would run past the guest's wait.
func TestADockerStoreAcquisitionIsBoundedByWhatIsLeftOfTheRequest(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
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
		session := service.byToken[credentials.Token]
		session.mu.Lock()
		held := 2 * time.Minute
		time.AfterFunc(held, session.mu.Unlock)

		if response := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"}); response.Code != http.StatusCreated {
			t.Fatalf("Docker store status = %d: %s", response.Code, response.Body.String())
		}
		budgets := append(append([]time.Duration{}, storage.cloneBudgets...), storage.createBudgets...)
		if len(budgets) == 0 {
			t.Fatal("the store was never asked for a volume with a deadline")
		}
		for _, budget := range budgets {
			if budget > dockerStoreRequestLimit-held {
				t.Fatalf("the acquisition had %s left after the session was held for %s; want at most %s",
					budget, held, dockerStoreRequestLimit-held)
			}
		}
	})
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

func TestOrdinaryCacheRequestsCannotClaimTheActionsNamespace(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	response := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "actions-cache/scope/version/key", "size_bytes": int64(1 << 30),
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("reserved Actions key returned HTTP %d: %s", response.Code, response.Body.String())
	}
	if storage.created != 0 || storage.cloned != 0 {
		t.Fatalf("reserved Actions key allocated storage: created=%d cloned=%d", storage.created, storage.cloned)
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

// A LEASE IS RENEWED ONLY WHEN IT IS DUE. Each renewal takes the site's one cache
// index lock, and renewing every volume seven hours ahead on every five-minute
// pass was lock traffic a guest's Docker image store queued behind (#320).
func TestAnActiveLeaseIsRenewedOnlyWhenItIsDue(t *testing.T) {
	t.Parallel()

	service, storage, token := testCacheService(t, provider.TrustTrusted)
	storage.current = "baseline"

	now := time.Now()
	service.now = func() time.Time { return now }

	if attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(1 << 30),
	}); attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	// The clone's lease has an hour left, so the first pass renews it.
	if err := service.RenewActive(t.Context(), now.Add(7*time.Hour)); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}
	if storage.renewed != 1 {
		t.Fatalf("renewals after a due lease = %d, want 1", storage.renewed)
	}

	// Five minutes later it has nearly seven hours left and is not due.
	now = now.Add(5 * time.Minute)
	if err := service.RenewActive(t.Context(), now.Add(7*time.Hour)); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}
	if storage.renewed != 1 {
		t.Fatalf("renewals five minutes later = %d, want still 1", storage.renewed)
	}

	// Once less than cacheRenewWithin is left, it is renewed again.
	now = now.Add(7*time.Hour - cacheRenewWithin)
	if err := service.RenewActive(t.Context(), now.Add(7*time.Hour)); err != nil {
		t.Fatalf("RenewActive: %v", err)
	}
	if storage.renewed != 2 {
		t.Fatalf("renewals once due again = %d, want 2", storage.renewed)
	}
}

// THE GUEST IS TOLD WHICH BUILD CACHES ITS TIER ENABLES, and only with a cache
// session to serve them.
func TestRunnerTellsTheGuestWhichBuildCachesItsTierEnables(t *testing.T) {
	enabled := true
	for name, tc := range map[string]struct {
		cache   *config.TierCache
		service bool
		want    []provider.GuestCache
	}{
		"go and bazel": {
			cache: &config.TierCache{
				Go:    &config.GoCache{Enabled: &enabled, TestResults: true},
				Bazel: &config.CacheToggle{Enabled: &enabled},
				Git:   &config.CacheToggle{Enabled: &enabled},
			},
			service: true,
			want: []provider.GuestCache{provider.GuestCacheGo, provider.GuestCacheGoTestResults,
				provider.GuestCacheBazel, provider.GuestCacheGit},
		},
		"no cache block": {service: true},
		"no cache service": {
			cache: &config.TierCache{Go: &config.GoCache{Enabled: &enabled}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := &fakeProvider{kind: config.ProviderDocker}
			var options []Option
			if tc.service {
				service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
					&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
				if err != nil {
					t.Fatalf("NewCacheService: %v", err)
				}
				options = append(options, WithCacheService(service))
			}
			tier := dockerTier()
			tier.Cache = tc.cache
			a, host := newAllocatorWithHost(t)
			runner := New(a, host, &fakeJIT{setID: 7}, p, nil, options...)
			if err := runner.Launch(t.Context(), assignedLease(t, a),
				nodeapi.TierSpecOf(tier, config.ProviderDocker), Job{RequestID: 11, Event: "push"}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if got := p.launched[0].GuestCaches; !slices.Equal(got, tc.want) {
				t.Errorf("guest caches = %v, want %v", got, tc.want)
			}
		})
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

	// CLOSED WHEN THE COMPUTE STOPPED: the token attaches nothing more, and the
	// volume waits for the retry loop rather than holding up the teardown.
	closed := cacheRequest(t, service, spec.CacheToken, "/v1/volumes",
		map[string]any{"key": "acme/api/npm", "size_bytes": int64(1 << 30)})
	if closed.Code == http.StatusCreated {
		t.Fatal("a stopped compute's token still attached a volume")
	}
	if storage.discarded != 0 {
		t.Errorf("discarded %d volumes on the teardown path, want the discard left to RetryClosed",
			storage.discarded)
	}

	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.discarded != 1 {
		t.Errorf("discarded %d attached volumes after the compute stopped, want 1", storage.discarded)
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

		// Unreachable; see internal/node/reap_test.go for why it is written.
		return
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
	// CLOSED, NOT DISCARDED: the discard belongs to the retry loop, off the path
	// that proved the compute gone.
	if storage.discarded != 0 {
		t.Fatalf("discarded = %d while reconciling, want the discard left to RetryClosed", storage.discarded)
	}
	select {
	case <-service.ClosedSessions():
	default:
		t.Fatal("closing a session with volumes left to discard did not wake the retry loop")
	}
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
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
