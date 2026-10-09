package nodeclient_test

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// A PLANE TOO OLD FOR DESTINATIONS IS SENT THE REST OF THE REPORT, counters
// included: its strict decoder would refuse the whole body for the one key it
// does not know, so below the version the key is not in the body at all. A
// plane at the version is sent the destinations as they were totalled.
func TestDestinationsReachOnlyAPlaneThatKnowsThem(t *testing.T) {
	t.Parallel()

	cycles := int64(4_000_000)
	report := alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 10, IntervalMillis: 1000, WindowMillis: 10_000,
		Unmeasured: []string{alloc.UsageEnergy},
		Counters:   &alloc.JobCounters{Cycles: &cycles},
		Destinations: &alloc.JobDestinations{
			Destinations: []alloc.JobDestination{{Addr: "140.82.112.3", SentBytes: 9, ReceivedBytes: 99,
				Connections: 2}},
			Incomplete: true, Tap: &alloc.TapTotals{SentBytes: 10, ReceivedBytes: 100},
		},
	}
	for _, tc := range []struct {
		version      int
		destinations bool
	}{
		{nodeapi.VersionJobDestinations - 1, false},
		{nodeapi.VersionJobDestinations, true},
	} {
		plane := &usageRecordingPlane{version: tc.version}
		srv := httptest.NewServer(plane)
		t.Cleanup(srv.Close)
		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		if err := c.Register(t.Context(), testRegistration()); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := c.RecordLeaseUsage(t.Context(), "l1", 1, report, nil); err != nil {
			t.Fatalf("RecordLeaseUsage on wire %d: %v", tc.version, err)
		}
		plane.mu.Lock()
		got, raw := plane.got, plane.raw
		plane.mu.Unlock()
		if got == nil {
			t.Fatalf("nothing reached the wire-%d plane", tc.version)
		}
		if sent := strings.Contains(raw, `"destinations"`); sent != tc.destinations {
			t.Errorf("a wire-%d plane was sent destinations %v, want %v: %s", tc.version, sent,
				tc.destinations, raw)
		}
		want := report
		if !tc.destinations {
			want.Destinations = nil
		}
		if !reflect.DeepEqual(got.Usage, want) {
			t.Errorf("a wire-%d plane was told %+v,\nwant %+v", tc.version, got.Usage, want)
		}
	}
	if report.Destinations == nil {
		t.Error("stripping the destinations for an older plane took them from the caller's report")
	}
}
