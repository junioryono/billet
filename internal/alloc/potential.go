package alloc

import (
	"context"
	"slices"

	"github.com/junioryono/billet/internal/config"
)

// PotentialCapacity reports what each tier could place after current work ends.
//
// THIS IS ELIGIBILITY FOR WAITING, NEVER AN ADVERTISEMENT. It ignores execution
// leases but keeps host policy, remote shape charges, deployment ceilings and
// explicit floors.
func (a *Allocator) PotentialCapacity(ctx context.Context) (map[string]int, error) {
	out := make(map[string]int, len(a.tiers))
	err := a.db.View(ctx, func(tx querier) error {
		nodes, err := a.liveNodes(ctx, tx)
		if err != nil {
			return err
		}

		for _, label := range a.sortedLabels() {
			p, err := a.potentialIn(ctx, tx, label, nodes)
			if err != nil {
				return err
			}

			out[label] = p.value()
		}

		return nil
	})

	return out, err
}

// TierAdmission is what the admission order needs to know about one tier.
type TierAdmission struct {
	// CanGrow reports whether room could ever come for this tier: whether, once
	// current work ends, it could hold more than it holds now. False for a tier at
	// its own max_concurrent, pinned to a host that is gone, or with a shape no
	// live host fits. Such a tier may still have demand, but freed room elsewhere
	// will never let it buy, so it must never hold other tiers back.
	//
	// IT ERRS TOWARDS TRUE WHERE THE MODEL IS UNCERTAIN, because a tier wrongly
	// excluded is starved, which is what the order exists to prevent, while one
	// wrongly included merely waits its turn: the allocator's atomic escrow, not
	// this, decides what may be bought. So a model that says no is re-asked as
	// headroom, which is what the allocator would grant this instant.
	CanGrow bool
	// Nodes are the hosts an empty fleet could place this tier on, by name: not
	// every live host, only those whose size, provider, policy and macOS slots
	// admit this tier's shape. Two tiers compete for host room only where these
	// overlap.
	Nodes []string
	// CeilingShared reports that the deployment's own vCPU or memory ceiling is
	// smaller than what the live hosts could hold, which makes it one pot every
	// tier buys from. It is a fact about the deployment rather than about this
	// tier, and it is carried per tier because a waiter is judged by the view it
	// last saw.
	//
	// A PER-TIER TEST WAS NOT ENOUGH. Asking whether the ceiling cut THIS tier's
	// own total said no for two tiers that each fit under it alone — an 8 vCPU
	// tier on one host, a 4 vCPU tier on another, an 8 vCPU ceiling — while the
	// two of them together could not, so a stream of the small one starved the
	// large one on a fleet where they shared no host.
	CeilingShared bool
	// Target is the target this tier's share is keyed by.
	Target string
	// ShareBound reports that this tier's own target share, not the fleet, is
	// what stops it buying one more now. Room another target's jobs free can
	// never reach it until its own target's work ends, so it must not hold that
	// target's tiers back: that is the starvation a share exists to prevent.
	ShareBound bool
}

