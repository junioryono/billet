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

// A PLANE TOO OLD FOR THE REPLY TIMES IS SENT THE DESTINATIONS WITHOUT THEM:
// its strict decoder would refuse the whole report for the four keys it does
// not know, so below the version none is in the body, and the destinations it
// keeps read as not timed rather than as nothing slow. The unanswered counts a
// wire-35 plane does know still reach it. A plane at the version is sent the
// times as they were taken.
func TestReplyTimesReachOnlyAPlaneThatKnowsThem(t *testing.T) {
	t.Parallel()

	report := alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 10, IntervalMillis: 1000, WindowMillis: 10_000,
		Unmeasured: []string{alloc.UsageEnergy},
		Destinations: &alloc.JobDestinations{
			Destinations: []alloc.JobDestination{{Addr: "172.31.1.1", SentBytes: 9, ReceivedBytes: 99,
				Connections: 3, Unanswered: 1, TimedReplies: 2, SlowReplies: 1, SlowestReplyMillis: 1600}},
			Other: alloc.JobDestination{SentBytes: 1, Connections: 3, TimedReplies: 3,
				SlowestReplyMillis: 40},
			UnansweredCounted: true,
			RepliesTimed:      true,
		},
	}
	for _, tc := range []struct {
		version int
		timed   bool
	}{
		{nodeapi.VersionReplyTimes - 1, false},
		{nodeapi.VersionReplyTimes, true},
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
		for _, key := range []string{`"timed_replies"`, `"slow_replies"`, `"slowest_reply_ms"`, `"replies_timed"`} {
			if sent := strings.Contains(raw, key); sent != tc.timed {
				t.Errorf("a wire-%d plane was sent %s %v, want %v: %s", tc.version, key, sent, tc.timed, raw)
			}
		}
		if !strings.Contains(raw, `"unanswered_counted"`) {
			t.Errorf("a wire-%d plane was not sent the unanswered counts its wire carries: %s", tc.version, raw)
		}
		want := report
		if !tc.timed {
			without := report.Destinations.WithoutReplyTimes()
			want.Destinations = &without
		}
		if !reflect.DeepEqual(got.Usage, want) {
			t.Errorf("a wire-%d plane was told %+v,\nwant %+v", tc.version, got.Usage.Destinations,
				want.Destinations)
		}
	}
	if !report.Destinations.RepliesTimed || report.Destinations.Destinations[0].SlowestReplyMillis != 1600 ||
		report.Destinations.Other.TimedReplies != 3 {
		t.Errorf("stripping the times for an older plane took them from the caller's report: %+v",
			report.Destinations)
	}
}
