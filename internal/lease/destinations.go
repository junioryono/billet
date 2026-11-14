package lease

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
)

// MaxJobDestinations is how many destinations one report names. A node keeps
// that many by name and totals the rest under Other, so the ledger holds at
// most one more row than this for a job.
const MaxJobDestinations = 256

// SlowReplyMillis is the wait for a connection's first reply past which a
// report counts it slow: TCP's initial retransmission timeout, the node's
// flows.SlowReply, which a test holds this to.
const SlowReplyMillis = 1000

// JobDestinations is what a job's traffic came to by destination, read from the
// host's connection tracker.
//
// ATTRIBUTED BY THE GUEST'S SOURCE ADDRESS, which a root guest can forge on a
// host that does not drop spoofed packets: the totals are for people to read,
// and nothing is decided from them.
type JobDestinations struct {
	// Destinations, largest total first, at most MaxJobDestinations.
	Destinations []JobDestination `json:"destinations,omitempty"`
	// Other is the traffic to every destination beyond the ones named; its Addr
	// is empty.
	Other JobDestination `json:"other"`
	// Incomplete says flows may have been missed, so every total is a lower
	// bound.
	Incomplete bool `json:"incomplete"`
	// Tap is the guest's tap's own totals, read when the job's last sample was
	// taken; nil when the tap was not read.
	Tap *TapTotals `json:"tap,omitempty"`
	// UnansweredCounted says each destination's Unanswered was counted, so a
	// zero there is a destination that answered everything; false, from a node
	// or a wire before it, says nothing about what went unanswered.
	UnansweredCounted bool `json:"unanswered_counted,omitempty"`
	// RepliesTimed says each destination's first replies were timed, so a zero
	// SlowReplies there is a destination that answered within SlowReplyMillis
	// every time it was timed; false, from a node or a wire before it or a job
	// whose reply events were not all read, says nothing about how fast
	// anything answered.
	RepliesTimed bool `json:"replies_timed,omitempty"`
	// Openings is the same traffic split by when each connection opened: per
	// destination, per bucket of OpeningSeconds, so a report can say which step
	// of the job talked to which destination. Empty, with OpeningSeconds zero,
	// from a node or a wire before VersionDestinationOpenings, which says
	// nothing of when.
	//
	// BY WHEN A CONNECTION OPENED, NOT WHEN ITS BYTES MOVED: the host's tracker
	// keeps a connection's totals and not their times, so a connection open
	// across two steps is counted in the one it opened in.
	Openings       []DestinationOpening `json:"openings,omitempty"`
	OpeningSeconds int64                `json:"opening_seconds,omitempty"`
}

// MaxDestinationOpenings is how many openings one report carries, the node's
// flows.MaxOpenings, which widens its buckets until a job's fit.
const MaxDestinationOpenings = 2048

// maxOpeningSeconds bounds a bucket's width: a power of two past any job's
// length, which the node never needs to reach.
const maxOpeningSeconds = 1 << 24

// DestinationOpening is the traffic of the connections a job opened to one
// destination in one bucket of time.
type DestinationOpening struct {
	// Ordinal is the destination's index in Destinations, or
	// MaxJobDestinations for the traffic beyond them, as the ledger numbers
	// the rows.
	Ordinal int `json:"ordinal"`
	// Start is the bucket's start, in Unix seconds, a multiple of the report's
	// OpeningSeconds.
	Start         int64 `json:"start"`
	SentBytes     int64 `json:"sent_bytes"`
	ReceivedBytes int64 `json:"received_bytes"`
	Connections   int64 `json:"connections"`
}

// WithoutOpenings is d as a wire before the openings carries it: no split by
// time, which says less, never something else.
func (d JobDestinations) WithoutOpenings() JobDestinations {
	d.Openings, d.OpeningSeconds = nil, 0

	return d
}

// JobDestination is the traffic a job exchanged with one remote address, from
// the guest's side.
type JobDestination struct {
	Addr          string `json:"addr,omitempty"`
	SentBytes     int64  `json:"sent_bytes"`
	ReceivedBytes int64  `json:"received_bytes"`
	Connections   int64  `json:"connections"`
	// Unanswered is how many of Connections the guest sent on and got no packet
	// back from: a SYN nothing answered, or a datagram, a DNS query among them,
	// with no reply. Counted only where the report's UnansweredCounted says so.
	Unanswered int64 `json:"unanswered,omitempty"`
	// TimedReplies is how many of the answered connections had their wait for
	// a first reply timed, SlowReplies how many of those waited longer than
	// SlowReplyMillis, and SlowestReplyMillis the longest of their waits. Kept
	// only where the report's RepliesTimed says so.
	TimedReplies       int64 `json:"timed_replies,omitempty"`
	SlowReplies        int64 `json:"slow_replies,omitempty"`
	SlowestReplyMillis int64 `json:"slowest_reply_ms,omitempty"`
}