// AdmissionView reads every tier's TierAdmission in one ledger snapshot, so one
// reader's answer for one tier is consistent with the fleet the others were
// measured on.
//
// THE ORDER STILL COMPARES VIEWS TAKEN AT DIFFERENT TIMES: each listener reads
// this for its own tier when it polls, and a waiter keeps the view it last saw
// until its next refusal refreshes it. A host that joins between two listeners'
// polls is therefore in one tier's view and not the other's, which is why a
// waiter's ground is refreshed at every refusal rather than dated with it.
func (a *Allocator) AdmissionView(ctx context.Context) (map[string]TierAdmission, error) {
	out := make(map[string]TierAdmission, len(a.tiers))
	err := a.db.View(ctx, func(tx querier) error {
		nodes, err := a.liveNodes(ctx, tx)
		if err != nil {
			return err
		}

		// WHAT EACH TIER HOLDS ON THE FLEET THE POTENTIAL MODELS, read once: the
		// same count a floor is measured against, where a lease on a host that is
		// gone counts for nothing, because the modelled total does not include that
		// host either. Counting it here would say a tier with a stranded lease
		// cannot grow, and starve it. The tier's own cap is a different question,
		// asked below over every lease.
		held, err := a.countOpenPerTier(ctx, tx)
		if err != nil {
			return err
		}

		shared, err := a.ceilingSharedOver(ctx, tx, nodes)
		if err != nil {
			return err
		}

		for _, label := range a.sortedLabels() {
			t := a.tiers[label]

			p, err := a.potentialIn(ctx, tx, label, nodes)
			if err != nil {
				return err
			}

			grow := p.canGrow(held[label])

			// ITS OWN CAP IS COUNTED THE WAY THE CAP IS ENFORCED: over every open
			// lease, live host or not. The fleet comparison above deliberately
			// ignores a lease stranded on a departed host, and max_concurrent does
			// not, so a tier at its cap with a stranded lease would otherwise be
			// told it could grow and hold the whole order behind a purchase the
			// allocator refuses until that lease is resolved.
			if grow && t.MaxConcurrent > 0 {
				all, err := a.countOpenByTier(ctx, tx, label)
				if err != nil {
					return err
				}

				grow = all < t.MaxConcurrent
			}

			// AND WHAT THE ALLOCATOR WOULD GRANT NOW SETTLES ANY DOUBT. The
			// modelled total packs an empty fleet greedily, so it can refuse a
			// placement something cleverer would fit; room the allocator can hand
			// out this instant is proof no model can override.
			if !grow {
				room, err := a.headroom(ctx, tx, t)
				if err != nil {
					return err
				}

				grow = room > 0
			}

			bound, err := a.shareBound(ctx, tx, t)
			if err != nil {
				return err
			}

			out[label] = TierAdmission{
				CanGrow: grow, Nodes: p.nodes, CeilingShared: shared,
				Target: config.ShareTarget(t), ShareBound: bound,
			}
		}

		return nil
	})

	return out, err
}

// potential is one tier's capacity once current work ends.
type potential struct {
	// total is what an empty fleet could hold of this tier, capped by its
	// max_concurrent.
	total int
	// additional is the room the allocator could grant now, read only when total
	// is zero: a floor already backed on another host can leave the empty-fleet
	// pack with nothing while the real allocator still has room.
	additional int
	// nodes are the hosts an empty fleet could place this tier on: those whose
	// size, provider, policy and macOS slots admit its shape, which is narrower
	// than the hosts the placement policy would consider.
	nodes []string
}

// value is the single number PotentialCapacity has always reported.
func (p potential) value() int {
	if p.total == 0 {
		return p.additional
	}

	return p.total
}

// canGrow compares like with like: a TOTAL against what the tier already holds,
// and ADDITIONAL room against nothing, because it is already net of it.
func (p potential) canGrow(open int) bool {
	if p.total == 0 {
		return p.additional > 0
	}

	return p.total > open
}

func (a *Allocator) sortedLabels() []string {
	labels := make([]string, 0, len(a.tiers))
	for label := range a.tiers {
		labels = append(labels, label)
	}

	slices.Sort(labels)

	return labels
}

