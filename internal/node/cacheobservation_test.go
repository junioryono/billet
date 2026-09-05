package node

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// recordingObserver is a CacheObserver that keeps what it was told and refuses
// the first `refuse` calls, the way a control plane having a bad minute would.
type recordingObserver struct {
	mu     sync.Mutex
	refuse int
	calls  []observedCall
	// inspect runs inside every call, before the answer, so a test can look at
	// what is on disk at the moment the plane is told.
	inspect func(obs alloc.CacheObservation)
}

type observedCall struct {
	instance string
	leaseID  string
	epoch    int64
	obs      alloc.CacheObservation
}

func (o *recordingObserver) ObserveCache(
	_ context.Context, instance, leaseID string, epoch int64, obs alloc.CacheObservation,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.calls = append(o.calls, observedCall{instance: instance, leaseID: leaseID, epoch: epoch, obs: obs})

	if o.inspect != nil {
		o.inspect(obs)
	}

	if o.refuse > 0 {
		o.refuse--

		return errors.New("the control plane could not be reached")
	}

	return nil
}

func (o *recordingObserver) recorded() []observedCall {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]observedCall(nil), o.calls...)
}

// observedService is a cache service with one trusted, non-intercepting
// session and a recording observer.
func observedService(t *testing.T, storage *fakeCacheStore) (*CacheService, *recordingObserver, string) {
	t.Helper()

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	observer := &recordingObserver{}
	service.SetCacheObserver(observer)

	// THE SESSION NAMES ITS LEASE, the way the runner's launch scopes it, so an
	// observation is attributable after the runner has forgotten the instance.
	credentials, err := service.PrepareScoped("billet-one", CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: "one", Epoch: 3,
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service, observer, credentials.Token
}

// THE IMAGE-STORE OBSERVATION NAMES WHAT THE STORE ANSWERED, at the moment the
// guest asked: a clone is warm and names its generation, a miss that became a
// fresh volume is cold, and a store that failed is unavailable.
func TestTheImageStoreObservationNamesWhatTheStoreAnswered(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		storage *fakeCacheStore
		status  int
		want    alloc.CacheObservation
	}{
		"cold": {
			storage: &fakeCacheStore{}, status: http.StatusCreated,
			want: alloc.CacheObservation{ImageCache: alloc.ImageCacheCold},
		},
		"warm": {
			storage: &fakeCacheStore{current: "gen-7"}, status: http.StatusCreated,
			want: alloc.CacheObservation{ImageCache: alloc.ImageCacheWarm, CacheGeneration: "gen-7"},
		},
		"unavailable": {
			storage: &fakeCacheStore{createErr: errors.New("rbd: pool is read-only")},
			status:  http.StatusServiceUnavailable,
			want:    alloc.CacheObservation{ImageCache: alloc.ImageCacheUnavailable},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, observer, token := observedService(t, tc.storage)

			docker := cacheRequest(t, service, token, "/v1/docker-store",
				map[string]any{"architecture": "amd64"})
			if docker.Code != tc.status {
				t.Fatalf("Docker store status = %d, want %d: %s", docker.Code, tc.status, docker.Body.String())
			}

			calls := observer.recorded()
			if len(calls) != 1 || calls[0].instance != "billet-one" || calls[0].obs != tc.want ||
				calls[0].leaseID != "one" || calls[0].epoch != 3 {
				t.Fatalf("observer was told %+v, want one observation %+v for billet-one under "+
					"lease one at epoch 3", calls, tc.want)
			}
		})
	}
}

