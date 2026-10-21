package alloc

import (
	"context"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// RoomRefusal is why the allocator would grant a tier nothing now, as each
// limit stands at one instant. Diagnostic only: nothing decides from it, and a
// purchase is still the escrow's atomic decision, so by the time it is read the
// room may have moved either way.
//
// IT NAMES EVERY LIMIT THAT BINDS, not the first, because they are not
// alternatives: a tier at its share can be at the ceiling too, and an operator
// who raised one would find the other.
type RoomRefusal struct {
	// Sealed is admission refusing new work. An admission state that cannot be
	// read counts as sealed here, as it does at the purchase.
	Sealed bool
	// AtCap is the tier's max_concurrent when its open leases have reached it,
	// and zero otherwise.
	AtCap int
	// ShareBound and CeilingBound are the target's share and the deployment
	// ceiling, judged as the admission view judges them.
	ShareBound, CeilingBound bool
	// HostsFull names the live hosts the tier's shape fits on when none of them
	// has room for one more now, other tiers' outstanding floors charged.
	HostsFull []string
	// NoHost is a tier whose shape no live host can place at all.
	NoHost bool
	// Budgets is the deployment ceiling and the target's share together
	// leaving no charged shape that fits, though each alone would.
	Budgets bool
}

// Binds reports whether any limit is named.
func (r RoomRefusal) Binds() bool {
	return r.Sealed || r.AtCap > 0 || r.ShareBound || r.CeilingBound || len(r.HostsFull) > 0 ||
		r.NoHost || r.Budgets
}

// String names every limit that binds, or says none does.
func (r RoomRefusal) String() string {
	var parts []string

	if r.Sealed {
		parts = append(parts, "admission is sealed")
	}

	if r.AtCap > 0 {
		parts = append(parts, fmt.Sprintf("the tier holds its max_concurrent of %d", r.AtCap))
	}

	if r.ShareBound {
		parts = append(parts, "its target's share is spent")
	}

	if r.CeilingBound {
		parts = append(parts, "the deployment ceiling (server.max_vcpu, max_memory) is spent")
	}

	if r.NoHost {
		parts = append(parts, "no live host can place its shape")
	}

	if len(r.HostsFull) > 0 {
		parts = append(parts, "no room on its hosts ("+strings.Join(r.HostsFull, ", ")+")")
	}

	if r.Budgets {
		parts = append(parts, "the deployment ceiling and its target's share together leave no shape that fits")
	}

	if len(parts) == 0 {
		return "no limit binds now; room freed after the purchase was refused"
	}

	return strings.Join(parts, "; ")
}

// WhyNoRoom reads, in one snapshot, every limit that stops a tier buying one
// more now.
func (a *Allocator) WhyNoRoom(ctx context.Context, tier string) (RoomRefusal, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return RoomRefusal{}, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	var out RoomRefusal

	err := a.db.View(ctx, func(tx querier) error {
		admission, err := state.ReadAdmission(ctx, tx)
		if err != nil {
			return fmt.Errorf("alloc: read admission: %w", err)
		}

		out.Sealed = admission.Sealed()

		if t.MaxConcurrent > 0 {
			open, err := a.countOpenByTier(ctx, tx, t.Label)
			if err != nil {
				return err
			}

			if open >= t.MaxConcurrent {
				out.AtCap = t.MaxConcurrent
			}
		}

		b, err := a.boundBy(ctx, tx, t)
		if err != nil {
			return err
		}

		out.ShareBound, out.CeilingBound = b.share, b.ceiling

		p, _, err := a.placerWithFloors(ctx, tx, t)
		if err != nil {
			return err
		}

		// THE HOSTS AN EMPTY ONE OF COULD PLACE IT, not every host with a charge:
		// eligibleNodes keeps a host-backed host smaller than the shape, and one
		// of those reported as full would read as contention that ends rather
		// than a shape that never fits there.
		var fits []string

		for _, n := range p.order {
			cost, ok := p.cost[n.name]
			if !ok || cost.vcpu > n.vcpu || cost.memory > n.memory {
				continue
			}

			if t.GuestOS == config.GuestMacOS && a.macOSLimit(n.name) <= 0 {
				continue
			}

			fits = append(fits, n.name)
		}

		switch {
		case len(fits) == 0:
			out.NoHost = true
		case p.total(t) == 0:
			out.HostsFull = fits
		}

		// AND THE TWO POTS TOGETHER. The share and the ceiling are each judged
		// alone, as the admission order needs them, so one can fit a shape the
		// other does not and both read as free while together they fit none.
		if !out.Binds() {
			room, err := a.headroom(ctx, tx, t)
			if err != nil {
				return err
			}

			out.Budgets = room == 0
		}

		return nil
	})

	return out, err
}
