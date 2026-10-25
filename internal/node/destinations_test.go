package node

import (
	"log/slog"
	"math"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/usage"
	"github.com/junioryono/billet/internal/usage/flows"
)

// netMonitor answers every job's final sample with a measured tap and CPU and
// nothing else.
type netMonitor struct{}

func (netMonitor) Start(string, usage.Target, int)                           {}
func (netMonitor) Forget(string)                                             {}
func (netMonitor) Resume(string, usage.Target, int, *usage.Checkpoint) error { return nil }
func (netMonitor) Checkpoint(string) (usage.Checkpoint, bool)                { return usage.Checkpoint{}, false }
func (netMonitor) Measuring(string) bool                                     { return false }
func (netMonitor) Final(string) (usage.Summary, bool) {
	sum := usage.Summary{Samples: 3, Interval: time.Second, Window: 3 * time.Second}
	sum.Measured.CPU, sum.Measured.Net = true, true
	sum.Latest.CPUUser = 5
	sum.Latest.NetTx, sum.Latest.NetTxPackets = 10_140, 10
	sum.Latest.NetRx, sum.Latest.NetRxPackets = 2_000_280, 20

	return sum, true
}

// THE DESTINATIONS REACH THE LEDGER WITH THE JOB'S USAGE, through the
// runner's own launch and destroy: the flows the watcher totalled, the tap's
// totals less each packet's header beside them, and the verdict as it was.
// A job whose flows were not totalled reports its usage without any, never
// an empty list.
func TestTheRunnerReportsAJobsDestinationsWithItsUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		unmeasured bool
		result     flows.Result
		want       *alloc.JobDestinations
	}{
		"totalled": {
			result: flows.Result{
				Destinations: []flows.Destination{
					{Addr: netip.MustParseAddr("140.82.112.3"), Sent: 9_000, Received: 1_999_000, Connections: 4,
						Unanswered: 1, TimedReplies: 3, SlowReplies: 1, SlowestReply: 2400 * time.Millisecond},
					{Addr: netip.MustParseAddr("2001:db8::1"), Sent: 100, Received: 200, Connections: 1},
				},
				Other:        flows.Destination{Sent: 7, Received: 8, Connections: 2, Unanswered: 2},
				Incomplete:   true,
				RepliesTimed: true,
			},
			want: &alloc.JobDestinations{
				Destinations: []alloc.JobDestination{
					{Addr: "140.82.112.3", SentBytes: 9_000, ReceivedBytes: 1_999_000, Connections: 4,
						Unanswered: 1, TimedReplies: 3, SlowReplies: 1, SlowestReplyMillis: 2400},
					{Addr: "2001:db8::1", SentBytes: 100, ReceivedBytes: 200, Connections: 1},
				},
				Other:             alloc.JobDestination{SentBytes: 7, ReceivedBytes: 8, Connections: 2, Unanswered: 2},
				Incomplete:        true,
				Tap:               &alloc.TapTotals{SentBytes: 10_000, ReceivedBytes: 2_000_000},
				UnansweredCounted: true,
				RepliesTimed:      true,
			},
		},
		"totalled and empty": {
			want: &alloc.JobDestinations{Tap: &alloc.TapTotals{SentBytes: 10_000, ReceivedBytes: 2_000_000},
				UnansweredCounted: true},
		},
		"not totalled": {unmeasured: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := &guestProvider{measuredProvider: &measuredProvider{
				fakeProvider: &fakeProvider{kind: config.ProviderDocker}, root: t.TempDir()}}
			a, host := newAllocatorWithHost(t)
			rec := newRecordingFlows()
			rec.result, rec.unmeasured = tc.result, tc.unmeasured
			r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithMonitor(netMonitor{}), WithFlows(rec, "/leases"))

			lease := assignedLease(t, a)
			if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if err := r.Destroy(t.Context(), lease.RequestID); err != nil {
				t.Fatalf("Destroy: %v", err)
			}

			got, err := a.LeaseUsage(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("LeaseUsage: %v", err)
			}
			if got.CPUUserMicros != 5 {
				t.Errorf("the usage beside the destinations reads cpu %d, want 5", got.CPUUserMicros)
			}
			if !reflect.DeepEqual(got.Destinations, tc.want) {
				t.Errorf("the ledger holds destinations %+v,\nwant %+v", got.Destinations, tc.want)
			}
		})
	}
}