// THE ACTIONS OBSERVATION IS WHAT INTERCEPTION FINALLY DID FOR THE FIRST
// CacheService CALL, driven through the proxy, where that is decided: served
// from the site store, refused by the kill switch, spliced upstream for a client
// billet does not serve locally OR for a local handler that failed and was
// retried through GitHub, and unavailable for a reserved call that failed
// locally and had nowhere to go.
func TestTheActionsObservationNamesWhatInterceptionDid(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		allowed   bool
		anonymous bool
		// storeErr makes the local handler fail on a call GitHub can still answer.
		storeErr error
		// reserved stages an upload archive on the session first, so the finalize
		// is bound to a reservation only billet holds; the finalize then fails
		// locally because nothing was uploaded to it.
		reserved bool
		want     alloc.ActionsCache
		status   int
	}{
		"served":   {allowed: true, want: alloc.ActionsCacheServed, status: http.StatusOK},
		"disabled": {allowed: false, want: alloc.ActionsCacheDisabled},
		"spliced":  {allowed: true, anonymous: true, want: alloc.ActionsCacheSpliced},
		"spliced after a local failure": {
			allowed: true, storeErr: errors.New("rbd: pool is read-only"),
			want: alloc.ActionsCacheSpliced,
		},
		"unavailable on a reserved failure": {
			allowed: true, reserved: true,
			want: alloc.ActionsCacheUnavailable, status: http.StatusBadGateway,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, storage, session, _ := testActionsService(t)
			storage.createErr = tc.storeErr
			observer := &recordingObserver{}
			service.SetCacheObserver(observer)
			service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
				return tc.allowed, nil
			}))
			proxy := service.actionsProxy()
			if proxy == nil {
				t.Fatal("the intercepting session has no proxy")
			}

			path, body := actionsCreatePath, `{"key":"linux-npm-abc","version":"v1"}`
			if tc.reserved {
				session.mu.Lock()
				session.actions["reserved"] = &actionsArchive{
					ID: "reserved", Mode: actionsModeUpload, CacheKey: "linux-npm-abc",
					Version: "v1", StoreKey: "k",
				}
				session.mu.Unlock()
				path, body = actionsFinalizePath, `{"key":"linux-npm-abc","version":"v1","size_bytes":"4"}`
			}

			first := actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+path, body)
			if tc.anonymous {
				// A client billet does not recognise: not the toolkit, not the
				// loopback adapter. It goes upstream, and that is what is observed.
				first.Header.Del("User-Agent")
			}

			response, handled := proxy.respond(first, session)
			if handled != (tc.status != 0) {
				t.Fatalf("respond handled=%t, want %t", handled, tc.status != 0)
			}
			if handled && response.StatusCode != tc.status {
				t.Fatalf("respond status = %d, want %d", response.StatusCode, tc.status)
			}
			closeActionsResponse(t, response)

			calls := observer.recorded()
			want := alloc.CacheObservation{ActionsCache: tc.want}
			if len(calls) != 1 || calls[0].obs != want {
				t.Fatalf("observer was told %+v, want one observation %+v", calls, want)
			}

			// A BLOB LEG IS NOT A CacheService CALL and observes nothing more; nor
			// does a second call, because the first observation is kept.
			blob := actionsRequestForTest(t, http.MethodHead,
				"https://"+actionsResultsHost+actionsBlobPrefix+"nothing", "")
			blobResponse, _ := proxy.respond(blob, session)
			closeActionsResponse(t, blobResponse)
			again := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsCreatePath, `{"key":"linux-npm-def","version":"v1"}`)
			if tc.anonymous {
				again.Header.Del("User-Agent")
			}
			secondResponse, _ := proxy.respond(again, session)
			closeActionsResponse(t, secondResponse)
			if told := observer.recorded(); len(told) != 1 {
				t.Fatalf("observer was told %d times, want the first observation only", len(told))
			}
		})
	}
}

// closeActionsResponse closes a locally served response; a spliced call has none.
func closeActionsResponse(t *testing.T, response *http.Response) {
	t.Helper()

	if response == nil || response.Body == nil {
		return
	}

	if err := response.Body.Close(); err != nil {
		t.Fatalf("close the response body: %v", err)
	}
}

// sessionRecord reads one session's durable record as it is on disk right now.
func sessionRecord(t *testing.T, service *CacheService, token string) durableCacheSession {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(service.stateDir, token+".json"))
	if err != nil {
		t.Fatalf("read the session record: %v", err)
	}

	var record durableCacheSession
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode the session record: %v", err)
	}

	return record
}

