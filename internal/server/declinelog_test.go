package server

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

const servedMessage = "the tier is served again; assignments were declined for want of escrow"

// AN EXPLAINED DECLINE IS SAID ONCE PER REASON, at info, not once per request:
// on a full fleet every assignment past what billet can buy is declined for the
// same reason, and a line each buried a controller's real errors (4,049 in eight
// and a half hours on 2026-10-24). The line after a change of reason says how
// many were declined under the last, and so does the line that says the tier
// was served.
func TestAnExplainedDeclineIsSaidOncePerReason(t *testing.T) {
	t.Parallel()

	_, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	small := listeners[0]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}
	for i, job := range []string{"J-1", "J-2", "J-3"} {
		if err := small.handle(t.Context(), directlyAssigned(int64(i+1), job, 3+i)); err != nil {
			t.Fatalf("small tier handle %s: %v", job, err)
		}
	}

	declines := lines.with(t, declineMessage)
	if len(declines) != 1 {
		t.Fatalf("three declines for one reason logged %d lines, want one: %v", len(declines), declines)
	}
	if got := stringAttr(declines[0], "level"); got != slog.LevelInfo.String() {
		t.Errorf("an explained decline was logged at %q, want INFO", got)
	}

	decline := func(request int64, r refusal) {
		small.mu.Lock()
		small.backed = assignmentBacking{request: request, refused: r, set: true}
		small.logDecline(dispatch.Job{JobID: "J", RequestID: request})
		small.mu.Unlock()
	}

	// A NEW REASON IS SAID, with the two declined unsaid under the last.
	decline(4, refusal{waiter: "b-large"})
	declines = lines.with(t, declineMessage)
	if len(declines) != 2 {
		t.Fatalf("a change of reason logged %d lines in all, want two: %v", len(declines), declines)
	}
	if got, ok := declines[1]["declined_under_previous_reason"].(float64); !ok || got != 2 {
		t.Errorf("the line after the change counts %v declined under the last reason, want 2: %v",
			declines[1]["declined_under_previous_reason"], declines[1])
	}

	// SERVED, THE TIER SAYS HOW MANY IT DID NOT SAY, and the next decline again,
	// even for the same reason.
	decline(5, refusal{waiter: "b-large"})
	if served := lines.with(t, servedMessage); len(served) != 0 {
		t.Fatalf("a tier not yet served logged %v", served)
	}
	small.refusalServed()
	served := lines.with(t, servedMessage)
	if len(served) != 1 {
		t.Fatalf("serving the tier logged %d summaries, want one: %v", len(served), served)
	}
	if got, ok := served[0]["declined_under_previous_reason"].(float64); !ok || got != 1 {
		t.Errorf("the summary counts %v declined unsaid, want 1: %v",
			served[0]["declined_under_previous_reason"], served[0])
	}
	decline(6, refusal{waiter: "b-large"})
	if declines = lines.with(t, declineMessage); len(declines) != 3 {
		t.Errorf("a decline after the tier was served logged %d lines in all, want three", len(declines))
	}
	small.refusalServed()
	if served = lines.with(t, servedMessage); len(served) != 1 {
		t.Errorf("serving a tier with nothing unsaid logged %d summaries in all, want one", len(served))
	}
}

// A DECLINE NOBODY CAN EXPLAIN IS AN ERROR EVERY TIME: no reason recorded for
// this request, one that could not be read, an allocator that says no limit
// binds, or a refusal met by another request's purchase is the race the line
// exists for (a promise lost, a lease dropped under an acquisition), never
// contention.
func TestAnUnexplainedDeclineIsAnErrorEveryTime(t *testing.T) {
	t.Parallel()

	_, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	l := listeners[0]

	explained := refusal{waiter: "b-large"}
	backings := []assignmentBacking{
		{},
		{request: 2, set: true},
		{request: 3, refused: refusal{unread: errors.New("the ledger did not answer")}, set: true},
		{request: 4, refused: refusal{room: &alloc.RoomRefusal{}}, set: true},
		{request: 99, refused: explained, set: true},
	}
	for i, b := range backings {
		l.mu.Lock()
		l.refused = explained
		l.backed = b
		l.logDecline(dispatch.Job{JobID: "J", RequestID: int64(i + 1)})
		l.mu.Unlock()
	}

	declines := lines.with(t, declineMessage)
	if len(declines) != len(backings) {
		t.Fatalf("%d unexplained declines logged %d lines: %v", len(backings), len(declines), declines)
	}
	for _, d := range declines {
		if got := stringAttr(d, "level"); got != slog.LevelError.String() {
			t.Errorf("an unexplained decline was logged at %q, want ERROR: %v", got, d)
		}
	}
}

// BACKING FOUND, THEN GONE, IS NOT EXPLAINED BY AN OLDER REFUSAL. backAssignment
// finds a lease to take the request, or buys one, and the heartbeat drops it
// before the assignment: the tier's last refusal belongs to another purchase,
// and the decline is the race, an error.
func TestBackingThatVanishedIsAnError(t *testing.T) {
	t.Parallel()

	_, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	l := listeners[0]

	vanish := func(request int64, hold bool) {
		l.mu.Lock()
		l.refused = refusal{waiter: "b-large"}
		if hold {
			l.held = append(l.held, &alloc.Lease{ID: "found"})
		}
		l.mu.Unlock()

		if err := l.backAssignment(t.Context(), request); err != nil {
			t.Fatalf("back assignment %d: %v", request, err)
		}

		l.mu.Lock()
		if len(l.held) == 0 {
			t.Errorf("request %d: backAssignment found or bought no lease", request)
		}
		l.held = nil
		l.logDecline(dispatch.Job{JobID: "J", RequestID: request})
		l.mu.Unlock()
	}
	vanish(1, true)
	vanish(2, false)

	declines := lines.with(t, declineMessage)
	if len(declines) != 2 {
		t.Fatalf("two declines logged %d lines: %v", len(declines), declines)
	}
	for _, d := range declines {
		if got := stringAttr(d, "level"); got != slog.LevelError.String() {
			t.Errorf("a decline whose backing vanished was logged at %q, want ERROR: %v", got, d)
		}
	}
}
