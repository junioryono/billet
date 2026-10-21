package app

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/server"
)

// resizeGate answers the node wire's resize route, asking the control plane's
// admission order before a remote fallback grows a lease to a larger shape
// (#172).
//
// THE WIRE IS SERVED BEFORE THE SCHEDULER EXISTS, and the admission order is
// the scheduler's, so the order is bound here by Schedule once server.New has
// built it. Until then no listener has run and the queue it would have read is
// empty, which admits everything, so the store's own Resize is the same
// answer.
type resizeGate struct {
	alloc *alloc.Allocator
	order atomic.Pointer[server.Server]
}

// resize is the route's nodeplane.Resizer.
func (g *resizeGate) resize(ctx context.Context, leaseID string, epoch int64, instanceType string,
	vcpu int, memory config.ByteSize,
) error {
	order := g.order.Load()
	if order == nil {
		return g.alloc.Resize(ctx, leaseID, epoch, instanceType, vcpu, memory)
	}

	lease, err := g.alloc.Lease(ctx, leaseID)
	if err != nil {
		return err
	}

	protect, holder := order.ResizeAdmission(ctx, lease.Tier)
	if holder != "" {
		return fmt.Errorf("%w: fallback %q for lease %s would take room the admission order holds for %s",
			alloc.ErrNoCapacity, instanceType, leaseID, holder)
	}

	return g.alloc.ResizeLeaving(ctx, leaseID, epoch, instanceType, vcpu, memory, protect)
}
