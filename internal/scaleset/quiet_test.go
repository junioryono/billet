package scaleset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// AN ORDINARY SHUTDOWN MUST NOT LOG AT ERROR.
//
// The vendored client hands its retryable HTTP transport billet's own logger,
// and that transport reports every failed request at Error level. A shutdown
// cancels the long poll in flight, so the last thing an operator sees on a
// perfectly clean `systemctl restart` is:
//
//	level=ERROR msg="request failed" error="context canceled" method=GET url=...
//
// Once billet ships as a unit file that is the FIRST line in journalctl after
// every restart, and an error that appears every single time is an error people
// learn to scroll past. The next one that matters scrolls past with it.
//
// The rule keys on the ERROR VALUE, not the message. A cancellation is always
// billet's own doing and so is never something an operator can act on. Matching
// the string "request failed" instead would rot the first time upstream reworded
// it, and would say nothing about why the record was safe to demote.
func TestACancelledRequestIsNotLoggedAsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want slog.Level
	}{
		{"a cancelled request", context.Canceled, slog.LevelDebug},
		{"a wrapped cancellation", fmt.Errorf("get message: %w", context.Canceled), slog.LevelDebug},
		// A DEADLINE IS NOT A CANCELLATION. A request that ran out of time during
		// ordinary operation means GitHub is slow or unreachable, which is exactly
		// what an operator needs to see. An earlier version demoted these too.
		{"a deadline that expired", context.DeadlineExceeded, slog.LevelError},
		// A REAL FAILURE JOINED WITH A CANCELLATION IS STILL A REAL FAILURE.
		// errors.Is is satisfied by any branch, so a rule written as a bare
		// errors.Is would demote this and hide the half that matters.
		{"a failure joined with a cancellation",
			errors.Join(errors.New("401 Bad credentials"), context.Canceled), slog.LevelError},
		{"cancellations joined together",
			errors.Join(context.Canceled, fmt.Errorf("poll: %w", context.Canceled)), slog.LevelDebug},
		// EVERYTHING ELSE IS UNTOUCHED. A demotion that swallowed real failures
		// would be a worse bug than the noise it was fixing: a 401 from GitHub
		// during a shutdown is exactly the thing an operator has to see.
		{"a real failure", errors.New("401 Bad credentials"), slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer

			log := slog.New(quieten(slog.NewTextHandler(&buf,
				&slog.HandlerOptions{Level: slog.LevelDebug})))

			log.Error("request failed", "error", tc.err, "method", "GET")

			got := buf.String()
			if !strings.Contains(got, "level="+tc.want.String()) {
				t.Errorf("logged %q, want level=%s", strings.TrimSpace(got), tc.want)
			}

			// The record still says what happened, whichever level it came out at.
			if !strings.Contains(got, "request failed") {
				t.Errorf("the message was lost: %q", strings.TrimSpace(got))
			}
		})
	}
}

// A record with no error attribute is not a cancellation and must not be
// demoted. Guessing from the absence of evidence is how a demotion starts
// hiding things nobody meant it to.
func TestARecordWithNoErrorIsLeftAlone(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(quieten(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug})))

	log.Error("something went wrong", "url", "https://example.invalid")

	if got := buf.String(); !strings.Contains(got, "level=ERROR") {
		t.Errorf("logged %q, want level=ERROR", strings.TrimSpace(got))
	}
}

// The demotion must survive the wrappers slog puts between a logger and its
// handler, or it works in a test and not in the client, which attaches
// attributes and groups.
func TestTheDemotionSurvivesWithAttrs(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(quieten(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))).With("component", "listener")

	log.Error("request failed", "error", context.Canceled)

	if got := buf.String(); !strings.Contains(got, "level=DEBUG") {
		t.Errorf("logged %q, want level=DEBUG", strings.TrimSpace(got))
	}
}

// NOT TESTED HERE: that New actually passes the quietened logger to the vendored
// client. That link is one argument — gh.WithLogger(quiet) — and the vendored
// client exposes no way to read back the logger it was given, so any test of it
// would have to assert on a field added for the test. The behaviour above is
// what carries the risk; the argument is visible in review and covered by the
// end-to-end check that a restart logs no ERROR.
