package fleetops

import (
	"cmp"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
)

// stepDestinationsShown is how many of a step's destinations a report names by
// address; the rest are counted and totalled after them.
const stepDestinationsShown = 3

// stepDestinationsLabel heads a step's destinations wherever a report shows them.
const stepDestinationsLabel = "destinations, by when each connection opened"

// stepDestinationsClock is said once wherever a report shows a step's
// destinations, with or without a series: the step is on GitHub's clock and the
// connections on the host's, with no correction for skew between them, so a
// connection opened near a step's edge can be counted in the step beside it.
const stepDestinationsClock = "a step's destinations are by when each connection opened, on the host's " +
	"clock against GitHub's for the step, with no correction for skew between them, so a connection " +
	"opened near a step's edge can be counted in the step beside it"

// splitByTime says a job's destinations were split by when each connection
// opened, so its steps have destinations to show.
func splitByTime(measured *alloc.RecordedUsage) bool {
	return measured != nil && measured.Destinations != nil && measured.Destinations.OpeningSeconds > 0
}

// sentence is s with its first letter capitalised and a full stop after it, for
// a page that sets a note as a sentence.
func sentence(s string) string {
	if s == "" {
		return s
	}

	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// stepDestinations words where a step's traffic went: the destinations its
// connections opened to between its start and its end, largest first, from the
// host's connection tracker. It is empty for a job whose destinations were not
// split by time, which a node before the split, or a job whose flows were not
// totalled, sent none of: the report then says nothing per step rather than that
// a step talked to nothing.
//
// BY WHEN A CONNECTION OPENED: a connection's bytes are all in the bucket it
// opened in, so one open across two steps is the first one's. A bucket wider
// than a second is placed by its start, which the words say. Every figure is
// what was counted in the step's buckets, never a bound on the step's own: the
// clocks differ, and a bucket can straddle the step's end.
func stepDestinations(d *alloc.JobDestinations, from, to time.Time) string {
	if d == nil || d.OpeningSeconds == 0 || !from.Before(to) {
		return ""
	}

	// rank is the sum of every opening's bytes, exact in 128 bits, where the
	// shown counts are held at the largest int64.
	type total struct {
		ordinal                     int
		sent, received, connections int64
		rankHi, rankLo              uint64
	}
	byOrdinal := map[int]*total{}
	for _, o := range d.Openings {
		// [from, to) to the second, as GitHub gives a step's times.
		if o.Start < from.Unix() || o.Start >= to.Unix() {
			continue
		}
		t := byOrdinal[o.Ordinal]
		if t == nil {
			t = &total{ordinal: o.Ordinal}
			byOrdinal[o.Ordinal] = t
		}
		t.sent = addCapped(t.sent, o.SentBytes)
		t.received = addCapped(t.received, o.ReceivedBytes)
		t.connections = addCapped(t.connections, o.Connections)
		for _, v := range []int64{o.SentBytes, o.ReceivedBytes} {
			var carry uint64
			t.rankLo, carry = bits.Add64(t.rankLo, uint64(v), 0)
			t.rankHi += carry
		}
	}

	prefix := ""
	if d.OpeningSeconds > 1 {
		prefix = fmt.Sprintf("to within %ds (each connection counted in the step its %d-second bucket "+
			"starts in), ", d.OpeningSeconds, d.OpeningSeconds)
	}
	// AN INCOMPLETE CAPTURE PROVES NOTHING ABSENT: the job's flows may have been
	// missed anywhere in its life, so a step where none was seen is not a step
	// that opened none, and what was seen in one is only that.
	if d.Incomplete {
		if len(byOrdinal) == 0 {
			return prefix + "no connection seen opening in this step, though the job's flows are incomplete, " +
				"so some may have been missed"
		}
		prefix += "what was seen, since the job's flows are incomplete: "
	}
	if len(byOrdinal) == 0 {
		return prefix + "no connection opened in this step"
	}

	// THE TRAFFIC BEYOND THE NAMED DESTINATIONS IS NEVER RANKED AMONG THEM: it is
	// any number of addresses, so it is said apart, last, as itself.
	beyond := byOrdinal[alloc.MaxJobDestinations]
	named := make([]*total, 0, len(byOrdinal))
	for _, t := range byOrdinal {
		if t != beyond {
			named = append(named, t)
		}
	}
	// LARGEST FIRST, AND THE ORDER IS TOTAL: equal totals fall back to the
	// ordinal, which is the job's own largest-first order.
	slices.SortFunc(named, func(x, y *total) int {
		return cmp.Or(cmp.Compare(y.rankHi, x.rankHi), cmp.Compare(y.rankLo, x.rankLo),
			cmp.Compare(x.ordinal, y.ordinal))
	})

	traffic := func(sent, received, connections int64) string {
		return fmt.Sprintf("sent %s, received %s, %s", cli.HumanBytes(sent), cli.HumanBytes(received),
			connectionsText(connections))
	}
	parts := make([]string, 0, stepDestinationsShown+2)
	for _, t := range named[:min(len(named), stepDestinationsShown)] {
		addr := "an address the report does not name"
		if t.ordinal < len(d.Destinations) {
			addr = d.Destinations[t.ordinal].Addr
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", addr, traffic(t.sent, t.received, t.connections)))
	}
	if rest := named[min(len(named), stepDestinationsShown):]; len(rest) > 0 {
		var sent, received, connections int64
		for _, t := range rest {
			sent, received, connections = addCapped(sent, t.sent), addCapped(received, t.received),
				addCapped(connections, t.connections)
		}
		parts = append(parts, fmt.Sprintf("%d more named (%s)", len(rest), traffic(sent, received, connections)))
	}
	if beyond != nil {
		parts = append(parts, fmt.Sprintf("traffic beyond the named destinations (%s)",
			traffic(beyond.sent, beyond.received, beyond.connections)))
	}

	return prefix + strings.Join(parts, "; ")
}

// connectionsText is a count of connections in words.
func connectionsText(n int64) string {
	if n == 1 {
		return "1 connection"
	}

	return groupDigits(n) + " connections"
}

// addCapped is a+b for two counts that are not negative, held at the largest
// int64 rather than wrapping.
func addCapped(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}

	return a + b
}