// WithoutUnanswered is d as a wire before the unanswered counts carries it:
// the counts and the mark that they were counted gone, which says less, never
// something else.
func (d JobDestinations) WithoutUnanswered() JobDestinations {
	d.UnansweredCounted = false
	d.Destinations = slices.Clone(d.Destinations)
	for i := range d.Destinations {
		d.Destinations[i].Unanswered = 0
	}
	d.Other.Unanswered = 0

	return d
}

// WithoutReplyTimes is d as a wire before the reply times carries it: the
// times and the mark that they were taken gone, which says less, never
// something else.
func (d JobDestinations) WithoutReplyTimes() JobDestinations {
	d.RepliesTimed = false
	d.Destinations = slices.Clone(d.Destinations)
	for i := range d.Destinations {
		d.Destinations[i] = d.Destinations[i].withoutReplyTimes()
	}
	d.Other = d.Other.withoutReplyTimes()

	return d
}

func (d JobDestination) withoutReplyTimes() JobDestination {
	d.TimedReplies, d.SlowReplies, d.SlowestReplyMillis = 0, 0, 0

	return d
}

// TapTotals is what the guest's tap counted, from the guest's side, with each
// packet's Ethernet header taken off so it compares with what the connection
// tracker counts.
//
// A COMPARISON OF TWO COUNTERS READ A MOMENT APART, not a measurement of what
// was missed: the tap is read before the destroy and the flows after it.
type TapTotals struct {
	SentBytes     int64 `json:"sent_bytes"`
	ReceivedBytes int64 `json:"received_bytes"`
}

// Validate refuses destinations the ledger could not keep faithfully: more
// than it keeps by name, an address that is not one in its canonical spelling,
// carries a zone or is named twice, an Other that names an address, or a
// negative total.
func (d JobDestinations) Validate() error {
	if len(d.Destinations) > MaxJobDestinations {
		return fmt.Errorf("alloc: a usage report names %d destinations, and at most %d are kept by name",
			len(d.Destinations), MaxJobDestinations)
	}
	seen := make(map[string]bool, len(d.Destinations))
	for _, dest := range d.Destinations {
		// NO ZONE, which is free text a reader's report would print.
		addr, err := netip.ParseAddr(dest.Addr)
		if err != nil || addr.Zone() != "" || addr.String() != dest.Addr {
			return fmt.Errorf("alloc: destination %q is not an address in its canonical form", dest.Addr)
		}
		if seen[dest.Addr] {
			return fmt.Errorf("alloc: destination %s is named twice", dest.Addr)
		}
		seen[dest.Addr] = true
		if err := dest.validateTotals(dest.Addr, d.UnansweredCounted, d.RepliesTimed); err != nil {
			return err
		}
	}
	if d.Other.Addr != "" {
		return fmt.Errorf("alloc: the traffic beyond the named destinations names an address (%q)",
			d.Other.Addr)
	}
	if err := d.Other.validateTotals("other", d.UnansweredCounted, d.RepliesTimed); err != nil {
		return err
	}
	if d.Tap != nil && (d.Tap.SentBytes < 0 || d.Tap.ReceivedBytes < 0) {
		return fmt.Errorf("alloc: the tap's totals are negative (sent %d, received %d)",
			d.Tap.SentBytes, d.Tap.ReceivedBytes)
	}

	return d.validateOpenings()
}

