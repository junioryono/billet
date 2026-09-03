package state

import (
	"errors"
	"strings"
	"testing"
)

// A FRESH DEPLOYMENT ADMITS WORK. The migration inserts the row, so no caller
// ever meets "no admission row" and has to decide what that means.
func TestAFreshDeploymentIsOpen(t *testing.T) {
	db := open(t)

	a, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if a.Mode != AdmissionOpen {
		t.Errorf("a fresh deployment is %v, want open", a.Mode)
	}
	if a.Sealed() {
		t.Error("a fresh deployment reports itself sealed")
	}
	if a.Generation != 0 {
		t.Errorf("generation is %d, want 0", a.Generation)
	}
}

// THE SEAL SURVIVES THE PROCESS. A seal held in memory is not a safety
// property: an interrupted command would silently reopen admission while the
// operator believes a shutdown is still in progress.
func TestASealSurvivesReopeningTheLedger(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	sealed, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceOperator,
		Reason:     "replacing the disks",
		Actor:      "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()

	a, err := again.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if !a.Sealed() {
		t.Fatal("a deployment sealed before the restart admits work after it")
	}
	// AND IT STILL SAYS WHO AND WHY, because the operator who finds a sealed
	// deployment is often not the one who sealed it.
	if a.Actor != "ops@example.com" || a.Reason != "replacing the disks" {
		t.Errorf("the seal lost its attribution: %+v", a)
	}
	if a.Generation != sealed.Generation {
		t.Errorf("generation moved across the restart: %d became %d",
			sealed.Generation, a.Generation)
	}
}

// THE GENERATION IS A FENCE. One operator's resume must not undo a seal a
// second operator took in between — the first would be reopening a decision it
// never saw.
func TestAResumeCannotUndoASealItNeverSaw(t *testing.T) {
	db := open(t)

	first, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceOperator, Reason: "first", Actor: "a",
	})
	if err != nil {
		t.Fatalf("first seal: %v", err)
	}

	// A second decision lands while the first operator is still deciding.
	if _, err := db.Seal(t.Context(), SealRequest{
		Expect: first.Generation, Provenance: ProvenanceOperator, Reason: "second", Actor: "b",
	}); err != nil {
		t.Fatalf("second seal: %v", err)
	}

	// The first operator now resumes against what they last saw.
	_, err = db.Resume(t.Context(), ResumeRequest{Expect: first.Generation, Actor: "a"})
	if !errors.Is(err, ErrAdmissionGeneration) {
		t.Fatalf("a stale resume was accepted: %v", err)
	}

	// AND THE DEPLOYMENT IS STILL SEALED, which is the point — asserting only the
	// error would pass against a resume that failed after reopening admission.
	a, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}
	if !a.Sealed() {
		t.Fatal("the second operator's seal was cleared by the first operator's resume")
	}
	if a.Reason != "second" {
		t.Errorf("the surviving seal is %q, want the second one", a.Reason)
	}
}

// A LIFECYCLE COMMAND MAY CLEAR ITS OWN SEAL AND NOT AN OPERATOR'S. `billet
// local up` reopening a maintenance seal because it happened to restart the
// services would admit work into a deployment somebody deliberately quiesced,
// and the evidence would be a job running in their maintenance window.
func TestResumeWillNotClearASealItDidNotTake(t *testing.T) {
	db := open(t)

	operator, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceOperator, Reason: "maintenance", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	_, err = db.Resume(t.Context(), ResumeRequest{
		Expect: operator.Generation, Clears: ProvenanceLocalDown, Actor: "billet local up",
	})
	if err == nil {
		t.Fatal("a lifecycle command cleared an operator's maintenance seal")
	}
	if !strings.Contains(err.Error(), "maintenance") && !strings.Contains(err.Error(), "operator") {
		t.Errorf("the refusal does not say whose seal it is: %v", err)
	}

	after, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}
	if !after.Sealed() {
		t.Fatal("the operator's seal was cleared anyway")
	}

	// AND THE REFUSAL MOVED NOTHING. A resume that declined after bumping the
	// generation would invalidate an unrelated operator's fence as a side effect
	// of doing nothing.
	if after.Generation != operator.Generation {
		t.Errorf("a refused resume moved the generation from %d to %d",
			operator.Generation, after.Generation)
	}

	// AND ITS OWN KIND IT DOES CLEAR, or the pairing above would be satisfied by
	// a resume that never works.
	own, err := db.Seal(t.Context(), SealRequest{
		Expect: after.Generation, Provenance: ProvenanceLocalDown,
		Reason: "down", Actor: "billet local down",
	})
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}

	if _, err := db.Resume(t.Context(), ResumeRequest{
		Expect: own.Generation, Clears: ProvenanceLocalDown, Actor: "billet local up",
	}); err != nil {
		t.Fatalf("a lifecycle command could not clear its own seal: %v", err)
	}

	if a, err := db.Admission(t.Context()); err != nil {
		t.Fatalf("Admission: %v", err)
	} else if a.Sealed() {
		t.Fatal("its own seal was not cleared")
	}
}

