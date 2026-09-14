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
// explicit floors. A tier with no eligible host must not freeze arbitration;
// a large tier whose host is merely busy must keep its turn while room returns.
func (a *Allocator) PotentialCapacity(ctx context.Context) (map[string]int, error) {
	out := make(map[string]int, len(a.tiers))
	err := a.db.View(ctx, func(tx querier) error {
		nodes, err := a.liveNodes(ctx, tx)
		if err != nil {
			return err
		}
		labels := make([]string, 0, len(a.tiers))
		for label := range a.tiers {
			labels = append(labels, label)
		}
		slices.Sort(labels)

		for _, label := range labels {
			free := &fleet{
				vcpu: make(map[string]int), memory: make(map[string]config.ByteSize),
				macOS: make(map[string]int),
			}
			for _, n := range nodes {
				free.vcpu[n.name], free.memory[n.name] = n.vcpu, n.memory
				free.macOS[n.name] = a.macOSLimit(n.name)
			}
			vcpu, memory := a.limits.MaxVCPU, a.limits.MaxMemory
			for _, other := range labels {
				floor := a.tiers[other]
				if other == label || floor.Reserved == 0 {
					continue
				}
				cost, err := a.holdFloor(ctx, tx, floor, floor.Reserved, free)
				if err != nil {
					return err
				}
				vcpu -= cost.vcpu
				memory -= cost.memory
			}
			t := a.tiers[label]
			p, err := free.forTier(ctx, tx, a, t)
			if err != nil {
				return err
			}
			p.deploymentVCPU, p.deploymentMemory = max(vcpu, 0), max(memory, 0)
			out[label] = p.total(t)
			if t.MaxConcurrent > 0 {
				out[label] = min(out[label], t.MaxConcurrent)
			}
			// CURRENT PLACEMENT CAN BE BETTER THAN AN EMPTY-FLEET FLOOR PACK.
			// A floor already backed on another host must not hide room the real
			// allocator can grant now merely because the hypothetical pack cannot.
			if out[label] == 0 {
				room, err := a.headroom(ctx, tx, t)
				if err != nil {
					return err
				}
				out[label] = room
			}
		}

		return nil
	})

	return out, err
}
