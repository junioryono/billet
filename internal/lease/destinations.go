package lease

import (
	"errors"
	"fmt"
	"net/netip"
)

// MaxJobDestinations is how many destinations one report names. A node keeps
// that many by name and totals the rest under Other, so the ledger holds at
// most one more row than this for a job.
const MaxJobDestinations = 256

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
}

// JobDestination is the traffic a job exchanged with one remote address, from
// the guest's side.
type JobDestination struct {
	Addr          string `json:"addr,omitempty"`
	SentBytes     int64  `json:"sent_bytes"`
	ReceivedBytes int64  `json:"received_bytes"`
	Connections   int64  `json:"connections"`
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
		if err := dest.validateTotals(dest.Addr); err != nil {
			return err
		}
	}
	if d.Other.Addr != "" {
		return fmt.Errorf("alloc: the traffic beyond the named destinations names an address (%q)",
			d.Other.Addr)
	}
	if err := d.Other.validateTotals("other"); err != nil {
		return err
	}
	if d.Tap != nil && (d.Tap.SentBytes < 0 || d.Tap.ReceivedBytes < 0) {
		return fmt.Errorf("alloc: the tap's totals are negative (sent %d, received %d)",
			d.Tap.SentBytes, d.Tap.ReceivedBytes)
	}

	return nil
}

func (d JobDestination) validateTotals(name string) error {
	if d.SentBytes < 0 || d.ReceivedBytes < 0 || d.Connections < 0 {
		return fmt.Errorf("alloc: destination %s has a negative total (sent %d, received %d, "+
			"connections %d)", name, d.SentBytes, d.ReceivedBytes, d.Connections)
	}

	return nil
}

// errTapWithoutNet is a tap reading beside a network group the same report
// names unmeasured: the two come from one reading of the tap.
var errTapWithoutNet = errors.New("alloc: a usage report names net unmeasured and gives the tap's totals")
