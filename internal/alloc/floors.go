package alloc

import (
	"context"
	"slices"

	"github.com/junioryono/billet/internal/config"
)

// reserveFloors takes the room other tiers' floors still need off the machines,
// before anybody else is offered them.
//
// A FLOOR IS A PROMISE ABOUT MACHINES, NOT ABOUT A TOTAL. `reserved: 2` says
// "always keep room for two of this tier", which one deployment-wide pool could
// honour: if the fleet had the vCPU, the fleet could run them. It stops being a
// guarantee once room lives on particular machines — two hosts with room for one
// each satisfy a floor of two in aggregate and on neither, so another tier takes
// one slot on each, the arithmetic is content, and the reserved tier can no longer
// place a single instance.
//
// GREEDY, AND THEREFORE CONSERVATIVE. Each floor is packed onto the hosts the
// ordering prefers, which is not always what a perfect packer would find. The error
// only runs one way: this may refuse a placement something cleverer could have
// fitted, and will never hand out room a floor was counting on. Refusing work is
// recoverable — the job waits — while breaking a floor is not.
//
// A FLOOR NOTHING CAN KEEP MUST NOT FREEZE THE FLEET. A tier reserved for more than
// the machines could ever hold is a configuration mistake, and reading it literally
// would refuse every other tier forever. Billet holds back what it can and lets the
// rest compete.
func (a *Allocator) reserveFloors(
	ctx context.Context, tx querier, forTier string, free *fleet,
) (int, config.ByteSize, error) {
	open, err := a.countOpenPerTier(ctx, tx)
	if err != nil {
		return 0, 0, err
	}

	var (
		heldVCPU   int
		heldMemory config.ByteSize
	)

	// IN A FIXED ORDER, because these floors compete with each other for the same
	// hosts and Go map iteration is randomised. Without it one fleet answers
	// differently on different runs.
	labels := make([]string, 0, len(a.tiers))
	for label := range a.tiers {
		labels = append(labels, label)
	}

	slices.Sort(labels)

	for _, label := range labels {
		// A tier never holds capacity back from itself: its own floor is not an
		// obstacle to filling it.
		if label == forTier {
			continue
		}

		t := a.tiers[label]

		missing := t.Reserved - open[label]
		if t.Reserved == 0 || missing <= 0 {
			continue
		}

		kept, err := a.holdFloor(ctx, tx, t, missing, free)
		if err != nil {
			return 0, 0, err
		}

		// ONLY WHAT WAS ACTUALLY KEPT counts against the deployment ceiling. The
		// old arithmetic deducted every configured floor from that ceiling without
		// asking whether a machine could serve it, so a reservation on a tier with
		// no suitable host anywhere took the ceiling away from tiers that were
		// perfectly placeable — a Tart floor on a fleet of Docker boxes left an
		// entirely healthy deployment advertising nothing.
		heldVCPU += kept * t.VCPU
		heldMemory += config.ByteSize(kept) * t.Memory
	}

	return heldVCPU, heldMemory, nil
}

// holdFloor takes one tier's outstanding reservation off the machines it could
// run on.
//
// The candidate set is that tier's, not the caller's — a floor on a macOS tier
// is kept on the Mac, and holding it anywhere else would reserve room that tier
// could never use. Only the hosts they SHARE are actually affected, which is
// exactly the contention a floor is meant to survive.
func (a *Allocator) holdFloor(
	ctx context.Context, tx querier, t config.Tier, missing int, free *fleet,
) (int, error) {
	// ITS OWN CANDIDATES, SPENDING THE SHARED FLEET. A floor on a macOS tier is
	// kept on the Mac; holding it against whichever machines the ASKING tier
	// happens to use is wrong twice — it denies them room to protect a
	// reservation that could never be kept there, and leaves the only host that
	// matters untouched.
	held, err := free.forTier(ctx, tx, a, t)
	if err != nil {
		return 0, err
	}

	kept := 0

	for range missing {
		if _, ok := held.next(t); !ok {
			// The fleet cannot keep the rest of this floor. Stopping here is what
			// keeps an impossible reservation from freezing everyone else.
			break
		}

		kept++
	}

	return kept, nil
}