// A REPORT THE PLANE REFUSED IS KEPT ON THE SESSION AND RESENT BY THE PROCESS
// THAT ENDS THE COMPUTE, which need not be the one that observed it: the
// service is rebuilt over the same state directory, the way a restarted node
// rebuilds it, and its cleanup is what resends.
//
// THE RECORD IS ON DISK BEFORE THE PLANE IS TOLD, and the observer checks that
// at the moment it is called: an observation reported first and written second
// is one a crash in between would report once and never again.
func TestAnUnreportedObservationIsResentAfterARestart(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{current: "gen-3"}
	service, observer, token := observedService(t, storage)
	observer.refuse = 1
	observer.inspect = func(alloc.CacheObservation) {
		record := sessionRecord(t, service, token)
		if record.Observed.ImageCache != string(alloc.ImageCacheWarm) ||
			record.Observed.CacheGeneration != "gen-3" || record.Observed.Reported {
			t.Errorf("the plane was told before the record was durable: on disk %+v", record.Observed)
		}
		if record.Slots[0] == nil || !record.Slots[0].Docker {
			t.Error("the plane was told before the volume was in durable custody")
		}
	}

	docker := cacheRequest(t, service, token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"})
	if docker.Code != http.StatusCreated {
		t.Fatalf("Docker store status = %d: %s", docker.Code, docker.Body.String())
	}
	if calls := observer.recorded(); len(calls) != 1 {
		t.Fatalf("observer was told %d times before the restart, want the one refused report", len(calls))
	}

	// DURABLE AS UNREPORTED, in between.
	if record := sessionRecord(t, service, token); record.Observed.Reported {
		t.Fatalf("session record after a refused report = %+v, want it unreported", record.Observed)
	}

	// THE RESTART. A second service over the same directory loads the session,
	// and its observer is what the resend reaches.
	restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", service.rootState,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService after the restart: %v", err)
	}
	after := &recordingObserver{}
	restarted.SetCacheObserver(after)

	if err := restarted.Cleanup(t.Context(), "billet-one"); err != nil {
		t.Fatalf("Cleanup after the restart: %v", err)
	}

	calls := after.recorded()
	want := alloc.CacheObservation{
		ImageCache: alloc.ImageCacheWarm, CacheGeneration: "gen-3", ActionsCache: alloc.ActionsCacheOff,
	}
	if len(calls) != 1 || calls[0].instance != "billet-one" || calls[0].obs != want ||
		calls[0].leaseID != "one" || calls[0].epoch != 3 {
		t.Fatalf("the restarted service told its observer %+v, want %+v for billet-one under "+
			"lease one at epoch 3, which only the session record can name after a restart",
			calls, want)
	}
}

// blockedCall dispatches one CacheService call through the real proxy and
// holds it inside the kill-switch read until released, so a test can look at
// the session while the call is in flight. The policy blocks the FIRST call
// only; later calls, such as the ones cleanup's settlement makes, answer at
// once.
func blockedCall(
	t *testing.T, service *CacheService, session *cacheSession,
) (entered <-chan struct{}, release func(), done <-chan bool) {
	t.Helper()

	enteredCh := make(chan struct{})
	releaseCh := make(chan struct{})
	doneCh := make(chan bool, 1)

	var once sync.Once
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(enteredCh)
			<-releaseCh
		}

		return true, nil
	}))

	proxy := service.actionsProxy()
	if proxy == nil {
		t.Fatal("the intercepting session has no proxy")
	}

	create := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath, `{"key":"linux-npm-abc","version":"v1"}`)

	go func() {
		response, handled := proxy.respond(create, session)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		doneCh <- handled
	}()

	var releaseOnce sync.Once

	return enteredCh, func() { releaseOnce.Do(func() { close(releaseCh) }) }, doneCh
}

// A CALL STILL BEING ANSWERED IS NOT SETTLED AS UNUSED. The proxy marks a
// CacheService call in flight, durably for the first one, before it is
// dispatched, and clears the mark once its outcome is recorded; a settlement in
// between leaves the Actions half alone, the outcome recorded afterwards is
// reported with the lease the session carries, and a session cleanup has
// already finished is not written back to disk as an orphan.
//
// DRIVEN THROUGH THE PROXY, so a dispatch that stopped marking the call would
// fail this rather than a helper called by hand.
func TestSettlementLeavesAnInFlightActionsCallAlone(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	recordPath := filepath.Join(service.stateDir, session.token+".json")

	entered, release, done := blockedCall(t, service, session)
	defer release()
	<-entered

	// THE FIRST CALL IS MARKED DURABLY BEFORE IT IS DISPATCHED.
	if record := sessionRecord(t, service, session.token); !record.Observed.ActionsPending {
		t.Fatalf("session record while the first call is in flight = %+v, want it pending",
			record.Observed)
	}

	if err := service.Cleanup(t.Context(), session.instance); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	calls := observer.recorded()
	if len(calls) != 1 || calls[0].obs != (alloc.CacheObservation{ImageCache: alloc.ImageCacheUnused}) {
		t.Fatalf("settlement with a call in flight told the observer %+v, want the image half "+
			"only", calls)
	}
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the session record survived its cleanup: %v", err)
	}

	// THE CALL FINISHES INTO A CLOSED SESSION: the handler refuses it, the proxy
	// hands it to GitHub, and that is the outcome recorded and reported.
	release()
	if handled := <-done; handled {
		t.Fatal("a call finishing into a closed session was answered locally")
	}

	calls = observer.recorded()
	want := alloc.CacheObservation{ImageCache: alloc.ImageCacheUnused, ActionsCache: alloc.ActionsCacheSpliced}
	if len(calls) != 2 || calls[1].obs != want {
		t.Fatalf("the late outcome was reported as %+v, want %+v", calls, want)
	}

	// NOT WRITTEN BACK: the session is finished, and a record here would load
	// on the next start as a session for compute that does not exist.
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a late outcome recreated the finished session's record: %v", err)
	}
	service.mu.Lock()
	_, indexed := service.byInstance[session.instance]
	service.mu.Unlock()
	if indexed {
		t.Fatal("a late outcome put the finished session back in the service's indexes")
	}
}

