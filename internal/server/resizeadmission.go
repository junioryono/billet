package server

import (
	"context"
)

// ResizeAdmission is the admission order's answer for a lease of tier growing
// to a larger shape than it was escrowed at, which a remote fallback asks for
// when the cloud will not sell the first (#172).
//
// ASKED AS A PURCHASE OUT OF TURN IS ASKED. The growth spends what a waiting
// tier may be accumulating, so it is judged as the pool path judges a
// purchase: admitted when the tier may buy, otherwise allowed only while each
// waiter ahead could still be granted one lease of its own (protect, which the
// allocator decides in the resize's transaction, alloc.ResizeLeaving), and
// refused outright when the order is held for a waiter that does not fit at
// all, which holder names. A refusal fails the launch as a fallback the
// budget refuses does, and its lease is released; nothing running is touched.
//
// THE QUEUE IS THE CONTROL PLANE'S, and the node plane's resize route reaches
// it only through this: internal/app binds it to the route once the scheduler
// exists. Before that no listener has run, the queue is empty, and an empty
// queue admits everything, which is what an unbound route does.
func (s *Server) ResizeAdmission(ctx context.Context, tier string) (protect []string, holder string) {
	if s == nil || !s.order.gates() || s.alloc == nil {
		return nil, ""
	}

	where := admissionOf(ctx, s.alloc, tier, s.log)
	if s.order.mayBuy(tier, where) {
		return nil, ""
	}

	if ahead := s.order.ahead(tier, where); len(ahead) > 0 {
		return ahead, ""
	}

	if waiter, _, ok := s.order.heldBy(tier, where); ok {
		return nil, waiter
	}

	// The holder moved between the two reads; the order refused all the same.
	return nil, "a waiter ahead of it"
}
