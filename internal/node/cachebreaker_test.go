package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lease"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// hangingStore stops answering when told to: what a site's store does when its OSDs
// or its monitors go, which is to wait rather than refuse. Everything asked while it
// hangs waits for its context or for release.
type hangingStore struct {
	*fakeCacheStore
	hanging atomic.Bool
	release chan struct{}
	mu      sync.Mutex
	asked   int
	// writerErr is what asking for a key's writer comes to, when it fails.
	writerErr error
}

func newHangingStore() *hangingStore {
	return &hangingStore{fakeCacheStore: &fakeCacheStore{current: "g1"}, release: make(chan struct{})}
}

func (h *hangingStore) wait(ctx context.Context) error {
	h.mu.Lock()
	h.asked++
	h.mu.Unlock()
	if !h.hanging.Load() {
		return nil
	}
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hangingStore) askedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.asked
}

func (h *hangingStore) Clone(ctx context.Context, key, generation string) (storecontract.Volume, error) {
	if err := h.wait(ctx); err != nil {
		return storecontract.Volume{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.fakeCacheStore.Clone(ctx, key, generation)
}

func (h *hangingStore) Create(ctx context.Context, key string, size int64) (storecontract.Volume, error) {
	if err := h.wait(ctx); err != nil {
		return storecontract.Volume{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.fakeCacheStore.Create(ctx, key, size)
}

func (h *hangingStore) Current(ctx context.Context, key string) (string, error) {
	if err := h.wait(ctx); err != nil {
		return "", err
	}

	return "", storecontract.ErrMiss
}

func (h *hangingStore) Match(ctx context.Context, exact string, restore []string) (string, string, error) {
	if err := h.wait(ctx); err != nil {
		return "", "", err
	}

	return h.fakeCacheStore.Match(ctx, exact, restore)
}

func (h *hangingStore) AcquireWriter(
	ctx context.Context, key, holder string, ttl time.Duration,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	if h.writerErr != nil {
		return storecontract.WriterLease{}, 0, h.writerErr
	}

	return h.fakeCacheStore.AcquireWriter(ctx, key, holder, ttl)
}

func (h *hangingStore) Discard(ctx context.Context, volume storecontract.Volume) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.fakeCacheStore.Discard(ctx, volume)
}

func (h *hangingStore) discards() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.discarded
}

// storeReports is the control plane's end of what a breaker said.
type storeReports struct {
	mu     sync.Mutex
	refuse atomic.Bool
	got    []lease.CacheStoreReport
	bounds []time.Duration
}

func (r *storeReports) ReportCacheStore(ctx context.Context, report lease.CacheStoreReport) error {
	if deadline, ok := ctx.Deadline(); ok {
		r.mu.Lock()
		r.bounds = append(r.bounds, time.Until(deadline))
		r.mu.Unlock()
	}
	if r.refuse.Load() {
		return errors.New("the plane is away")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, report)

	return nil
}

func (r *storeReports) states() []lease.CacheStoreState {
	r.mu.Lock()
	defer r.mu.Unlock()

	states := make([]lease.CacheStoreState, 0, len(r.got))
	for _, report := range r.got {
		states = append(states, report.State)
	}

	return states
}

const testStoreDeadline = time.Minute

func guardedService(t *testing.T, storage storecontract.Store) *CacheService {
	t.Helper()

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.GuardStore("ceph", testStoreDeadline)
	service.actionIO = &fakeActionsVolumeManager{}

	return service
}

func dockerStoreFor(t *testing.T, service *CacheService, instance string) *httptest.ResponseRecorder {
	t.Helper()

	credentials, err := service.PrepareScoped(instance, CacheSessionScope{Trust: provider.TrustTrusted})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return cacheRequest(t, service, credentials.Token, "/v1/docker-store", map[string]any{"architecture": "amd64"})
}

func imageCacheOf(service *CacheService, instance string) alloc.ImageCache {
	session := service.sessionOf(instance)
	session.mu.Lock()
	defer session.mu.Unlock()

	return alloc.ImageCache(session.observed.ImageCache)
}

// onProbation puts a breaker where an answered probe does.
func onProbation(b *storeBreaker) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.state, b.since, b.trialUntil = lease.CacheStoreProbation, time.Now(), time.Time{}
}

// volumesAsked is how many volumes the store has been asked for.
func (h *hangingStore) volumesAsked() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.cloned + h.created
}