// THE REPORT SAYS WHAT THE TRACKER TOTALLED, AND NOTHING IT CANNOT KEEP: a
// total too large for the ledger is held at its largest and the result marked
// a lower bound; traffic to an address that is not one, or beyond the
// destinations the ledger keeps by name, is counted under Other rather than
// lost; and a tap reading below zero, or none, is no comparison at all.
func TestTheReportsDestinationsAreWhatTheLedgerCanKeep(t *testing.T) {
	t.Parallel()

	if alloc.MaxJobDestinations != flows.MaxDestinations {
		t.Fatalf("the ledger keeps %d destinations by name and the node %d", alloc.MaxJobDestinations,
			flows.MaxDestinations)
	}
	if time.Duration(alloc.SlowReplyMillis)*time.Millisecond != flows.SlowReply {
		t.Fatalf("a report calls a reply slow past %d ms and the node past %s", alloc.SlowReplyMillis,
			flows.SlowReply)
	}

	res := flows.Result{Destinations: []flows.Destination{
		{Addr: netip.MustParseAddr("10.0.0.1"), Sent: math.MaxUint64, Received: 1, Connections: 2, Unanswered: 1},
		{Sent: 5, Received: 6, Connections: 1, Unanswered: 1},
	}, Other: flows.Destination{Sent: 1, Received: 2, Connections: 3, Unanswered: 2}}

	got := jobDestinationsOf(res, tapComparison{tapKnown: true, tapSent: -1, tapReceived: 4})
	want := &alloc.JobDestinations{
		Destinations: []alloc.JobDestination{{Addr: "10.0.0.1", SentBytes: math.MaxInt64, ReceivedBytes: 1,
			Connections: 2, Unanswered: 1}},
		Other:             alloc.JobDestination{SentBytes: 6, ReceivedBytes: 8, Connections: 4, Unanswered: 3},
		Incomplete:        true,
		UnansweredCounted: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the report carries %+v,\nwant %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("what the node reports does not validate: %v", err)
	}

	// REPLY TIMES ARE CARRIED WHERE THE RESULT TIMED THEM, held to what a report
	// may say: no more timed than answered, no more slow than timed, and the
	// slowest wait in milliseconds; and a destination merged into Other brings
	// its times with it. Where the result did not time replies, none are sent.
	timed := flows.Result{RepliesTimed: true, Destinations: []flows.Destination{
		{Addr: netip.MustParseAddr("10.0.0.1"), Connections: 3, Unanswered: 1, TimedReplies: 5, SlowReplies: 9,
			SlowestReply: 1500*time.Millisecond + 900*time.Microsecond},
		{Connections: 2, TimedReplies: 2, SlowReplies: 1, SlowestReply: 4 * time.Second},
	}, Other: flows.Destination{Connections: 1, TimedReplies: 1, SlowestReply: 30 * time.Millisecond}}
	got = jobDestinationsOf(timed, tapComparison{})
	if !got.RepliesTimed || len(got.Destinations) != 1 ||
		got.Destinations[0] != (alloc.JobDestination{Addr: "10.0.0.1", Connections: 3, Unanswered: 1,
			TimedReplies: 2, SlowReplies: 2, SlowestReplyMillis: 1500}) ||
		got.Other != (alloc.JobDestination{Connections: 3, TimedReplies: 3, SlowReplies: 1,
			SlowestReplyMillis: 4000}) {
		t.Errorf("the timed result reports %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a timed report does not validate: %v", err)
	}
	timed.RepliesTimed = false
	if got = jobDestinationsOf(timed, tapComparison{}); got.RepliesTimed || got.Destinations[0].TimedReplies != 0 ||
		got.Other.SlowestReplyMillis != 0 {
		t.Errorf("a result that did not time replies reports %+v", got)
	}

	// A RESULT THAT CANNOT TELL WHAT WENT UNANSWERED REPORTS IT NOT COUNTED,
	// with no count left on any destination, rather than as the counts it saw.
	res.UnansweredUnknown = true
	got = jobDestinationsOf(res, tapComparison{tapKnown: true, tapSent: -1, tapReceived: 4})
	if got.UnansweredCounted || got.Destinations[0].Unanswered != 0 || got.Other.Unanswered != 0 {
		t.Errorf("a result that could not tell what went unanswered reports %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a report that did not count does not validate: %v", err)
	}

	// ONE MORE THAN THE LEDGER KEEPS BY NAME goes under Other, and the
	// largest Other a sum can reach is held there rather than wrapping.
	res = flows.Result{Other: flows.Destination{Sent: math.MaxInt64}}
	for i := range alloc.MaxJobDestinations + 1 {
		res.Destinations = append(res.Destinations, flows.Destination{
			Addr: netip.AddrFrom4([4]byte{10, 1, byte(i / 256), byte(i % 256)}), Sent: 1, Connections: 1})
	}
	got = jobDestinationsOf(res, tapComparison{tapKnown: true, tapSent: 3, tapReceived: 4})
	if len(got.Destinations) != alloc.MaxJobDestinations || got.Other.SentBytes != math.MaxInt64 ||
		got.Other.Connections != 1 || !got.Incomplete {
		t.Errorf("past the bound the report names %d with Other %+v, incomplete %v", len(got.Destinations),
			got.Other, got.Incomplete)
	}
	if got.Tap == nil || *got.Tap != (alloc.TapTotals{SentBytes: 3, ReceivedBytes: 4}) {
		t.Errorf("tap = %+v, want 3 sent and 4 received", got.Tap)
	}

	if got := jobDestinationsOf(flows.Result{}, tapComparison{}); got.Tap != nil || got.Incomplete ||
		got.Destinations != nil {
		t.Errorf("a job with no flows and no tap reports %+v", got)
	}
}

// A JOB WHOSE FLOWS WERE NEVER FOLLOWED REPORTS NONE: finalFlows answers nil,
// not an empty total, with or without a watcher.
func TestFlowsNotTotalledReportNone(t *testing.T) {
	t.Parallel()

	r := &Runner{log: slog.New(slog.DiscardHandler)}
	if got := r.finalFlows(t.Context(), "billet-1", time.Now(), usage.Summary{}, false); got != nil {
		t.Errorf("without a watcher the report carries %+v", got)
	}

	rec := newRecordingFlows()
	rec.unmeasured = true
	r = &Runner{log: slog.New(slog.DiscardHandler), flows: rec}
	if got := r.finalFlows(t.Context(), "billet-1", time.Now(), usage.Summary{}, false); got != nil {
		t.Errorf("for a job never followed the report carries %+v", got)
	}
}
