package fleetops

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
)

// shownDestinations is how many destinations `jobs show` lists by name; the
// rest are counted and totalled on one line.
const shownDestinations = 20

// renderDestinations writes where a job's traffic went: the largest
// destinations by total bytes, the traffic beyond the ones the node kept by
// name, which of them got nothing back, whether the totals are complete, and
// the tap's totals beside them.
//
// A JOB WHOSE FLOWS WERE NOT TOTALLED SAYS SO, never an empty table, which
// would read as a job that sent nothing.
func renderDestinations(w io.Writer, d *alloc.JobDestinations) {
	fmt.Fprintln(w)
	if d == nil {
		fmt.Fprintln(w, "destinations not measured (node.monitoring.flows, on a firecracker node)")
		return
	}
	pad := strings.Repeat(" ", 11)

	sorted := slices.Clone(d.Destinations)
	slices.SortStableFunc(sorted, func(x, y alloc.JobDestination) int {
		return exact(y.SentBytes, y.ReceivedBytes).Cmp(exact(x.SentBytes, x.ReceivedBytes))
	})
	shown := sorted[:min(len(sorted), shownDestinations)]

	fmt.Fprintf(w, "destinations %d by total bytes, from the host's connection tracker "+
		"(attributed by the guest's source address)\n", len(sorted))
	if len(shown) > 0 {
		width := len("address")
		for _, dest := range shown {
			width = max(width, len(dest.Addr))
		}
		row := func(addr, sent, received, connections, unanswered string) {
			fmt.Fprintf(w, "%s%-*s  %10s  %10s  %11s", pad, width, addr, sent, received, connections)
			if d.UnansweredCounted {
				fmt.Fprintf(w, "  %10s", unanswered)
			}
			fmt.Fprintln(w)
		}
		row("address", "sent", "received", "connections", "unanswered")
		for _, dest := range shown {
			row(dest.Addr, cli.HumanBytes(dest.SentBytes), cli.HumanBytes(dest.ReceivedBytes),
				groupDigits(dest.Connections), groupDigits(dest.Unanswered))
		}
	}
	if rest := sorted[len(shown):]; len(rest) > 0 {
		fmt.Fprintf(w, "%sand %d more: %s\n", pad, len(rest), sumOf(rest...).said(d.UnansweredCounted))
	}

	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	line("other", "%s (beyond the %d destinations the node keeps by name)",
		sumOf(d.Other).said(d.UnansweredCounted), alloc.MaxJobDestinations)
	line("unanswered", "%s", unansweredText(d))
	if d.Incomplete {
		line("complete", "no: flows may have been missed, so every total here is a lower bound")
	} else {
		line("complete", "yes")
	}

	if d.Tap == nil {
		line("tap", "not read, so there is nothing to compare the destinations with")
		return
	}
	attributed := sumOf(append(slices.Clone(d.Destinations), d.Other)...)
	line("tap", "sent %s, received %s; tap minus attributed: sent %s, received %s",
		cli.HumanBytes(d.Tap.SentBytes), cli.HumanBytes(d.Tap.ReceivedBytes),
		signedBytes(new(big.Int).Sub(big.NewInt(d.Tap.SentBytes), attributed.sent)),
		signedBytes(new(big.Int).Sub(big.NewInt(d.Tap.ReceivedBytes), attributed.received)))
	fmt.Fprintf(w, "%s(a comparison of two counters read a moment apart, not a measurement of what "+
		"was missed)\n", pad)
}

// unansweredText says which destinations got nothing back on some of their
// connections, the most first, by name and not only among the largest by
// bytes: a destination a job stalled on moved next to nothing, so the table
// may never list it.
//
// A REPORT THAT DID NOT COUNT THEM SAYS SO, never "none", which would clear a
// destination nothing was measured against.
func unansweredText(d *alloc.JobDestinations) string {
	if !d.UnansweredCounted {
		return "not counted (by a node, or over a node wire, older than the count), so this says " +
			"nothing about a connection that got nothing back"
	}
	total := sumOf(append(slices.Clone(d.Destinations), d.Other)...).unanswered
	if total.Sign() == 0 {
		return "none: every connection the guest sent on got something back"
	}
	var stalled []alloc.JobDestination
	for _, dest := range d.Destinations {
		if dest.Unanswered > 0 {
			stalled = append(stalled, dest)
		}
	}
	slices.SortStableFunc(stalled, func(x, y alloc.JobDestination) int {
		return cmp.Compare(y.Unanswered, x.Unanswered)
	})
	var named []string
	for _, dest := range stalled[:min(len(stalled), shownDestinations)] {
		named = append(named, dest.Addr+" "+groupDigits(dest.Unanswered))
	}
	if rest := stalled[min(len(stalled), shownDestinations):]; len(rest) > 0 {
		named = append(named, fmt.Sprintf("%d more destinations %s", len(rest),
			bigCount(sumOf(rest...).unanswered)))
	}
	if d.Other.Unanswered > 0 {
		named = append(named, "other "+groupDigits(d.Other.Unanswered))
	}

	return fmt.Sprintf("%s connections got nothing back (a SYN nothing answered, or a datagram, a DNS "+
		"query among them, with no reply): %s", bigCount(total), strings.Join(named, ", "))
}

// traffic is destinations' totals summed exactly: up to 257 counts that each
// fit an int64 need not fit one together, and a sum held at the largest int64
// would print a total, and a difference from the tap, that are simply wrong.
type traffic struct{ sent, received, connections, unanswered *big.Int }

func sumOf(dests ...alloc.JobDestination) traffic {
	t := traffic{sent: new(big.Int), received: new(big.Int), connections: new(big.Int), unanswered: new(big.Int)}
	for _, d := range dests {
		t.sent.Add(t.sent, big.NewInt(d.SentBytes))
		t.received.Add(t.received, big.NewInt(d.ReceivedBytes))
		t.connections.Add(t.connections, big.NewInt(d.Connections))
		t.unanswered.Add(t.unanswered, big.NewInt(d.Unanswered))
	}

	return t
}

func (t traffic) String() string {
	return fmt.Sprintf("sent %s, received %s, %s connections", bigBytes(t.sent), bigBytes(t.received),
		bigCount(t.connections))
}

// said is t with its unanswered count where the report counted one.
func (t traffic) said(unansweredCounted bool) string {
	if !unansweredCounted {
		return t.String()
	}

	return t.String() + ", " + bigCount(t.unanswered) + " unanswered"
}

// bigCount writes a count that may not fit an int64, saying so where it does
// not rather than printing a smaller number.
func bigCount(n *big.Int) string {
	if n.IsInt64() {
		return groupDigits(n.Int64())
	}

	return "more than " + groupDigits(math.MaxInt64)
}

// exact is a destination's total bytes, which can pass the largest int64.
func exact(sent, received int64) *big.Int {
	return new(big.Int).Add(big.NewInt(sent), big.NewInt(received))
}

// bigBytes writes a byte count that may not fit an int64, saying so where it
// does not rather than printing a smaller number.
func bigBytes(n *big.Int) string {
	if n.IsInt64() {
		return cli.HumanBytes(n.Int64())
	}

	return "more than " + cli.HumanBytes(math.MaxInt64)
}

// signedBytes writes a difference of byte counts with its sign.
func signedBytes(n *big.Int) string {
	if n.Sign() < 0 {
		return "-" + bigBytes(new(big.Int).Neg(n))
	}

	return "+" + bigBytes(n)
}