// anActionsSession is a job the Actions cache serves.
func anActionsSession(t *testing.T, service *CacheService, instance string) (*cacheSession, string) {
	t.Helper()

	credentials, err := service.PrepareScoped(instance, CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
		Cache: &config.CacheSpec{
			Publish: config.CachePublishTrustedOnly, Docker: config.CacheSetting{Enabled: true},
			StickyDisks: config.CacheSetting{Enabled: true}, Go: config.CacheSetting{Enabled: true},
			Actions: config.CacheSetting{Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service.sessionOf(instance), credentials.Token
}

func sessionLate(service *CacheService, instance string) int {
	session := service.sessionOf(instance)
	session.mu.Lock()
	defer session.mu.Unlock()

	return len(session.late)
}

// THE WHOLE OF WHAT #531 ASKS. A store that hangs holds a launch for a deadline and no
// longer; three of those stop anyone asking, so the next launch starts cold at once and
// says why; what came back late is discarded; and a store that answers again is used.
func TestAHangingStoreCostsADeadlineThenNothingAndComesBack(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		storage := newHangingStore()
		service := guardedService(t, storage)
		reports := &storeReports{}
		guard := make(chan struct{})
		go func() {
			defer close(guard)
			service.RunStoreGuard(ctx, reports)
		}()

		// A STORE THAT ANSWERS IS USED, and the first job's deadline does not move it.
		if got := dockerStoreFor(t, service, "billet-warm"); got.Code != http.StatusCreated {
			t.Fatalf("a store that answers: status %d: %s", got.Code, got.Body.String())
		}
		if got := imageCacheOf(service, "billet-warm"); got != alloc.ImageCacheWarm {
			t.Fatalf("a store that answers: observed %q", got)
		}

		storage.hanging.Store(true)

		// EACH WAITS THE DEADLINE, TO THE TICK, AND GOES COLD.
		for i, instance := range []string{"billet-one", "billet-two", "billet-three"} {
			start := time.Now()
			got := dockerStoreFor(t, service, instance)
			if got.Code != http.StatusServiceUnavailable {
				t.Fatalf("launch %d: status %d, want the cold 503: %s", i, got.Code, got.Body.String())
			}
			if waited := time.Since(start); waited != testStoreDeadline {
				t.Fatalf("launch %d waited %s, want the deadline, %s", i, waited, testStoreDeadline)
			}
			if got := imageCacheOf(service, instance); got != alloc.ImageCacheUnavailable {
				t.Fatalf("launch %d: observed %q", i, got)
			}
		}
		synctest.Wait()
		if open := service.breaker.isOpen(); !open {
			t.Fatal("three deadlines left the breaker closed")
		}
		// LAST, because a retry of the first report may already have said closed.
		if got := reports.states(); len(got) == 0 || got[len(got)-1] != lease.CacheStoreOpen {
			t.Fatalf("the plane was told %v, want open last", got)
		}
		report := service.breaker.report()
		if report.Failures != breakerFailures || report.Reason != lease.CacheStoreTimeout {
			t.Fatalf("the report = %+v", report)
		}

		// THE NEXT LAUNCH STARTS COLD NOW, AND THE STORE IS NOT ASKED.
		asked := storage.askedCount()
		start := time.Now()
		got := dockerStoreFor(t, service, "billet-four")
		if got.Code != http.StatusServiceUnavailable {
			t.Fatalf("with the breaker open: status %d: %s", got.Code, got.Body.String())
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("with the breaker open a launch waited %s", waited)
		}
		if got := imageCacheOf(service, "billet-four"); got != alloc.ImageCacheBypassed {
			t.Fatalf("with the breaker open: observed %q", got)
		}
		if storage.askedCount() != asked {
			t.Fatal("with the breaker open the store was asked")
		}

		// WHAT COMES BACK LATE WAS NEVER THE GUEST'S: it is in custody, then discarded.
		discarded := storage.discards()
		storage.hanging.Store(false)
		close(storage.release)
		synctest.Wait()
		// TWO, AND NOT THREE: three deadlines on, the first call has reached its own
		// three-minute bound and ended with nothing, which is what the bound is for.
		if got := storage.discards() - discarded; got != 2 {
			t.Fatalf("%d late volumes were discarded, want 2", got)
		}
		for _, instance := range []string{"billet-one", "billet-two", "billet-three"} {
			if got := sessionLate(service, instance); got != 0 {
				t.Fatalf("%s still holds %d late volumes", instance, got)
			}
			session := service.sessionOf(instance)
			if session.slots[0] != nil {
				t.Fatalf("%s took a volume that came back too late", instance)
			}
		}

		// THE STORE ANSWERS A PROBE, AND THE NEXT JOB IS WHAT CLOSES IT.
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		if got := service.breaker.report().State; got != lease.CacheStoreProbation {
			t.Fatalf("after an answered probe: state %q", got)
		}
		if got := dockerStoreFor(t, service, "billet-five"); got.Code != http.StatusCreated {
			t.Fatalf("after the store came back: status %d: %s", got.Code, got.Body.String())
		}
		if got := imageCacheOf(service, "billet-five"); got != alloc.ImageCacheWarm {
			t.Fatalf("after the store came back: observed %q", got)
		}
		synctest.Wait()
		if got := service.breaker.report().State; got != lease.CacheStoreClosed {
			t.Fatalf("after a job used the store: state %q", got)
		}
		want := []lease.CacheStoreState{lease.CacheStoreOpen, lease.CacheStoreProbation, lease.CacheStoreClosed}
		if got := reports.states(); len(got) < len(want) || !slices.Equal(got[len(got)-len(want):], want) {
			t.Fatalf("the plane was told %v, want it to end %v", got, want)
		}

		cancel()
		<-guard
	})
}

// A SESSION CANNOT FINISH WHILE A CALL IS OUT, or what it comes back with has no custody;
// and a discard that fails leaves the volume durable for the cleanup.
func TestALateVolumeIsDiscardedOnceAndHeldUntilItIs(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		storage.discardFailures = 1
		service := guardedService(t, storage)
		storage.hanging.Store(true)

		if got := dockerStoreFor(t, service, "billet-one"); got.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d", got.Code)
		}
		session := service.sessionOf("billet-one")

		// THE COMPUTE ENDS WITH THE CALL STILL OUT.
		if err := service.Close(t.Context(), "billet-one"); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := service.RetryClosed(t.Context()); err != nil {
			t.Fatalf("RetryClosed: %v", err)
		}
		if service.sessionOf("billet-one") == nil {
			t.Fatal("the session finished with a call to the store still out")
		}

		// IT COMES BACK AND THE FIRST DISCARD FAILS, so the volume stays, durably, and
		// the cache loop is told there is a session to look at again.
		select {
		case <-service.closed:
		default:
		}
		storage.hanging.Store(false)
		close(storage.release)
		synctest.Wait()
		select {
		case <-service.closed:
		default:
			t.Fatal("a late call ending on a closed session did not wake the cache loop")
		}
		session.mu.Lock()
		late := len(session.late)
		session.mu.Unlock()
		if late != 1 {
			t.Fatalf("after a failed discard the session holds %d late volumes, want 1", late)
		}
		record := sessionRecord(t, service, session.token)
		if len(record.Late) != 1 {
			t.Fatalf("the record names %d late volumes, want 1", len(record.Late))
		}

		// THE CLEANUP FINISHES IT, and only then the session.
		if err := service.RetryClosed(t.Context()); err != nil {
			t.Fatalf("RetryClosed: %v", err)
		}
		if service.sessionOf("billet-one") != nil {
			t.Fatal("the session outlived its last volume")
		}
		// THE ATTEMPT THAT FAILED AND THE ONE THAT DID NOT.
		if got := storage.discards(); got != 2 {
			t.Fatalf("%d discards were attempted, want 2", got)
		}
	})
}

// A RESTARTED NODE FINDS WHAT A CRASH LEFT, because the record names it.
func TestARestartedNodeDiscardsALateVolumeItsRecordNames(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	dir := t.TempDir()
	first, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", dir, storage,
		&fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	if _, err := first.Prepare("billet-one", provider.TrustTrusted); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	session := first.sessionOf("billet-one")
	session.mu.Lock()
	session.late = []storecontract.Volume{{Key: "k", Handle: "pool/late", Device: "/dev/rbd9"}}
	session.closed = true
	if err := first.persistSession(session); err != nil {
		t.Fatalf("persist: %v", err)
	}
	session.mu.Unlock()

	second, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", dir, storage,
		&fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService after a restart: %v", err)
	}
	if err := second.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.discarded != 1 {
		t.Fatalf("%d volumes were discarded after a restart, want 1", storage.discarded)
	}
	if second.sessionOf("billet-one") != nil {
		t.Fatal("the session outlived its late volume")
	}
}

// EVERY CACHE ANSWERS AT ONCE, the store unasked, and says so.
func TestAnOpenBreakerAnswersEveryCacheWithoutAskingTheStore(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
			return true, nil
		}))
		failThree(service.breaker)
		credentials, err := service.PrepareScoped("billet-lease-all", CacheSessionScope{
			Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
			Cache: &config.CacheSpec{
				Publish: config.CachePublishTrustedOnly, Docker: config.CacheSetting{Enabled: true},
				StickyDisks: config.CacheSetting{Enabled: true}, Go: config.CacheSetting{Enabled: true},
			},
		})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		session := service.sessionOf("billet-lease-all")
		asked := storage.askedCount()
		start := time.Now()

		// A STICKY DISK.
		if got := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
			"key": "cold", "size_bytes": 1 << 20,
		}); got.Code != http.StatusServiceUnavailable {
			t.Fatalf("a sticky disk: status %d: %s", got.Code, got.Body.String())
		}
		session.mu.Lock()
		sticky := session.building[config.CacheSticky]
		session.mu.Unlock()
		if sticky != alloc.BuildCacheBypassed {
			t.Fatalf("a sticky disk: noted %q", sticky)
		}

		// THE GO CACHE, a 503 being what its helper reads as a miss.
		const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/cas/go/cas/"+digest, http.NoBody)
		request.Header.Set("Authorization", "Bearer "+credentials.Token)
		recorder := httptest.NewRecorder()
		service.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("the Go cache: status %d: %s", recorder.Code, recorder.Body.String())
		}
		session.mu.Lock()
		goCache := session.building[config.CacheGo]
		session.mu.Unlock()
		if goCache != alloc.BuildCacheBypassed {
			t.Fatalf("the Go cache: noted %q", goCache)
		}

		// THE ACTIONS CACHE: A LOOKUP AND A RESERVATION GO TO GITHUB, EACH SAYING WHY.
		for _, path := range []string{actionsDownloadPath, actionsCreatePath} {
			own, _ := anActionsSession(t, service, "billet-lease-"+strings.ReplaceAll(path, "/", "-"))
			response, handled, err := service.actionsResponse(
				twirp(t, path, `{"key":"npm","restore_keys":[],"version":"v1"}`), own)
			if response != nil {
				response.Body.Close()
			}
			if err != nil || handled || response != nil {
				t.Fatalf("%s: answered %v, %v, %v, want it left to GitHub", path, response, handled, err)
			}
			own.mu.Lock()
			actions := own.observed.ActionsCache
			own.mu.Unlock()
			if alloc.ActionsCache(actions) != alloc.ActionsCacheBypassed {
				t.Fatalf("%s: observed %q", path, actions)
			}
		}
		if store, open, known := service.StoreBreakerOpen(); store != "ceph" || !open || !known {
			t.Fatalf("StoreBreakerOpen = %q, %v, %v", store, open, known)
		}
		// AND A FINALIZE, BOUND TO A RESERVATION ONLY THIS NODE HOLDS, FAILS HERE.
		response, handled, err := service.actionsResponse(
			twirp(t, actionsFinalizePath, `{"key":"npm","version":"v1","sizeBytes":"1"}`), session)
		if response != nil {
			response.Body.Close()
		}
		if !handled || !errors.Is(err, errStoreOpen) {
			t.Fatalf("a finalize: handled %v, %v", handled, err)
		}

		// A BLOB IS NOT READ OR WRITTEN: BOTH WAIT IN THE KERNEL ON A VOLUME WHOSE OSDS HAVE
		// GONE, HOLDING WHAT ITS CLEANUP NEEDS. It asks nothing of the session either, whose
		// lock a request could be holding for a deadline.
		session.mu.Lock()
		session.actions["blob-id"] = &actionsArchive{ID: "blob-id", Signature: "sig", Mode: actionsModeDownload}
		blob, blobHandled, blobErr := service.actionsResponse(actionsRequestForTest(t, http.MethodGet,
			"https://"+actionsResultsHost+actionsBlobPrefix+"blob-id?sig=sig", ""), session)
		session.mu.Unlock()
		if blob != nil {
			blob.Body.Close()
		}
		if blob != nil || !blobHandled || !errors.Is(blobErr, errStoreOpen) {
			t.Fatalf("a blob: answered %v, handled %v, %v", blob, blobHandled, blobErr)
		}

		if waited := time.Since(start); waited != 0 {
			t.Fatalf("an open breaker took %s to answer", waited)
		}
		if storage.askedCount() != asked {
			t.Fatal("the store was asked with the breaker open")
		}
	})
}

