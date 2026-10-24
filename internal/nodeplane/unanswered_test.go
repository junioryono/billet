package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A REPORT FROM A PAIRING THAT NEGOTIATED BELOW THE UNANSWERED COUNTS' VERSION
// IS KEPT WITH ITS DESTINATIONS NOT COUNTED, and one at the version keeps the
// counts: a node that sends them anyway is not refused, and what it sent is
// not kept as a count the wire it negotiated does not carry.
func TestUnansweredCountsFromAWireThatDoesNotCarryThemAreDropped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version int
		counted bool
	}{
		{nodeapi.VersionUnansweredFlows - 1, false},
		{nodeapi.VersionUnansweredFlows, true},
	} {
		store := &fakeStore{}
		_, base := serve(t, store)
		incarnation := fmt.Sprintf("wire-%d", tc.version)
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
		usage := aUsage()
		usage.Destinations = someDestinations()
		usage.Destinations.Destinations[0].Unanswered = 4
		usage.Destinations.UnansweredCounted = true
		if status := post("/v1/nodes/n1/leases/l1/usage",
			nodeapi.UsageRequest{Epoch: 7, Usage: usage}); status != http.StatusNoContent {
			t.Fatalf("a usage report at wire %d answered %d", tc.version, status)
		}

		store.mu.Lock()
		got := store.usages
		store.mu.Unlock()
		if len(got) != 1 || got[0].usage.Destinations == nil {
			t.Fatalf("at wire %d the ledger was asked to record %+v, want one report with destinations",
				tc.version, got)
		}
		want := *usage.Destinations
		if !tc.counted {
			want = usage.Destinations.WithoutUnanswered()
		}
		if !reflect.DeepEqual(*got[0].usage.Destinations, want) {
			t.Errorf("at wire %d the ledger kept destinations %+v,\nwant %+v", tc.version,
				*got[0].usage.Destinations, want)
		}
		usage.Destinations = got[0].usage.Destinations
		if !reflect.DeepEqual(got[0].usage, usage) {
			t.Errorf("at wire %d the rest of the report changed: %+v", tc.version, got[0].usage)
		}
	}
}
