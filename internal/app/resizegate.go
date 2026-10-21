package app

import (
	"context"
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
	bound atomic.Pointer[boundOrder]
}

// resizeOrder is what the gate asks: the waiters a lease of tier must leave
// room for (server.Server.ResizeAdmission).
type resizeOrder interface {
	ResizeAdmission(ctx context.Context, tier string) []string
}

var _ resizeOrder = (*server.Server)(nil)

// boundOrder carries the order the gate was bound to.
type boundOrder struct{ order resizeOrder }

// bind makes every later resize ask order.
func (g *resizeGate) bind(order resizeOrder) { g.bound.Store(&boundOrder{order: order}) }

// resize is the route's nodeplane.Resizer.
func (g *resizeGate) resize(ctx context.Context, leaseID string, epoch int64, instanceType string,
	vcpu int, memory config.ByteSize,
) error {
	bound := g.bound.Load()
	if bound == nil {
		return g.alloc.Resize(ctx, leaseID, epoch, instanceType, vcpu, memory)
	}

	return g.resizeAsking(ctx, bound.order, leaseID, epoch, instanceType, vcpu, memory)
}

// resizeAsking grows a lease leaving room for the waiters order names for its
// tier. The order is read before the allocator's transaction opens, never
// inside it: a ledger writer calls out to nothing.
func (g *resizeGate) resizeAsking(ctx context.Context, order resizeOrder, leaseID string, epoch int64,
	instanceType string, vcpu int, memory config.ByteSize,
) error {
	lease, err := g.alloc.Lease(ctx, leaseID)
	if err != nil {
		return err
	}

	protect := order.ResizeAdmission(ctx, lease.Tier)

	return g.alloc.ResizeLeaving(ctx, leaseID, epoch, instanceType, vcpu, memory, protect)
}
