package rollout

import (
	"errors"
	"strings"
	"testing"
)

// EVERY PHASE IS REACHABLE FROM WHERE A COMPONENT STARTS.
//
// A phase with no path to it is a phase nothing can ever be in, which reads in
// the report as a state billet supports and is not. Reachability is checked by
// walking the machine rather than by listing the answer, so adding a phase and
// forgetting to connect it fails here rather than never being observed.
func TestEveryPhaseIsReachableFromPending(t *testing.T) {
	t.Parallel()

	seen := map[Phase]bool{PhasePending: true}
	queue := []Phase{PhasePending}

	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]

		for _, to := range validTransitions[from] {
			if seen[to] {
				continue
			}

			seen[to] = true

			queue = append(queue, to)
		}
	}

	for phase := range validTransitions {
		if !seen[phase] {
			t.Errorf("%s is not reachable from pending, so nothing can ever be in it", phase)
		}
	}
}

// A TERMINAL PHASE HAS NO SUCCESSORS. A component that has been recorded as
// converged, exempted or decommissioned must not walk back into the rollout — the
// same rule the lease state machine holds, and for the same reason: a decision
// somebody recorded is not one the machine may undo.
func TestTerminalPhasesHaveNoSuccessors(t *testing.T) {
	t.Parallel()

	for phase, next := range validTransitions {
		if !phase.Terminal() {
			continue
		}

		if len(next) != 0 {
			t.Errorf("%s is terminal and can still become %v", phase, next)
		}
	}
}

// AND EVERY PHASE WITH NO SUCCESSORS IS ONE Terminal AGREES ABOUT, which is the
// same rule read the other way. A phase that is a dead end but not terminal would
// hold a rollout open with nothing able to move it — the shape of a fleet that
// looks wedged and is.
func TestEveryDeadEndIsTerminal(t *testing.T) {
	t.Parallel()

	for phase, next := range validTransitions {
		if len(next) == 0 && !phase.Terminal() {
			t.Errorf("%s has no successors and is not terminal, so a component in it would "+
				"hold the rollout open forever", phase)
		}
	}
}

// COMMITTED IS THE ONLY CONVERGED PHASE. An exempted or decommissioned host is
// still running the old release, and calling either converged is how a protocol
// gets retired while a live machine still needs it.
func TestOnlyCommittedCountsAsConverged(t *testing.T) {
	t.Parallel()

	for phase := range validTransitions {
		if phase.Converged() && phase != PhaseCommitted {
			t.Errorf("%s reports itself converged; only a component running the target is",
				phase)
		}
	}

	if !PhaseCommitted.Converged() {
		t.Error("committed does not report itself converged")
	}

	for _, decided := range []Phase{PhaseExempt, PhaseDecommissioned} {
		if !decided.Terminal() {
			t.Errorf("%s is not terminal, so an operator's decision would not let a rollout "+
				"complete", decided)
		}

		if decided.Converged() {
			t.Errorf("%s reports itself converged, but that host is still on the old release",
				decided)
		}
	}
}

// A PHASE THIS BUILD DOES NOT UNDERSTAND IS NOT ONE IT MAY ACT ON.
//
// A newer binary can write a phase an older one has never heard of, and treating
// that as pending would restart an install on a component already midway through
// one. The diagnostic says which direction the mismatch runs, because the
// operator's instinct is to restart the thing that is complaining.
func TestAnUnknownPhaseIsRefusedRatherThanTreatedAsPending(t *testing.T) {
	t.Parallel()

	err := Transition("quiescing", PhaseDraining)
	if !errors.Is(err, ErrBadTransition) {
		t.Fatalf("Transition from an unknown phase: %v, want ErrBadTransition", err)
	}

	if !strings.Contains(err.Error(), "newer binary") {
		t.Errorf("the refusal does not say which side is behind: %v", err)
	}

	if KnownPhase("quiescing") {
		t.Error("an unknown phase is reported as known")
	}
}

// INSTALLING CANNOT BE REACHED WITHOUT PROVING THE COMPONENT IS IDLE. This is the
// edge that stops a rollout replacing a binary under a running job, so it is
// asserted directly rather than left to the table above.
func TestTheOnlyWayToInstallingIsThroughReadyToInstall(t *testing.T) {
	t.Parallel()

	for phase, next := range validTransitions {
		for _, to := range next {
			if to == PhaseInstalling && phase != PhaseReadyToInstall {
				t.Errorf("%s can reach installing directly; installation may only follow the "+
					"phase that records there are no active workload obligations", phase)
			}
		}
	}
}

// AND COMMITTED IS ONLY REACHABLE THROUGH VERIFYING, because the only thing that
// distinguishes "the binary is in place" from "the component works" is the
// verification.
func TestTheOnlyWayToCommittedIsThroughVerifying(t *testing.T) {
	t.Parallel()

	for phase, next := range validTransitions {
		for _, to := range next {
			if to == PhaseCommitted && phase != PhaseVerifying {
				t.Errorf("%s can reach committed directly, without anything having verified "+
					"the component works", phase)
			}
		}
	}
}
