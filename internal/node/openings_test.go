package node

import (
	"math"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/usage/flows"
)

// THE OPENINGS ARE NUMBERED AS THE REPORT'S DESTINATIONS ARE: a destination the
// report folds into the traffic beyond them, here one with no address, has its
// openings folded there too and merged with the tracker's own Other in the same
// bucket; the rest keep their places; a count too large is held at its largest;
// and the report that results is one the ledger keeps.
func TestTheOpeningsAreNumberedAsTheReportsDestinations(t *testing.T) {
	t.Parallel()

	if alloc.MaxDestinationOpenings != flows.MaxOpenings {
		t.Fatalf("a report carries %d openings and the node makes up to %d", alloc.MaxDestinationOpenings,
			flows.MaxOpenings)
	}

	at := func(s int64) time.Time { return time.Unix(1_794_650_000+s, 0).UTC() }
	res := flows.Result{
		Destinations: []flows.Destination{
			{Addr: netip.MustParseAddr("10.0.0.1"), Sent: 30, Received: 300, Connections: 3},
			{Sent: 5, Received: 6, Connections: 1},
			{Addr: netip.MustParseAddr("10.0.0.2"), Sent: 7, Received: 70, Connections: 1},
		},
		Other: flows.Destination{Sent: 1, Received: 2, Connections: 1},
		Openings: []flows.Opening{
			{Dest: 0, Start: at(0), Sent: 10, Received: 100, Connections: 1},
			{Dest: 0, Start: at(4), Sent: 20, Received: 200, Connections: 2},
			{Dest: 1, Start: at(4), Sent: 5, Received: 6, Connections: 1},
			{Dest: 2, Start: at(8), Sent: 7, Received: 70, Connections: 1},
			{Dest: -1, Start: at(4), Sent: 1, Received: 2, Connections: 1},
		},
		OpeningWidth: 2 * time.Second,
	}

	got, _ := jobDestinationsOf(res, tapComparison{})
	want := []alloc.DestinationOpening{
		{Ordinal: 0, Start: 1_794_650_000, SentBytes: 10, ReceivedBytes: 100, Connections: 1},
		{Ordinal: 0, Start: 1_794_650_004, SentBytes: 20, ReceivedBytes: 200, Connections: 2},
		{Ordinal: 1, Start: 1_794_650_008, SentBytes: 7, ReceivedBytes: 70, Connections: 1},
		{Ordinal: alloc.MaxJobDestinations, Start: 1_794_650_004, SentBytes: 6, ReceivedBytes: 8, Connections: 2},
	}
	if got.OpeningSeconds != 2 || !reflect.DeepEqual(got.Openings, want) {
		t.Errorf("the report's openings are %+v every %d s,\nwant %+v every 2 s", got.Openings,
			got.OpeningSeconds, want)
	}
	if got.Destinations[1].Addr != "10.0.0.2" {
		t.Errorf("the report's destinations are %+v; want the addressless one folded away", got.Destinations)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the report the node would send is refused: %v", err)
	}

	// A COUNT TOO LARGE FOR THE LEDGER IS HELD AT ITS LARGEST, as its
	// destination's total is, and the report is a lower bound that still holds.
	res.Destinations[0].Sent = math.MaxUint64
	res.Openings[0].Sent = math.MaxUint64
	got, saturated := jobDestinationsOf(res, tapComparison{})
	if !saturated || !got.Incomplete || got.Openings[0].SentBytes != math.MaxInt64 {
		t.Errorf("a count past an int64 gave %+v (saturated %v)", got.Openings[0], saturated)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a report with a count held at its largest is refused: %v", err)
	}

	// THROUGH THE TRACKER ITSELF, connections whose bytes together pass a uint64
	// give a destination total held at its largest, as its openings are, never
	// one that wrapped below them: the report the node builds from them is kept,
	// not refused whole with every other measurement of the job.
	guest, dest := netip.MustParseAddr("192.168.100.23"), netip.MustParseAddr("140.82.112.4")
	acct := flows.NewAccountant()
	began := time.Unix(1_794_650_000, 0).UTC()
	acct.Watch("l1", guest, began, nil)
	for i, sent := range []uint64{math.MaxUint64, 1} {
		acct.Observe(flows.Flow{ID: uint32(i + 1), Start: began.Add(time.Duration(i+1) * time.Second),
			Protocol: 6, Source: guest, Dest: dest, DestPort: 443, OrigBytes: sent, ReplyBytes: 1,
			OrigPackets: 1, ReplyPackets: 1})
	}
	tracked, ok := acct.Final("l1", nil, began.Add(time.Hour))
	if !ok {
		t.Fatal("the watched job had no result")
	}
	if tracked.Destinations[0].Sent != math.MaxUint64 {
		t.Errorf("the tracker's total sent is %d, want it held at the largest uint64", tracked.Destinations[0].Sent)
	}
	got, _ = jobDestinationsOf(tracked, tapComparison{})
	if err := got.Validate(); err != nil {
		t.Errorf("a report from connections whose bytes pass a uint64 is refused: %v", err)
	}

	// NO SPLIT IS NO OPENINGS AND NO WIDTH, never an empty split.
	res.Openings, res.OpeningWidth = nil, 0
	if got, _ = jobDestinationsOf(res, tapComparison{}); got.Openings != nil || got.OpeningSeconds != 0 {
		t.Errorf("a result with no split gave openings %+v every %d s", got.Openings, got.OpeningSeconds)
	}
}
