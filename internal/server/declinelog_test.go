package server

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

// AN EXPLAINED DECLINE IS SAID ONCE PER REASON, at info, not once per request:
// on a full fleet every assignment past what billet can buy is declined for the
// same reason, and a line each buried a controller's real errors (4,049 in eight
// and a half hours on 2026-10-24). The line after a change of reason says how
// many were declined under the last.
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

	// A NEW REASON IS SAID, with the two declined unsaid under the last.
	small.mu.Lock()
	small.refused = refusal{waiter: "b-large"}
	small.logDecline(dispatch.Job{JobID: "J-4", RequestID: 4})
	small.mu.Unlock()

	declines = lines.with(t, declineMessage)
	if len(declines) != 2 {
		t.Fatalf("a change of reason logged %d lines in all, want two: %v", len(declines), declines)
	}
	if got, ok := declines[1]["declined_under_previous_reason"].(float64); !ok || got != 2 {
		t.Errorf("the line after the change counts %v declined under the last reason, want 2: %v",
			declines[1]["declined_under_previous_reason"], declines[1])
	}

	// SERVED, THE TIER SAYS THE NEXT ONE AGAIN, even for the same reason.
	small.refusalServed()
	small.mu.Lock()
	small.refused = refusal{waiter: "b-large"}
	small.logDecline(dispatch.Job{JobID: "J-5", RequestID: 5})
	small.mu.Unlock()
	if declines = lines.with(t, declineMessage); len(declines) != 3 {
		t.Errorf("a decline after the tier was served logged %d lines in all, want three", len(declines))
	}
}

// A DECLINE NOBODY CAN EXPLAIN IS AN ERROR EVERY TIME: no reason recorded, or one
// that could not be read, is the race the line exists for (a promise lost, a
// lease dropped under an acquisition), never contention.
func TestAnUnexplainedDeclineIsAnErrorEveryTime(t *testing.T) {
	t.Parallel()

	_, lines, listeners := loggedListeners(t, contenders(), 8, config.AdmissionFill)
	l := listeners[0]

	for i, r := range []refusal{{}, {}, {unread: errors.New("the ledger did not answer")}} {
		l.mu.Lock()
		l.refused = r
		l.logDecline(dispatch.Job{JobID: "J", RequestID: int64(i + 1)})
		l.mu.Unlock()
	}

	declines := lines.with(t, declineMessage)
	if len(declines) != 3 {
		t.Fatalf("three unexplained declines logged %d lines, want three: %v", len(declines), declines)
	}
	for _, d := range declines {
		if got := stringAttr(d, "level"); got != slog.LevelError.String() {
			t.Errorf("an unexplained decline was logged at %q, want ERROR: %v", got, d)
		}
	}
}