// EVERY TRANSITION MOVES THE GENERATION, including a reseal — otherwise a
// generation names a state rather than a decision, and two different decisions
// become indistinguishable to the fence above.
func TestEveryTransitionMovesTheGeneration(t *testing.T) {
	db := open(t)

	var seen []int64
	gen := int64(0)

	for i := range 3 {
		sealed, err := db.Seal(t.Context(), SealRequest{
			Expect: gen, Provenance: ProvenanceOperator, Reason: "round", Actor: "ops",
		})
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		seen = append(seen, sealed.Generation)

		opened, err := db.Resume(t.Context(), ResumeRequest{Expect: sealed.Generation, Actor: "ops"})
		if err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		seen = append(seen, opened.Generation)
		gen = opened.Generation
	}

	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("the generation did not move at step %d: %v", i, seen)
		}
	}
}

// A PROVENANCE BILLET DOES NOT ISSUE IS REFUSED, so a typo cannot create a seal
// nothing is entitled to clear.
func TestSealRefusesAProvenanceItDoesNotIssue(t *testing.T) {
	db := open(t)

	for _, p := range []string{"", "operator ", "LOCAL-DOWN", "whatever"} {
		if _, err := db.Seal(t.Context(), SealRequest{Provenance: p}); err == nil {
			t.Errorf("a seal with provenance %q was accepted", p)
		}
	}

	if a, err := db.Admission(t.Context()); err != nil {
		t.Fatalf("Admission: %v", err)
	} else if a.Sealed() {
		t.Fatal("a refused seal sealed the deployment anyway")
	}
}

// RESUMING AN OPEN DEPLOYMENT IS NOT AN ERROR, and does not move the
// generation: a lifecycle command that always resumes on its way out must not
// invalidate an unrelated operator's fence by doing nothing.
func TestResumingAnOpenDeploymentChangesNothing(t *testing.T) {
	db := open(t)

	before, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if _, err := db.Resume(t.Context(), ResumeRequest{Expect: before.Generation, Actor: "x"}); err != nil {
		t.Fatalf("resuming an open deployment failed: %v", err)
	}

	after, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}
	if after.Generation != before.Generation {
		t.Errorf("generation moved from %d to %d on a no-op resume",
			before.Generation, after.Generation)
	}
}

