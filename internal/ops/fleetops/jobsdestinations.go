package fleetops

import (
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
// name, whether the totals are complete, and the tap's totals beside them.
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
		fmt.Fprintf(w, "%s%-*s  %10s  %10s  %11s\n", pad, width, "address", "sent", "received", "connections")
		for _, dest := range shown {
			fmt.Fprintf(w, "%s%-*s  %10s  %10s  %11s\n", pad, width, dest.Addr, cli.HumanBytes(dest.SentBytes),
				cli.HumanBytes(dest.ReceivedBytes), groupDigits(dest.Connections))
		}
	}
	if rest := sorted[len(shown):]; len(rest) > 0 {
		fmt.Fprintf(w, "%sand %d more: %s\n", pad, len(rest), sumOf(rest...))
	}

	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	line("other", "%s (beyond the %d destinations the node keeps by name)", sumOf(d.Other),
		alloc.MaxJobDestinations)
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

// traffic is destinations' totals summed exactly: up to 257 counts that each
// fit an int64 need not fit one together, and a sum held at the largest int64
// would print a total, and a difference from the tap, that are simply wrong.
type traffic struct{ sent, received, connections *big.Int }

func sumOf(dests ...alloc.JobDestination) traffic {
	t := traffic{sent: new(big.Int), received: new(big.Int), connections: new(big.Int)}
	for _, d := range dests {
		t.sent.Add(t.sent, big.NewInt(d.SentBytes))
		t.received.Add(t.received, big.NewInt(d.ReceivedBytes))
		t.connections.Add(t.connections, big.NewInt(d.Connections))
	}

	return t
}

func (t traffic) String() string {
	connections := "more than " + groupDigits(math.MaxInt64)
	if t.connections.IsInt64() {
		connections = groupDigits(t.connections.Int64())
	}

	return fmt.Sprintf("sent %s, received %s, %s connections", bigBytes(t.sent), bigBytes(t.received),
		connections)
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
