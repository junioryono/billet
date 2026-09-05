package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// heldSessions refuses the first n attempts the way GitHub does, then answers.
//
// EMBEDS THE PACKAGE'S PROVISIONER FAKE so only the one method under test is
// written here. Restating the rest would make this file break every time the
// interface grows, for no assertion it makes.
type heldSessions struct {
	fakeProvisioner

	refusals int64
	attempts atomic.Int64
}

func (h *heldSessions) Session(context.Context, int, string) (Session, error) {
	if h.attempts.Add(1) <= h.refusals {
		// The shape the real API answers with, wrapped in the sentinel the client
		// puts on it. Measured against a real organization: `409 Conflict ...
		// RunnerScaleSetSessionConflictException ... already has an active session`.
		return nil, fmt.Errorf("%w: scale set 1: 409 Conflict", ErrSessionHeld)
	}

	return nil, errors.New("no session is needed past this point in this test")
}

// A RESTART WAITS FOR AN ABANDONED SESSION RATHER THAN FAILING TO START.
//
// MEASURED, NOT ASSUMED: a live conformance run against a real organization
// showed that GitHub does not let a successor displace a message session an
// abandoned control plane left behind — it answers 409 and keeps the old one
// until it expires. Returning that error, which is what this did, meant a control
// plane killed and restarted (every upgrade, every crash, every `systemctl
// restart`) failed to start, took its tier's listener with it, and said nothing
// an operator could act on.
//
// A JOB IS NOT LOST WHILE IT WAITS: GitHub queues one for 24 hours when no runner
// is available, and compute already running is held by its node and re-adopted.
// What the wait costs is scheduling latency, which is the trade ADR-001 already
// makes by choosing recovery in minutes over HA.
func TestARestartWaitsForAnAbandonedSessionRatherThanFailing(t *testing.T) {
	original := sessionRetryFor
	sessionRetryFor = time.Millisecond

	t.Cleanup(func() { sessionRetryFor = original })

	held := &heldSessions{refusals: 3}
	s := &Server{prov: held, log: slog.New(slog.DiscardHandler), owner: "billet"}

	// DRIVEN THROUGH runTier, WHICH IS THE BRANCH THAT HAS TO DO THE WAITING.
	//
	// Calling openSession here instead would assert that the waiting works and
	// nothing about whether anything waits: putting `s.prov.Session` back in
	// runTier — which is exactly the line this change replaced — would leave such a
	// test green while every restart went back to failing. runTier returns before
	// building a listener, because the fake's answer after the refusals is an
	// error, so this reaches the real call site without needing a live listener.
	err := s.runTier(t.Context(), &config.Tier{Label: "linux"}, &ScaleSet{ID: 1}, s.prov)

	// It gets past the refusals: the error it ends on is the one AFTER them, not
	// the conflict.
	if errors.Is(err, ErrSessionHeld) {
		t.Fatalf("it gave up on a held session rather than waiting: %v", err)
	}

	if got := held.attempts.Load(); got != held.refusals+1 {
		t.Errorf("it tried %d times against %d refusals, so it is not retrying",
			got, held.refusals)
	}
}

// AND A REASON THAT IS NOT A HELD SESSION IS REPORTED AT ONCE.
//
// Everything else that stops a session opening is a reason to say so and stop;
// only this one resolves by itself. Retrying the rest would turn a
// misconfiguration into a control plane that never starts and never explains.
func TestASessionThatFailsForAnotherReasonIsNotRetried(t *testing.T) {
	held := &heldSessions{refusals: 0}
	s := &Server{prov: held, log: slog.New(slog.DiscardHandler), owner: "billet"}

	_, err := s.openSession(t.Context(), &config.Tier{Label: "linux"}, &ScaleSet{ID: 1}, s.prov)
	if err == nil {
		t.Fatal("a failure to open a session was reported as success")
	}

	if got := held.attempts.Load(); got != 1 {
		t.Errorf("it tried %d times for a reason that will not resolve itself", got)
	}
}

// AND A CANCELLED CONTEXT ENDS THE WAIT.
//
// The wait is unbounded by design — nothing here knows when GitHub will expire
// the other session — so the only thing that ends it is the deployment stopping.
func TestTheWaitForASessionEndsWhenTheDeploymentStops(t *testing.T) {
	original := sessionRetryFor
	sessionRetryFor = time.Hour

	t.Cleanup(func() { sessionRetryFor = original })

	held := &heldSessions{refusals: 1_000_000}
	s := &Server{prov: held, log: slog.New(slog.DiscardHandler), owner: "billet"}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		_, err := s.openSession(ctx, &config.Tier{Label: "linux"}, &ScaleSet{ID: 1}, s.prov)
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("stopping the deployment ended the wait with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stopping the deployment did not end the wait")
	}
}
