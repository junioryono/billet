package alloc

import (
	"fmt"
	"math"
)

// AdvertisedCeiling is the most instances of a tier's shape this deployment
// could run at once, which is what its scale set tells GitHub it can produce.
//
// CONFIGURATION ONLY: the tier's max_concurrent, and the deployment ceiling
// divided by the shape. It reads no node and no open lease, so it is the same
// number on every poll and only a configuration change moves it. That is the
// point. GitHub.com assigns work only to a scale set that is advertising, and
// tells one advertising zero nothing at all (#140), so a number that follows
// live headroom goes to zero exactly when work is waiting and hides that work
// from billet. PotentialCapacity is not a substitute: it reads live nodes and
// can fall back to current headroom.
//
// AN ADVERTISEMENT, NOT A RESERVATION. Nothing is held against it. Every
// guarantee — the ceiling, per-node fit, floors, max_concurrent, macOS slots —
// is enforced when a lease is bought, atomically, in Escrow. A job GitHub
// assigns beyond what can run now waits, assigned, until a lease can be
// bought for it.
//
// A remote provider may charge a larger instance shape than the tier's own,
// and a macOS host licenses only a few guests; both are left to that purchase
// rather than second-guessed here, because over-advertising costs a job a
// wait, while under-advertising costs it being seen at all.
func (a *Allocator) AdvertisedCeiling(tier string) (int, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	ceiling := math.MaxInt32
	if t.MaxConcurrent > 0 {
		ceiling = min(ceiling, t.MaxConcurrent)
	}

	if a.limits.MaxVCPU > 0 && t.VCPU > 0 {
		ceiling = min(ceiling, a.limits.MaxVCPU/t.VCPU)
	}

	if a.limits.MaxMemory > 0 && t.Memory > 0 {
		ceiling = min(ceiling, int(a.limits.MaxMemory/t.Memory))
	}

	return ceiling, nil
}
