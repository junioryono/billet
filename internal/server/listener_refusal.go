package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// refusal is why a purchase this listener asked for bought nothing.
//
// SAID BECAUSE IT COULD NOT BE TOLD OTHERWISE (#248). On 2026-09-25 a tier
// declined 224 assignments in thirty minutes with no reason on any line, and
// two causes fitted: the admission order holding freed room for another tier,
// or the allocator refusing the purchase. A starved tier and a full fleet read
// the same. Each refusal now names the one it was.
type refusal struct {
	// draining is a listener that buys nothing while it drains.
	draining bool
	// waiter is the tier the admission order holds the line for, and since is
	// when it began waiting.
	waiter string
	since  time.Time
	// room is the allocator's own reason, read after it granted nothing, and
	// protecting the waiters ahead a purchase out of turn had to leave room for.
	room       *alloc.RoomRefusal
	protecting []string
	// unread is why the allocator's reason could not be read, said rather than
	// taken for no reason at all.
	unread error
}

// byOrder reports whether the admission order, not the allocator, refused.
func (r refusal) byOrder() bool {
	return r.waiter != "" || (r.room != nil && !r.room.Binds() && len(r.protecting) > 0)
}

func (r refusal) String() string {
	switch {
	case r.draining:
		return "this listener is draining and buys nothing"
	case r.waiter != "":
		return fmt.Sprintf("the admission order holds freed room for %s, waiting since %s",
			r.waiter, r.since.UTC().Format(time.RFC3339))
	case r.unread != nil:
		return fmt.Sprintf("the allocator granted nothing, and why could not be read: %v", r.unread)
	case r.room != nil && !r.room.Binds() && len(r.protecting) > 0:
		return "buying ahead of its turn would leave " + strings.Join(r.protecting, ", ") +
			" without room for one lease"
	case r.room != nil:
		return "the allocator has no room: " + r.room.String()
	default:
		return "no reason was recorded"
	}
}

// orderRefusal names the waiter the admission order holds the line for ahead
// of this tier at where.
func (l *Listener) orderRefusal(where alloc.TierAdmission) refusal {
	waiter, since, ok := l.order.heldBy(l.tier, where)
	if !ok {
		// The holder moved between the refusal and this read: the order refused
		// all the same, and an empty name would read as the allocator's.
		return refusal{waiter: "a waiter that has since been served or moved"}
	}

	return refusal{waiter: waiter, since: since}
}

// roomRefusal asks the allocator why a purchase it was asked for granted
// nothing, protect being the waiters ahead it was asked to leave room for.
func (l *Listener) roomRefusal(ctx context.Context, protect []string) refusal {
	if l.isDraining() {
		return refusal{draining: true}
	}

	room, err := l.alloc.WhyNoRoom(ctx, l.tier)
	if err != nil {
		return refusal{unread: err}
	}

	return refusal{room: &room, protecting: protect}
}

// refuse records why a purchase bought nothing, for the decline that follows
// it and for the status report. announce logs it, once per change of reason
// rather than on every poll, for the pool path, whose refusal no decline line
// carries.
func (l *Listener) refuse(r refusal, announce bool) {
	reason := r.String()

	l.mu.Lock()
	l.refused = r
	changed := reason != l.refusedLogged
	if announce {
		l.refusedLogged = reason
	}
	l.mu.Unlock()

	if !announce || !changed {
		return
	}

	args := []any{"tier", l.tier, "reason", reason}
	if r.waiter != "" {
		args = append(args, "held_by", r.waiter)
	}

	l.log.Info("assigned work is waiting: a purchase for it bought nothing", args...)
}

// served forgets the last refusal once this tier's demand is met, so the next
// one is said again.
func (l *Listener) refusalServed() {
	l.mu.Lock()
	l.refused, l.refusedLogged = refusal{}, ""
	l.mu.Unlock()
}

// declineReason is the last refusal as log attributes. It requires l.mu.
func (l *Listener) declineReason() []any {
	args := []any{"reason", l.refused.String()}
	if l.refused.waiter != "" {
		args = append(args, "held_by", l.refused.waiter)
	}

	return args
}
