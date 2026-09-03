package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/rollout"
)

// `billet rollout retry` IS A ROUTED COMMAND THAT RETURNS A HOST TO THE ROLLOUT.
//
// The blocker a digest mismatch writes tells an operator to run two things, and
// the coordinator test that covers the second half drives the phase machine
// directly — it proves that a returned host converges and claims nothing about
// the command that returns it. This is that half: routing, and the phase it
// asks for.
//
// WITHOUT IT, EVERY PIECE IS TESTED AND THE INSTRUCTION CAN STILL BE WRONG. A
// `retry` that was never routed, or that asked for a phase the state machine
// refuses from `blocked`, would leave an operator following a documented repair
// that does nothing — which is the defect this whole area keeps producing in
// different clothes.
func TestRolloutRetryIsRoutedAndAsksForTheRightPhase(t *testing.T) {
	t.Parallel()

	// ROUTED: an unknown subcommand names what does exist, and `retry` is in it.
	err := cmdRollout(t.Context(), []string{"no-such-subcommand"})
	if err == nil {
		t.Fatal("an unknown rollout subcommand was accepted")
	}

	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("`retry` is not among the rollout subcommands: %v", err)
	}

	// AND IT REACHES ITS OWN USAGE rather than the dispatcher's, which is what
	// says the route resolves to a command rather than falling through.
	err = cmdRollout(t.Context(), []string{"retry"})
	if err == nil {
		t.Fatal("`billet rollout retry` with no host was accepted")
	}

	if !strings.Contains(err.Error(), "billet rollout retry") {
		t.Errorf("`retry` did not reach its own usage: %v", err)
	}
}

// AND THE PHASE IT ASKS FOR IS ONE `blocked` CAN LEAVE.
//
// `blocked` has exactly three successors — pending, decommissioned, exempt — so a
// retry that asked for anything else would be refused by the state machine on
// every host it was ever run against, and the failure would surface only in front
// of an operator following the blocker's own instructions.
func TestRolloutRetryAsksForAPhaseBlockedCanReach(t *testing.T) {
	t.Parallel()

	if !rollout.CanTransition(rollout.PhaseBlocked, rollout.PhasePending) {
		t.Error("`billet rollout retry` returns a host to pending, which a blocked host " +
			"cannot become; an operator following the blocker would be refused by the " +
			"state machine")
	}
}
