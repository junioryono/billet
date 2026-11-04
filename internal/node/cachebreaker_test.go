package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
}

func (r *storeReports) ReportCacheStore(_ context.Context, report lease.CacheStoreReport) error {
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

		// IT COMES BACK AND THE FIRST DISCARD FAILS, so the volume stays, durably.
		storage.hanging.Store(false)
		close(storage.release)
		synctest.Wait()
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
		for range breakerFailures {
			service.breaker.outcome("clone", errors.New("rbd: exit status 110"))
		}
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

		// THE ACTIONS CACHE: A LOOKUP AND A RESERVATION GO TO GITHUB.
		for _, path := range []string{actionsDownloadPath, actionsCreatePath} {
			response, handled, err := service.actionsResponse(
				twirp(t, path, `{"key":"npm","restore_keys":[],"version":"v1"}`), session)
			if response != nil {
				response.Body.Close()
			}
			if err != nil || handled || response != nil {
				t.Fatalf("%s: answered %v, %v, %v, want it left to GitHub", path, response, handled, err)
			}
		}
		session.mu.Lock()
		actions := session.observed.ActionsCache
		session.mu.Unlock()
		if alloc.ActionsCache(actions) != alloc.ActionsCacheBypassed {
			t.Fatalf("the Actions cache: observed %q", actions)
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
	for range breakerFailures {
		service.breaker.outcome("clone", errors.New("rbd: exit status 110"))
	}
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
	if err := service.SettleDocker(t.Context(), "billet-commit", true); err != nil {
		t.Fatalf("SettleDocker: %v", err)
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

		for range breakerFailures {
			service.breaker.outcome("clone", errors.New("rbd: exit status 110"))
		}
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

		cancel()
		<-done
	})
}

// A CANDIDATE THE STORE REFUSES IS ONE JOB'S BAD WRITE, and a node logging hundreds of
// those a day must not send every job cold; the same refusal that timed out is the store.
func TestOnlyAPublicationsTimeoutCountsAgainstTheStore(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		err      error
		wantOpen bool
	}{
		{"a filesystem that is not clean", errors.New("ceph: verify candidate as published: the ext4 filesystem is not clean"), false},
		{"a store that did not answer", fmt.Errorf("rbd did not answer: %w", context.DeadlineExceeded), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := newHangingStore()
			storage.snapshotErr = tc.err
			service := guardedService(t, storage)
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
				if got := cacheRequest(t, service, credentials.Token, "/v1/volumes/0/commit", nil); got.Code != http.StatusOK {
					t.Fatalf("commit: status %d: %s", got.Code, got.Body.String())
				}
			}
			if storage.snapshots != breakerFailures {
				t.Fatalf("%d snapshots were tried, want %d", storage.snapshots, breakerFailures)
			}
			if got := !service.breaker.allow(); got != tc.wantOpen {
				t.Fatalf("open = %v, want %v", got, tc.wantOpen)
			}
		})
	}
}

// A DISCARD IS NOT WHAT A JOB WAITS ON: every `rbd rm` failed for an hour on 2026-09-25
// while clones worked.
func TestAStoreThatWillNotDiscardIsStillAsked(t *testing.T) {
	t.Parallel()

	storage := newHangingStore()
	storage.discardFailures = 1000
	service := guardedService(t, storage)
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
		if err := service.Cleanup(t.Context(), instance); err == nil {
			t.Fatal("a discard that failed was not reported")
		}
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
