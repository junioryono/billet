package state

import (
	"errors"
	"strings"
	"testing"
)

// sealForForce puts the deployment into the one state a force may be taken
// against, and returns the generation it settled on.
func sealForForce(t *testing.T, db *DB) int64 {
	t.Helper()

	current, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	sealed, err := db.Seal(t.Context(), SealRequest{
		Expect:     current.Generation,
		Provenance: ProvenanceOperator,
		Reason:     "the fleet is wedged",
		Actor:      "ops@example.com",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	return sealed.Generation
}

func oneTarget(lease string) []ForceTarget {
	return []ForceTarget{{
		LeaseID: lease, Tier: "linux-4", Node: "epyc-1", RunID: "9001",
		SchedulerRequest: 7, Phase: "busy",
	}}
}

// A FORCE MAY NOT BE TAKEN AGAINST A DEPLOYMENT THAT IS STILL ADMITTING WORK.
//
// The command enumerates a set, shows it to a person, and acts on their answer.
// With admission open, a job accepted between the enumeration and the answer is
// destroyed without ever having appeared in the list that was approved.
func TestAForceIsRefusedWhileAdmissionIsOpen(t *testing.T) {
	db := open(t)

	_, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		Reason: "wedged", Actor: "ops", Targets: oneTarget("l1"),
	})
	if !errors.Is(err, ErrForceDestroyNotSealed) {
		t.Fatalf("RequestForceDestroy on an open deployment: %v, want ErrForceDestroyNotSealed", err)
	}
}

// A SHUTDOWN'S SEAL IS NOT AN OPERATOR'S. `billet local up` clears a local-down
// seal, so authorising destruction against one would let an ordinary restart
// reopen admission underneath a force that is still running.
func TestAForceIsRefusedAgainstAShutdownSeal(t *testing.T) {
	db := open(t)

	if _, err := db.Seal(t.Context(), SealRequest{
		Provenance: ProvenanceLocalDown, Reason: "stopping", Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	current, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	_, err = db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: current.Generation,
		Reason:          "wedged", Actor: "ops", Targets: oneTarget("l1"),
	})
	if !errors.Is(err, ErrForceDestroyNotSealed) {
		t.Fatalf("RequestForceDestroy against a local-down seal: %v, want ErrForceDestroyNotSealed",
			err)
	}
}

// ADMISSION MOVING BETWEEN THE LIST AND THE ANSWER INVALIDATES THE LIST. The
// operator approved a set enumerated against one state of the deployment; a
// resume and reseal in between means that set describes a deployment that has
// admitted work since.
func TestAForceIsRefusedWhenAdmissionMovedUnderneathIt(t *testing.T) {
	db := open(t)
	generation := sealForForce(t, db)

	// Somebody else resumes and reseals while the operator is reading the list.
	if _, err := db.Resume(t.Context(), ResumeRequest{
		Expect: generation, Clears: ProvenanceOperator, Actor: "someone-else",
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	sealForForce(t, db)

	_, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation,
		Reason:          "wedged", Actor: "ops", Targets: oneTarget("l1"),
	})
	if !errors.Is(err, ErrAdmissionGeneration) {
		t.Fatalf("RequestForceDestroy against a stale seal: %v, want ErrAdmissionGeneration", err)
	}
}

// A REASON IS NOT OPTIONAL. It is the only record of why somebody's build was
// failed on purpose.
func TestAForceNeedsAReasonAndTargets(t *testing.T) {
	db := open(t)
	generation := sealForForce(t, db)

	if _, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Actor: "ops", Targets: oneTarget("l1"),
	}); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Errorf("a force with no reason: %v, want an error naming the reason", err)
	}

	if _, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "wedged", Actor: "ops",
	}); err == nil || !strings.Contains(err.Error(), "destroy nothing") {
		t.Errorf("a force with no targets: %v, want an error saying it would destroy nothing", err)
	}
}

