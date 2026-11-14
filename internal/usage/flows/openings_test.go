package flows

import (
	"net/netip"
	"slices"
	"testing"
	"time"
)

// EACH DESTINATION'S TRAFFIC IS SPLIT BY THE SECOND ITS CONNECTIONS OPENED, a
// connection's bytes all in the second it opened, and the split adds up to the
// destination's totals.
func TestOpeningsSplitEachDestinationByTheSecondItsConnectionsOpened(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)

	at := func(s int) time.Time { return started.Add(time.Duration(s) * time.Second) }
	a.Observe(startedAt(flow(1, guest, github, 100, 5000), at(0)))
	a.Observe(startedAt(flow(2, guest, github, 50, 2000), at(0).Add(900*time.Millisecond)))
	a.Observe(startedAt(flow(3, guest, github, 7, 70), at(30)))
	a.Observe(startedAt(flow(4, guest, npm, 10, 900), at(12)))

	r, _ := a.Final("lease-1", nil, ended)
	if r.OpeningWidth != time.Second {
		t.Errorf("width = %s, want a second", r.OpeningWidth)
	}
	want := []Opening{
		{Dest: 0, Start: at(0), Sent: 150, Received: 7000, Connections: 2},
		{Dest: 0, Start: at(30), Sent: 7, Received: 70, Connections: 1},
		{Dest: 1, Start: at(12), Sent: 10, Received: 900, Connections: 1},
	}
	if !slices.Equal(r.Openings, want) {
		t.Errorf("openings = %+v, want %+v", r.Openings, want)
	}
	if r.Destinations[0].Addr != github || r.Destinations[1].Addr != npm {
		t.Errorf("destinations = %+v, want github then npm, which the openings index", r.Destinations)
	}
}

// THE TRAFFIC BEYOND THE NAMED DESTINATIONS IS SPLIT TOO, under -1 and last,
// and each named destination's openings are its whole traffic.
func TestOpeningsBeyondTheCapAreTheOtherTraffic(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)
	for i := range MaxDestinations + 3 {
		dst := netip.AddrFrom4([4]byte{10, 1, byte(i / 256), byte(i % 256)})
		a.Observe(flow(uint32(i+1), guest, dst, uint64(i+1), uint64(i+1)))
	}

	r, _ := a.Final("lease-1", nil, ended)
	if len(r.Openings) != MaxDestinations+1 {
		t.Fatalf("%d openings, want one for each named destination and one for the rest", len(r.Openings))
	}
	last := r.Openings[len(r.Openings)-1]
	if last.Dest != -1 || last.Connections != 3 || last.Sent != r.Other.Sent || last.Received != r.Other.Received {
		t.Errorf("the last opening = %+v, want the other traffic, %+v", last, r.Other)
	}
	for i, d := range r.Destinations {
		if o := r.Openings[i]; o.Dest != i || o.Sent != d.Sent || o.Received != d.Received ||
			o.Connections != d.Connections {
			t.Fatalf("opening %d = %+v, want destination %d's whole traffic %+v", i, o, i, d)
		}
	}
}

// A JOB WHOSE CONNECTIONS OPEN ACROSS MORE BUCKETS THAN THE BOUND has its
// buckets widened by doubling until they fit, losing nothing: a coarser answer,
// never a partial one. Every bucket starts on a multiple of the width.
func TestOpeningsWidenUntilTheyFitAndLoseNothing(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)
	// One connection a second to one destination, for more seconds than the
	// bound.
	const n = MaxOpenings + 5
	for i := range n {
		a.Observe(startedAt(flow(uint32(i+1), guest, github, 1, 2), started.Add(time.Duration(i)*time.Second)))
	}

	r, _ := a.Final("lease-1", nil, began.Add(48*time.Hour))
	if len(r.Openings) > MaxOpenings {
		t.Fatalf("%d openings, want at most %d", len(r.Openings), MaxOpenings)
	}
	if r.OpeningWidth != 2*time.Second {
		t.Errorf("width = %s, want 2s, the first doubling that fits", r.OpeningWidth)
	}
	var connections, sent uint64
	for i, o := range r.Openings {
		connections, sent = connections+o.Connections, sent+o.Sent
		if o.Start.Unix()%2 != 0 {
			t.Errorf("opening %d starts at %s, not on a multiple of the width", i, o.Start)
		}
		if i > 0 && !r.Openings[i-1].Start.Before(o.Start) {
			t.Errorf("opening %d at %s is not after the one before it", i, o.Start)
		}
	}
	if connections != n || sent != n {
		t.Errorf("the openings hold %d connections and %d bytes sent, want %d of each", connections, sent, n)
	}
}

// floorDiv rounds down on both sides of zero, so a bucket is the same width
// whichever side of 1970 it falls.
func TestFloorDivRoundsDown(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ a, b, want int64 }{{7, 2, 3}, {-7, 2, -4}, {-8, 2, -4}, {0, 4, 0}, {8, 4, 2}} {
		if got := floorDiv(tc.a, tc.b); got != tc.want {
			t.Errorf("floorDiv(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
