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

// A PLANE TOO OLD FOR THE OPENINGS IS SENT THE DESTINATIONS WITHOUT THEM: its
// strict decoder would refuse the whole report for the two keys it does not
// know, so below the version neither is in the body, and the destinations it
// keeps read as not split by time rather than as no traffic in any step. The
// reply times a wire-48 plane does know still reach it. A plane at the version
// is sent the split as the node made it.
func TestOpeningsReachOnlyAPlaneThatKnowsThem(t *testing.T) {
	t.Parallel()

	report := alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 10, IntervalMillis: 1000, WindowMillis: 10_000,
		Unmeasured: []string{alloc.UsageEnergy},
		Destinations: &alloc.JobDestinations{
			Destinations: []alloc.JobDestination{{Addr: "172.31.1.1", SentBytes: 9, ReceivedBytes: 99,
				Connections: 3, TimedReplies: 2, SlowestReplyMillis: 40}},
			Other:        alloc.JobDestination{SentBytes: 1, Connections: 1},
			RepliesTimed: true,
			Openings: []alloc.DestinationOpening{
				{Ordinal: 0, Start: 1_794_650_000, SentBytes: 5, ReceivedBytes: 60, Connections: 2},
				{Ordinal: 0, Start: 1_794_650_030, SentBytes: 4, ReceivedBytes: 39, Connections: 1},
				{Ordinal: alloc.MaxJobDestinations, Start: 1_794_650_010, SentBytes: 1, Connections: 1},
			},
			OpeningSeconds: 1,
		},
	}
	for _, tc := range []struct {
		version int
		split   bool
	}{
		{nodeapi.VersionDestinationOpenings - 1, false},
		{nodeapi.VersionDestinationOpenings, true},
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
		for _, key := range []string{`"openings"`, `"opening_seconds"`} {
			if sent := strings.Contains(raw, key); sent != tc.split {
				t.Errorf("a wire-%d plane was sent %s %v, want %v: %s", tc.version, key, sent, tc.split, raw)
			}
		}
		if !strings.Contains(raw, `"replies_timed"`) {
			t.Errorf("a wire-%d plane was not sent the reply times its wire carries: %s", tc.version, raw)
		}
		want := report
		if !tc.split {
			without := report.Destinations.WithoutOpenings()
			want.Destinations = &without
		}
		if !reflect.DeepEqual(got.Usage, want) {
			t.Errorf("a wire-%d plane was told %+v,\nwant %+v", tc.version, got.Usage.Destinations,
				want.Destinations)
		}
	}
	if len(report.Destinations.Openings) != 3 || report.Destinations.OpeningSeconds != 1 {
		t.Errorf("stripping the openings for an older plane took them from the caller's report: %+v",
			report.Destinations)
	}
}
