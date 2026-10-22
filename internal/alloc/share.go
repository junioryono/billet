package alloc

import (
	"context"
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// unboundedVCPU and unboundedMemory are the room a dimension no share bounds.
const (
	unboundedVCPU   = int(^uint(0) >> 1)
	unboundedMemory = config.ByteSize(1<<63 - 1)
)

// shareUsage is what each target's tiers hold, read once per question.
type shareUsage struct {
	byTarget map[string]placementCost
	// unattributed is held by leases whose tier left the catalogue. Which target
	// they belonged to cannot be told, so every share is charged for them until
	// they end: a share that forgot them would sell their room again.
	unattributed placementCost
}

func (a *Allocator) readShareUsage(ctx context.Context, tx querier) (shareUsage, error) {
	out := shareUsage{byTarget: map[string]placementCost{}}
	if len(a.limits.Shares) == 0 {
		return out, nil
	}

	rows, err := state.ReadQueries(tx).UsageByTier(ctx)
	if err != nil {
		return shareUsage{}, fmt.Errorf("alloc: measure the targets' shares: %w", err)
	}

	for _, row := range rows {
		used := placementCost{vcpu: int(row.Vcpu), memory: config.ByteSize(row.Memory)}

		owner, known := a.tiers[row.Tier]
		if !known {
			out.unattributed.vcpu += used.vcpu
			out.unattributed.memory += used.memory

			continue
		}

		target := config.ShareTarget(owner)
		sum := out.byTarget[target]
		sum.vcpu += used.vcpu
		sum.memory += used.memory
		out.byTarget[target] = sum
	}

	return out, nil
}

// room is what a target may still take under its share, with returned handed
// back first (a resize is authorised as if the lease's current shape had been
// returned). A dimension the share leaves unbounded, and a target with no share,
// answer unbounded.
func (a *Allocator) room(u shareUsage, target string, returned placementCost) placementCost {
	share, ok := a.limits.Shares[target]
	if !ok {
		return placementCost{vcpu: unboundedVCPU, memory: unboundedMemory}
	}

	used := u.byTarget[target]
	out := placementCost{vcpu: unboundedVCPU, memory: unboundedMemory}

	if share.VCPU > 0 {
		out.vcpu = share.VCPU - used.vcpu - u.unattributed.vcpu + returned.vcpu
	}

	if share.Memory > 0 {
		out.memory = share.Memory - used.memory - u.unattributed.memory + returned.memory
	}

	return out
}

// roomForTier is what a tier's target may still take once its other tiers'
// outstanding floors are kept. A tier that left the catalogue gets the least
// any share has left: its lease is charged against every share, so every
// share must still hold it.
func (a *Allocator) roomForTier(u shareUsage, label string, returned placementCost,
	floors floorCharge,
) placementCost {
	after := func(target string) placementCost {
		r := a.room(u, target, returned)
		if _, ok := a.limits.Shares[target]; ok {
			f := floors.byTarget[target]
			r.vcpu, r.memory = r.vcpu-f.vcpu, r.memory-f.memory
		}

		return r
	}

	if t, ok := a.tiers[label]; ok {
		return after(config.ShareTarget(t))
	}

	out := placementCost{vcpu: unboundedVCPU, memory: unboundedMemory}

	for _, target := range a.shareTargets() {
		r := after(target)
		out.vcpu, out.memory = min(out.vcpu, r.vcpu), min(out.memory, r.memory)
	}

	return out
}

// shareRoomFor reads the ledger and answers roomForTier.
func (a *Allocator) shareRoomFor(ctx context.Context, tx querier, label string,
	returned placementCost, floors floorCharge,
) (placementCost, error) {
	usage, err := a.readShareUsage(ctx, tx)
	if err != nil {
		return placementCost{}, err
	}

	return a.roomForTier(usage, label, returned, floors), nil
}

// shareTargets is every target with a share, in a fixed order.
func (a *Allocator) shareTargets() []string {
	out := make([]string, 0, len(a.limits.Shares))
	for target := range a.limits.Shares {
		out = append(out, target)
	}

	slices.Sort(out)

	return out
}

// anyChargeFits reports whether one lease of this tier, charged as it would be
// on some host it could be placed on, fits room whole. Each candidate's shape is
// tested as a vector: the least vCPU of one shape and the least memory of
// another describe a shape nobody sells. With no candidate host, the tier
// request is what is tested.
func anyChargeFits(p *placer, t config.Tier, room placementCost) bool {
	tested := false

	for _, n := range p.order {
		c, ok := p.cost[n.name]
		if !ok {
			continue
		}

		tested = true

		if c.vcpu <= room.vcpu && c.memory <= room.memory {
			return true
		}
	}

	return !tested && t.VCPU <= room.vcpu && t.Memory <= room.memory
}

// bound is what stops a tier buying one more now, where that is a pot other
// tiers buy from rather than the tier's own hosts.
type bound struct {
	// share is the tier's own target share, measured at the shape a lease of it
	// is charged on the hosts it could use: a remote tier is charged the shape
	// it buys, not its request.
	share bool
	// ceiling is the deployment ceiling: the tier's hosts could place one more
	// now, current work and other tiers' outstanding floors charged, and the
	// ceiling refuses it.
	ceiling bool
}

// boundBy measures both pots from one placer, so the admission view walks the
// fleet once per tier for them. The share and the ceiling are judged apart: the
// ceiling's question narrows by the deployment's room alone, never by the share,
// so a tier can be bound by either, both or neither.
func (a *Allocator) boundBy(ctx context.Context, tx querier, t config.Tier) (bound, error) {
	p, floors, err := a.placerWithFloors(ctx, tx, t)
	if err != nil {
		return bound{}, err
	}

	var out bound

	if _, ok := a.limits.Shares[config.ShareTarget(t)]; ok {
		room, err := a.shareRoomFor(ctx, tx, t.Label, placementCost{}, floors)
		if err != nil {
			return bound{}, err
		}

		out.share = !anyChargeFits(p, t, room)
	}

	out.ceiling, err = a.ceilingRefuses(ctx, tx, t, p, floors)
	if err != nil {
		return bound{}, err
	}

	return out, nil
}

// ceilingRefuses reports whether the deployment ceiling, and not the tier's
// hosts or its own cap, is what refuses one more of it now. It spends p's
// deployment room, so p is not reused after it.
//
// ASKED OF THE TIER, NOW, WITH CURRENT WORK CHARGED. A waiter whose hosts have
// room the ceiling will not let it use is waiting for ceiling, which every
// purchase anywhere spends; a waiter whose hosts are full is waiting for room on
// those hosts, which only a tier sharing one can take. The deployment-wide fact
// this replaced (is the ceiling smaller than the hosts?) said the first of
// every tier as soon as the ceiling sat a couple of vCPU below the hosts, and a
// tier waiting for a Mac's macOS slot held every Linux tier on another machine
// (#187). Nor is it the per-tier test rejected before that, which asked whether
// the ceiling cut the tier's total on an empty fleet: that ignores what is
// running, and said no for a large tier whose host was empty while a small tier
// on another host spent the whole ceiling.
func (a *Allocator) ceilingRefuses(ctx context.Context, tx querier, t config.Tier, p *placer,
	floors floorCharge,
) (bool, error) {
	if t.MaxConcurrent > 0 {
		open, err := a.countOpenByTier(ctx, tx, t.Label)
		if err != nil {
			return false, err
		}

		if open >= t.MaxConcurrent {
			return false, nil
		}
	}

	// THE HOSTS FIRST, while the placer still carries no deployment room.
	if p.total(t) == 0 {
		return false, nil
	}

	used, err := a.usage(ctx, tx)
	if err != nil {
		return false, err
	}

	// THE CEILING AS HEADROOM CHARGES IT (headroomWithPlacer), including what a
	// lease stranded off the fleet holds: usage counts it, and no live host can
	// use what it holds.
	p.deploymentVCPU = max(a.limits.MaxVCPU-used.VCPU-floors.vcpu, 0)
	p.deploymentMemory = max(a.limits.MaxMemory-used.Memory-floors.memory, 0)

	return p.total(t) == 0, nil
}
