package node

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
}

type observedCall struct {
	instance string
	obs      alloc.CacheObservation
}

func (o *recordingObserver) ObserveCache(_ context.Context, instance string, obs alloc.CacheObservation) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.calls = append(o.calls, observedCall{instance: instance, obs: obs})

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

	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
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
			if len(calls) != 1 || calls[0].instance != "billet-one" || calls[0].obs != tc.want {
				t.Fatalf("observer was told %+v, want one observation %+v for billet-one", calls, tc.want)
			}
		})
	}
}

// THE ACTIONS OBSERVATION IS WHAT INTERCEPTION DID FOR THE FIRST CacheService
// CALL: served from the site store, refused by the kill switch, or spliced
// upstream for a client billet does not serve locally.
func TestTheActionsObservationNamesWhatInterceptionDid(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		allowed   bool
		anonymous bool
		want      alloc.ActionsCache
	}{
		"served":   {allowed: true, want: alloc.ActionsCacheServed},
		"disabled": {allowed: false, want: alloc.ActionsCacheDisabled},
		"spliced":  {allowed: true, anonymous: true, want: alloc.ActionsCacheSpliced},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, _, session, _ := testActionsService(t)
			observer := &recordingObserver{}
			service.SetCacheObserver(observer)
			service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
				return tc.allowed, nil
			}))

			create := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsCreatePath, `{"key":"linux-npm-abc","version":"v1"}`)
			if tc.anonymous {
				// A client billet does not recognise: not the toolkit, not the
				// loopback adapter. It goes upstream, and that is what is observed.
				create.Header.Del("User-Agent")
			}

			response, _, err := service.actionsResponse(create, session)
			if err != nil {
				t.Fatalf("actionsResponse: %v", err)
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
			blobResponse, _, err := service.actionsResponse(blob, session)
			if err != nil {
				t.Fatalf("blob actionsResponse: %v", err)
			}
			closeActionsResponse(t, blobResponse)
			again := actionsRequestForTest(t, http.MethodPost,
				"https://"+actionsResultsHost+actionsCreatePath, `{"key":"linux-npm-def","version":"v1"}`)
			if tc.anonymous {
				again.Header.Del("User-Agent")
			}
			secondResponse, _, err := service.actionsResponse(again, session)
			if err != nil {
				t.Fatalf("second actionsResponse: %v", err)
			}
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

// A REPORT THE PLANE REFUSED IS KEPT ON THE SESSION AND RESENT WHEN THE
// COMPUTE ENDS, with what the session never observed filled in.
func TestAnUnreportedObservationIsResentWhenTheSessionEnds(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{current: "gen-3"}
	service, observer, token := observedService(t, storage)
	observer.refuse = 1

	docker := cacheRequest(t, service, token, "/v1/docker-store",
		map[string]any{"architecture": "amd64"})
	if docker.Code != http.StatusCreated {
		t.Fatalf("Docker store status = %d: %s", docker.Code, docker.Body.String())
	}

	// DURABLE AS UNREPORTED, in between: a restart here must resend it too.
	raw, err := os.ReadFile(filepath.Join(service.stateDir, token+".json"))
	if err != nil {
		t.Fatalf("read the session record: %v", err)
	}
	var record durableCacheSession
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode the session record: %v", err)
	}
	if record.Observed.ImageCache != string(alloc.ImageCacheWarm) ||
		record.Observed.CacheGeneration != "gen-3" || record.Observed.Reported {
		t.Fatalf("session record after a refused report = %+v, want the warm observation unreported",
			record.Observed)
	}

	if err := service.Cleanup(t.Context(), "billet-one"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	calls := observer.recorded()
	want := alloc.CacheObservation{
		ImageCache: alloc.ImageCacheWarm, CacheGeneration: "gen-3", ActionsCache: alloc.ActionsCacheOff,
	}
	if len(calls) != 2 || calls[1].obs != want {
		t.Fatalf("observer was told %+v, want the refused warm report and then %+v at the end", calls, want)
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