// ceilingSharedOver reports whether the deployment's own ceiling is smaller than
// what these hosts could hold, which is what makes it one pot every tier buys
// from rather than a number each tier meets on its own.
//
// WHAT IS CHARGED OFF THE FLEET IS SPENT, NOT FREE. A lease on a host the fleet
// no longer contains, or one aimed at no host at all, holds its shape against
// the ceiling until something proves its compute gone, and nothing on a live
// host can use what it holds. Measured against the ceiling's face value, a
// deployment whose ceiling exactly matched its hosts reported the ceiling as
// nobody's constraint while a stranded charge was making it everybody's, and a
// tier waiting on one host could not stop another replenishing a second.
func (a *Allocator) ceilingSharedOver(ctx context.Context, tx querier, nodes []nodeRow) (bool, error) {
	spent, err := a.usage(ctx, tx)
	if err != nil {
		return false, err
	}

	byNode, err := a.usageByNode(ctx, tx)
	if err != nil {
		return false, err
	}

	var (
		vcpu     int
		memory   config.ByteSize
		onFleet  int
		onFleetM config.ByteSize
	)
	for _, n := range nodes {
		vcpu += n.vcpu
		memory += n.memory

		used := byNode[n.name]
		onFleet += used.VCPU
		onFleetM += used.Memory
	}

	elsewhere, elsewhereM := max(spent.VCPU-onFleet, 0), max(spent.Memory-onFleetM, 0)

	return (a.limits.MaxVCPU > 0 && a.limits.MaxVCPU-elsewhere < vcpu) ||
		(a.limits.MaxMemory > 0 && a.limits.MaxMemory-elsewhereM < memory), nil
}

// potentialIn computes one tier's potential inside an open ledger view.
func (a *Allocator) potentialIn(ctx context.Context, tx querier, label string,
	nodes []nodeRow,
) (potential, error) {
	free := &fleet{
		vcpu: make(map[string]int), memory: make(map[string]config.ByteSize),
		macOS: make(map[string]int),
	}
	for _, n := range nodes {
		free.vcpu[n.name], free.memory[n.name] = n.vcpu, n.memory
		free.macOS[n.name] = a.macOSLimit(n.name)
	}

	// FLOORS ARE CHARGED HERE THE WAY THE ALLOCATOR CHARGES THEM, through the one
	// function that does it. This loop used to hold every configured floor in
	// full, including one already backed on another host, so a tier whose only
	// host the model then re-packed that floor onto was reported unable to grow
	// and never joined the waiting order at all — the starvation this view exists
	// to prevent. reserveFloors holds only what is outstanding.
	owedVCPU, owedMemory, err := a.reserveFloors(ctx, tx, label, free)
	if err != nil {
		return potential{}, err
	}

	vcpu, memory := a.limits.MaxVCPU-owedVCPU, a.limits.MaxMemory-owedMemory

	t := a.tiers[label]

	// ONCE CURRENT WORK ENDS the whole share is free again, so the model is
	// bounded by the share itself rather than by what is left of it.
	if share, ok := a.limits.Shares[config.ShareTarget(t)]; ok {
		if share.VCPU > 0 {
			vcpu = min(vcpu, share.VCPU)
		}

		if share.Memory > 0 {
			memory = min(memory, share.Memory)
		}
	}

	p, err := free.forTier(ctx, tx, a, t)
	if err != nil {
		return potential{}, err
	}

	// THE HOSTS THAT COULD EVER HOLD ONE, measured while the placer still carries
	// no deployment ceiling: a host is this tier's only if an empty fleet could
	// place the tier's shape there. eligibleNodes answers a wider question (which
	// hosts the policy would consider at all), and a waiter that claimed a host
	// too small for it would block tiers that host can really serve.
	hosts := make([]string, 0, len(p.order))
	for _, n := range p.order {
		if p.roomFor(n.name, t) > 0 {
			hosts = append(hosts, n.name)
		}
	}

	p.deploymentVCPU, p.deploymentMemory = max(vcpu, 0), max(memory, 0)

	out := potential{total: p.total(t), nodes: hosts}

	if t.MaxConcurrent > 0 {
		out.total = min(out.total, t.MaxConcurrent)
	}

	// CURRENT PLACEMENT CAN BE BETTER THAN AN EMPTY-FLEET PACK, which is greedy
	// and deliberately conservative: an outstanding floor it puts on the one host
	// this tier fits must not hide room the real allocator can grant now.
	if out.total == 0 {
		room, err := a.headroom(ctx, tx, t)
		if err != nil {
			return potential{}, err
		}

		out.additional = room
	}

	return out, nil
}
