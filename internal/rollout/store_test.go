package rollout

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
)

const (
	targetDigest = "1111111111111111111111111111111111111111111111111111111111111111"
	otherDigest  = "2222222222222222222222222222222222222222222222222222222222222222"
)

func open(t *testing.T) (*state.DB, *Store) {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db, New(db)
}

func start(t *testing.T, s *Store, nodes ...string) *Rollout {
	t.Helper()

	if len(nodes) == 0 {
		nodes = []string{"epyc-1"}
	}

	r, err := s.Start(t.Context(), StartRequest{
		Channel: "stable", TargetVersion: "v0.4.0", TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: nodes,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	return r
}

// converge walks one node all the way to committed, the way a healthy rollout
// does, so a test about completion does not restate the whole sequence.
func converge(t *testing.T, s *Store, r *Rollout, node string) {
	t.Helper()

	for _, phase := range []Phase{
		PhaseDraining, PhaseReadyToInstall, PhaseInstalling, PhaseVerifying, PhaseCommitted,
	} {
		if err := s.Advance(t.Context(), AdvanceRequest{
			RolloutID: r.ID, Node: node, To: phase,
		}); err != nil {
			t.Fatalf("advance %s to %s: %v", node, phase, err)
		}
	}
}

func convergeController(t *testing.T, s *Store, r *Rollout) {
	t.Helper()

	for _, phase := range []Phase{
		PhaseDraining, PhaseReadyToInstall, PhaseInstalling, PhaseVerifying, PhaseCommitted,
	} {
		if err := s.Advance(t.Context(), AdvanceRequest{
			RolloutID: r.ID, To: phase,
		}); err != nil {
			t.Fatalf("advance the controller to %s: %v", phase, err)
		}
	}
}

// STARTING THE SAME ROLLOUT TWICE FINDS THE ONE THAT IS RUNNING.
//
// A repeated instruction must not duplicate or retarget a decision. An operator
// who runs the command again — or an automation that retries it — must not end up
// with two rollouts each draining hosts the other is installing on.
func TestStartingTheSameTargetTwiceResumesTheSameRollout(t *testing.T) {
	_, s := open(t)

	first := start(t, s)
	second := start(t, s)

	if first.ID != second.ID {
		t.Fatalf("a repeated start created rollout %s beside %s", second.ID, first.ID)
	}

	if second.Generation != first.Generation {
		t.Errorf("a repeated start moved the generation from %d to %d",
			first.Generation, second.Generation)
	}
}

// A DIFFERENT TARGET WHILE ONE IS OPEN IS REFUSED, not silently applied. Work is
// already underway against the first, and retargeting would leave some hosts on
// one release and some on another with one record describing both.
func TestADifferentTargetIsRefusedWhileARolloutIsOpen(t *testing.T) {
	_, s := open(t)

	start(t, s)

	_, err := s.Start(t.Context(), StartRequest{
		TargetVersion: "v0.5.0", TargetDigest: otherDigest,
		Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: []string{"epyc-1"},
	})
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("Start with a different target: %v, want ErrOpen", err)
	}
}

// THE SAME DECISION IS COMPARED ON THE DIGEST, NOT THE VERSION.
//
// Two releases can carry one tag only if something moved it, and that is exactly
// the case where continuing the first rollout is right rather than starting a
// second against bytes nobody vouched for.
func TestARetargetedTagIsNotTheSameRollout(t *testing.T) {
	_, s := open(t)

	start(t, s)

	_, err := s.Start(t.Context(), StartRequest{
		TargetVersion: "v0.4.0", TargetDigest: otherDigest,
		Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: []string{"epyc-1"},
	})
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("Start with the same tag and different bytes: %v, want ErrOpen", err)
	}
}