// A PUBLICATION THE STORE CANNOT TAKE LOSES WHAT THE JOB WROTE AND NOTHING PUBLISHED,
// and costs the teardown no wait.
func TestAnOpenBreakerPublishesNothingAndKeepsTheClone(t *testing.T) {
	t.Parallel()

	storage := newHangingStore()
	service := guardedService(t, storage)
	credentials, err := service.PrepareScoped("billet-commit", CacheSessionScope{Trust: provider.TrustTrusted})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	attached := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
		"key": "disk", "size_bytes": 1 << 20,
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach: status %d: %s", attached.Code, attached.Body.String())
	}
	failThree(service.breaker)
	before := storage.writerAcquisitions

	committed := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/commit", nil)
	if committed.Code != http.StatusOK {
		t.Fatalf("commit: status %d: %s", committed.Code, committed.Body.String())
	}
	if body := committed.Body.String(); !strings.Contains(body, `"published":false`) ||
		!strings.Contains(body, "not answering") {
		t.Fatalf("commit said %s", body)
	}
	if storage.writerAcquisitions != before {
		t.Fatal("a commit took a writer from a store that is not answering")
	}
	session := service.sessionOf("billet-commit")
	if session.slots[0] == nil {
		t.Fatal("the clone left the session's custody without being discarded")
	}
}

