package alloc

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// unboundedVCPU and unboundedMemory are the room a dimension no share bounds.
const (
	unboundedVCPU   = int(^uint(0) >> 1)
	unboundedMemory = config.ByteSize(1<<63 - 1)
)

// shareRoom is what a tier's target may still take under its share: the share
// less what that target's tiers hold, with returnedVCPU and returnedMemory
// handed back first (a resize is authorised as if the lease's current shape had
// been returned). A dimension the share leaves unbounded, and a target with no
// share at all, answer unbounded.
//
// A TIER'S TARGET IS CONFIGURATION, so the ledger is read per tier and summed
// here. A lease whose tier is no longer in the catalogue belongs to no target
// and counts against none; the deployment ceiling still charges it.
func (a *Allocator) shareRoom(ctx context.Context, tx querier, t config.Tier,
	returnedVCPU int, returnedMemory config.ByteSize,
) (int, config.ByteSize, error) {
	target := config.ShareTarget(t)

	share, ok := a.limits.Shares[target]
	if !ok {
		return unboundedVCPU, unboundedMemory, nil
	}

	rows, err := state.ReadQueries(tx).UsageByTier(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("alloc: measure target %q's share: %w", target, err)
	}

	usedVCPU, usedMemory := -returnedVCPU, -returnedMemory

	for _, row := range rows {
		owner, known := a.tiers[row.Tier]
		if !known || config.ShareTarget(owner) != target {
			continue
		}

		usedVCPU += int(row.Vcpu)
		usedMemory += config.ByteSize(row.Memory)
	}

	vcpu, memory := unboundedVCPU, unboundedMemory
	if share.VCPU > 0 {
		vcpu = share.VCPU - usedVCPU
	}

	if share.Memory > 0 {
		memory = share.Memory - usedMemory
	}

	return vcpu, memory, nil
}

// shareBound reports whether a tier's own target share, rather than the fleet,
// is what stops it buying one more at its requested shape.
func (a *Allocator) shareBound(ctx context.Context, tx querier, t config.Tier) (bool, error) {
	vcpu, memory, err := a.shareRoom(ctx, tx, t, 0, 0)
	if err != nil {
		return false, err
	}

	return vcpu < t.VCPU || memory < t.Memory, nil
}