// A ROLLOUT SURVIVES THE PROCESS THAT CREATED IT.
//
// A control plane restart, or a leadership handoff, must resume the same rollout
// against the same target — not require a second human decision, and not create a
// second rollout.
func TestARolloutSurvivesReopeningTheLedger(t *testing.T) {
	dir := t.TempDir()

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	s := New(db)
	first := start(t, s)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: first.ID, Node: "epyc-1", To: PhaseDraining,
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	t.Cleanup(func() { _ = again.Close() })

	resumed, err := New(again).Open(t.Context())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if resumed.ID != first.ID || resumed.TargetDigest != targetDigest {
		t.Fatalf("the resumed rollout is %s -> %s, want %s -> %s",
			resumed.ID, resumed.TargetDigest, first.ID, targetDigest)
	}

	nodes, err := New(again).Nodes(t.Context(), resumed.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if len(nodes) != 1 || nodes[0].Phase != PhaseDraining {
		t.Errorf("the node's phase did not survive: %+v", nodes)
	}
}

// A ROLLOUT COVERING NO NODE IS REFUSED.
//
// It converges the moment it starts and reports the fleet up to date — which is
// exactly what an operator whose nodes have not registered would be told, and
// would believe.
func TestARolloutWithNoNodesIsRefused(t *testing.T) {
	_, s := open(t)

	_, err := s.Start(t.Context(), StartRequest{
		TargetVersion: "v0.4.0", TargetDigest: targetDigest,
		Policy: DefaultPolicy(), CreatedBy: "ops",
	})
	if err == nil {
		t.Fatal("a rollout covering no node was accepted")
	}

	if !strings.Contains(err.Error(), "no registered nodes") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// THE STATE MACHINE REFUSES A STEP IT DOES NOT ALLOW.
//
// Installing without draining is the one that matters: it is the shape of a
// rollout that replaces a binary while a job is still running on the host.
func TestInstallingWithoutDrainingIsRefused(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseInstalling,
	})
	if !errors.Is(err, ErrBadTransition) {
		t.Fatalf("installing straight from pending: %v, want ErrBadTransition", err)
	}
}

// A REPEATED INSTRUCTION IS ACCEPTED AND CHANGES NOTHING. Instructions cross a
// network and are retried; refusing a redelivery would turn one into a blocked
// host.
func TestRepeatingAPhaseIsAccepted(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	for range 2 {
		if err := s.Advance(t.Context(), AdvanceRequest{
			RolloutID: r.ID, Node: "epyc-1", To: PhaseDraining,
		}); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}
}

// BLOCKING NEEDS A REASON. A cordoned host with nothing recorded is one nobody
// can clear, and blocking is billet saying it could not prove something.
func TestBlockingWithoutAReasonIsRefused(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseBlocked,
	})
	if err == nil || !strings.Contains(err.Error(), "needs a reason") {
		t.Fatalf("blocking with no reason: %v, want a refusal", err)
	}
}

// AND SO DOES AN EXEMPTION, because it is what lets a rollout complete without
// that component converging.
func TestExemptingWithoutAReasonIsRefused(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	for _, phase := range []Phase{PhaseExempt, PhaseDecommissioned} {
		err := s.Advance(t.Context(), AdvanceRequest{
			RolloutID: r.ID, Node: "epyc-1", To: phase,
		})
		if err == nil || !strings.Contains(err.Error(), "operator's reason") {
			t.Errorf("recording %s with no reason: %v, want a refusal", phase, err)
		}
	}
}

// "MOST NODES UPDATED" IS NOT A COMPLETED ROLLOUT.
//
// The issue says so in those words, and this is where it is enforced: completion
// means every required component reports the target, or an operator recorded a
// decision about it.
func TestARolloutCannotCompleteWithANodeStillOutstanding(t *testing.T) {
	_, s := open(t)
	r := start(t, s, "epyc-1", "mac-1")

	convergeController(t, s, r)
	converge(t, s, r, "epyc-1")

	err := s.Finish(t.Context(), r.ID, StateCompleted, "done")
	if err == nil {
		t.Fatal("a rollout completed with a node still outstanding")
	}

	if !strings.Contains(err.Error(), "mac-1") {
		t.Errorf("the refusal does not name the node holding it open: %v", err)
	}
}

// AND IT CANNOT COMPLETE WHILE THE CONTROLLER ITSELF IS BEHIND, which is the
// half an operator watching node phases would not notice.
func TestARolloutCannotCompleteWithTheControllerBehind(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	converge(t, s, r, "epyc-1")

	err := s.Finish(t.Context(), r.ID, StateCompleted, "done")
	if err == nil || !strings.Contains(err.Error(), "controller") {
		t.Fatalf("completing with the controller behind: %v, want a refusal", err)
	}
}