// validateOpenings refuses openings no split of these destinations gives: more
// than are kept, a width that is not a power of two, a bucket off its width or
// before 1970, an ordinal naming no destination, two openings out of order or
// for the same bucket, a negative count, or a destination whose openings add up
// to more than its totals.
//
// MORE THAN THE TOTALS, NOT OTHER THAN THEM: a node keeps a total too large for
// the ledger at its largest, and the openings it clamped then add up to less.
func (d JobDestinations) validateOpenings() error {
	if d.OpeningSeconds == 0 {
		if len(d.Openings) > 0 {
			return fmt.Errorf("alloc: a usage report has %d openings and no width", len(d.Openings))
		}

		return nil
	}
	w := d.OpeningSeconds
	if w < 1 || w > maxOpeningSeconds || w&(w-1) != 0 {
		return fmt.Errorf("alloc: a usage report's openings are %d seconds wide, which is not a power of two "+
			"from 1 to %d", w, maxOpeningSeconds)
	}
	if len(d.Openings) > MaxDestinationOpenings {
		return fmt.Errorf("alloc: a usage report has %d openings, and at most %d are kept",
			len(d.Openings), MaxDestinationOpenings)
	}

	type sum struct{ sent, received, connections int64 }
	sums := map[int]*sum{}
	for i, o := range d.Openings {
		if o.Ordinal < 0 || (o.Ordinal >= len(d.Destinations) && o.Ordinal != MaxJobDestinations) {
			return fmt.Errorf("alloc: opening %d names destination %d of %d", i, o.Ordinal, len(d.Destinations))
		}
		if o.Start <= 0 || o.Start%w != 0 {
			return fmt.Errorf("alloc: opening %d starts at %d, which is not a %d-second bucket after 1970",
				i, o.Start, w)
		}
		if o.SentBytes < 0 || o.ReceivedBytes < 0 || o.Connections < 0 {
			return fmt.Errorf("alloc: opening %d has a negative total", i)
		}
		if i > 0 {
			p := d.Openings[i-1]
			if p.Ordinal > o.Ordinal || (p.Ordinal == o.Ordinal && p.Start >= o.Start) {
				return fmt.Errorf("alloc: opening %d is not after the one before it", i)
			}
		}
		s := sums[o.Ordinal]
		if s == nil {
			s = &sum{}
			sums[o.Ordinal] = s
		}
		// Each count is at most MaxInt64 and there are at most 2048 of them, so
		// a sum past MaxInt64 saturates rather than wrapping.
		s.sent, s.received, s.connections = addCapped(s.sent, o.SentBytes), addCapped(s.received, o.ReceivedBytes),
			addCapped(s.connections, o.Connections)
	}
	for ordinal, s := range sums {
		dest := d.Other
		if ordinal != MaxJobDestinations {
			dest = d.Destinations[ordinal]
		}
		if s.sent > dest.SentBytes || s.received > dest.ReceivedBytes || s.connections > dest.Connections {
			return fmt.Errorf("alloc: the openings of destination %d add up to more than its totals", ordinal)
		}
	}

	return nil
}

// addCapped is a+b for two counts that are not negative, held at the largest
// int64 rather than wrapping.
func addCapped(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}

	return a + b
}

func (d JobDestination) validateTotals(name string, unansweredCounted, repliesTimed bool) error {
	if d.SentBytes < 0 || d.ReceivedBytes < 0 || d.Connections < 0 || d.Unanswered < 0 ||
		d.TimedReplies < 0 || d.SlowReplies < 0 || d.SlowestReplyMillis < 0 {
		return fmt.Errorf("alloc: destination %s has a negative total (sent %d, received %d, "+
			"connections %d, unanswered %d, timed replies %d, slow replies %d, slowest reply %d ms)",
			name, d.SentBytes, d.ReceivedBytes, d.Connections, d.Unanswered, d.TimedReplies, d.SlowReplies,
			d.SlowestReplyMillis)
	}
	// AN UNANSWERED COUNT IS A COUNT OF ITS CONNECTIONS, and only of a report
	// that says it counted them: a count with no such mark could not be told
	// from an uncounted zero once it was kept.
	if d.Unanswered > d.Connections {
		return fmt.Errorf("alloc: destination %s has %d unanswered of %d connections",
			name, d.Unanswered, d.Connections)
	}
	if d.Unanswered > 0 && !unansweredCounted {
		return fmt.Errorf("alloc: destination %s has %d unanswered in a report that did not count them",
			name, d.Unanswered)
	}

	return d.validateReplyTimes(name, repliesTimed)
}

// validateReplyTimes refuses reply times no measurement gives: more timed
// replies than answered connections, more slow ones than timed, a slowest wait
// with nothing timed or too short for the slow ones it stands for, and any of
// them in a report that did not time replies, which could not be told from an
// untimed zero once kept.
func (d JobDestination) validateReplyTimes(name string, repliesTimed bool) error {
	if !repliesTimed {
		if d.TimedReplies != 0 || d.SlowReplies != 0 || d.SlowestReplyMillis != 0 {
			return fmt.Errorf("alloc: destination %s has reply times in a report that did not time replies",
				name)
		}

		return nil
	}
	if answered := d.Connections - d.Unanswered; d.TimedReplies > answered {
		return fmt.Errorf("alloc: destination %s has %d timed replies of %d answered connections",
			name, d.TimedReplies, answered)
	}
	if d.SlowReplies > d.TimedReplies {
		return fmt.Errorf("alloc: destination %s has %d slow replies of %d timed", name, d.SlowReplies,
			d.TimedReplies)
	}
	if d.TimedReplies == 0 && d.SlowestReplyMillis != 0 {
		return fmt.Errorf("alloc: destination %s has a slowest reply of %d ms and no timed reply", name,
			d.SlowestReplyMillis)
	}
	if d.SlowReplies > 0 && d.SlowestReplyMillis < SlowReplyMillis {
		return fmt.Errorf("alloc: destination %s has %d slow replies and its slowest took %d ms, under %d",
			name, d.SlowReplies, d.SlowestReplyMillis, SlowReplyMillis)
	}

	return nil
}

// errTapWithoutNet is a tap reading beside a network group the same report
// names unmeasured: the two come from one reading of the tap.
var errTapWithoutNet = errors.New("alloc: a usage report names net unmeasured and gives the tap's totals")
