//go:build linux

package flows

import (
	"net/netip"
	"testing"
	"time"

	"github.com/ti-mo/conntrack"
)

// The tracker's original direction is the guest's: what it sent, to whom.
func TestAConntrackFlowIsReadInTheGuestsTerms(t *testing.T) {
	t.Parallel()

	var cf conntrack.Flow

	start := time.Date(2026, 10, 9, 12, 0, 1, 0, time.UTC)

	cf.ID = 42
	cf.Timestamp.Start = start
	cf.TupleOrig.IP.SourceAddress = netip.MustParseAddr("192.168.100.23")
	cf.TupleOrig.IP.DestinationAddress = netip.MustParseAddr("140.82.112.4")
	cf.TupleOrig.Proto.Protocol = 6
	cf.TupleOrig.Proto.DestinationPort = 443
	// The reply tuple is after NAT; billet names the destination the guest
	// asked for, never the translated one.
	cf.TupleReply.IP.SourceAddress = netip.MustParseAddr("140.82.112.4")
	cf.TupleReply.IP.DestinationAddress = netip.MustParseAddr("203.0.113.9")
	cf.CountersOrig = conntrack.Counter{Packets: 3, Bytes: 300}
	cf.CountersReply = conntrack.Counter{Direction: true, Packets: 9, Bytes: 9000}

	f, ok := fromConntrack(&cf)
	if !ok {
		t.Fatal("a complete flow was not read")
	}

	want := Flow{
		ID: 42, Start: start, Protocol: 6,
		Source: netip.MustParseAddr("192.168.100.23"), Dest: netip.MustParseAddr("140.82.112.4"), DestPort: 443,
		OrigBytes: 300, ReplyBytes: 9000, OrigPackets: 3, ReplyPackets: 9,
	}
	if f != want {
		t.Errorf("flow = %+v, want %+v", f, want)
	}

	if _, ok := fromConntrack(&conntrack.Flow{ID: 1}); ok {
		t.Error("a flow without addresses was read")
	}
}
