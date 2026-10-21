package server

import (
	"context"
)

// ResizeAdmission names the waiters a lease of tier must leave room for when it
// grows to a larger shape than it was escrowed at, which a remote fallback asks
// for when the cloud will not sell the first (#172).
//
// ASKED AS A PURCHASE OUT OF TURN IS ASKED. The growth spends what a waiting
// tier may be accumulating, so it is judged as the pool path judges a
// purchase: nothing to protect when the tier may buy, otherwise every waiter
// ahead of it, each of which must still be granted one lease once the new
// shape is charged. That last judgement is the allocator's, in the resize's
// own transaction (alloc.ResizeLeaving), which also decides whether the shape
// grows at all, so this refuses nothing itself: a shrink or the same shape
// again takes nothing from anyone and is never refused here.
//
// THE QUEUE IS THE CONTROL PLANE'S, and the node plane's resize route reaches
// it only through this: internal/app binds it to the route once the scheduler
// exists. Before that no listener has run, the queue is empty, and an empty
// queue admits everything, which is what an unbound route does.
func (s *Server) ResizeAdmission(ctx context.Context, tier string) []string {
	if s == nil || !s.order.gates() || s.alloc == nil {
		return nil
	}

	where := admissionOf(ctx, s.alloc, tier, s.log)
	if s.order.mayBuy(tier, where) {
		return nil
	}

	// AHEAD MAY BE EMPTY by the time it is read, the waiter served or stalled
	// since mayBuy; then nothing holds the line and the growth is the
	// allocator's alone, as a purchase's would be.
	return s.order.ahead(tier, where)
}
