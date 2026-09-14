package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/junioryono/billet/internal/alloc"
)

// printTierCapacity separates charged leases from a listener's last observation.
//
// HEADROOM IS ADDITIONAL ROOM, NOT AN ADVERTISEMENT. A tier can hold and advertise
// a discovery slot while the allocator has no room left to grant another one.
func printTierCapacity(w io.Writer, label string, r alloc.TierCapacity) {
	fmt.Fprintf(w, "tier      %s\n", label)
	fmt.Fprintf(w, "          discovery %d, pending %d, launching %d, running %d, cleanup %d, unknown %d\n",
		r.Discovery, r.Pending, r.Launching, r.Running, r.Cleanup, r.Unknown)
	fmt.Fprintf(w, "          reserved floor %d, additional headroom %d\n", r.Floor, r.Headroom)
	if r.ObservationError != "" {
		fmt.Fprintf(w, "          advertisement unknown: %s\n", r.ObservationError)
		return
	}
	if r.ObservedAt == "" {
		fmt.Fprintln(w, "          advertisement unknown: no listener observation")
		return
	}
	count := func(n *int) string {
		if n == nil {
			return "unknown"
		}
		return strconv.Itoa(*n)
	}
	fmt.Fprintf(w, "          advertisement last confirmed %s, sent %s, exchange %s (observed %s)\n",
		count(r.Listener.Confirmed), count(r.Listener.Sent), r.Listener.Exchange, r.ObservedAt)
}