// ONLY ONE FORCE RUNS AT A TIME. Two would each enumerate a set the other was
// midway through destroying, and neither record would then describe what
// happened to anything.
func TestASecondForceIsRefusedWhileOneIsOpen(t *testing.T) {
	db := open(t)
	generation := sealForForce(t, db)

	if _, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "wedged", Actor: "ops",
		Targets: oneTarget("l1"),
	}); err != nil {
		t.Fatalf("RequestForceDestroy: %v", err)
	}

	_, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "also wedged", Actor: "ops2",
		Targets: oneTarget("l2"),
	})
	if !errors.Is(err, ErrForceDestroyOpen) {
		t.Fatalf("a second force: %v, want ErrForceDestroyOpen", err)
	}
}

// THE RECORD SURVIVES THE PROCESS, because a listener acts on it a poll later
// and a control plane may restart in between. A force held in memory would be
// silently forgotten by exactly the restart an operator forces after.
func TestAForceSurvivesReopeningTheLedger(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	generation := sealForForce(t, db)

	recorded, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "wedged", Actor: "ops",
		Targets: oneTarget("l1"),
	})
	if err != nil {
		t.Fatalf("RequestForceDestroy: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	t.Cleanup(func() { _ = again.Close() })

	open, found, err := again.OpenForceDestroy(t.Context())
	if err != nil {
		t.Fatalf("OpenForceDestroy: %v", err)
	}
	if !found {
		t.Fatal("a force-destroy did not survive reopening the ledger")
	}
	if open.Generation != recorded.Generation {
		t.Errorf("reopened at generation %d, want %d", open.Generation, recorded.Generation)
	}
	if open.Reason != "wedged" || open.Actor != "ops" {
		t.Errorf("attribution did not survive: %q by %q", open.Reason, open.Actor)
	}

	targets, err := again.PendingForceTargets(t.Context(), open.Generation, "linux-4")
	if err != nil {
		t.Fatalf("PendingForceTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].LeaseID != "l1" || targets[0].SchedulerRequest != 7 {
		t.Errorf("targets did not survive: %+v", targets)
	}
}

// A LISTENER ONLY SEES ITS OWN TIER. Acting on another tier's lease would be
// tearing down compute it never held and cannot release.
func TestPendingTargetsAreScopedToOneTier(t *testing.T) {
	db := open(t)
	generation := sealForForce(t, db)

	recorded, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "wedged", Actor: "ops",
		Targets: []ForceTarget{
			{LeaseID: "l1", Tier: "linux-4", SchedulerRequest: 1, Phase: "busy"},
			{LeaseID: "l2", Tier: "macos-8", SchedulerRequest: 2, Phase: "busy"},
		},
	})
	if err != nil {
		t.Fatalf("RequestForceDestroy: %v", err)
	}

	linux, err := db.PendingForceTargets(t.Context(), recorded.Generation, "linux-4")
	if err != nil {
		t.Fatalf("PendingForceTargets: %v", err)
	}
	if len(linux) != 1 || linux[0].LeaseID != "l1" {
		t.Errorf("linux-4 sees %+v, want only l1", linux)
	}
}

