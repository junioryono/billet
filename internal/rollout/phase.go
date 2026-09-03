// Package rollout is billet's durable fleet decision: one immutable target, and
// where every controller and node has got to on the way to it.
//
// WHY A STATE MACHINE AND NOT A LOOP. Updating a fleet is a sequence of
// irreversible acts on machines that are running somebody's builds — draining a
// host, replacing a binary, migrating a ledger — and each of them has to be
// resumable by a process that did not start it. A control plane restart, a
// leadership handoff or an operator's second `billet rollout status` must all
// find the same answer, so where each component has got to is a durable phase
// rather than a position in a function.
package rollout

import (
	"fmt"
	"slices"
)

// Phase is where one component has got to.
//
// ONE VOCABULARY FOR CONTROLLERS AND NODES, because the sequence is the same
// thing happening to a different kind of machine and two enums would be two
// places to get the ordering right. What differs is who carries out each step,
// not what the steps are.
type Phase string

const (
	// PhasePending is a component the rollout has not started on.
	//
	// WHERE A DISCONNECTED NODE STAYS. It is not gone: its compute may be running
	// and it will come back speaking whatever it spoke before, so it holds the
	// rollout open rather than being written off. Only an operator proving compute
	// absence moves it to decommissioned.
	PhasePending Phase = "pending"

	// PhaseDraining is a component that has stopped taking new work and is
	// waiting for what it already has.
	//
	// UNBOUNDED, AND THAT IS THE WHOLE POINT. A job may run for days; elapsed time
	// is not evidence that one stopped making progress, and a rollout that
	// installed anyway would fail a build GitHub does not requeue. Nothing in this
	// package times this phase out.
	PhaseDraining Phase = "draining"

	// PhaseReadyToInstall is a component with no active workload obligations.
	//
	// A PHASE RATHER THAN AN INSTANT, because reaching it is the proof that
	// authorises the next step and that proof has to be recorded. An installer
	// that checked and acted would be checking on one side of a restart and acting
	// on the other.
	PhaseReadyToInstall Phase = "ready_to_install"

	// PhaseInstalling is a component whose binary is being replaced.
	PhaseInstalling Phase = "installing"

	// PhaseVerifying is a replaced component proving it works.
	//
	// SEPARATE FROM INSTALLING because they fail differently. An install that
	// fails has changed less than a verification that fails, and the recovery
	// journal has to say which side of the switch the failure landed on.
	PhaseVerifying Phase = "verifying"

	// PhaseCommitted is a component running the target and healthy. Terminal.
	PhaseCommitted Phase = "committed"

	// PhaseRollingBack is a failed component restoring its previous release.
	PhaseRollingBack Phase = "rolling_back"

	// PhaseRolledBack is a component proved healthy on its previous release.
	//
	// NOT TERMINAL. A successfully rolled-back node may return to service when its
	// old release remains compatible and policy permits, so this is a state the
	// rollout can leave — which is exactly why it is distinct from blocked.
	PhaseRolledBack Phase = "rolled_back"

	// PhaseBlocked is a component nothing can safely act on.
	//
	// WHAT AN UNPROVABLE ROLLBACK LEAVES BEHIND, and it advertises no capacity. The
	// alternative to a cordon is guessing, and the two guesses are "assume the old
	// binary is fine" (which may be running against a migrated ledger) and "assume
	// the compute is gone" (which sells a running job's slot twice).
	PhaseBlocked Phase = "blocked"

	// PhaseDecommissioned is a component an operator has removed from the fleet
	// after proving its compute is gone. Terminal.
	PhaseDecommissioned Phase = "decommissioned"

	// PhaseExempt is a component an operator has recorded a decision to skip.
	//
	// DISTINCT FROM DECOMMISSIONED, because the machine is still there. An
	// exemption says "this host is not part of this rollout"; a decommission says
	// "this host is gone". Collapsing them would let a rollout complete while a
	// live host kept an old protocol open with nothing recording that anybody
	// decided so.
	PhaseExempt Phase = "exempt"
)

// Terminal reports whether a phase ends this rollout's interest in a component.
func (p Phase) Terminal() bool {
	return p == PhaseCommitted || p == PhaseDecommissioned || p == PhaseExempt
}

