package fleetops

import (
	"cmp"
	"fmt"
	"math"
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

// stepDestinations words where a step's traffic went: the destinations its
// connections opened to between its start and its end, largest first, from the
// host's connection tracker. It is empty for a job whose destinations were not
// split by time, which a node before the split, or a job whose flows were not
// totalled, sent none of: the report then says nothing per step rather than that
// a step talked to nothing.
//
// BY WHEN A CONNECTION OPENED: a connection's bytes are all in the bucket it
// opened in, so one open across two steps is the first one's, and one opened in
// the second before a step's end and still running is that step's. A bucket
// wider than a second is placed by its start, which the words say.
//
// ON TWO CLOCKS, as a step's usage is: GitHub's for the step and the host's for
// the connection, with no correction for skew between them.
func stepDestinations(d *alloc.JobDestinations, from, to time.Time) string {
	if d == nil || d.OpeningSeconds == 0 || !from.Before(to) {
		return ""
	}

	type total struct {
		ordinal                     int
		sent, received, connections int64
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
	}

	prefix := ""
	if d.OpeningSeconds > 1 {
		prefix = fmt.Sprintf("to within %ds, ", d.OpeningSeconds)
	}
	if len(byOrdinal) == 0 {
		return prefix + "no connection opened in this step"
	}

	all := make([]*total, 0, len(byOrdinal))
	for _, t := range byOrdinal {
		all = append(all, t)
	}
	// LARGEST FIRST, AND THE ORDER IS TOTAL: equal totals fall back to the
	// ordinal, which is the job's own largest-first order.
	slices.SortFunc(all, func(x, y *total) int {
		return cmp.Or(cmp.Compare(addCapped(y.sent, y.received), addCapped(x.sent, x.received)),
			cmp.Compare(x.ordinal, y.ordinal))
	})

	name := func(ordinal int) string {
		if ordinal == alloc.MaxJobDestinations || ordinal >= len(d.Destinations) {
			return "traffic beyond the named destinations"
		}
		return d.Destinations[ordinal].Addr
	}
	parts := make([]string, 0, stepDestinationsShown+1)
	for _, t := range all[:min(len(all), stepDestinationsShown)] {
		parts = append(parts, fmt.Sprintf("%s (sent %s, received %s, %s)", name(t.ordinal),
			cli.HumanBytes(t.sent), cli.HumanBytes(t.received), connectionsText(t.connections)))
	}
	if rest := all[min(len(all), stepDestinationsShown):]; len(rest) > 0 {
		var sent, received, connections int64
		for _, t := range rest {
			sent, received, connections = addCapped(sent, t.sent), addCapped(received, t.received),
				addCapped(connections, t.connections)
		}
		parts = append(parts, fmt.Sprintf("and %d more (sent %s, received %s, %s)", len(rest),
			cli.HumanBytes(sent), cli.HumanBytes(received), connectionsText(connections)))
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
