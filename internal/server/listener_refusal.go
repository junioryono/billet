package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
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

// explained reports whether the refusal names its cause: a drain, the admission
// order, or a limit the allocator says binds or the waiters it had to leave room
// for. Under contention every decline is one of these; one with no reason, one
// whose reason could not be read, and an allocator that says no limit binds are
// not.
func (r refusal) explained() bool {
	return r.draining || r.waiter != "" ||
		(r.room != nil && r.unread == nil && (r.room.Binds() || len(r.protecting) > 0))
}

// attrs is the refusal as log attributes.
func (r refusal) attrs() []any {
	args := []any{"reason", r.String()}
	if r.waiter != "" {
		args = append(args, "held_by", r.waiter)
	}

	return args
}

// assignmentBacking is what backAssignment found for one request: the refusal
// its own purchase met, or none when backing was there or was bought, and how
// many leases and promises had been dropped when it began.
type assignmentBacking struct {
	request int64
	refused refusal
	lost    uint64
	set     bool
}

// forgetBacking removes request's running lease and promise, counting either it
// held as backing dropped: the cleanup loop releases on a goroutine of its own,
// so a promise backAssignment found can go before the assignment reads it. It
// requires l.mu.
func (l *Listener) forgetBacking(request int64) {
	_, promised := l.acquiring[request]
	_, running := l.running[request]
	if promised || running {
		l.backingLost++
	}
	delete(l.running, request)
	delete(l.runningJobs, request)
	delete(l.acquiring, request)
}

// backingLostNow is how many leases and promises have been dropped.
func (l *Listener) backingLostNow() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.backingLost
}

// backedBy records what backAssignment, begun when lost leases and promises had
// been dropped, found for request.
func (l *Listener) backedBy(request int64, r refusal, lost uint64) {
	l.mu.Lock()
	l.backed = assignmentBacking{request: request, refused: r, lost: lost, set: true}
	l.mu.Unlock()
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
	reason, unlogged := l.declineLogged, l.declinesUnlogged
	l.refused, l.refusedLogged = refusal{}, ""
	l.declineLogged, l.declinesUnlogged = "", 0
	l.mu.Unlock()

	if unlogged > 0 {
		l.log.Info("the tier is served again; assignments were declined for want of escrow "+
			"since the line that gave this reason",
			"tier", l.tier, "reason", reason, "declined_under_previous_reason", unlogged)
	}
}

// logDecline logs an assignment declined for want of escrow, explained only by
// the refusal backAssignment met for this request, and only when no lease or
// promise was dropped since it began: a refusal left from another purchase says
// nothing about why backing this one found was gone, and a lease or promise
// dropped meanwhile may have been this request's own (bought and lost, held under
// another alias, or promised while draining). It requires l.mu.
//
// AN EXPLAINED DECLINE IS CONTENTION, NOT A FAULT. The advertisement is never
// gated, so on a full fleet GitHub assigns more than billet can buy, and every
// such assignment is declined with the order's or the allocator's reason: on
// 2026-10-24 one controller logged 4,049 of these at error in eight and a half
// hours, nine in ten of them the order holding freed room for a larger waiter,
// and they were most of its errors and warnings. So an explained decline is
// said once per reason, with how many were declined under the reason before,
// and one with no explanation (a promise lost to a race) is an error every time.
func (l *Listener) logDecline(job dispatch.Job) {
	var why refusal
	if l.backed.set && l.backed.request == job.RequestID && l.backed.lost == l.backingLost {
		why = l.backed.refused
	}
	args := append([]any{"tier", l.tier, "request", job.RequestID, "run", job.RunID}, why.attrs()...)
	if !why.explained() {
		l.log.Error("assigned a request with no escrow to back it; declining it", args...)

		return
	}
	reason := why.String()
	if reason == l.declineLogged {
		l.declinesUnlogged++

		return
	}
	args = append(args, "declined_under_previous_reason", l.declinesUnlogged)
	l.declineLogged, l.declinesUnlogged = reason, 0
	l.log.Info("assigned a request with no escrow to back it; declining it, and the others "+
		"declined for this reason until it changes or the tier is served", args...)
}

// declineReason is the last refusal as log attributes. It requires l.mu.
func (l *Listener) declineReason() []any {
	return l.refused.attrs()
}
