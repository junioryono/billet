package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// planeAt registers one node at a wire version and answers a post under its
// incarnation.
func planeAt(t *testing.T, store *fakeStore, version int) func(path string, body any) int {
	t.Helper()

	_, base := serve(t, store)
	incarnation := fmt.Sprintf("cache-store-%d", version)
	post := func(path string, body any) int {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		req.Header.Set(nodeapi.HeaderIncarnation, incarnation)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()

		return resp.StatusCode
	}
	if got := post("/v1/register", nodeapi.RegisterRequest{
		MinVersion: nodeapi.MinVersion, Version: version, Incarnation: incarnation,
		Node: "n1", Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
		Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); got != http.StatusOK {
		t.Fatalf("register at %d: status %d", version, got)
	}

	return post
}

// A WORD ITS WIRE DOES NOT CARRY IS KEPT AS WHAT IT WAS BEFORE THE WORD, and one that
// does keeps it.
func TestBypassedFromAWireThatDoesNotCarryItIsKeptAsUnavailable(t *testing.T) {
	t.Parallel()

	for version, want := range map[int]alloc.ImageCache{
		nodeapi.VersionCacheBreaker - 1: alloc.ImageCacheUnavailable,
		nodeapi.VersionCacheBreaker:     alloc.ImageCacheBypassed,
	} {
		store := &fakeStore{}
		post := planeAt(t, store, version)
		if got := post("/v1/nodes/n1/leases/l1/cache", nodeapi.CacheObservationRequest{
			Epoch: 7, ImageCache: "bypassed",
		}); got != http.StatusNoContent {
			t.Fatalf("v%d: status %d", version, got)
		}
		store.mu.Lock()
		observed := store.observations
		store.mu.Unlock()
		if len(observed) != 1 || observed[0].obs.ImageCache != want {
			t.Fatalf("v%d: recorded %+v, want %q", version, observed, want)
		}
	}
}

func TestTheRouteKeepsAStoreReportAndRefusesWhatItCouldNotKeep(t *testing.T) {
	t.Parallel()

	good := nodeapi.CacheStoreRequest{
		Store: "ceph", State: "open", Since: "2026-11-04T08:00:00Z", Failures: 3, Reason: "timeout",
	}
	for name, tc := range map[string]struct {
		body   nodeapi.CacheStoreRequest
		status int
		kept   int
	}{
		"a report":         {good, http.StatusNoContent, 1},
		"a closed one":     {nodeapi.CacheStoreRequest{Store: "ebs-s3", State: "closed", Since: good.Since}, http.StatusNoContent, 1},
		"an unknown store": {nodeapi.CacheStoreRequest{Store: "zfs", State: "open", Since: good.Since}, http.StatusBadRequest, 0},
		"an unknown state": {nodeapi.CacheStoreRequest{Store: "ceph", State: "half", Since: good.Since}, http.StatusBadRequest, 0},
		"an unknown reason": {nodeapi.CacheStoreRequest{
			Store: "ceph", State: "open", Since: good.Since, Reason: "bad luck",
		}, http.StatusBadRequest, 0},
		"no time":    {nodeapi.CacheStoreRequest{Store: "ceph", State: "open"}, http.StatusBadRequest, 0},
		"a bad time": {nodeapi.CacheStoreRequest{Store: "ceph", State: "open", Since: "soon"}, http.StatusBadRequest, 0},
	} {
		store := &fakeStore{}
		post := planeAt(t, store, nodeapi.VersionCacheBreaker)
		if got := post("/v1/nodes/n1/cache-store", tc.body); got != tc.status {
			t.Errorf("%s: status %d, want %d", name, got, tc.status)
		}
		store.mu.Lock()
		kept := store.storeReports
		store.mu.Unlock()
		if len(kept) != tc.kept {
			t.Errorf("%s: kept %+v", name, kept)
		}
	}

	// WHAT IS KEPT IS WHAT WAS SAID, UNDER THE NAME AND THE PROCESS THAT SAID IT.
	store := &fakeStore{}
	post := planeAt(t, store, nodeapi.VersionCacheBreaker)
	if got := post("/v1/nodes/n1/cache-store", good); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	want := alloc.CacheStoreReport{
		Store: "ceph", State: alloc.CacheStoreOpen, Since: time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC),
		Failures: 3, Reason: alloc.CacheStoreTimeout,
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.storeReports) != 1 || !store.storeReports[0].Since.Equal(want.Since) {
		t.Fatalf("kept %+v", store.storeReports)
	}
	got := store.storeReports[0]
	got.Since = want.Since
	if got != want {
		t.Errorf("kept %+v, want %+v", got, want)
	}
	if reporter := store.storeReporters[0]; reporter != fmt.Sprintf("n1/cache-store-%d", nodeapi.VersionCacheBreaker) {
		t.Errorf("kept under %q", reporter)
	}
}

// A SUPERSEDED PROCESS'S WORD WOULD BE TAKEN AS ITS REPLACEMENT'S, because the row is read
// under the registration in force.
func TestAStoreReportFromASupersededProcessIsRefused(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	post := planeAt(t, store, nodeapi.VersionCacheBreaker)
	report := nodeapi.CacheStoreRequest{
		Store: "ceph", State: "closed", Since: "2026-11-04T00:00:00Z",
	}
	if got := post("/v1/register", nodeapi.RegisterRequest{
		MinVersion: nodeapi.MinVersion, Version: nodeapi.VersionCacheBreaker, Incarnation: "replacement",
		Node: "n1", Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
		Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); got != http.StatusOK {
		t.Fatalf("the replacement's registration: status %d", got)
	}
	// post still speaks as the first process.
	if got := post("/v1/nodes/n1/cache-store", report); got != http.StatusConflict {
		t.Fatalf("a superseded process's report: status %d, want %d", got, http.StatusConflict)
	}
	store.mu.Lock()
	kept := len(store.storeReports)
	store.mu.Unlock()
	if kept != 0 {
		t.Fatalf("a superseded process's report was kept: %d", kept)
	}
}

// THE LEDGER REFUSING IT IS A SUPERSEDED PROCESS TOO, one the plane's own memory did not
// catch: its request crossed the replacement's registration, or the plane has restarted.
func TestAStoreReportTheLedgerFencesIsSaidToBeSuperseded(t *testing.T) {
	t.Parallel()

	store := &fakeStore{cacheStoreErr: fmt.Errorf("%w: gone", alloc.ErrCacheStoreStale)}
	post := planeAt(t, store, nodeapi.VersionCacheBreaker)
	if got := post("/v1/nodes/n1/cache-store", nodeapi.CacheStoreRequest{
		Store: "ceph", State: "closed", Since: "2026-11-04T00:00:00Z",
	}); got != http.StatusConflict {
		t.Fatalf("status %d, want %d", got, http.StatusConflict)
	}
}