// AN EXPLICIT DECISION LETS IT COMPLETE, which is the other half of the rule: a
// host that is gone for good must not hold a rollout open forever.
func TestAnExemptedNodeLetsARolloutComplete(t *testing.T) {
	_, s := open(t)
	r := start(t, s, "epyc-1", "mac-1")

	convergeController(t, s, r)
	converge(t, s, r, "epyc-1")

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "mac-1", To: PhaseExempt,
		ExemptReason: "retired; its compute is proved gone",
	}); err != nil {
		t.Fatalf("exempt: %v", err)
	}

	if err := s.Finish(t.Context(), r.ID, StateCompleted, "converged"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if _, err := s.Open(t.Context()); !errors.Is(err, ErrNoRollout) {
		t.Errorf("a finished rollout is still open: %v", err)
	}
}

// AN ABORT IS NOT SUBJECT TO THE CONVERGENCE RULE. Abandoning a rollout before it
// has mutated anything is a decision an operator is entitled to make, and
// requiring convergence to record it would make the abort impossible exactly when
// it is wanted.
func TestARolloutCanBeAbortedWithoutConverging(t *testing.T) {
	_, s := open(t)
	r := start(t, s, "epyc-1", "mac-1")

	if err := s.Finish(t.Context(), r.ID, StateAborted, "the candidate was withdrawn"); err != nil {
		t.Fatalf("abort: %v", err)
	}

	if _, err := s.Open(t.Context()); !errors.Is(err, ErrNoRollout) {
		t.Errorf("an aborted rollout is still open: %v", err)
	}
}

// A NEW ROLLOUT MAY BE STARTED ONCE THE LAST ONE FINISHED, and its generation
// moves so instructions from the two are distinguishable on the wire.
func TestASecondRolloutIsAllowedAfterTheFirstFinishes(t *testing.T) {
	_, s := open(t)

	first := start(t, s)

	if err := s.Finish(t.Context(), first.ID, StateAborted, "withdrawn"); err != nil {
		t.Fatalf("abort: %v", err)
	}

	second, err := s.Start(t.Context(), StartRequest{
		TargetVersion: "v0.5.0", TargetDigest: otherDigest,
		Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("a second rollout after the first finished: %v", err)
	}

	if second.Generation <= first.Generation {
		t.Errorf("the second rollout is generation %d, want above %d",
			second.Generation, first.Generation)
	}
}

// BACKOFF IS DURABLE AND SO IS THE ATTEMPT COUNT, because a retry schedule held
// in memory restarts every time the control plane does — which turns a bounded
// exponential backoff into an unbounded retry loop against a host that is down.
func TestBackoffRecordsAnAttemptAndANextTime(t *testing.T) {
	db, _ := open(t)

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	s := New(db, WithClock(func() time.Time { return now }))

	r := start(t, s)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseBlocked,
		Blocker: "the host did not answer", Backoff: 5 * time.Minute,
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if len(nodes) != 1 {
		t.Fatalf("Nodes returned %d rows", len(nodes))
	}

	if nodes[0].Attempts != 1 {
		t.Errorf("attempts is %d, want 1", nodes[0].Attempts)
	}

	want := now.Add(5 * time.Minute).Format(time.RFC3339Nano)
	if nodes[0].NextAttemptAt != want {
		t.Errorf("next attempt is %q, want %q", nodes[0].NextAttemptAt, want)
	}

	if nodes[0].Blocker == "" {
		t.Error("the blocker was not recorded")
	}
}

// PRIOR RELEASE, DISPATCH EPOCH AND CONVERGED DIGEST ARE WRITE-ONCE-OR-KEEP.
//
// Each is recorded by whichever pass first learns it, and every pass after that
// passes nothing -- the coordinator advances a host four more times after
// dispatching it. If a later advance overwrote them with the zero value, a
// rollback would lose the release it was meant to return to, the one causal fence
// a rollout has about a host coming back would be erased, and a completed rollout
// could not say which of its hosts were proved against the manifest it decided
// on.
//
// The rule lives in three CASE expressions in AdvanceRolloutNode, and this is
// what holds them: measured, replacing any of the three with a plain assignment
// left every other test in this package green.
func TestALaterAdvanceKeepsWhatAnEarlierOneRecorded(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseDraining,
		PriorRelease: "v0.3.26", DispatchEpoch: 7, ConvergedDigest: otherDigest,
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// FOUR MORE ADVANCES CARRYING NOTHING, which is what the coordinator does.
	for _, phase := range []Phase{
		PhaseReadyToInstall, PhaseInstalling, PhaseVerifying, PhaseCommitted,
	} {
		if err := s.Advance(t.Context(), AdvanceRequest{
			RolloutID: r.ID, Node: "epyc-1", To: phase,
		}); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if len(nodes) != 1 {
		t.Fatalf("Nodes returned %d rows", len(nodes))
	}

	if nodes[0].PriorRelease != "v0.3.26" {
		t.Errorf("prior release is %q, want v0.3.26; a rollback has nowhere to go",
			nodes[0].PriorRelease)
	}

	if nodes[0].DispatchEpoch != 7 {
		t.Errorf("dispatch epoch is %d, want 7; the rollout can no longer tell a host "+
			"that has not started from one that upgraded and rolled itself back",
			nodes[0].DispatchEpoch)
	}

	if nodes[0].ConvergedDigest != otherDigest {
		t.Errorf("converged digest is %q, want %q", nodes[0].ConvergedDigest, otherDigest)
	}
}

// A BLOCKED NODE CAN BE RETRIED BY A PERSON AND NOTHING ELSE. Nothing automatic
// leaves that phase: it exists because billet could not prove something.
func TestABlockedNodeReturnsToPendingOnRetry(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseBlocked, Blocker: "unprovable rollback",
	}); err != nil {
		t.Fatalf("block: %v", err)
	}

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending,
	}); err != nil {
		t.Fatalf("retry a blocked node: %v", err)
	}

	// AND IT CANNOT JUMP STRAIGHT BACK TO COMMITTED, which would be recording a
	// convergence nobody observed.
	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseCommitted,
	}); !errors.Is(err, ErrBadTransition) {
		t.Errorf("committing straight from pending: %v, want ErrBadTransition", err)
	}
}

