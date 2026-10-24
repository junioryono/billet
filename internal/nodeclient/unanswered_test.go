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

// A PLANE TOO OLD FOR THE UNANSWERED COUNTS IS SENT THE DESTINATIONS WITHOUT
// THEM: its strict decoder would refuse the whole report for the two keys it
// does not know, so below the version neither is in the body, and the
// destinations it keeps read as not counted rather than as nothing unanswered.
// A plane at the version is sent them as they were counted.
func TestUnansweredCountsReachOnlyAPlaneThatKnowsThem(t *testing.T) {
	t.Parallel()

	report := alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 10, IntervalMillis: 1000, WindowMillis: 10_000,
		Unmeasured: []string{alloc.UsageEnergy},
		Destinations: &alloc.JobDestinations{
			Destinations: []alloc.JobDestination{{Addr: "140.82.112.3", SentBytes: 9, ReceivedBytes: 99,
				Connections: 2, Unanswered: 1}},
			Other:             alloc.JobDestination{SentBytes: 1, Connections: 3, Unanswered: 3},
			Tap:               &alloc.TapTotals{SentBytes: 10, ReceivedBytes: 100},
			UnansweredCounted: true,
		},
	}
	for _, tc := range []struct {
		version int
		counted bool
	}{
		{nodeapi.VersionUnansweredFlows - 1, false},
		{nodeapi.VersionUnansweredFlows, true},
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
		for _, key := range []string{`"unanswered"`, `"unanswered_counted"`} {
			if sent := strings.Contains(raw, key); sent != tc.counted {
				t.Errorf("a wire-%d plane was sent %s %v, want %v: %s", tc.version, key, sent, tc.counted, raw)
			}
		}
		want := report
		if !tc.counted {
			without := report.Destinations.WithoutUnanswered()
			want.Destinations = &without
		}
		if !reflect.DeepEqual(got.Usage, want) {
			t.Errorf("a wire-%d plane was told %+v,\nwant %+v", tc.version, got.Usage.Destinations,
				want.Destinations)
		}
	}
	if !report.Destinations.UnansweredCounted || report.Destinations.Destinations[0].Unanswered != 1 ||
		report.Destinations.Other.Unanswered != 3 {
		t.Errorf("stripping the counts for an older plane took them from the caller's report: %+v",
			report.Destinations)
	}
}