// THE GATE COMES BEFORE ANYTHING THAT WOULD WAIT ON THE STORE, with the job waiting
// behind it: a job that may not publish discards on the request's path, and so does a
// guest that rejects what it wrote.
func TestAnOpenBreakerDiscardsNothingOnAJobsPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, path string
		trust      provider.TrustClass
		want       string
	}{
		{"an untrusted job's commit", "/v1/volumes/0/commit", provider.TrustUntrusted, `"published":false`},
		{"a guest's discard", "/v1/volumes/0/discard", provider.TrustTrusted, `"discarded":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := newHangingStore()
			service := guardedService(t, storage)
			credentials, err := service.PrepareScoped("billet-gate", CacheSessionScope{Trust: tc.trust})
			if err != nil {
				t.Fatalf("PrepareScoped: %v", err)
			}
			if got := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
				"key": "disk", "size_bytes": 1 << 20,
			}); got.Code != http.StatusCreated {
				t.Fatalf("attach: status %d: %s", got.Code, got.Body.String())
			}
			failThree(service.breaker)

			got := cacheRequest(t, service, credentials.Token, tc.path, nil)
			if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), tc.want) ||
				!strings.Contains(got.Body.String(), "not answering") {
				t.Fatalf("status %d: %s", got.Code, got.Body.String())
			}
			if storage.discards() != 0 {
				t.Fatal("the store was asked to discard with the breaker open")
			}
			if service.sessionOf("billet-gate").slots[0] == nil {
				t.Fatal("the clone left the session's custody without being discarded")
			}
		})
	}
}

// SETTLING RUNS ON THE TEARDOWN'S PATH, so a store that has stopped answering costs it
// nothing, and what the job wrote goes with its session.
func TestAnOpenBreakerSettlesADockerStoreWithoutAskingTheStore(t *testing.T) {
	t.Parallel()

	for _, open := range []bool{false, true} {
		t.Run(fmt.Sprintf("open=%v", open), func(t *testing.T) {
			t.Parallel()

			storage := newHangingStore()
			service := guardedService(t, storage)
			credentials, err := service.PrepareScoped("billet-settle", CacheSessionScope{
				Trust: provider.TrustTrusted,
				Cache: &config.CacheSpec{
					Publish: config.CachePublishTrustedOnly, Docker: config.CacheSetting{Enabled: true},
				},
			})
			if err != nil {
				t.Fatalf("PrepareScoped: %v", err)
			}
			if got := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
				map[string]any{"architecture": "amd64"}); got.Code != http.StatusCreated {
				t.Fatalf("attach: status %d: %s", got.Code, got.Body.String())
			}
			if open {
				failThree(service.breaker)
			}
			before := storage.snapshots

			settled := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				settled <- service.SettleDocker(t.Context(), "billet-settle", true)
			}()
			t.Cleanup(func() { <-done })
			if !open {
				proof := map[string]any{
					"filesystem": map[string]any{"type": "ext4", "uuid": "docker-fs", "clean": true},
				}
				deadline := time.Now().Add(2 * time.Minute)
				for {
					ready := cacheRequest(t, service, credentials.Token, "/v1/docker-store/ready", proof)
					if ready.Code == http.StatusOK {
						break
					}
					if ready.Code != http.StatusConflict || time.Now().After(deadline) {
						t.Fatalf("ready: status %d: %s", ready.Code, ready.Body.String())
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if err := <-settled; err != nil {
				t.Fatalf("SettleDocker: %v", err)
			}
			// BOTH WAYS, or a gate on the wrong path would pass by there being nothing to do.
			if got := storage.snapshots - before; (got == 0) != open {
				t.Fatalf("%d snapshots with the breaker open=%v", got, open)
			}
		})
	}
}

// ONE JOB COUNTS ONCE AND ITS OWN NONSENSE NOT AT ALL: a key the store would refuse is
// refused before it reaches the store.
func TestAJobsBadKeyNeverReachesTheStore(t *testing.T) {
	t.Parallel()

	storage := newHangingStore()
	service := guardedService(t, storage)
	for i := range 2 * breakerFailures {
		credentials, err := service.PrepareScoped(fmt.Sprintf("billet-nul-%d", i),
			CacheSessionScope{Trust: provider.TrustTrusted})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		if got := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
			"key": "a\x00b", "size_bytes": 1 << 20,
		}); got.Code != http.StatusBadRequest {
			t.Fatalf("a NUL in a key: status %d: %s", got.Code, got.Body.String())
		}
	}
	if storage.askedCount() != 0 {
		t.Fatalf("the store was asked %d times about a key it would refuse", storage.askedCount())
	}
	if !service.breaker.allow() {
		t.Fatal("jobs' bad keys stopped everyone asking the store")
	}
}

// A GUEST THAT HANGS UP ON EACH REQUEST STARTS ONE CALL, not one a request: each would hold
// a place in the store's queue that no job is waiting on.
func TestASessionHasOneLateCallOut(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		storage.hanging.Store(true)
		credentials, err := service.PrepareScoped("billet-hangup", CacheSessionScope{Trust: provider.TrustTrusted})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		attach := func() *httptest.ResponseRecorder {
			return cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
				"key": "disk", "size_bytes": 1 << 20,
			})
		}

		if got := attach(); got.Code != http.StatusServiceUnavailable {
			t.Fatalf("first: status %d: %s", got.Code, got.Body.String())
		}
		synctest.Wait()
		asked := storage.askedCount()
		if asked != 1 {
			t.Fatalf("the store was asked %d times, want 1", asked)
		}

		start := time.Now()
		for range 5 {
			if got := attach(); got.Code != http.StatusServiceUnavailable {
				t.Fatalf("with a call out: status %d: %s", got.Code, got.Body.String())
			}
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("with a call out a request waited %s", waited)
		}
		synctest.Wait()
		if storage.askedCount() != asked {
			t.Fatalf("with a call out the store was asked %d more times", storage.askedCount()-asked)
		}

		storage.hanging.Store(false)
		close(storage.release)
		synctest.Wait()
		if got := attach(); got.Code != http.StatusCreated {
			t.Fatalf("with nothing out: status %d: %s", got.Code, got.Body.String())
		}
	})
}

// A VOLUME THAT CAME BACK LATE AND COULD NOT BE DISCARDED STILL HOLDS THE SESSION'S ONE
// PLACE: a guest hanging up on each request would otherwise leave as many as it likes
// outside its slots.
func TestALateVolumeStillHeldStopsTheSessionAsking(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		storage.discardFailures = 1000
		service := guardedService(t, storage)
		storage.hanging.Store(true)
		credentials, err := service.PrepareScoped("billet-held", CacheSessionScope{Trust: provider.TrustTrusted})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		attach := func() *httptest.ResponseRecorder {
			return cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
				"key": "disk", "size_bytes": 1 << 20,
			})
		}

		if got := attach(); got.Code != http.StatusServiceUnavailable {
			t.Fatalf("first: status %d: %s", got.Code, got.Body.String())
		}
		storage.hanging.Store(false)
		close(storage.release)
		synctest.Wait()
		if got := sessionLate(service, "billet-held"); got != 1 {
			t.Fatalf("the session holds %d late volumes, want 1", got)
		}

		asked := storage.askedCount()
		for range 5 {
			if got := attach(); got.Code != http.StatusServiceUnavailable {
				t.Fatalf("with a late volume held: status %d: %s", got.Code, got.Body.String())
			}
		}
		if storage.askedCount() != asked {
			t.Fatalf("with a late volume held the store was asked %d more times", storage.askedCount()-asked)
		}
	})
}

// ONLY WHAT ASKS FOR A VOLUME HOLDS A TRIAL. A lookup that misses, a job its own backoff
// holds back and a kind that is off ask for nothing, and if they held it the store's
// recovery would wait on them, indefinitely on a tier of nothing else.
func TestWhatAsksForNoVolumeHoldsNoTrial(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		storage := newHangingStore()
		storage.current = ""
		service := guardedService(t, storage)
		service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
			return true, nil
		}))
		guard := make(chan struct{})
		go func() {
			defer close(guard)
			service.RunStoreGuard(ctx, &storeReports{})
		}()
		failThree(service.breaker)
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		if got := service.breaker.report().State; got != lease.CacheStoreProbation {
			t.Fatalf("state %q, want probation", got)
		}

		credentials, err := service.PrepareScoped("billet-trial", CacheSessionScope{
			Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
			Cache: &config.CacheSpec{
				Publish: config.CachePublishTrustedOnly, Docker: config.CacheSetting{Enabled: true},
				StickyDisks: config.CacheSetting{Enabled: true}, Go: config.CacheSetting{Enabled: true},
				Actions: config.CacheSetting{Enabled: true},
			},
		})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		session := service.sessionOf("billet-trial")

		// A GO REQUEST HELD BACK BY ITS OWN BACKOFF.
		session.mu.Lock()
		session.casRetryAt = map[config.CacheKind]time.Time{config.CacheGo: time.Now().Add(time.Hour)}
		session.mu.Unlock()
		const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		held := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/cas/go/cas/"+digest, http.NoBody)
		held.Header.Set("Authorization", "Bearer "+credentials.Token)
		service.ServeHTTP(httptest.NewRecorder(), held)

		// AN ACTIONS LOOKUP THAT MISSES.
		looked, handled, err := service.actionsResponse(
			twirp(t, actionsDownloadPath, `{"key":"npm","restore_keys":[],"version":"v1"}`), session)
		if looked != nil {
			looked.Body.Close()
		}
		if !handled || err != nil {
			t.Fatalf("a lookup on probation: handled %v, %v", handled, err)
		}
		if got := service.breaker.report().State; got != lease.CacheStoreProbation {
			t.Fatalf("what handed over no volume moved probation to %q", got)
		}

		// THE TRIAL IS STILL THERE FOR THE JOB THAT ASKS, whose volume ends it.
		if got := dockerStoreFor(t, service, "billet-next"); got.Code != http.StatusCreated {
			t.Fatalf("the job that asked: status %d: %s", got.Code, got.Body.String())
		}
		if got := service.breaker.report().State; got != lease.CacheStoreClosed {
			t.Fatalf("after a volume was handed over: state %q", got)
		}

		cancel()
		<-guard
	})
}

// A DISCARD IS NEVER COUNTED, THE ONE AN OVERSIZED CLONE NEEDS INCLUDED: the clone it
// discards was handed over, and what follows is a job starting cold at its own size.
func TestAnOversizedClonesSlowDiscardDoesNotCount(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := &slowDiscardStore{hangingStore: newHangingStore(), size: 1 << 40}
		service := guardedService(t, storage)
		for i := range breakerFailures {
			got := dockerStoreFor(t, service, fmt.Sprintf("billet-oversized-%d", i))
			if got.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d: %s", got.Code, got.Body.String())
			}
		}
		synctest.Wait()
		service.breaker.mu.Lock()
		failures := len(service.breaker.failures)
		service.breaker.mu.Unlock()
		if failures != 0 {
			t.Fatalf("a discard that outlasted the deadline counted %d failures", failures)
		}
		close(storage.release)
		synctest.Wait()
	})
}

// slowDiscardStore hands over a clone too large for any tier and does not discard it
// until released.
type slowDiscardStore struct {
	*hangingStore
	size int64
}

func (s *slowDiscardStore) SizeOf(context.Context, storecontract.Volume) (int64, error) {
	return s.size, nil
}

func (s *slowDiscardStore) Discard(ctx context.Context, volume storecontract.Volume) error {
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}

	return s.hangingStore.Discard(ctx, volume)
}

// THE ACTIONS FINALIZE IS WHAT A JOB WAITS ON TOO: its writer and its snapshot count when
// they time out, and their refusals do not.
func TestAnActionsFinalizesTimeoutsCountAgainstTheStore(t *testing.T) {
	t.Parallel()

	timedOut := fmt.Errorf("rbd did not answer: %w", context.DeadlineExceeded)
	notClean := errors.New("not clean")
	for _, tc := range []struct {
		name     string
		break_   func(*hangingStore)
		want     error
		wantOpen bool
	}{
		{"a writer that timed out", func(s *hangingStore) { s.writerErr = timedOut }, timedOut, true},
		{"a snapshot that timed out", func(s *hangingStore) { s.snapshotErr = timedOut }, timedOut, true},
		{"a publication that timed out", func(s *hangingStore) { s.publishErr = timedOut }, timedOut, true},
		{"a refused snapshot", func(s *hangingStore) { s.snapshotErr = notClean }, notClean, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := newHangingStore()
			storage.current = ""
			service := guardedService(t, storage)
			service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
				return true, nil
			}))
			tc.break_(storage)
			for i := range breakerFailures {
				instance := fmt.Sprintf("billet-finalize-%d", i)
				if _, err := service.PrepareScoped(instance, CacheSessionScope{
					Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
					WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
					Cache: &config.CacheSpec{
						Publish: config.CachePublishTrustedOnly, Actions: config.CacheSetting{Enabled: true},
					},
				}); err != nil {
					t.Fatalf("PrepareScoped: %v", err)
				}
				session := service.sessionOf(instance)
				session.mu.Lock()
				session.actions["reserved"] = &actionsArchive{
					ID: "reserved", Signature: "sig", Mode: actionsModeUpload, CacheKey: "npm",
					Version: "v1", StoreKey: "actions/npm-" + instance, Unmounted: true,
					Volume: storecontract.Volume{Key: "actions/npm-" + instance, Handle: "pool/" + instance},
				}
				session.mu.Unlock()
				archivePath := service.actionsArchivePath(session, session.actions["reserved"])
				if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(archivePath, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}

				response, err := service.finalizeActionsCache(t.Context(),
					strings.NewReader(`{"key":"npm","version":"v1","size_bytes":"1"}`), session)
				if response != nil {
					response.Body.Close()
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("a finalize the store failed: %v, want %v", err, tc.want)
				}
			}
			if got := !service.breaker.allow(); got != tc.wantOpen {
				t.Fatalf("open = %v, want %v", got, tc.wantOpen)
			}
		})
	}
}

// A READ THAT FINDS WHAT IT ASKED FOR, OR NOTHING THERE, IS THE STORE WORKING: its timer
// must not go on to count it, or three jobs' cache hits would open the breaker on a
// healthy store.
func TestAContentAddressedReadThatIsAnsweredDoesNotCountLater(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		service := guardedService(t, newHangingStore())
		const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		const linked = "aab0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

		root := t.TempDir()
		dir := filepath.Join(root, "cas", digest[:2])
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		object := filepath.Join(dir, digest)
		if err := os.WriteFile(object, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(root, "cas", linked[:2])
		if err := os.MkdirAll(linkDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(object, filepath.Join(linkDir, linked)); err != nil {
			t.Fatal(err)
		}

		for i := range breakerFailures {
			hv := &hostVolume{}
			handle := &casHandle{
				s: service, session: &cacheSession{instance: fmt.Sprintf("hit-%d", i)}, hv: hv, root: root,
				release: func() {},
			}
			file, err := handle.Open("cas", digest)
			if err != nil {
				t.Fatalf("a read of what is there: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if !hv.hit.Load() {
				t.Fatal("a hit was not noted")
			}
			handle.session = &cacheSession{instance: fmt.Sprintf("link-%d", i)}
			if _, err := handle.Open("cas", linked); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a read of a link: %v", err)
			}
		}
		time.Sleep(2 * casReadLimit)
		synctest.Wait()
		if !service.breaker.allow() {
			t.Fatal("reads the store answered counted when their timers would have fired")
		}
	})
}

// A READ OF A CONTENT-ADDRESSED VOLUME IS WHAT THE BREAKER HEARS OF A STORE WHOSE OSDS
// HAVE GONE: it counts as it fails, as what is not there does not.
func TestAContentAddressedReadThatFailsCounts(t *testing.T) {
	t.Parallel()

	storage := newHangingStore()
	service := guardedService(t, storage)
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// A ROOT THAT IS NOT THERE IS THE STORE ANSWERING THAT NOTHING IS.
	missing := t.TempDir()
	// A ROOT THAT IS A FILE MAKES THE OPEN FAIL WITH SOMETHING OTHER THAN NOT-THERE.
	broken := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(broken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	read := func(who, root string) {
		handle := &casHandle{
			s: service, session: &cacheSession{instance: who}, hv: &hostVolume{}, root: root,
			release: func() {},
		}
		if file, err := handle.Open("cas", digest); err == nil {
			file.Close()
			t.Fatal("a read of nothing returned a file")
		}
	}
	for i := range breakerFailures {
		read(fmt.Sprintf("missing-%d", i), missing)
	}
	if !service.breaker.allow() {
		t.Fatal("reads of objects that are not there opened the breaker")
	}
	for i := range breakerFailures {
		read(fmt.Sprintf("broken-%d", i), broken)
	}
	if service.breaker.allow() {
		t.Fatal("reads that failed left the breaker closed")
	}
}

// AN OBSERVATION A STORE BEHIND NO BREAKER MAKES IS UNCHANGED: one nobody guards asks as
// it always did, whatever its calls come to.
func TestAStoreNobodyGuardsIsAlwaysAsked(t *testing.T) {
	t.Parallel()

	service, _, token := observedService(t, &fakeCacheStore{current: "g1"})
	if store, open, known := service.StoreBreakerOpen(); known || open || store != "" {
		t.Fatalf("StoreBreakerOpen = %q, %v, %v", store, open, known)
	}
	if got := cacheRequest(t, service, token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"}); got.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", got.Code, got.Body.String())
	}
	service.ResendStoreState()
	service.RunStoreGuard(t.Context(), &storeReports{})
}

// A REPORT THE PLANE DID NOT TAKE IS SENT AGAIN, and a registration says it unchanged.
func TestABreakersReportReachesThePlaneEventually(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// STILL HANGING, so no probe moves the breaker while this watches its reports.
		storage := newHangingStore()
		storage.hanging.Store(true)
		service := guardedService(t, storage)
		reports := &storeReports{}
		reports.refuse.Store(true)
		done := make(chan struct{})
		go func() {
			defer close(done)
			service.RunStoreGuard(ctx, reports)
		}()

		failThree(service.breaker)
		synctest.Wait()
		if got := reports.states(); len(got) != 0 {
			t.Fatalf("a refused report was recorded: %v", got)
		}

		reports.refuse.Store(false)
		time.Sleep(storeReportRetry)
		synctest.Wait()
		if got := reports.states(); len(got) != 1 || got[0] != lease.CacheStoreOpen {
			t.Fatalf("after the retry the plane was told %v", got)
		}

		// NOTHING NEW, NOTHING SENT.
		time.Sleep(storeReportRetry)
		synctest.Wait()
		if got := reports.states(); len(got) != 1 {
			t.Fatalf("an unchanged state was sent again: %v", got)
		}

		// UNTIL A REGISTRATION ASKS.
		service.ResendStoreState()
		synctest.Wait()
		if got := reports.states(); len(got) != 2 {
			t.Fatalf("a registration did not say the state again: %v", got)
		}

		// AND ONE THAT FAILS IS OWED: THE STATE HAS NOT MOVED, BUT THE NEW PLANE HAS NOT
		// HEARD IT.
		reports.refuse.Store(true)
		service.ResendStoreState()
		synctest.Wait()
		reports.refuse.Store(false)
		time.Sleep(storeReportRetry)
		synctest.Wait()
		if got := reports.states(); len(got) != 3 {
			t.Fatalf("a registration's report that failed was not sent again: %v", got)
		}
		reports.mu.Lock()
		bounds := slices.Clone(reports.bounds)
		reports.mu.Unlock()
		for _, bound := range bounds {
			if bound != storeReportLimit {
				t.Fatalf("a report was bounded at %s, want %s", bound, storeReportLimit)
			}
		}
		if len(bounds) == 0 {
			t.Fatal("no report was bounded")
		}

		cancel()
		<-done
	})
}

// A CANDIDATE THE STORE REFUSES IS ONE JOB'S BAD WRITE, and a node logging hundreds of
// those a day must not send every job cold; the same refusal that timed out is the store.
func TestOnlyAPublicationsTimeoutCountsAgainstTheStore(t *testing.T) {
	t.Parallel()

	timedOut := fmt.Errorf("rbd did not answer: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name     string
		err      error
		writer   error
		publish  error
		wantOpen bool
	}{
		{name: "a filesystem that is not clean", err: errors.New("ceph: verify candidate as published: the ext4 filesystem is not clean")},
		{name: "a snapshot that did not answer", err: timedOut, wantOpen: true},
		{name: "a writer that did not answer", writer: timedOut, wantOpen: true},
		{name: "a publication that did not answer", publish: timedOut, wantOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := newHangingStore()
			storage.snapshotErr, storage.writerErr, storage.publishErr = tc.err, tc.writer, tc.publish
			service := guardedService(t, storage)
			// ATTACHED FIRST, because a volume handed over between them clears the count.
			var tokens []string
			for i := range breakerFailures {
				credentials, err := service.PrepareScoped(fmt.Sprintf("billet-publish-%d", i),
					CacheSessionScope{Trust: provider.TrustTrusted})
				if err != nil {
					t.Fatalf("PrepareScoped: %v", err)
				}
				if got := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
					"key": "disk", "size_bytes": 1 << 20,
				}); got.Code != http.StatusCreated {
					t.Fatalf("attach: status %d: %s", got.Code, got.Body.String())
				}
				tokens = append(tokens, credentials.Token)
			}
			for _, token := range tokens {
				if got := cacheRequest(t, service, token, "/v1/volumes/0/commit", nil); got.Code != http.StatusOK {
					t.Fatalf("commit: status %d: %s", got.Code, got.Body.String())
				}
			}
			if got := !service.breaker.allow(); got != tc.wantOpen {
				t.Fatalf("open = %v, want %v", got, tc.wantOpen)
			}
		})
	}
}

// A MOUNTED VOLUME IS NOT READ EITHER: a read of one whose OSDs have gone waits in the
// kernel, and on probation it decides nothing.
func TestAMountedContentAddressedVolumeIsNotReadUntilTheBreakerCloses(t *testing.T) {
	t.Parallel()

	for name, put := range map[string]func(*storeBreaker){
		"open":      failThree,
		"probation": onProbation,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{}
			service, _, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
			service.GuardStore("ceph", testStoreDeadline)
			ours := "our object"
			path := "/v1/cas/go/cas/" + digestOf(ours)
			if got := casRequest(t, service, token, http.MethodPut, path, ours); got.Code != http.StatusOK {
				t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
			}
			put(service.breaker)

			if got := casRequest(t, service, token, http.MethodGet, path, ""); got.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET = %d, want the 503 its helper reads as a miss", got.Code)
			}
			session := service.sessionOf(instance)
			session.mu.Lock()
			noted := session.building[config.CacheGo]
			session.mu.Unlock()
			if noted != alloc.BuildCacheBypassed {
				t.Fatalf("noted %q", noted)
			}
		})
	}
}

// NOTHING WAS ASKED, SO THERE IS NOTHING TO BACK OFF FROM: with another job's trial out a
// job's Go cache goes without, says why, and asks again as soon as it can.
func TestAContentAddressedRequestRefusedATrialDoesNotBackOff(t *testing.T) {
	t.Parallel()

	storage := newHangingStore()
	service := guardedService(t, storage)
	session, token := anActionsSession(t, service, "billet-no-backoff")
	onProbation(service.breaker)
	if !service.breaker.admit() {
		t.Fatal("no trial to take")
	}

	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/cas/go/cas/"+digest, http.NoBody)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if noted := session.building[config.CacheGo]; noted != alloc.BuildCacheBypassed {
		t.Fatalf("noted %q", noted)
	}
	if _, backing := session.casRetryAt[config.CacheGo]; backing {
		t.Fatal("a request that asked nothing backed off")
	}
}

// A MERGE'S CLONE IS A VOLUME HANDED OVER: it ends a probation and clears the count, as a
// job's own clone does.
func TestAMergesCloneSettlesAProbation(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, _, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	service.GuardStore("ceph", testStoreDeadline)
	ours := "our object"
	if got := casRequest(t, service, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf(ours), ours); got.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
	}
	// ANOTHER JOB PUBLISHED MEANWHILE, so this one merges into a clone of its generation.
	storage.current = "theirs"
	onProbation(service.breaker)
	// THE PUBLICATION IS REFUSED, so only the clone can have settled it.
	storage.publishErr = errors.New("not clean")

	endSession(t, service, instance, publishingAuthority())
	if got := service.breaker.report().State; got != lease.CacheStoreClosed {
		t.Fatalf("after a merge's clone was handed over: state %q", got)
	}
}

// A RETRIED PUBLICATION'S CONFLICT DOES NOT HIDE ITS TIMEOUT: the timeout is the last thing
// the store did, and the error carries both.
func TestAConflictBesideATimeoutStillCounts(t *testing.T) {
	t.Parallel()

	service := guardedService(t, newHangingStore())
	joined := errors.Join(storecontract.ErrConflict, fmt.Errorf("rbd: %w", context.DeadlineExceeded))
	for i := range breakerFailures {
		service.countedPublication(fmt.Sprintf("job-%d", i), "a cache publication", joined)
	}
	if !service.breaker.isOpen() {
		t.Fatal("three publications that timed out left the breaker closed")
	}
}

// A JOB THAT MAY NOT PUBLISH SETTLES NOTHING, so its discard takes no trial and waits for
// the verdict like anything else that decides nothing.
func TestACommitThatWouldOnlyDiscardTakesNoTrial(t *testing.T) {
	t.Parallel()

	storage := newHangingStore()
	service := guardedService(t, storage)
	credentials, err := service.PrepareScoped("billet-untrusted", CacheSessionScope{Trust: provider.TrustUntrusted})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	if got := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
		"key": "disk", "size_bytes": 1 << 20,
	}); got.Code != http.StatusCreated {
		t.Fatalf("attach: status %d: %s", got.Code, got.Body.String())
	}
	onProbation(service.breaker)

	got := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/commit", nil)
	if !strings.Contains(got.Body.String(), "not answering") {
		t.Fatalf("a commit on probation: %s", got.Body.String())
	}
	if storage.discards() != 0 {
		t.Fatal("a discard asked the store on probation")
	}
	if !service.breaker.admit() {
		t.Fatal("a commit that publishes nothing held the trial")
	}
}

// A FULL GENERATION'S REPLACEMENT IS A VOLUME LIKE ANY OTHER, for the deadline and for the
// trial: past the deadline the job goes on without its cache and holds the session no
// longer, and with another job's trial out it is not asked for.
func TestAFreshStartAsksForItsVolumeThroughTheGuard(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		if _, err := service.PrepareScoped("billet-fresh", CacheSessionScope{Trust: provider.TrustTrusted}); err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		session := service.sessionOf("billet-fresh")
		session.mu.Lock()
		defer session.mu.Unlock()

		hv := &hostVolume{Kind: config.CacheGo, Volume: storecontract.Volume{Key: "k", Handle: "pool/full"}}

		onProbation(service.breaker)
		if !service.breaker.admit() {
			t.Fatal("no trial to take")
		}
		asked := storage.volumesAsked()
		if err := service.startFresh(t.Context(), session, hv, "k", 1<<20); !errors.Is(err, errStoreOpen) {
			t.Fatalf("with another job's trial out: %v", err)
		}
		if storage.volumesAsked() != asked {
			t.Fatal("a volume was asked for with another job's trial out")
		}
	})
}

// A GUEST THAT HANGS UP GOES ON WITHOUT THE STORE AT ONCE, and the call goes on.
func TestAGuestHangingUpEndsItsWaitAndNotTheCall(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		storage.hanging.Store(true)
		credentials, err := service.PrepareScoped("billet-hangup", CacheSessionScope{Trust: provider.TrustTrusted})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(time.Second, cancel)

		start := time.Now()
		got := cacheRequestWithin(t, ctx, service, credentials.Token, "/v1/volumes", map[string]any{
			"key": "disk", "size_bytes": 1 << 20,
		})
		if got.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d: %s", got.Code, got.Body.String())
		}
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("a request that hung up after a second took %s", waited)
		}

		storage.hanging.Store(false)
		close(storage.release)
		synctest.Wait()
		if got := storage.discards(); got != 1 {
			t.Fatalf("%d late volumes were discarded, want the call's one", got)
		}
	})
}

// A REQUEST'S OWN TIME RUNNING OUT SAYS NOTHING OF THE STORE: a guest names its own
// deadline, in a gRPC client's grpc-timeout, and three jobs with a short one must not
// open the breaker on a store that was only taking its time.
func TestARequestsOwnDeadlineDoesNotCountAgainstTheStore(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
			return true, nil
		}))
		storage.hanging.Store(true)

		for i := range breakerFailures {
			credentials, err := service.PrepareScoped(fmt.Sprintf("billet-short-%d", i),
				CacheSessionScope{Trust: provider.TrustTrusted})
			if err != nil {
				t.Fatalf("PrepareScoped: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			got := cacheRequestWithin(t, ctx, service, credentials.Token, "/v1/volumes", map[string]any{
				"key": "disk", "size_bytes": 1 << 20,
			})
			cancel()
			if got.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d: %s", got.Code, got.Body.String())
			}
		}

		// THE STORE ANSWERS, INSIDE ITS OWN DEADLINE, EVERY CALL THE REQUESTS LEFT BEHIND.
		time.Sleep(testStoreDeadline - 10*time.Second)
		storage.hanging.Store(false)
		close(storage.release)
		synctest.Wait()
		service.breaker.mu.Lock()
		failures := len(service.breaker.failures)
		service.breaker.mu.Unlock()
		if failures != 0 {
			t.Fatalf("requests' own deadlines counted %d failures against the store", failures)
		}
	})
}

// THE SAME OF A LOOKUP, WHICH THE REQUEST'S DEADLINE DOES CUT SHORT.
func TestALookupItsRequestsDeadlineEndedDoesNotCount(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
			return true, nil
		}))
		storage.hanging.Store(true)

		for i := range breakerFailures {
			session, _ := anActionsSession(t, service, fmt.Sprintf("billet-lookup-%d", i))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			response, handled, err := service.actionsResponse(twirp(t, actionsDownloadPath,
				`{"key":"npm","restore_keys":[],"version":"v1"}`).WithContext(ctx), session)
			cancel()
			if response != nil {
				response.Body.Close()
			}
			if !handled || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a lookup past its request's deadline: handled %v, %v", handled, err)
			}
		}
		time.Sleep(2 * testStoreDeadline)
		synctest.Wait()
		service.breaker.mu.Lock()
		failures := len(service.breaker.failures)
		service.breaker.mu.Unlock()
		if failures != 0 || service.breaker.isOpen() {
			t.Fatalf("requests' own deadlines counted %d failures against the store", failures)
		}
		close(storage.release)
	})
}

// A LOOKUP THE STORE LEAVES HANGING IS BOUNDED AT THE DEADLINE, AND COUNTS.
func TestALookupThatHangsCountsAtTheDeadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
			return true, nil
		}))
		storage.hanging.Store(true)

		for i := range breakerFailures {
			session, _ := anActionsSession(t, service, fmt.Sprintf("billet-hung-%d", i))
			start := time.Now()
			response, handled, err := service.actionsResponse(twirp(t, actionsDownloadPath,
				`{"key":"npm","restore_keys":[],"version":"v1"}`), session)
			if response != nil {
				response.Body.Close()
			}
			if !handled || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("handled %v, %v", handled, err)
			}
			if waited := time.Since(start); waited != testStoreDeadline {
				t.Fatalf("a lookup waited %s, want the deadline", waited)
			}
		}
		synctest.Wait()
		if !service.breaker.isOpen() {
			t.Fatal("lookups that hung left the breaker closed")
		}
		close(storage.release)
	})
}

// EVERY ROUTE THAT ASKS FOR A VOLUME CAN END A PROBATION, and none asks while another
// job's trial is out.
func TestEachRouteThatAsksForAVolumeTakesTheTrial(t *testing.T) {
	t.Parallel()

	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	twirpRoute := func(path string) func(*testing.T, *CacheService, *cacheSession, string) {
		return func(t *testing.T, service *CacheService, session *cacheSession, _ string) {
			t.Helper()
			response, _, err := service.actionsResponse(
				twirp(t, path, `{"key":"npm","restore_keys":[],"version":"v1"}`), session)
			if response != nil {
				response.Body.Close()
			}
			if err != nil && !errors.Is(err, errStoreOpen) {
				t.Fatalf("%s: %v", path, err)
			}
		}
	}
	for name, route := range map[string]func(*testing.T, *CacheService, *cacheSession, string){
		"the Docker image store": func(t *testing.T, service *CacheService, _ *cacheSession, token string) {
			t.Helper()
			cacheRequest(t, service, token, "/v1/docker-store", map[string]any{"architecture": "amd64"})
		},
		"a sticky disk": func(t *testing.T, service *CacheService, _ *cacheSession, token string) {
			t.Helper()
			cacheRequest(t, service, token, "/v1/volumes", map[string]any{"key": "disk", "size_bytes": 1 << 20})
		},
		"the Go cache": func(t *testing.T, service *CacheService, _ *cacheSession, token string) {
			t.Helper()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/cas/go/cas/"+digest, http.NoBody)
			request.Header.Set("Authorization", "Bearer "+token)
			service.ServeHTTP(httptest.NewRecorder(), request)
		},
		"an Actions restore":     twirpRoute(actionsDownloadPath),
		"an Actions reservation": twirpRoute(actionsCreatePath),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				storage := newHangingStore()
				service := guardedService(t, storage)
				service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
					return true, nil
				}))
				session, token := anActionsSession(t, service, "billet-route")
				onProbation(service.breaker)

				// ANOTHER JOB'S TRIAL IS OUT.
				if !service.breaker.admit() {
					t.Fatal("no trial to take")
				}
				asked := storage.volumesAsked()
				route(t, service, session, token)
				if storage.volumesAsked() != asked {
					t.Fatal("a route asked for a volume with another job's trial out")
				}

				// ITS TIME UP, THIS ROUTE TAKES IT, AND ITS VOLUME ENDS THE PROBATION.
				time.Sleep(testStoreDeadline)
				session.mu.Lock()
				session.casRetryAt = nil
				session.mu.Unlock()
				route(t, service, session, token)
				if storage.volumesAsked() == asked {
					t.Fatal("a route asked for no volume with the trial free")
				}
				if got := service.breaker.report().State; got != lease.CacheStoreClosed {
					t.Fatalf("after a volume was handed over: state %q", got)
				}
			})
		})
	}
}

// THE PROXY SAYS WHY, AND TURNS AWAY WHAT GITHUB COULD NOT ANSWER.
func TestTheProxysAnswerToAStoreNotAsked(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		storage := newHangingStore()
		service := guardedService(t, storage)
		service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
			return true, nil
		}))
		proxy := &actionsProxy{service: service}
		failThree(service.breaker)
		observed := func(session *cacheSession) alloc.ActionsCache {
			session.mu.Lock()
			defer session.mu.Unlock()

			return alloc.ActionsCache(session.observed.ActionsCache)
		}

		// A FINALIZE THIS NODE HOLDS NO RESERVATION FOR GOES TO GITHUB, WHICH HAS IT.
		spliced, _ := anActionsSession(t, service, "billet-proxy-spliced")
		response, handled := proxy.respond(twirp(t, actionsFinalizePath,
			`{"key":"npm","version":"v1","size_bytes":"1"}`), spliced)
		if response != nil {
			response.Body.Close()
		}
		if response != nil || handled {
			t.Fatalf("a finalize of GitHub's: %v, %v", response, handled)
		}
		if got := observed(spliced); got != alloc.ActionsCacheBypassed {
			t.Fatalf("a finalize of GitHub's: observed %q", got)
		}

		// ONE IT DOES FAILS HERE, BEING NO ONE ELSE'S.
		reserved, _ := anActionsSession(t, service, "billet-proxy-reserved")
		reserved.mu.Lock()
		reserved.actions["held"] = &actionsArchive{
			ID: "held", Signature: "sig", Mode: actionsModeUpload, CacheKey: "npm", Version: "v1",
		}
		reserved.mu.Unlock()
		response, handled = proxy.respond(twirp(t, actionsFinalizePath,
			`{"key":"npm","version":"v1","size_bytes":"1"}`), reserved)
		if !handled || response == nil || response.StatusCode != http.StatusBadGateway {
			t.Fatalf("a finalize of this node's: %v, %v", response, handled)
		}
		response.Body.Close()
		if got := observed(reserved); got != alloc.ActionsCacheUnavailable {
			t.Fatalf("a finalize of this node's: observed %q", got)
		}

		// AND SO DOES A BLOB.
		response, handled = proxy.respond(actionsRequestForTest(t, http.MethodGet,
			"https://"+actionsResultsHost+actionsBlobPrefix+"held?sig=sig", ""), reserved)
		if !handled || response == nil || response.StatusCode != http.StatusBadGateway {
			t.Fatalf("a blob: %v, %v", response, handled)
		}
		response.Body.Close()

		// ON PROBATION WITH ANOTHER JOB'S TRIAL OUT, A RESTORE GOES TO GITHUB AND SAYS WHY.
		onProbation(service.breaker)
		if !service.breaker.admit() {
			t.Fatal("no trial to take")
		}
		waiting, _ := anActionsSession(t, service, "billet-proxy-waiting")
		response, handled = proxy.respond(twirp(t, actionsDownloadPath,
			`{"key":"npm","restore_keys":[],"version":"v1"}`), waiting)
		if response != nil {
			response.Body.Close()
		}
		if response != nil || handled {
			t.Fatalf("a restore on probation: %v, %v", response, handled)
		}
		if got := observed(waiting); got != alloc.ActionsCacheBypassed {
			t.Fatalf("a restore on probation: observed %q", got)
		}
	})
}

// A PUBLICATION IS A WAY BACK: a node with no job left to ask for a volume would otherwise
// sit on probation until its window dropped what the job wrote. While it cannot be asked,
// it waits, and costs nothing.
func TestAPublicationWaitsForTheStoreAndCanEndAProbation(t *testing.T) {
	t.Parallel()

	settled := func(t *testing.T, service *CacheService) {
		t.Helper()
		if got := service.breaker.report().State; got != lease.CacheStoreClosed {
			t.Fatalf("after a publication: state %q", got)
		}
	}

	t.Run("deferred", func(t *testing.T) {
		t.Parallel()

		storage := &fakeCacheStore{}
		service, token, instance := scopedService(t, provider.TrustUntrusted, defaultBranchCache(), nil, storage)
		service.GuardStore("ceph", testStoreDeadline)
		attachAndCommit(t, service, token)
		failThree(service.breaker)

		if err := service.SettleCompleted(t.Context(), instance, true, publishingAuthority()); err != nil {
			t.Fatalf("SettleCompleted: %v", err)
		}
		if err := service.Close(t.Context(), instance); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// THE SESSION WAITS FOR ITS PUBLICATION, inside the window.
		if err := service.RetryClosed(t.Context()); err == nil || !strings.Contains(err.Error(), "publication pending") {
			t.Fatalf("RetryClosed with the breaker open: %v", err)
		}
		if storage.snapshots != 0 || storage.published != 0 {
			t.Fatalf("with the breaker open: %d snapshots, %d published", storage.snapshots, storage.published)
		}

		onProbation(service.breaker)
		if err := service.RetryClosed(t.Context()); err != nil {
			t.Fatalf("RetryClosed: %v", err)
		}
		if storage.published != 1 {
			t.Fatalf("on probation: %d published, want 1", storage.published)
		}
		settled(t, service)
	})

	t.Run("content-addressed", func(t *testing.T) {
		t.Parallel()

		storage := &fakeCacheStore{}
		service, _, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
		service.GuardStore("ceph", testStoreDeadline)
		ours := "our object"
		if got := casRequest(t, service, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf(ours), ours); got.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
		}
		failThree(service.breaker)

		endSession(t, service, instance, publishingAuthority())
		if storage.snapshots != 0 || storage.published != 0 {
			t.Fatalf("with the breaker open: %d snapshots, %d published", storage.snapshots, storage.published)
		}

		onProbation(service.breaker)
		if err := service.RetryClosed(t.Context()); err != nil {
			t.Fatalf("RetryClosed: %v", err)
		}
		if storage.published == 0 {
			t.Fatal("on probation nothing was published")
		}
		settled(t, service)
	})

	t.Run("a job's own commit", func(t *testing.T) {
		t.Parallel()

		storage := newHangingStore()
		service := guardedService(t, storage)
		credentials, err := service.PrepareScoped("billet-own", CacheSessionScope{Trust: provider.TrustTrusted})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		if got := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
			"key": "disk", "size_bytes": 1 << 20,
		}); got.Code != http.StatusCreated {
			t.Fatalf("attach: status %d: %s", got.Code, got.Body.String())
		}
		onProbation(service.breaker)
		got := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/commit", map[string]any{
			"filesystem": map[string]any{"type": "ext4", "uuid": "fs-1", "clean": true},
		})
		if !strings.Contains(got.Body.String(), `"published":true`) {
			t.Fatalf("a commit on probation: %s", got.Body.String())
		}
		settled(t, service)
	})
}

// A DISCARD IS NOT WHAT A JOB WAITS ON: every `rbd rm` failed for an hour on 2026-09-25
// while clones worked.
func TestAStoreThatWillNotDiscardIsStillAsked(t *testing.T) {
	t.Parallel()

	storage := newHangingStore()
	storage.discardFailures = 1000
	service := guardedService(t, storage)
	// ATTACHED FIRST, because a volume handed over between them clears the count.
	var instances []string
	for i := range 2 * breakerFailures {
		instance := fmt.Sprintf("billet-discard-%d", i)
		credentials, err := service.PrepareScoped(instance, CacheSessionScope{Trust: provider.TrustTrusted})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		if got := cacheRequest(t, service, credentials.Token, "/v1/docker-store",
			map[string]any{"architecture": "amd64"}); got.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", got.Code, got.Body.String())
		}
		instances = append(instances, instance)
	}
	for _, instance := range instances {
		if err := service.Cleanup(t.Context(), instance); err == nil {
			t.Fatal("a discard that failed was not reported")
		}
	}
	if got := storage.discards(); got < len(instances) {
		t.Fatalf("%d discards were attempted, want at least %d", got, len(instances))
	}
	if !service.breaker.allow() {
		t.Fatal("discards that failed stopped jobs asking the store")
	}
}

// WHAT A CACHE DID IS KEPT AT ITS MOST TELLING: a job that was served, or told why not,
// says that rather than that a later call was skipped.
func TestBypassedSaysLessThanAnythingElseACacheDid(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ first, then, want alloc.BuildCache }{
		{alloc.BuildCacheWarm, alloc.BuildCacheBypassed, alloc.BuildCacheWarm},
		{alloc.BuildCacheBypassed, alloc.BuildCacheWarm, alloc.BuildCacheWarm},
		{alloc.BuildCacheUnavailable, alloc.BuildCacheBypassed, alloc.BuildCacheUnavailable},
		{alloc.BuildCacheBypassed, alloc.BuildCacheUnavailable, alloc.BuildCacheUnavailable},
		{"", alloc.BuildCacheBypassed, alloc.BuildCacheBypassed},
	} {
		session := &cacheSession{}
		if tc.first != "" {
			noteBuildCache(session, config.CacheGo, tc.first)
		}
		noteBuildCache(session, config.CacheGo, tc.then)
		if got := session.building[config.CacheGo]; got != tc.want {
			t.Errorf("%q then %q: kept %q, want %q", tc.first, tc.then, got, tc.want)
		}
	}
}
