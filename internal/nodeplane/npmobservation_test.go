package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// THE NPM CACHE'S OUTCOME IS KEPT ONLY FROM A WIRE THAT CARRIES IT
// (VersionNpmObservation): a pairing negotiated below it that sends one anyway
// has the observation kept without it, and one that sent nothing else is
// answered as recorded and leaves the ledger alone. One the drop did not empty
// is judged as before: an empty observation is still refused.
func TestAnNpmOutcomeFromAWireThatDoesNotCarryItIsDropped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version  int
		body     nodeapi.CacheObservationRequest
		status   int
		recorded []alloc.CacheObservation
	}{
		{nodeapi.VersionNpmObservation - 1,
			nodeapi.CacheObservationRequest{Epoch: 7, GitCache: "warm", NpmCache: "warm"}, http.StatusNoContent,
			[]alloc.CacheObservation{{BuildCaches: alloc.BuildCaches{Git: alloc.BuildCacheWarm}}}},
		{nodeapi.VersionNpmObservation - 1,
			nodeapi.CacheObservationRequest{Epoch: 7, NpmCache: "warm"}, http.StatusNoContent, nil},
		{nodeapi.VersionNpmObservation,
			nodeapi.CacheObservationRequest{Epoch: 7, GitCache: "warm", NpmCache: "warm"}, http.StatusNoContent,
			[]alloc.CacheObservation{{BuildCaches: alloc.BuildCaches{Git: alloc.BuildCacheWarm,
				Npm: alloc.BuildCacheWarm}}}},
		// What the drop did not empty is still Validate's: an observation of
		// nothing, or of only a generation, is refused at either version.
		{nodeapi.VersionNpmObservation - 1, nodeapi.CacheObservationRequest{Epoch: 7}, http.StatusBadRequest, nil},
		{nodeapi.VersionNpmObservation, nodeapi.CacheObservationRequest{Epoch: 7}, http.StatusBadRequest, nil},
		{nodeapi.VersionNpmObservation - 1,
			nodeapi.CacheObservationRequest{Epoch: 7, CacheGeneration: "g"}, http.StatusBadRequest, nil},
		{nodeapi.VersionNpmObservation,
			nodeapi.CacheObservationRequest{Epoch: 7, CacheGeneration: "g"}, http.StatusBadRequest, nil},
		// AND ONE THE DROP LEAVES WITH ONLY A GENERATION is refused too: it is not
		// emptied, and a generation alone names no warm store.
		{nodeapi.VersionNpmObservation - 1,
			nodeapi.CacheObservationRequest{Epoch: 7, CacheGeneration: "g", NpmCache: "warm"},
			http.StatusBadRequest, nil},
	} {
		store := &fakeStore{}
		_, base := serve(t, store)
		incarnation := fmt.Sprintf("wire-%d-%s-%s-%s", tc.version, tc.body.GitCache, tc.body.CacheGeneration,
			tc.body.NpmCache)
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
				t.Fatalf("post %s: %v", path, err)
			}
			defer resp.Body.Close()

			return resp.StatusCode
		}

		if status := post("/v1/register", nodeapi.RegisterRequest{
			MinVersion: nodeapi.MinVersion, Version: tc.version, Incarnation: incarnation,
			Node: "n1", Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
			Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
		}); status != http.StatusOK {
			t.Fatalf("register at wire %d answered %d", tc.version, status)
		}
		if status := post("/v1/nodes/n1/leases/l1/cache", tc.body); status != tc.status {
			t.Fatalf("wire %d, %+v: the observation answered %d, want %d", tc.version, tc.body, status, tc.status)
		}

		store.mu.Lock()
		var got []alloc.CacheObservation
		for _, recorded := range store.observations {
			got = append(got, recorded.obs)
		}
		store.mu.Unlock()
		if fmt.Sprint(got) != fmt.Sprint(tc.recorded) {
			t.Errorf("wire %d, %+v: the ledger was asked to record %+v, want %+v", tc.version, tc.body, got,
				tc.recorded)
		}
	}
}
