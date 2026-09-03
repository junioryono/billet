package main

import (
	"testing"

	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
)

// blockedRollout stages the state an operator meets: one host the coordinator
// refused to converge, sitting in `blocked`.
func blockedRollout(t *testing.T, node string) (*rollout.Store, string, string) {
	t.Helper()

	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	store := rollout.New(db)

	r, err := store.Start(t.Context(), rollout.StartRequest{
		Channel:       "stable",
		TargetVersion: "v0.4.0",
		TargetDigest:  "b0dd6ea60e9ba0bfb28ac1cbcdd39b3a3d1a2eaf8b8dd48d9e2a2e0d3c4b5a69",
		CreatedBy:     "the test",
		Nodes:         []string{node},
	})
	if err != nil {
		t.Fatalf("start a rollout: %v", err)
	}

	if err := store.Advance(t.Context(), rollout.AdvanceRequest{
		RolloutID: r.ID,
		Node:      node,
		To:        rollout.PhaseBlocked,
		Blocker:   "the manifest this host installed is not the one this rollout decided",
	}); err != nil {
		t.Fatalf("block the host: %v", err)
	}

	return store, cfgPath, r.ID
}

func phaseOf(t *testing.T, store *rollout.Store, rolloutID, node string) rollout.Phase {
	t.Helper()

	nodes, err := store.Nodes(t.Context(), rolloutID)
	if err != nil {
		t.Fatalf("read the rollout's hosts: %v", err)
	}

	for i := range nodes {
		if nodes[i].Node == node {
			return nodes[i].Phase
		}
	}

	t.Fatalf("%s is not in rollout %s at all", node, rolloutID)

	return ""
}

// `billet rollout retry` PUTS A BLOCKED HOST BACK TO PENDING, and this runs the
// command to find out.
//
// THE ROUTING AND THE PHASE TABLE WERE BOTH TESTED AND NEITHER ANSWERED THIS.
// One proved `retry` reaches a command, the other that `blocked` may become
// `pending` — and the command could have asked for `exempt`, which `blocked` may
// ALSO become, with both tests still green and an operator following the
// blocker's own instructions quietly exempting the host from the rollout instead
// of retrying it. That is the failure this area keeps producing: every piece
// correct, the instruction still wrong. What settles it is running the command
// and reading back what it wrote.
func TestRolloutRetryReturnsABlockedHostToTheRollout(t *testing.T) {
	const node = "epyc-1"

	store, cfgPath, rolloutID := blockedRollout(t, node)

	if got := phaseOf(t, store, rolloutID, node); got != rollout.PhaseBlocked {
		t.Fatalf("the fixture did not block the host: it is %s", got)
	}

	capture(t, func() {
		if err := cmdRollout(t.Context(), []string{"retry", "--config", cfgPath, node}); err != nil {
			t.Fatalf("billet rollout retry: %v", err)
		}
	})

	if got := phaseOf(t, store, rolloutID, node); got != rollout.PhasePending {
		t.Errorf("`billet rollout retry` left %s as %s rather than %s, so the repair the "+
			"blocker prescribes does not return the host to the rollout",
			node, got, rollout.PhasePending)
	}
}

// AND `exempt` IS A DIFFERENT DECISION, which is what makes the assertion above
// worth making.
//
// Both are reachable from `blocked`, so nothing in the phase machine separates
// them: only the command's own mapping does. Exempting a host REMOVES it from the
// rollout's convergence rather than retrying it, so the two verbs must not be
// interchangeable, and a test that pinned neither would let them become so.
func TestRolloutExemptIsNotTheSameDecisionAsRetry(t *testing.T) {
	const node = "epyc-2"

	store, cfgPath, rolloutID := blockedRollout(t, node)

	capture(t, func() {
		if err := cmdRollout(t.Context(), []string{
			"exempt", "--config", cfgPath, "--reason", "being replaced", node,
		}); err != nil {
			t.Fatalf("billet rollout exempt: %v", err)
		}
	})

	if got := phaseOf(t, store, rolloutID, node); got != rollout.PhaseExempt {
		t.Errorf("`billet rollout exempt` left %s as %s rather than %s", node, got,
			rollout.PhaseExempt)
	}
}
