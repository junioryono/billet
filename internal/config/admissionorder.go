package config

import "fmt"

// AdmissionOrder decides which waiting tier may buy capacity when the fleet
// cannot hold every tier that wants work at once.
//
// IT DECIDES PURCHASES, NEVER ADVERTISEMENTS. Every tier tells GitHub what it
// could run at all times, because a tier that advertises nothing is assigned
// nothing (#140); this is only about who takes the room a finished job leaves.
type AdmissionOrder string

const (
	// AdmissionFair starts the work of the tier that has waited longest, and
	// holds the freed room until that tier's shape fits.
	//
	// THE DEFAULT, because the alternative has an unbounded failure. A large
	// shape only ever fits when enough small jobs have finished at once, so on a
	// fleet that is never idle, first-come means a 64-vCPU job waits behind a
	// stream of 2-vCPU jobs for as long as they keep arriving. Holding the line
	// costs throughput while the room accumulates, which is bounded by the
	// longest job running, and nothing else can deadlock: there is one winner,
	// and it is chosen by a wait it cannot extend.
	AdmissionFair AdmissionOrder = "fair"

	// AdmissionFill starts anything that fits the room there is.
	//
	// For a deployment whose tiers are close enough in size that head-of-line
	// blocking costs more than it buys, or one that would rather keep every core
	// busy and accepts that its largest shape may wait indefinitely.
	AdmissionFill AdmissionOrder = "fill"
)

// Or returns the order, or fair when nothing was chosen.
func (o AdmissionOrder) Or() AdmissionOrder {
	if o == "" {
		return AdmissionFair
	}

	return o
}

// Validate reports whether the order is one billet implements.
//
// A TYPO MUST NOT SILENTLY BECOME THE DEFAULT, exactly as for server.placement:
// "fifo" or "filled" would otherwise fall through Or() and an operator who
// chose fill deliberately would never learn their fleet was holding the line.
func (o AdmissionOrder) Validate() error {
	switch o {
	case "", AdmissionFair, AdmissionFill:
		return nil
	default:
		return fmt.Errorf("server.admission_order is %q; it must be %q or %q",
			o, AdmissionFair, AdmissionFill)
	}
}

// Order reports the admission order this control plane is configured with.
//
// NIL-SAFE for the same reason DrainTimeoutDuration is: a `server:` block is
// optional in a config that only describes a node, and the caller assembling
// control-plane options should not have to know which blocks are present.
func (s *ServerConfig) Order() AdmissionOrder {
	if s == nil {
		return AdmissionFair
	}

	return s.AdmissionOrder.Or()
}
