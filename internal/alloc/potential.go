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
	// CeilingBound reports that the deployment ceiling is what refuses this tier
	// one more now: its hosts could place one, with current work and other
	// tiers' outstanding floors charged, and the ceiling will not let it. Every
	// purchase anywhere spends the ceiling, so such a waiter competes with every
	// buyer; a waiter whose own hosts are full competes only with the buyers on
	// them.
	//
	// A FACT ABOUT THE WAITER, NOT THE DEPLOYMENT (#187). The flag it replaced
	// said the ceiling was one pot whenever it sat below what the live hosts
	// could hold, which held for every tier at once: a ceiling two vCPU below the
	// hosts had a tier waiting for a Mac's macOS slot hold every Linux tier on
	// another machine, though room freed on Linux could never reach it. It is
	// refreshed at every refusal, like Nodes and ShareBound, so a host-bound
	// waiter that a purchase elsewhere leaves short of ceiling is ceiling-bound
	// from its next refusal; that one purchase is what it can lose.
	CeilingBound bool
	// Target is the target this tier's share is keyed by.
	Target string
	// TargetShared reports that the target has a share, which makes it one pot
	// its tiers buy from wherever their hosts are: two tiers of it pinned to
	// different hosts still take room from each other.
	TargetShared bool
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
// its own tier's entry (AdmissionFor) when it polls, and a waiter keeps the
// view it last saw until its next refusal refreshes it. A host that joins
// between two listeners' polls is therefore in one tier's view and not the
// other's, which is why a waiter's ground is refreshed at every refusal rather
// than dated with it.
func (a *Allocator) AdmissionView(ctx context.Context) (map[string]TierAdmission, error) {
	out := make(map[string]TierAdmission, len(a.tiers))
	err := a.db.View(ctx, func(tx querier) error {
		nodes, held, err := a.admissionReads(ctx, tx)
		if err != nil {
			return err
		}

		for _, label := range a.sortedLabels() {
			if out[label], err = a.admissionIn(ctx, tx, a.tiers[label], nodes, held); err != nil {
				return err
			}
		}

		return nil
	})

	return out, err
}

// AdmissionFor is one tier's entry of AdmissionView, read alone: what a listener
// asks at a refusal, which needs its own tier and pays for no other. A tier
// this allocator was not configured with is the zero value, as the view's
// missing entry is.
func (a *Allocator) AdmissionFor(ctx context.Context, tier string) (TierAdmission, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return TierAdmission{}, nil
	}

	var out TierAdmission
	err := a.db.View(ctx, func(tx querier) error {
		nodes, held, err := a.admissionReads(ctx, tx)
		if err != nil {
			return err
		}

		out, err = a.admissionIn(ctx, tx, t, nodes, held)

		return err
	})

	return out, err
}

// admissionReads is what every tier's admission is measured against: the live
// hosts, and what each tier holds on the fleet the potential models, which is
// the same count a floor is measured against. A lease on a host that is gone
// counts for nothing there, because the modelled total does not include that
// host either; counting it would say a tier with a stranded lease cannot grow,
// and starve it. The tier's own cap is a different question, asked over every
// lease (admissionIn).
func (a *Allocator) admissionReads(ctx context.Context, tx querier) ([]nodeRow, map[string]int, error) {
	nodes, err := a.liveNodes(ctx, tx)
	if err != nil {
		return nil, nil, err
	}

	held, err := a.countOpenPerTier(ctx, tx)
	if err != nil {
		return nil, nil, err
	}

	return nodes, held, nil
}

// admissionIn is one tier's TierAdmission inside the caller's snapshot.
func (a *Allocator) admissionIn(ctx context.Context, tx querier, t config.Tier, nodes []nodeRow,
	held map[string]int,
) (TierAdmission, error) {
	p, err := a.potentialIn(ctx, tx, t.Label, nodes)
	if err != nil {
		return TierAdmission{}, err
	}

	grow := p.canGrow(held[t.Label])

	// ITS OWN CAP IS COUNTED THE WAY THE CAP IS ENFORCED: over every open lease,
	// live host or not. The fleet comparison above deliberately ignores a lease
	// stranded on a departed host, and max_concurrent does not, so a tier at its
	// cap with a stranded lease would otherwise be told it could grow and hold
	// the whole order behind a purchase the allocator refuses until that lease is
	// resolved.
	if grow && t.MaxConcurrent > 0 {
		all, err := a.countOpenByTier(ctx, tx, t.Label)
		if err != nil {
			return TierAdmission{}, err
		}

		grow = all < t.MaxConcurrent
	}

	// AND WHAT THE ALLOCATOR WOULD GRANT NOW SETTLES ANY DOUBT. The modelled
	// total packs an empty fleet greedily, so it can refuse a placement something
	// cleverer would fit; room the allocator can hand out this instant is proof
	// no model can override.
	if !grow {
		room, err := a.headroom(ctx, tx, t)
		if err != nil {
			return TierAdmission{}, err
		}

		grow = room > 0
	}

	bound, err := a.boundBy(ctx, tx, t)
	if err != nil {
		return TierAdmission{}, err
	}

	_, hasShare := a.limits.Shares[config.ShareTarget(t)]

	return TierAdmission{
		CanGrow: grow, Nodes: p.nodes, CeilingBound: bound.ceiling,
		Target: config.ShareTarget(t), TargetShared: hasShare, ShareBound: bound.share,
	}, nil
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

// potentialIn computes one tier's potential inside an open ledger view.
func (a *Allocator) potentialIn(ctx context.Context, tx querier, label string,
	nodes []nodeRow,
) (potential, error) {
	free := &fleet{
		vcpu: make(map[string]int), memory: make(map[string]config.ByteSize),
		macOS: make(map[string]int), rows: nodes,
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
	floors, err := a.reserveFloors(ctx, tx, label, free, placementCost{})
	if err != nil {
		return potential{}, err
	}

	vcpu, memory := a.limits.MaxVCPU-floors.vcpu, a.limits.MaxMemory-floors.memory

	t := a.tiers[label]

	// ONCE CURRENT WORK ENDS the whole share is free again, less what its other
	// tiers' floors keep, so the model is bounded by the share itself rather
	// than by what is left of it. Those floors were held against the share's
	// room now, which is never more than the whole share, so this errs towards
	// growth, the direction the view is meant to err in.
	target := config.ShareTarget(t)
	if share, ok := a.limits.Shares[target]; ok {
		held := floors.byTarget[target]

		if share.VCPU > 0 {
			vcpu = min(vcpu, share.VCPU-held.vcpu)
		}

		if share.Memory > 0 {
			memory = min(memory, share.Memory-held.memory)
		}
	}

	p := free.forTier(a, t)

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
