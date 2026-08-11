package alloc

import (
	"context"
	"database/sql"
	"slices"

	"github.com/junioryono/billet/internal/config"
)

// reserveFloors takes the room other tiers' floors still need off the machines,
// before anybody else is offered them.
//
// A FLOOR IS A PROMISE ABOUT MACHINES, NOT ABOUT A TOTAL. `reserved: 2` says
// "always keep room for two of this tier", and deducting it from one
// deployment-wide pool was a real guarantee while capacity WAS one pool: if the
// fleet had the vCPU, the fleet could run them.
//
// It stops being a guarantee once room lives on particular machines. Two hosts
// with room for one each satisfy a floor of two in aggregate and satisfy it on
// neither — another tier takes one slot on each, the arithmetic is content, and
// the reserved tier can no longer place a single instance. Nothing
// overcommitted, nothing errored, and the promise has simply evaporated.
//
// GREEDY, AND THEREFORE CONSERVATIVE. Each floor is packed onto the hosts the
// ordering prefers, which is not always the arrangement a perfect packer would
// find. The error only ever runs one way: this may refuse a placement something
// cleverer could have fitted, and it will never hand out room a floor was
// counting on. Refusing work is recoverable — the job waits — while breaking a
// floor is not, because by the time anyone notices, the capacity is gone.
//
// A FLOOR NOTHING CAN KEEP MUST NOT FREEZE THE FLEET. A tier reserved for more
// than the machines could ever hold is a configuration mistake, and reading it
// literally would refuse every other tier forever while waiting for room that
// cannot exist. Billet holds back what it can and lets the rest compete.
func (a *Allocator) reserveFloors(
	ctx context.Context, tx *sql.Tx, forTier string, free *fleet,
) error {
	held, err := a.countOpenPerTier(ctx, tx)
	if err != nil {
		return err
	}

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

		missing := t.Reserved - held[label]
		if t.Reserved == 0 || missing <= 0 {
			continue
		}

		if err := a.holdFloor(ctx, tx, t, missing, free); err != nil {
			return err
		}
	}

	return nil
}

// holdFloor takes one tier's outstanding reservation off the machines it could
// run on.
//
// The candidate set is that tier's, not the caller's — a floor on a macOS tier
// is kept on the Mac, and holding it anywhere else would reserve room that tier
// could never use. Only the hosts they SHARE are actually affected, which is
// exactly the contention a floor is meant to survive.
func (a *Allocator) holdFloor(
	ctx context.Context, tx *sql.Tx, t config.Tier, missing int, free *fleet,
) error {
	// ITS OWN CANDIDATES, SPENDING THE SHARED FLEET. A floor on a macOS tier is
	// kept on the Mac; holding it against whichever machines the ASKING tier
	// happens to use is wrong twice — it denies them room to protect a
	// reservation that could never be kept there, and leaves the only host that
	// matters untouched.
	held, err := free.forTier(ctx, tx, a, t)
	if err != nil {
		return err
	}

	for range missing {
		if _, ok := held.next(t); !ok {
			// The fleet cannot keep the rest of this floor. Stopping here is what
			// keeps an impossible reservation from freezing everyone else.
			return nil
		}
	}

	return nil
}
