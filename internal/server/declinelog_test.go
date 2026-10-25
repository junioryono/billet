package server

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

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
		small.backed = assignmentBacking{request: request, refused: r, lost: small.backingLost, set: true}
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
// binds, a refusal met by another request's purchase, or one met before the
// heartbeat dropped a lease or a promise is the race the line
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
		{request: 6, refused: explained, lost: 1, set: true},
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

// BACKING FOUND, THEN GONE, IS NOT EXPLAINED BY AN OLDER REFUSAL. The request
// was declined for an explained reason before; this time backAssignment finds a
// lease to take it, and the heartbeat drops that lease before the assignment.
// Neither the tier's last refusal nor the request's own earlier one is why, and
// the decline is the race, an error.
func TestBackingThatVanishedIsAnError(t *testing.T) {
	t.Parallel()

	_, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	l := listeners[0]

	l.mu.Lock()
	l.refused = refusal{waiter: "b-large"}
	l.backed = assignmentBacking{request: 1, refused: l.refused, lost: l.backingLost, set: true}
	l.held = append(l.held, &alloc.Lease{ID: "found"})
	l.mu.Unlock()

	if err := l.backAssignment(t.Context(), 1); err != nil {
		t.Fatalf("back assignment: %v", err)
	}

	l.mu.Lock()
	l.held = nil
	l.mu.Unlock()

	assertDeclinedAsAnError(t, l, lines, 1)

	// BOUGHT, THEN GONE, the same: the fleet has room, so backAssignment buys.
	l.mu.Lock()
	l.backed = assignmentBacking{request: 2, refused: l.refused, lost: l.backingLost, set: true}
	l.mu.Unlock()

	if err := l.backAssignment(t.Context(), 2); err != nil {
		t.Fatalf("back assignment: %v", err)
	}

	l.mu.Lock()
	if len(l.held) == 0 {
		t.Fatal("backAssignment bought nothing in an empty fleet, so this case proves nothing")
	}
	l.held = nil
	l.mu.Unlock()

	assertDeclinedAsAnError(t, l, lines, 2)
}

// A LEASE OR PROMISE THE HEARTBEAT DROPPED MEANWHILE MAY HAVE BEEN THIS
// REQUEST'S OWN: a promise filed under another alias, a lease bought and lost, a
// promise held while draining. backAssignment meets an explained refusal (the
// fleet is full), the heartbeat then drops a promise, and the decline that
// follows is an error, not contention.
func TestADeclineAfterTheHeartbeatDroppedBackingIsAnError(t *testing.T) {
	t.Parallel()

	for _, dropped := range []string{"promise", "held"} {
		t.Run(dropped, func(t *testing.T) {
			t.Parallel()

			_, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
			l := listeners[0]

			if err := l.reconcilePool(t.Context(), 2); err != nil {
				t.Fatalf("small tier reconcile: %v", err)
			}
			if err := l.backAssignment(t.Context(), 5); err != nil {
				t.Fatalf("back assignment: %v", err)
			}

			gone := &alloc.Lease{ID: "dropped"}
			l.mu.Lock()
			if !l.backed.refused.explained() {
				t.Fatalf("a full fleet's refusal is not explained, so this test proves nothing: %v",
					l.backed.refused)
			}
			if dropped == "promise" {
				l.acquiring[77] = &promise{lease: gone, at: time.Now()}
			} else {
				l.held = append(l.held, gone)
			}
			l.mu.Unlock()

			l.heartbeatPass(t.Context())

			l.mu.Lock()
			_, kept := l.acquiring[77]
			kept = kept || slices.Contains(l.held, gone)
			l.mu.Unlock()
			if kept {
				t.Fatalf("the heartbeat kept a %s whose lease the allocator does not know", dropped)
			}

			assertDeclinedAsAnError(t, l, lines, 5)
		})
	}
}

// A PROMISE A RELEASE DROPPED MEANWHILE IS LOST BACKING TOO. A draining listener
// records its drain for a request whose promise is still there, and the cleanup
// loop, on its own goroutine, releases that promise before the assignment.
func TestADeclineAfterAReleaseDroppedThePromiseIsAnError(t *testing.T) {
	t.Parallel()

	a, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	l := listeners[0]

	lease := holdRunning(t, l, a, l.tier, 5)
	l.mu.Lock()
	delete(l.running, 5)
	l.acquiring[5] = &promise{lease: lease, at: time.Now()}
	l.cleanup[5] = &pendingCleanup{
		job: dispatch.Job{RequestID: 5}, lease: lease, outcome: alloc.PhaseDone, releaseOnly: true,
	}
	l.draining = true
	l.mu.Unlock()

	if err := l.backAssignment(t.Context(), 5); err != nil {
		t.Fatalf("back assignment: %v", err)
	}

	l.mu.Lock()
	if !l.backed.refused.draining {
		t.Fatalf("a draining listener recorded %v, so this test proves nothing", l.backed.refused)
	}
	l.mu.Unlock()

	if handled, released := l.releaseParked(t.Context(), 5); !handled || !released {
		t.Fatalf("releaseParked = %v, %v; want the parked release handled and settled", handled, released)
	}
	l.mu.Lock()
	_, promised := l.acquiring[5]
	l.mu.Unlock()
	if promised {
		t.Fatal("the settled release left its promise behind")
	}

	assertDeclinedAsAnError(t, l, lines, 5)
}

// A DROP BEFORE backAssignment BEGAN IS NOT THIS REQUEST'S: a tier that lost a
// lease earlier still says its contention at info.
func TestADropBeforeTheRequestDoesNotUnexplainIt(t *testing.T) {
	t.Parallel()

	_, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	l := listeners[0]

	if err := l.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}
	l.mu.Lock()
	l.backingLost = 3
	l.mu.Unlock()

	if err := l.backAssignment(t.Context(), 5); err != nil {
		t.Fatalf("back assignment: %v", err)
	}
	lease, needsCompute, err := l.assign(t.Context(), dispatch.Job{JobID: "J", RequestID: 5, RunID: 905})
	if err != nil || lease != nil || needsCompute {
		t.Fatalf("assign = %v, %v, %v; want a decline (nil, false, nil)", lease, needsCompute, err)
	}

	declines := lines.with(t, declineMessage)
	if len(declines) != 1 {
		t.Fatalf("the decline logged %d lines, want one: %v", len(declines), declines)
	}
	if got := stringAttr(declines[0], "level"); got != slog.LevelInfo.String() {
		t.Errorf("contention after an earlier drop was logged at %q, want INFO: %v", got, declines[0])
	}
}

// assertDeclinedAsAnError assigns request to l with no backing and asserts it is
// declined with one error line.
func assertDeclinedAsAnError(t *testing.T, l *Listener, lines *logLines, request int64) {
	t.Helper()

	before := len(lines.with(t, declineMessage))
	lease, needsCompute, err := l.assign(t.Context(), dispatch.Job{JobID: "J", RequestID: request, RunID: 900 + request})
	if err != nil || lease != nil || needsCompute {
		t.Fatalf("assign %d = %v, %v, %v; want a decline (nil, false, nil)", request, lease, needsCompute, err)
	}

	declines := lines.with(t, declineMessage)
	if len(declines) != before+1 {
		t.Fatalf("the decline of %d logged %d lines, want one: %v", request, len(declines)-before, declines)
	}
	if got := stringAttr(declines[before], "level"); got != slog.LevelError.String() {
		t.Errorf("the decline of %d was logged at %q, want ERROR: %v", request, got, declines[before])
	}
}
