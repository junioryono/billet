package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A SERIES WITH CAUSES FROM A PAIRING THAT NEGOTIATED BELOW THEIR VERSION IS
// DROPPED and the report kept; the marked series that wire carries is kept,
// and so is one with causes at the version.
func TestASeriesWithCausesFromAWireThatDoesNotCarryThemIsDropped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version int
		codec   int
		kept    bool
	}{
		{nodeapi.VersionSeriesGaps - 1, alloc.UsageSeriesCodecGaps, false},
		{nodeapi.VersionSeriesGaps - 1, alloc.UsageSeriesCodecClocked, true},
		{nodeapi.VersionSeriesGaps, alloc.UsageSeriesCodecGaps, true},
	} {
		store := &fakeStore{}
		_, base := serve(t, store)
		incarnation := fmt.Sprintf("wire-%d-codec-%d", tc.version, tc.codec)
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
		series := &alloc.UsageSeries{Codec: tc.codec, Data: []byte{1, 2, 3}}
		if status := post("/v1/nodes/n1/leases/l1/usage",
			nodeapi.UsageRequest{Epoch: 7, Usage: aUsage(), Series: series}); status != http.StatusNoContent {
			t.Fatalf("a usage report at wire %d answered %d", tc.version, status)
		}

		store.mu.Lock()
		got := store.usages
		store.mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("at wire %d the ledger was asked to record %d reports, want 1", tc.version, len(got))
		}
		if kept := got[0].series != nil; kept != tc.kept {
			t.Errorf("at wire %d a codec-%d series was kept %v, want %v", tc.version, tc.codec, kept, tc.kept)
		}
		if tc.kept && !reflect.DeepEqual(*got[0].series, *series) {
			t.Errorf("at wire %d the series changed: %+v", tc.version, got[0].series)
		}
		if !reflect.DeepEqual(got[0].usage, aUsage()) {
			t.Errorf("at wire %d the report changed with its series: %+v", tc.version, got[0].usage)
		}
	}
}