// A CALL A CRASH INTERRUPTED IS NOT SETTLED AS UNUSED EITHER. The pending mark
// is durable, so the process that loads the session after the crash leaves the
// Actions half unknown: the guest asked, and what it got could not be told.
func TestAnInterruptedFirstActionsCallStaysUnknownAfterARestart(t *testing.T) {
	t.Parallel()

	service, storage, session, _ := testActionsService(t)
	service.SetCacheObserver(&recordingObserver{})

	// The crash: the call is dispatched through the proxy and never returns
	// while the "next process" below loads what the record says.
	entered, release, _ := blockedCall(t, service, session)
	defer release()
	<-entered

	restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", service.rootState,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService after the restart: %v", err)
	}
	after := &recordingObserver{}
	restarted.SetCacheObserver(after)

	if err := restarted.Cleanup(t.Context(), session.instance); err != nil {
		t.Fatalf("Cleanup after the restart: %v", err)
	}

	calls := after.recorded()
	want := alloc.CacheObservation{ImageCache: alloc.ImageCacheUnused}
	if len(calls) != 1 || calls[0].obs != want {
		t.Fatalf("the restarted service told its observer %+v, want %+v with the Actions half "+
			"left unknown", calls, want)
	}
}

// A SESSION'S LEASE MUST NAME ITS OWN INSTANCE, at creation and on load, or a
// record could attribute what one guest saw to another job's lease.
func TestASessionRefusesALeaseThatNamesAnotherInstance(t *testing.T) {
	t.Parallel()

	service, _, _ := observedService(t, &fakeCacheStore{})

	for name, scope := range map[string]CacheSessionScope{
		"another instance's lease": {Trust: provider.TrustTrusted, LeaseID: "two", Epoch: 1},
		"an epoch with no lease":   {Trust: provider.TrustTrusted, Epoch: 1},
		"a negative epoch":         {Trust: provider.TrustTrusted, LeaseID: "other", Epoch: -1},
	} {
		if _, err := service.PrepareScoped("billet-other", scope); err == nil {
			t.Errorf("PrepareScoped accepted %s: %+v", name, scope)
		}
	}

	// A FRESH LEASE'S EPOCH IS ZERO, and a record from before the session
	// carried a lease names none; both are ordinary.
	if _, err := service.PrepareScoped("billet-fresh", CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: "fresh",
	}); err != nil {
		t.Errorf("PrepareScoped refused a fresh lease at epoch zero: %v", err)
	}
	if _, err := service.PrepareScoped("billet-legacy", CacheSessionScope{
		Trust: provider.TrustTrusted,
	}); err != nil {
		t.Errorf("PrepareScoped refused a session naming no lease: %v", err)
	}

	// AND ON LOAD, where the record is whatever is on disk. The token and the
	// filename pass the earlier checks, so the lease check is the one that
	// decides.
	token := strings.Repeat("ab", 32)
	record := durableCacheSession{
		Token: token, Instance: "billet-other", Trust: provider.TrustTrusted, LeaseID: "two",
	}
	err := record.valid(token + ".json")
	if err == nil || !strings.Contains(err.Error(), "names a different instance") {
		t.Errorf("a record whose lease names another instance loaded, or was refused for another "+
			"reason: %v", err)
	}
	record.LeaseID = "other"
	if err := record.valid(token + ".json"); err != nil {
		t.Errorf("a record whose lease names its own instance was refused: %v", err)
	}
}

