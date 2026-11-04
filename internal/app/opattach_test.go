package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeplane"
)

// attachLedger is a ledger holding one lease, recording what ended a hold.
type attachLedger struct {
	lease *alloc.Lease
	err   error
	ended []string
}

func (l *attachLedger) Job(context.Context, string) (alloc.JobRecord, error) {
	return alloc.JobRecord{}, alloc.ErrLeaseNotFound
}

func (l *attachLedger) Lease(context.Context, string) (*alloc.Lease, error) {
	if l.err != nil {
		return nil, l.err
	}

	return l.lease, nil
}

func (l *attachLedger) EndLeaseHold(_ context.Context, leaseID, by string) (bool, error) {
	l.ended = append(l.ended, leaseID+" by "+by)

	return true, nil
}

// EVERYTHING THAT CAN BE REFUSED IS REFUSED BEFORE A NODE IS ASKED, in a status, so the operator's
// command has a reason to print. A tier that did not say is refused whatever the lease's state, and
// could-not-tell is never no such job.
func TestAnAttachIsRefusedBeforeANodeIsAsked(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Tiers: []config.Tier{
		{Label: "open", Debug: &config.TierDebug{Attach: true}},
		{Label: "closed"},
	}}
	plane := nodeplane.New(slog.New(slog.DiscardHandler), "deployment", time.Minute)

	for name, tc := range map[string]struct {
		ledger *attachLedger
		status int
	}{
		"no such job": {&attachLedger{err: alloc.ErrLeaseNotFound}, http.StatusNotFound},
		"the ledger did not answer": {&attachLedger{err: errors.New("busy")},
			http.StatusServiceUnavailable},
		"a tier that did not say": {&attachLedger{lease: &alloc.Lease{Tier: "closed", Node: "n1",
			Phase: alloc.PhaseBusy}}, http.StatusForbidden},
		"a tier this config has not": {&attachLedger{lease: &alloc.Lease{Tier: "gone", Node: "n1",
			Phase: alloc.PhaseBusy}}, http.StatusForbidden},
		"a VM being torn down": {&attachLedger{lease: &alloc.Lease{Tier: "open", Node: "n1",
			Phase: alloc.PhaseTeardown}}, http.StatusConflict},
		"no node yet": {&attachLedger{lease: &alloc.Lease{Tier: "open", Phase: alloc.PhaseBusy}},
			http.StatusConflict},
		"a node not registered here": {&attachLedger{lease: &alloc.Lease{Tier: "open", Node: "n1",
			Phase: alloc.PhaseBusy}}, http.StatusServiceUnavailable},
		// PAST THE PHASE CHECK, so it reaches the node: a node that restarted holds a VM this way.
		"a VM in custody": {&attachLedger{lease: &alloc.Lease{Tier: "open", Node: "n1",
			Phase: alloc.PhaseCustody}}, http.StatusServiceUnavailable},
	} {
		s := &operatorServer{jobs: tc.ledger, plane: plane, log: slog.New(slog.DiscardHandler)}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, operatorAttachPath+"?lease=l1", nil)
		rec := httptest.NewRecorder()

		s.attach(t.Context(), cfg, rec, req)

		if rec.Code != tc.status {
			t.Errorf("%s: answered %d (%s), want %s", name, rec.Code, strings.TrimSpace(rec.Body.String()),
				strconv.Itoa(tc.status))
		}
	}
}

