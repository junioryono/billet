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
	// ceiling, each leaving no shape the hosts with room could take. Not the
	// admission view's flags of the same names, which judge a waiter for the
	// order; these say which limit raising would help.
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

		p, floors, err := a.placerWithFloors(ctx, tx, t)
		if err != nil {
			return err
		}

		// THE HOSTS AN EMPTY ONE OF COULD PLACE IT, not every host with a charge:
		// eligibleNodes keeps a host-backed host smaller than the shape, and one
		// of those reported as full would read as contention that ends rather
		// than a shape that never fits there.
		var fits []string

		for _, n := range p.order {
			if a.placesOnEmpty(n, t) {
				fits = append(fits, n.name)
			}
		}

		switch {
		case len(fits) == 0:
			out.NoHost = true

			return nil
		case p.total(t) == 0:
			out.HostsFull = fits

			return nil
		}

		// THE POTS, JUDGED ON THE HOSTS THAT HAVE ROOM, at the shapes those
		// hosts charge, so a pot is named only where raising it would help: a
		// share that fits a shape only a full host sells, or a ceiling that fits
		// one only a host with no room offers, binds as surely as one with no
		// room at all. These are the diagnostic's own measure, not the admission
		// order's flags, which judge the waiter for whom it holds back.
		return a.whichPotRefuses(ctx, tx, t, p, floors, &out)
	})

	return out, err
}

// placesOnEmpty reports whether an empty one of host n could place one lease of
// t: the shape it charges there fits the host's size, and a macOS guest has a
// licence slot there.
func (a *Allocator) placesOnEmpty(n nodeRow, t config.Tier) bool {
	cost, ok := n.cost(t)
	if !ok || cost.vcpu > n.vcpu || cost.memory > n.memory {
		return false
	}

	return t.GuestOS != config.GuestMacOS || a.macOSLimit(n.name) > 0
}

// ShapeHosts is, for every tier, the live hosts an empty one of could place a
// lease of it on, by name: where its provider, the host's policy, the host's
// size and a macOS licence slot all admit its shape. A tier with none waits
// whatever is freed, which nothing else reports until its jobs queue (#140).
// Whether a host is too old to read the tier's cache block is not asked here;
// billet status reports that wait on its own.
func (a *Allocator) ShapeHosts(ctx context.Context) (map[string][]string, error) {
	out := make(map[string][]string, len(a.tiers))

	err := a.db.View(ctx, func(tx querier) error {
		for _, label := range a.sortedLabels() {
			t := a.tiers[label]

			nodes, err := a.eligibleNodesFor(ctx, tx, t, false)
			if err != nil {
				return err
			}

			hosts := []string{}

			for _, n := range nodes {
				if a.placesOnEmpty(n, t) {
					hosts = append(hosts, n.name)
				}
			}

			out[label] = hosts
		}

		return nil
	})

	return out, err
}

// whichPotRefuses sets which of the target's share and the deployment ceiling
// leave no shape the hosts with room could take, alone or only together.
func (a *Allocator) whichPotRefuses(ctx context.Context, tx querier, t config.Tier, p *placer,
	floors floorCharge, out *RoomRefusal,
) error {
	used, err := a.usage(ctx, tx)
	if err != nil {
		return err
	}

	ceiling := placementCost{
		vcpu:   a.limits.MaxVCPU - used.VCPU - floors.vcpu,
		memory: a.limits.MaxMemory - used.Memory - floors.memory,
	}

	share := placementCost{vcpu: unboundedVCPU, memory: unboundedMemory}
	if _, ok := a.limits.Shares[config.ShareTarget(t)]; ok {
		share, err = a.shareRoomFor(ctx, tx, t.Label, placementCost{}, floors)
		if err != nil {
			return err
		}
	}

	within := func(room placementCost) int {
		c := p.clone()
		c.deploymentVCPU, c.deploymentMemory = max(room.vcpu, 0), max(room.memory, 0)

		return c.total(t)
	}

	shareAlone, ceilingAlone := within(share), within(ceiling)
	out.ShareBound = shareAlone == 0
	out.CeilingBound = ceilingAlone == 0

	// AND THE TWO TOGETHER: each can fit a shape the other does not, and both
	// read as free while together they fit none.
	both := placementCost{vcpu: min(share.vcpu, ceiling.vcpu), memory: min(share.memory, ceiling.memory)}
	out.Budgets = shareAlone > 0 && ceilingAlone > 0 && within(both) == 0

	return nil
}
