package config

import "fmt"

// PlacementPolicy decides which of several suitable machines a reservation is
// aimed at, once preference and eligibility have narrowed the field.
type PlacementPolicy string

const (
	// PlacementPack fills a machine before starting on the next one.
	//
	// THE DEFAULT, and the reason is that the failure it prevents is worse than
	// the one it causes. Spreading leaves every host partly used, and partly used
	// hosts cannot hold a LARGE tier: six 4-vCPU jobs spread across two 16-vCPU
	// machines leave four free on each, so an 8-vCPU job fits nowhere while eight
	// vCPU sit idle in the fleet. A job that cannot be placed is a worse outcome
	// than a job that shares a disk.
	//
	// The usual argument for spreading is contention, and it is weaker here than
	// it looks: billet escrows vCPU and memory, and the provider enforces both as
	// hard per-container limits, so two jobs on one host do not take each other's
	// cores or RAM. What they share is disk, page cache and network.
	//
	// It is also what makes a cloud host affordable — an instance with one job on
	// it cannot be shut down — and what billet's stated model already does:
	// Nomad bin-packs by default.
	PlacementPack PlacementPolicy = "pack"

	// PlacementSpread keeps machines as even as possible.
	//
	// For a deployment that would rather have every job on its own spindle than
	// fit the most work: fewer jobs contending for disk and page cache, and one
	// host dying takes a smaller share of what is running. The cost is
	// fragmentation, which is paid by whichever tier is largest.
	PlacementSpread PlacementPolicy = "spread"
)

// Or returns the policy, or pack when nothing was chosen.
//
// EMPTY MEANS PACK rather than being an error, because this is a tuning knob on
// a deployment that has more than one machine — a config written before it
// existed, or by someone with one host, should keep working and get the default.
func (p PlacementPolicy) Or() PlacementPolicy {
	if p == "" {
		return PlacementPack
	}

	return p
}

// Validate reports whether the policy is one billet implements.
//
// A TYPO MUST NOT SILENTLY BECOME THE DEFAULT. "packed" or "binpack" would
// otherwise fall through Or() to pack and look correct, and an operator who
// chose spread deliberately would never learn their fleet was doing the
// opposite.
func (p PlacementPolicy) Validate() error {
	switch p {
	case "", PlacementPack, PlacementSpread:
		return nil
	default:
		return fmt.Errorf("server.placement is %q; it must be %q or %q",
			p, PlacementPack, PlacementSpread)
	}
}
