package fleetops

import (
	"cmp"
	"fmt"
	"io"
	"math"
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
		return cmp.Compare(total(y.SentBytes, y.ReceivedBytes), total(x.SentBytes, x.ReceivedBytes))
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
		var sum alloc.JobDestination
		for _, dest := range rest {
			sum = plus(sum, dest)
		}
		fmt.Fprintf(w, "%sand %d more: %s\n", pad, len(rest), traffic(sum))
	}

	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	line("other", "%s (beyond the %d destinations the node keeps by name)", traffic(d.Other),
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
	attributed := d.Other
	for _, dest := range d.Destinations {
		attributed = plus(attributed, dest)
	}
	line("tap", "sent %s, received %s; tap minus attributed: sent %s, received %s",
		cli.HumanBytes(d.Tap.SentBytes), cli.HumanBytes(d.Tap.ReceivedBytes),
		signedBytes(d.Tap.SentBytes-attributed.SentBytes),
		signedBytes(d.Tap.ReceivedBytes-attributed.ReceivedBytes))
	fmt.Fprintf(w, "%s(a comparison of two counters read a moment apart, not a measurement of what "+
		"was missed)\n", pad)
}

// traffic is one total of sent and received bytes and connections.
func traffic(d alloc.JobDestination) string {
	return fmt.Sprintf("sent %s, received %s, %s connections", cli.HumanBytes(d.SentBytes),
		cli.HumanBytes(d.ReceivedBytes), groupDigits(d.Connections))
}

// plus adds two destinations' totals, each held at the largest int64 rather
// than wrapping to a negative number a reader would take for a count.
func plus(a, b alloc.JobDestination) alloc.JobDestination {
	return alloc.JobDestination{
		SentBytes: total(a.SentBytes, b.SentBytes), ReceivedBytes: total(a.ReceivedBytes, b.ReceivedBytes),
		Connections: total(a.Connections, b.Connections),
	}
}

// total adds two counts the ledger holds non-negative, saturating.
func total(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}

	return a + b
}

// signedBytes writes a difference of byte counts with its sign.
func signedBytes(n int64) string {
	if n < 0 {
		return "-" + cli.HumanBytes(-n)
	}

	return "+" + cli.HumanBytes(n)
}
