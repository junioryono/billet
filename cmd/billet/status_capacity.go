package main

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/server"
)

// printTierCapacity separates charged leases from a listener's last observation.
//
// HEADROOM IS ADDITIONAL ROOM, NOT AN ADVERTISEMENT. A tier can hold and advertise
// a discovery slot while the allocator has no room left to grant another one.
func printTierCapacity(w io.Writer, label string, r alloc.TierCapacity, now time.Time) {
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

	// PRINTED ONLY WHEN THERE IS A QUEUE. Nothing is reserved for an idle tier,
	// so a tier with queued work and a tier nobody wants are the same shape in
	// every count above; this line is the difference, and a zero printed beside
	// every healthy tier is a line nobody reads.
	if r.Listener.Waiting > 0 && r.Listener.WaitingSince != "" {
		fmt.Fprintf(w, "          waiting %d job(s) for room, oldest since %s\n",
			r.Listener.Waiting, r.Listener.WaitingSince)
		// AN OBSERVATION, NOT THE QUEUE'S DECISION. The listener publishes its
		// progress once a poll; one launching for longer still holds the line, and
		// one that stalled cannot publish that it has.
		if stalled := stalledFor(r.Listener.WaitingProgress, now); stalled > server.WaiterAllowance {
			fmt.Fprintf(w, "          NO ADMISSION PROGRESS PUBLISHED for %s (last %s): unless its listener "+
				"is in a launch, other tiers may buy ahead of it until it progresses\n",
				stalled.Round(time.Second), r.Listener.WaitingProgress)
		}
	}
}

// stalledFor is how long ago a waiter last progressed, or zero when the report
// predates the field or cannot be read: an unknown is not shown as a stall.
func stalledFor(progress string, now time.Time) time.Duration {
	at, err := time.Parse(time.RFC3339Nano, progress)
	if progress == "" || err != nil {
		return 0
	}

	return now.Sub(at)
}
