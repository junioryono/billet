package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

func someDestinations() *alloc.JobDestinations {
	return &alloc.JobDestinations{
		Destinations: []alloc.JobDestination{{Addr: "140.82.112.3", SentBytes: 1 << 20, ReceivedBytes: 300 << 20,
			Connections: 12}},
		Other: alloc.JobDestination{SentBytes: 5}, Incomplete: true,
		Tap: &alloc.TapTotals{SentBytes: 2 << 20, ReceivedBytes: 301 << 20},
	}
}

// A REPORT FROM A PAIRING THAT NEGOTIATED BELOW THE DESTINATIONS' VERSION IS
// KEPT WITHOUT THEM, counters and all, and one at the version keeps them: the
// plane records what the wire carries rather than refusing the report or
// keeping what it should not have been sent.
func TestDestinationsFromAWireThatDoesNotCarryThemAreDropped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version      int
		destinations bool
	}{
		{nodeapi.VersionJobDestinations - 1, false},
		{nodeapi.VersionJobDestinations, true},
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
		if status := post("/v1/nodes/n1/leases/l1/usage",
			nodeapi.UsageRequest{Epoch: 7, Usage: usage}); status != http.StatusNoContent {
			t.Fatalf("a usage report at wire %d answered %d", tc.version, status)
		}

		store.mu.Lock()
		got := store.usages
		store.mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("at wire %d the ledger was asked to record %d reports, want 1", tc.version, len(got))
		}
		if kept := got[0].usage.Destinations != nil; kept != tc.destinations {
			t.Errorf("at wire %d the ledger kept destinations %v, want %v", tc.version, kept, tc.destinations)
		}
		if got[0].usage.Counters == nil {
			t.Errorf("at wire %d the counters, which that wire carries, were dropped", tc.version)
		}
		if tc.destinations && !reflect.DeepEqual(got[0].usage.Destinations, usage.Destinations) {
			t.Errorf("at wire %d the destinations changed: %+v", tc.version, got[0].usage.Destinations)
		}
		usage.Destinations = got[0].usage.Destinations
		if !reflect.DeepEqual(got[0].usage, usage) {
			t.Errorf("at wire %d the rest of the report changed: %+v", tc.version, got[0].usage)
		}
	}
}

// DESTINATIONS THE LEDGER CANNOT KEEP ARE REFUSED AT THE BOUNDARY, as a
// permanent refusal, and never reach the ledger. Each case is the accepted
// report with one thing broken.
func TestUnkeepableDestinationsAreRefusedAtTheWire(t *testing.T) {
	t.Parallel()

	const valid = `"source":"host","samples":1,"interval_ms":1000,"window_ms":0,"unmeasured":["energy"]`
	accepted := `{"epoch":7,"usage":{` + valid + `,"destinations":{"destinations":[{"addr":"10.0.0.1",` +
		`"sent_bytes":1,"received_bytes":2,"connections":1}],"other":{"sent_bytes":0,"received_bytes":0,` +
		`"connections":0},"incomplete":false,"tap":{"sent_bytes":3,"received_bytes":4}}}}`
	for name, tc := range map[string]struct{ body, want string }{
		"accepted": {accepted, ""},
		"an address that is not one": {strings.Replace(accepted, `"10.0.0.1"`, `"github.com"`, 1),
			`"github.com" is not an address`},
		"the rest naming an address": {strings.Replace(accepted, `"other":{`, `"other":{"addr":"10.0.0.2",`, 1),
			"names an address"},
		"a negative total": {strings.Replace(accepted, `"sent_bytes":1,`, `"sent_bytes":-1,`, 1),
			"negative total"},
		"a tap beside a network unmeasured": {strings.Replace(accepted, `["energy"]`, `["energy","net"]`, 1),
			"names net unmeasured and gives the tap's totals"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := &fakeStore{}
			_, base := serve(t, store)
			c := dial(t, base)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				base+"/v1/nodes/n1/leases/l1/usage", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			req.Header.Set(nodeapi.HeaderIncarnation, c.Incarnation())

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()

			store.mu.Lock()
			defer store.mu.Unlock()
			if tc.want == "" {
				if resp.StatusCode != http.StatusNoContent || len(store.usages) != 1 ||
					store.usages[0].usage.Destinations == nil {
					t.Fatalf("the accepted report answered %d and reached the ledger as %+v", resp.StatusCode,
						store.usages)
				}
				return
			}
			var refusal nodeapi.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
				t.Fatalf("decode the refusal: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest || refusal.Code != nodeapi.CodeRefused ||
				!strings.Contains(refusal.Message, tc.want) {
				t.Fatalf("answered %d %q (%s), want 400 %q saying %q", resp.StatusCode, refusal.Code,
					refusal.Message, nodeapi.CodeRefused, tc.want)
			}
			if len(store.usages) != 0 {
				t.Fatalf("the ledger was asked to record %+v from a refused request", store.usages)
			}
		})
	}
}

// THE LARGEST REPORT A NODE CAN SEND FITS ONE REQUEST: a series at its bound,
// every counter, and every destination the node keeps by name at the longest
// address and the largest totals, through the plane's own body limit.
func TestTheLargestUsageReportFitsOneRequest(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	const most = int64(1<<63 - 1)
	usage := aUsage()
	usage.Destinations = &alloc.JobDestinations{
		Other:      alloc.JobDestination{SentBytes: most, ReceivedBytes: most, Connections: most},
		Incomplete: true, Tap: &alloc.TapTotals{SentBytes: most, ReceivedBytes: most},
	}
	for i := range alloc.MaxJobDestinations {
		usage.Destinations.Destinations = append(usage.Destinations.Destinations, alloc.JobDestination{
			Addr:      fmt.Sprintf("ffff:ffff:ffff:ffff:ffff:ffff:ffff:%x", 0xf000+i),
			SentBytes: most, ReceivedBytes: most, Connections: most,
		})
	}
	data := bytes.Repeat([]byte{0xff}, alloc.MaxUsageSeriesBytes)
	if err := c.RecordLeaseUsage(t.Context(), "l1", 7, usage,
		&alloc.UsageSeries{Codec: alloc.UsageSeriesCodec, Data: data}); err != nil {
		t.Fatalf("the largest report was refused: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.usages) != 1 || store.usages[0].usage.Destinations == nil ||
		len(store.usages[0].usage.Destinations.Destinations) != alloc.MaxJobDestinations {
		t.Fatalf("the largest report did not reach the ledger whole: %d report(s)", len(store.usages))
	}
}