// A NODE THAT IS NOT PART OF THE ROLLOUT IS AN ERROR RATHER THAN A NEW ROW.
//
// The node set is a snapshot taken when the decision was made; a host that
// registered afterwards is running whatever it was installed with and is not part
// of a decision taken before it existed.
func TestAdvancingAnUnknownNodeIsRefused(t *testing.T) {
	_, s := open(t)
	r := start(t, s)

	err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "arrived-later", To: PhaseDraining,
	})
	if err == nil || !strings.Contains(err.Error(), "is not part of rollout") {
		t.Fatalf("advancing an unknown node: %v, want a refusal", err)
	}
}

// "NOT CONVERGED" IS A SENTINEL AND EVERY OTHER FAILURE IS NOT.
//
// The coordinator attempts completion on every pass, so an outstanding host is
// what it hears on every tick but the last and must not be reported as a failed
// pass. Without a way to tell that from a storage error it suppressed both — and
// a Finish that could never succeed was reported as a successful pass forever,
// visible nowhere.
func TestFinishSeparatesAnOutstandingRolloutFromABrokenOne(t *testing.T) {
	_, s := open(t)

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: "v0.4.0", TargetDigest: targetDigest, PriorVersion: "v0.3.26",
		Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = s.Finish(t.Context(), r.ID, StateCompleted, "done")
	if !errors.Is(err, ErrOutstanding) {
		t.Errorf("finishing a rollout with an outstanding host returned %v, want "+
			"ErrOutstanding", err)
	}

	// A ROLLOUT THAT IS NOT THERE IS NOT "OUTSTANDING". It is a caller asking about
	// something that does not exist, and answering with the sentinel would have the
	// coordinator suppress it on every pass forever.
	err = s.Finish(t.Context(), "no-such-rollout", StateCompleted, "done")
	if err == nil || errors.Is(err, ErrOutstanding) {
		t.Errorf("finishing a rollout that does not exist returned %v, which the "+
			"coordinator would suppress as ordinary", err)
	}
}