// Converged reports whether a component is running the target.
//
// NARROWER THAN Terminal, DELIBERATELY. A rollout is complete when every required
// component is converged OR an operator has recorded a decision about it, and
// those are different facts: an exempted host is still running the old release.
// "Most nodes updated" is not success, and neither is "nothing left to do".
func (p Phase) Converged() bool { return p == PhaseCommitted }

// validTransitions is the state machine, written down rather than implied by
// scattered UPDATEs — the same rule internal/alloc's lease phases follow, and for
// the same reason: a transition not listed here is refused, so adding one is a
// decision somebody made rather than a line that happened to compile.
var validTransitions = map[Phase][]Phase{
	// A COMPONENT MAY BE EXEMPTED OR DECOMMISSIONED FROM ANY LIVE PHASE, because
	// an operator's decision about a host does not have to wait for that host to
	// reach a convenient point — and the host that most needs the decision is
	// usually the one that is stuck.
	PhasePending: {PhaseDraining, PhaseBlocked, PhaseDecommissioned, PhaseExempt},

	// A DRAINING NODE MAY GO STRAIGHT TO ROLLING BACK, and leaving that edge out
	// was a defect. The coordinator's view of a NODE is coarser than the node's
	// own: the host runs its whole transaction itself, so the only phases billet
	// records from outside are the instruction and the outcome. A host that
	// restored its previous release never showed billet an `installing` to fall
	// back from, and without this edge the only record billet could write was
	// refused — which left the host draining forever, holding the cohort.
	PhaseDraining: {
		PhaseReadyToInstall, PhaseRollingBack, PhaseBlocked, PhaseDecommissioned, PhaseExempt,
	},
	PhaseReadyToInstall: {PhaseInstalling, PhaseBlocked, PhaseDecommissioned, PhaseExempt},

	// INSTALLING AND VERIFYING FALL BACK, they do not fall forward. Neither may
	// reach committed without the other, because the only thing that distinguishes
	// "the binary is in place" from "the component works" is the verification.
	PhaseInstalling: {PhaseVerifying, PhaseRollingBack, PhaseBlocked},
	PhaseVerifying:  {PhaseCommitted, PhaseRollingBack, PhaseBlocked},

	PhaseRollingBack: {PhaseRolledBack, PhaseBlocked},

	// A ROLLED-BACK COMPONENT MAY BE TRIED AGAIN. That is what makes it different
	// from blocked: its previous release is proved healthy, so returning it to
	// pending is safe when policy permits.
	PhaseRolledBack: {PhasePending, PhaseBlocked, PhaseDecommissioned, PhaseExempt},

	// AND A BLOCKED ONE MAY BE RETRIED BY A PERSON. Nothing automatic leaves this
	// phase: it exists because billet could not prove something, and only an
	// operator can supply what it could not prove.
	PhaseBlocked: {PhasePending, PhaseDecommissioned, PhaseExempt},

	PhaseCommitted:      nil,
	PhaseDecommissioned: nil,
	PhaseExempt:         nil,
}

// ErrBadTransition means a phase change the state machine does not allow.
var ErrBadTransition = fmt.Errorf("rollout: that phase change is not one the state machine allows")

// KnownPhase reports whether a string is a phase this build understands.
//
// A ROW THIS BINARY CANNOT CLASSIFY IS NOT ONE IT MAY ACT ON. A newer binary can
// write a phase an older one has never heard of, and treating that as pending
// would restart an install on a component already midway through one.
func KnownPhase(p Phase) bool {
	_, ok := validTransitions[p]

	return ok
}

// CanTransition reports whether one phase may follow another.
//
// A REPEAT IS ALLOWED AND CHANGES NOTHING. Instructions are delivered over a
// network and retried, so a component reporting the phase it is already in is the
// ordinary case rather than an error — and refusing it would turn a redelivery
// into a blocked host.
func CanTransition(from, to Phase) bool {
	if from == to {
		return true
	}

	return slices.Contains(validTransitions[from], to)
}

// Transition validates one phase change and explains a refusal.
func Transition(from, to Phase) error {
	if !KnownPhase(from) {
		return fmt.Errorf("%w: %q is not a phase this build understands, so it will not act "+
			"on it. A newer binary may have written it; do not roll a control plane back "+
			"into the middle of a rollout it cannot read", ErrBadTransition, from)
	}

	if !KnownPhase(to) {
		return fmt.Errorf("%w: %q is not a phase this build understands", ErrBadTransition, to)
	}

	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s cannot become %s", ErrBadTransition, from, to)
	}

	return nil
}