// THE READ ANSWERS THROUGH ANY QUERIER, which is what lets one implementation
// serve both a status command on the read-only pool and an allocation deciding
// inside its own write transaction. The writer side is covered where it matters,
// in the allocator's own sealed test.
func TestAdmissionIsReadableThroughTheReadOnlyPool(t *testing.T) {
	db := open(t)

	if _, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceOperator, Reason: "r", Actor: "a",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var inside Admission

	if err := db.View(t.Context(), func(q Querier) error {
		var err error
		inside, err = ReadAdmission(t.Context(), q)

		return err
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	if !inside.Sealed() {
		t.Error("a transaction read admission as open on a sealed deployment")
	}
}

// AN UNREADABLE MODE IS UNKNOWN, AND UNKNOWN IS SEALED. The schema's CHECK makes
// this unreachable through billet, which is exactly why the code must not trust
// the value: something that got past the constraint is not something to guess
// about.
func TestAnUnrecognisedModeIsSealed(t *testing.T) {
	db := open(t)

	// Written underneath the constraint, the way a future migration or a
	// hand-edited ledger could.
	if _, err := db.w.ExecContext(t.Context(),
		`PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatalf("relax constraints: %v", err)
	}
	if _, err := db.w.ExecContext(t.Context(),
		`UPDATE admission SET mode = 'quiescing' WHERE id = 1`); err != nil {
		t.Skipf("this SQLite build enforces the check regardless: %v", err)
	}

	a, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if a.Mode != AdmissionUnknown {
		t.Errorf("an unrecognised mode read as %v", a.Mode)
	}
	if !a.Sealed() {
		t.Error("an unrecognised mode admitted work")
	}
}

// AN OPERATOR'S SEAL SURVIVES A LIFECYCLE RESUME THAT ARRIVED AFTER IT.
//
// WHAT THIS DOES AND DOES NOT PROVE, stated because the distinction is easy to
// lose: it drives the end-to-end outcome — the operator's seal is still there —
// but it does NOT isolate the transactional fix, because the reseal completes
// before the resume is called and the older two-step implementation would have
// rejected this too. Pinning the interleaving itself needs two live handles
// racing inside the write, which is not something this package can stage today.
// The invariant it guards is real; the isolation is the residual.
func TestAnOperatorSealSurvivesALaterLifecycleResume(t *testing.T) {
	db := open(t)

	// A lifecycle seal, which `local up` is entitled to clear.
	own, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceLocalDown, Reason: "down", Actor: "billet local down",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// An operator reseals before the lifecycle command's resume commits. The
	// resume below still carries the generation it read, which is now stale —
	// and even if it were not, the provenance it authorised no longer describes
	// the row.
	if _, err := db.Seal(t.Context(), SealRequest{
		Expect: own.Generation, Provenance: ProvenanceOperator,
		Reason: "maintenance", Actor: "ops",
	}); err != nil {
		t.Fatalf("reseal: %v", err)
	}

	if _, err := db.Resume(t.Context(), ResumeRequest{
		Expect: own.Generation, Clears: ProvenanceLocalDown, Actor: "billet local up",
	}); err == nil {
		t.Fatal("a lifecycle command cleared an operator's seal it had authorised against an " +
			"earlier row")
	}

	a, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}
	if !a.Sealed() || a.Provenance != ProvenanceOperator {
		t.Fatalf("the operator's seal did not survive: %+v", a)
	}
}

// TWO RESUMES CARRYING THE SAME EXPECTATION SETTLE ONCE — the first commits, the
// second finds it already open and changes nothing rather than moving the
// generation underneath whoever looks next. Sequential rather than concurrent:
// what is being pinned is the second call's behaviour, not the interleaving.
func TestASecondResumeOfTheSameGenerationChangesNothing(t *testing.T) {
	db := open(t)

	sealed, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceOperator, Actor: "ops",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	first, err := db.Resume(t.Context(), ResumeRequest{Expect: sealed.Generation, Actor: "a"})
	if err != nil {
		t.Fatalf("first resume: %v", err)
	}

	// The second carries the same stale expectation the first did.
	second, err := db.Resume(t.Context(), ResumeRequest{Expect: sealed.Generation, Actor: "b"})
	if err != nil {
		t.Fatalf("a second resume of an open deployment failed: %v", err)
	}

	if second.Generation != first.Generation {
		t.Errorf("the second resume moved the generation from %d to %d",
			first.Generation, second.Generation)
	}
	if second.Sealed() {
		t.Error("the deployment is sealed after two resumes")
	}
}

// AN UNREADABLE ROW CANNOT BE RESUMED. Opening from a mode billet does not
// recognise would turn "I could not tell what this says" into "admit work" —
// the collapse the three-valued answer exists to prevent — and it would do it
// while bypassing the provenance check, since an unreadable row has no
// provenance to authorise against.
func TestAnUnreadableModeCannotBeResumed(t *testing.T) {
	db := open(t)

	if _, err := db.w.ExecContext(t.Context(),
		`PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatalf("relax constraints: %v", err)
	}
	if _, err := db.w.ExecContext(t.Context(),
		`UPDATE admission SET mode = 'quiescing', provenance = 'local-down' WHERE id = 1`); err != nil {
		t.Skipf("this SQLite build enforces the check regardless: %v", err)
	}

	before, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}
	if before.Mode != AdmissionUnknown {
		t.Fatalf("the fixture did not reach the case: mode is %v", before.Mode)
	}

	// Both doors: an explicit resume, and a lifecycle one that would otherwise
	// find a provenance it is entitled to clear.
	for _, req := range []ResumeRequest{
		{Expect: before.Generation, Actor: "ops"},
		{Expect: before.Generation, Clears: ProvenanceLocalDown, Actor: "billet local up"},
	} {
		if _, err := db.Resume(t.Context(), req); err == nil {
			t.Errorf("an unreadable admission row was opened by %+v", req)
		}
	}

	after, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}
	if after.Mode == AdmissionOpen {
		t.Fatal("an unreadable admission row now admits work")
	}
	if after.Generation != before.Generation {
		t.Errorf("a refused resume moved the generation from %d to %d",
			before.Generation, after.Generation)
	}
}
