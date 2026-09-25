package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A RUNNER LOOKUP CUT SHORT BY THE SHUTDOWN IS THE SHUTDOWN, not a response
// billet cannot act on. job_identity wraps every failed ledger read as
// ErrUntrustworthySession, so a SIGTERM landing inside one used to make Run
// return without draining (TestADrainEndsWhenOnlyIdlePoolSlotsRemain failed that
// way twice on 2026-09-25: "Run returned before the drain began").
func TestCancelledWhileServingTellsTheShutdownFromAnUntrustworthyResponse(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	lookup := fmt.Errorf("%w: cannot resolve runner %q: %w",
		ErrUntrustworthySession, "billet-1", context.Canceled)
	response := fmt.Errorf("%w: runner %q resolves outside tier %q",
		ErrUntrustworthySession, "billet-1", "a")

	cases := []struct {
		name     string
		ctx      context.Context
		draining bool
		err      error
		want     bool
	}{
		{"a lookup the cancellation cut short", cancelled, false, lookup, true},
		{"a response billet cannot act on", cancelled, false, response, false},
		{"a domain error that does not wrap the cancellation", cancelled, false, errors.New("assign failed"), true},
		{"a deadline inside an untrustworthy wrap", cancelled, false,
			fmt.Errorf("%w: %w", ErrUntrustworthySession, context.DeadlineExceeded), true},
		{"not stopping", t.Context(), false, lookup, false},
		{"already draining", cancelled, true, lookup, false},
	}

	for _, c := range cases {
		if got := cancelledWhileServing(c.ctx, c.draining, c.err); got != c.want {
			t.Errorf("%s: cancelledWhileServing = %v, want %v", c.name, got, c.want)
		}
	}
}