// A FINISHED SESSION WRITES NO RECORD, whoever asks: the one write path refuses,
// so a late handler cannot put an orphan back for the next start to load.
func TestAFinishedSessionWritesNoRecord(t *testing.T) {
	t.Parallel()

	service, _, token := observedService(t, &fakeCacheStore{})

	service.mu.Lock()
	session := service.byToken[token]
	service.mu.Unlock()

	if err := service.Cleanup(t.Context(), "billet-one"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	session.mu.Lock()
	err := service.persistSession(session)
	session.mu.Unlock()

	if !errors.Is(err, errSessionFinished) {
		t.Fatalf("a write to a finished session was answered %v, want errSessionFinished", err)
	}
	if _, statErr := os.Stat(filepath.Join(service.stateDir, token+".json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the finished session's record is back: %v", statErr)
	}
}

// A GUEST THAT WENT AWAY AS ITS CALL WAS ANSWERED DOES NOT LOSE THE OUTCOME.
// The outcome is recorded under a detached, bounded context rather than the
// request's, which is already cancelled by then.
func TestAnOutcomeIsRecordedAfterTheGuestsRequestIsCancelled(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	service.observeActions(ctx, session, alloc.ActionsCacheServed)

	record := sessionRecord(t, service, session.token)
	if record.Observed.ActionsCache != string(alloc.ActionsCacheServed) || !record.Observed.Reported {
		t.Fatalf("session record after a cancelled request = %+v, want the served outcome reported",
			record.Observed)
	}
	if calls := observer.recorded(); len(calls) != 1 ||
		calls[0].obs != (alloc.CacheObservation{ActionsCache: alloc.ActionsCacheServed}) {
		t.Fatalf("observer was told %+v, want the served outcome", calls)
	}
}

// A SESSION THAT ENDS HAVING OBSERVED NOTHING SAYS SO: the guest never asked
// for an image store, and either never made a CacheService call or had no
// interception to make one through.
func TestAnUntouchedSessionSettlesAsUnused(t *testing.T) {
	t.Parallel()

	t.Run("without interception", func(t *testing.T) {
		t.Parallel()

		service, observer, _ := observedService(t, &fakeCacheStore{})

		if err := service.Cleanup(t.Context(), "billet-one"); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}

		calls := observer.recorded()
		want := alloc.CacheObservation{ImageCache: alloc.ImageCacheUnused, ActionsCache: alloc.ActionsCacheOff}
		if len(calls) != 1 || calls[0].obs != want {
			t.Fatalf("observer was told %+v, want %+v", calls, want)
		}
	})

	t.Run("with interception", func(t *testing.T) {
		t.Parallel()

		service, _, session, _ := testActionsService(t)
		observer := &recordingObserver{}
		service.SetCacheObserver(observer)

		if err := service.Cleanup(t.Context(), session.instance); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}

		calls := observer.recorded()
		want := alloc.CacheObservation{ImageCache: alloc.ImageCacheUnused, ActionsCache: alloc.ActionsCacheUnused}
		if len(calls) != 1 || calls[0].obs != want {
			t.Fatalf("observer was told %+v, want %+v", calls, want)
		}
	})
}

// THE RUNNER TURNS AN INSTANCE NAME INTO THE LEASE IT HOLDS, and the row shows
// the observation: the image store at the guest's request, and the settled
// halves when the compute is destroyed, before the runner forgets the request.
func TestTheRunnerRecordsCacheObservationsAgainstTheLeaseItHolds(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}
	storage := &fakeCacheStore{current: "gen-5"}
	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(), storage,
		&fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	a, host := newAllocatorWithHost(t)
	runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
	lease := assignedLease(t, a)
	if err := runner.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	docker := cacheRequest(t, service, p.launched[0].CacheToken, "/v1/docker-store",
		map[string]any{"architecture": "amd64"})
	if docker.Code != http.StatusCreated {
		t.Fatalf("Docker store status = %d: %s", docker.Code, docker.Body.String())
	}

	early, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement after the attach: %v", err)
	}
	if early.ImageCache != alloc.ImageCacheWarm || early.CacheGeneration != "gen-5" || early.ActionsCache != "" {
		t.Fatalf("history after the attach = %+v, want a warm gen-5 image store and no Actions "+
			"observation yet", early)
	}

	if err := runner.Destroy(t.Context(), 11); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	settled, err := a.HistoryPlacement(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryPlacement after the destroy: %v", err)
	}
	if settled.ImageCache != alloc.ImageCacheWarm || settled.CacheGeneration != "gen-5" ||
		settled.ActionsCache != alloc.ActionsCacheOff {
		t.Fatalf("history after the destroy = %+v, want the warm store kept and Actions settled as off",
			settled)
	}
}