// A LEASE AS A LAUNCH LEAVES IT CAN BE ATTACHED TO: nothing in production advances a running job's
// lease past launching, so a check written from the phases' names would refuse every job there is.
// On a real allocator, moved the way a node moves it. It reaches for the node, which is not there.
func TestALeaseAsALaunchLeavesItCanBeAttachedTo(t *testing.T) {
	t.Parallel()

	a, leaseID := jobOn(t)

	lease, err := a.Lease(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if err := a.Advance(t.Context(), leaseID, lease.Epoch, alloc.PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	cfg := &config.Config{Tiers: []config.Tier{{Label: "t", Debug: &config.TierDebug{Attach: true}}}}
	s := &operatorServer{jobs: a, plane: nodeplane.New(slog.New(slog.DiscardHandler), "deployment", time.Minute),
		log: slog.New(slog.DiscardHandler)}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, operatorAttachPath+"?lease="+leaseID, nil)
	rec := httptest.NewRecorder()

	s.attach(t.Context(), cfg, rec, req)

	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not registered") {
		t.Errorf("answered %d: %s; want the node asked for, not the job refused", rec.Code, rec.Body.String())
	}
}

// A CONNECTION GOING IS NOT SOMEBODY FINISHING. Only a shell that exited ends a hold, and only once
// the last session has closed; an operator's dropped link leaves the VM they were looking at to its
// bound, and so does a controller stopping.
func TestOnlyAShellExitingEndsAHold(t *testing.T) {
	t.Parallel()

	ledger := &attachLedger{}
	s := &operatorServer{jobs: ledger, log: slog.New(slog.DiscardHandler)}

	exited := func(lease string) {
		t.Helper()

		rec := httptest.NewRecorder()
		s.shellExited(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			operatorAttachExitPath+"?lease="+lease, nil))

		if rec.Code != http.StatusNoContent {
			t.Errorf("answered %d", rec.Code)
		}
	}

	// A DROPPED LINK.
	s.sessionBegan("dropped")
	s.sessionEnded(t.Context(), "dropped")

	// ONE OF TWO: the other is still looking.
	s.sessionBegan("two")
	s.sessionBegan("two")
	exited("two")
	s.sessionEnded(t.Context(), "two")

	if len(ledger.ended) != 0 {
		t.Errorf("ended %v before any last session's shell had exited", ledger.ended)
	}

	s.sessionEnded(t.Context(), "two")

	if len(ledger.ended) != 1 || ledger.ended[0] != "two by "+alloc.HoldEndedBySession {
		t.Errorf("the last session closing after a shell exited ended %v", ledger.ended)
	}

	// A SECOND WHOLE SESSION STARTS AFRESH: what the last one said does not carry over.
	s.sessionBegan("two")
	s.sessionEnded(t.Context(), "two")

	if len(ledger.ended) != 1 {
		t.Errorf("a later dropped link ended %v", ledger.ended)
	}

	// A CONTROLLER STOPPING.
	stopping, stop := context.WithCancel(t.Context())
	stop()

	s.sessionBegan("stopping")
	exited("stopping")
	s.sessionEnded(stopping, "stopping")

	if len(ledger.ended) != 1 {
		t.Errorf("a controller stopping ended %v", ledger.ended)
	}

	// THE WORD USUALLY ARRIVES AFTER THE SESSION HAS GONE, since the guest closes behind its exit frame
	// and the command has to dial here, and it still ends the hold: that was the last, and it finished.
	s.sessionBegan("late")
	s.sessionEnded(t.Context(), "late")

	if len(ledger.ended) != 1 {
		t.Errorf("a connection going ended %v", ledger.ended)
	}

	exited("late")

	if len(ledger.ended) != 2 || ledger.ended[1] != "late by "+alloc.HoldEndedBySession {
		t.Errorf("a word arriving after its session ended %v", ledger.ended)
	}

	// AND IT IS NO WORD ABOUT A LATER SESSION.
	s.sessionBegan("late")
	s.sessionEnded(t.Context(), "late")

	if len(ledger.ended) != 2 {
		t.Errorf("a stale word ended %v", ledger.ended)
	}
}

// A NODE'S CONFIG HAS NO CONTROL PLANE TO GO THROUGH, which is said rather than a panic.
func TestAttachingNeedsAControlPlane(t *testing.T) {
	t.Parallel()

	_, err := AttachJob(t.Context(), &config.Config{}, "l1")
	if err == nil || !strings.Contains(err.Error(), "no server section") {
		t.Errorf("a config with no server section: %v", err)
	}
}
