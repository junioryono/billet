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

// shareBound reports whether a tier's own target share, rather than the fleet,
// is what stops it buying one more now, measured at the shape a lease of it is
// charged on the hosts it could use: a remote tier is charged the shape it
// buys, not its request.
func (a *Allocator) shareBound(ctx context.Context, tx querier, t config.Tier) (bool, error) {
	if _, ok := a.limits.Shares[config.ShareTarget(t)]; !ok {
		return false, nil
	}

	p, floors, err := a.placerWithFloors(ctx, tx, t)
	if err != nil {
		return false, err
	}

	room, err := a.shareRoomFor(ctx, tx, t.Label, placementCost{}, floors)
	if err != nil {
		return false, err
	}

	return !anyChargeFits(p, t, room), nil
}