// THE REQUEST COMPLETES WHEN NOTHING IS PENDING, AND NOT BEFORE. A request that
// stayed open after its last target settled would block every later force with
// nothing saying why.
func TestAForceCompletesOnlyWhenEveryTargetIsSettled(t *testing.T) {
	db := open(t)
	generation := sealForForce(t, db)

	recorded, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "wedged", Actor: "ops",
		Targets: []ForceTarget{
			{LeaseID: "l1", Tier: "linux-4", SchedulerRequest: 1, Phase: "busy"},
			{LeaseID: "l2", Tier: "linux-4", SchedulerRequest: 2, Phase: "online"},
		},
	})
	if err != nil {
		t.Fatalf("RequestForceDestroy: %v", err)
	}

	if err := db.SettleForceTarget(t.Context(), recorded.Generation, "l1",
		ForceTargetDestroyed, ""); err != nil {
		t.Fatalf("SettleForceTarget: %v", err)
	}

	if _, found, err := db.OpenForceDestroy(t.Context()); err != nil {
		t.Fatalf("OpenForceDestroy: %v", err)
	} else if !found {
		t.Fatal("the force completed with a target still pending")
	}

	// A TARGET THAT COULD NOT BE DESTROYED STILL COMPLETES THE REQUEST. The
	// alternative is a force nothing can ever finish, blocking the next one; the
	// row keeps saying `failed`, which is what an operator acts on.
	if err := db.SettleForceTarget(t.Context(), recorded.Generation, "l2",
		ForceTargetFailed, "the destroy did not confirm"); err != nil {
		t.Fatalf("SettleForceTarget: %v", err)
	}

	if _, found, err := db.OpenForceDestroy(t.Context()); err != nil {
		t.Fatalf("OpenForceDestroy: %v", err)
	} else if found {
		t.Error("the force stayed open after every target settled")
	}

	last, found, err := db.LatestForceDestroy(t.Context())
	if err != nil {
		t.Fatalf("LatestForceDestroy: %v", err)
	}
	if !found || last.State != ForceCompleted || last.CompletedAt == "" {
		t.Errorf("the finished force reads %+v, want completed with a time", last)
	}
}

// SETTLING TWICE IS NOT AN ERROR. A listener that restarts re-observes the
// durable request and may act on a target a previous incarnation already
// finished; the destroy underneath is idempotent and so is this.
func TestSettlingATargetTwiceIsAccepted(t *testing.T) {
	db := open(t)
	generation := sealForForce(t, db)

	recorded, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "wedged", Actor: "ops",
		Targets: oneTarget("l1"),
	})
	if err != nil {
		t.Fatalf("RequestForceDestroy: %v", err)
	}

	for range 2 {
		if err := db.SettleForceTarget(t.Context(), recorded.Generation, "l1",
			ForceTargetDestroyed, ""); err != nil {
			t.Fatalf("SettleForceTarget: %v", err)
		}
	}

	targets, err := db.ForceTargets(t.Context(), recorded.Generation)
	if err != nil {
		t.Fatalf("ForceTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].State != ForceTargetDestroyed {
		t.Errorf("targets read %+v, want one destroyed", targets)
	}
}

// AN UNSETTLED DISPOSITION IS REFUSED, so `pending` cannot be written back over
// a target that has already been acted on.
func TestSettleRefusesADispositionThatIsNotSettled(t *testing.T) {
	db := open(t)

	err := db.SettleForceTarget(t.Context(), 1, "l1", ForceTargetPending, "")
	if err == nil || !strings.Contains(err.Error(), "not a settled") {
		t.Errorf("settling as pending: %v, want a refusal", err)
	}
}

// A FORCE MAY BE TAKEN AGAIN ONCE THE LAST ONE FINISHED, and the generation
// moves so the two are distinguishable in the record.
func TestASecondForceIsAllowedAfterTheFirstCompletes(t *testing.T) {
	db := open(t)
	generation := sealForForce(t, db)

	first, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "wedged", Actor: "ops",
		Targets: oneTarget("l1"),
	})
	if err != nil {
		t.Fatalf("RequestForceDestroy: %v", err)
	}

	if err := db.SettleForceTarget(t.Context(), first.Generation, "l1",
		ForceTargetDestroyed, ""); err != nil {
		t.Fatalf("SettleForceTarget: %v", err)
	}

	second, err := db.RequestForceDestroy(t.Context(), ForceDestroyRequest{
		ExpectAdmission: generation, Reason: "still wedged", Actor: "ops",
		Targets: oneTarget("l2"),
	})
	if err != nil {
		t.Fatalf("a second force after the first finished: %v", err)
	}
	if second.Generation <= first.Generation {
		t.Errorf("the second force is generation %d, want above %d",
			second.Generation, first.Generation)
	}
}
