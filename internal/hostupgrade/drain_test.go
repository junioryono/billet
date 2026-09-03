package hostupgrade

import (
	"context"
	"slices"
	"testing"
	"time"
)

// blockingNodeStopHost holds Run inside StopNode until the test lets go.
//
// IT JUDGES NOTHING AND REIMPLEMENTS NOTHING. The rule under test belongs to
// Run — that nothing after StopNode may happen until StopNode returns — so the
// fake's only job is to be slow, exactly as a real node draining a job is slow.
// A fake that decided anything about the ordering would be the thing the
// assertion was about.
//
// IT HONOURS ITS CONTEXT, which is not decoration. A fake that blocks
// unconditionally cannot tell "Run waited" from "Run passed a context nobody
// could cancel", so every assertion below would hold against a Run that had
// stopped honouring cancellation entirely. Cancellation is also what makes the
// goroutine collectable when an assertion fails before the release.
type blockingNodeStopHost struct {
	*fakeHost

	// entered is closed once Run is inside StopNode. It is also the
	// happens-before edge that makes reading `did` from the test goroutine safe:
	// nothing appends to it while StopNode is parked.
	entered chan struct{}
	release chan struct{}
}

func (h *blockingNodeStopHost) StopNode(ctx context.Context) error {
	close(h.entered)

	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}

	return h.fakeHost.StopNode(ctx)
}

// AN UPGRADE WAITS FOR THE NODE'S DRAIN, AND A DRAIN HAS NO BOUND.
//
// `Host.StopNode`'s contract says so in words — "UNBOUNDED: a drain waits for the
// work already running for as long as it runs, and no elapsed time here may end a
// job" — and until this nothing asserted that Run honours it. The step is where
// "a multi-day job blocks update" lands on a real host: the node stops first so
// its compute drains, and internal/e2e's TestAMultiDayJobBlocksUpdateAndTeardown
// proves the drain underneath it really does wait for a running container.
//
// WHAT THIS ADDS IS THE HALF THAT CANNOT BE RUN. Every method of cmd/billet's
// systemdHost stops a service, replaces a binary or migrates a database, so the
// SEQUENCE is the only thing testable — and the failure this catches is a step
// moving above the drain, after which an upgrade hides the binary, fences the
// ledger or migrates it while somebody's job is still running.
//
// WHAT IT DELIBERATELY DOES NOT CLAIM is that a cancelled upgrade keeps waiting.
// Run passes its context straight to StopNode and the real one runs `systemctl
// stop` under it, so cancelling ends billet's WAIT — and that is correct, because
// systemd goes on stopping the unit either way and no job dies for it. A test
// asserting otherwise passed here for a while, and only because the fake ignored
// its context.
func TestAnUpgradeDoesNothingElseWhileTheNodeIsDraining(t *testing.T) {
	j := newJournal(t)
	h := &blockingNodeStopHost{
		fakeHost: &fakeHost{},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}

	finished := make(chan error, 1)

	go func() { finished <- Run(t.Context(), Request{Journal: j, Host: h, Log: quiet()}) }()

	select {
	case <-h.entered:
	case err := <-finished:
		t.Fatalf("the upgrade finished without ever stopping the node: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the upgrade never reached StopNode")
	}

	// NOT ONE STEP FURTHER. Asserted as the whole prefix rather than as "the
	// binary is still there", because the failure is a step MOVING and any single
	// absence check would miss the other five.
	if want := []string{"preserve"}; !slices.Equal(h.did, want) {
		t.Errorf("while the node was draining the upgrade had already run\n  %v\nwant only\n  %v",
			h.did, want)
	}

	// AND THE JOURNAL HAS NOT ADVANCED. Preserving, stopping both services and
	// hiding the binary all complete before `stopped` is written, so a journal that
	// had moved here would mean a resume could skip a drain that never finished.
	if j.Step != StepStaged {
		t.Errorf("the journal records %s while the node is still draining, want %s",
			j.Step, StepStaged)
	}

	// It is still waiting a moment later, which is what "unbounded" means. The
	// window is short on purpose: this is the negative half of a claim whose
	// positive half — that it finishes as soon as the drain returns — is asserted
	// immediately below, so the two fail independently.
	select {
	case err := <-finished:
		t.Fatalf("the upgrade continued past a node that was still draining: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// THE DRAIN ENDS, AND THE UPGRADE RESUMES BY ITSELF — no second signal, no
	// operator. That is "the deferred operation resumes automatically after
	// completion", at this layer.
	close(h.release)

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("the upgrade failed once the drain finished: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the upgrade never finished after its drain returned")
	}

	if j.Step != StepCommitted {
		t.Errorf("the journal records %s, want committed", j.Step)
	}

	// AND IT RAN EVERYTHING AFTERWARDS, in order — so the wait delayed the
	// upgrade rather than skipping any of it. The full sequence is
	// TestAHealthyUpgradeRunsInOrder's subject; this asserts only that blocking in
	// the middle of it did not truncate the tail.
	if !slices.Contains(h.did, "stop-node") || !slices.Contains(h.did, "prove-stable") {
		t.Errorf("the upgrade did not complete after its drain returned: %v", h.did)
	}
}
