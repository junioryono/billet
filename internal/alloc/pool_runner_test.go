package alloc

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestPoolRunnerBindsTheActualJobWithoutChangingItsComputeLease(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	runner := PoolRunner{LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerName: "billet-" + lease.ID}
	if err := a.RegisterPoolRunner(t.Context(), runner); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	started, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		22, 202, "actual-job")
	if err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if started.LaunchRequestID != 11 || started.ActualRequestID != 22 ||
		started.Status != PoolRunnerBusy || started.RunnerID != 77 {
		t.Fatalf("started binding = %+v", started)
	}

	resolved, err := a.PoolRunnerByName(t.Context(), runner.RunnerName)
	if err != nil {
		t.Fatalf("PoolRunnerByName: %v", err)
	}
	if resolved.LeaseID != lease.ID || resolved.JobID != "actual-job" {
		t.Errorf("resolved binding = %+v", resolved)
	}

	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 77, runner.RunnerName,
		33, 303, "different-job"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a busy runner accepted a second job: %v", err)
	}
}

func TestOnlyIdlePoolMembersAreScaleDownCandidates(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 4, MaxMemory: 8 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	for request := int64(11); request <= 12; request++ {
		lease := reserve(t, a, "linux")
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 100+request, request); err != nil {
			t.Fatalf("Assign(%d): %v", request, err)
		}
		name := "billet-" + lease.ID
		if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
			LaunchRequestID: request, RunnerName: name}); err != nil {
			t.Fatalf("RegisterPoolRunner(%d): %v", request, err)
		}
		if request == 11 {
			if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
				request, 111, "busy-job"); err != nil {
				t.Fatalf("StartPoolRunner: %v", err)
			}
		}
	}

	idle, err := a.IdlePoolRunners(t.Context(), "linux")
	if err != nil {
		t.Fatalf("IdlePoolRunners: %v", err)
	}
	if len(idle) != 1 || idle[0].LaunchRequestID != 12 {
		t.Fatalf("idle runners = %+v, want only request 12", idle)
	}
}

func TestRecoveredBusyPoolRunnerYieldsToAuthoritativeJobStarted(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	name := "billet-" + lease.ID
	if err := a.PreserveRecoveredBusyPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerID: 71, RunnerName: name,
	}); err != nil {
		t.Fatalf("PreserveRecoveredBusyPoolRunner: %v", err)
	}
	started, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
		22, 202, "actual-job")
	if err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if started.ActualRequestID != 22 || started.RunID != 202 || started.JobID != "actual-job" {
		t.Fatalf("started recovered binding = %+v", started)
	}
	settled, err := a.RetireRecoveredPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerID: 71, RunnerName: name,
	})
	if err != nil {
		t.Fatalf("RetireRecoveredPoolRunner: %v", err)
	}
	if settled.Status != PoolRunnerBusy || settled.ActualRequestID != 22 {
		t.Fatalf("recovery retired an authoritative started binding: %+v", settled)
	}
}

func TestRecoveredPlaceholderCanRetireAfterGitHubStopsReportingBusy(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.PreserveRecoveredBusyPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerID: 71, RunnerName: "billet-" + lease.ID,
	}); err != nil {
		t.Fatalf("PreserveRecoveredBusyPoolRunner: %v", err)
	}
	settled, err := a.RetireRecoveredPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11,
		RunnerID: 71, RunnerName: "billet-" + lease.ID,
	})
	if err != nil {
		t.Fatalf("RetireRecoveredPoolRunner: %v", err)
	}
	if settled.Status != PoolRunnerRetiring {
		t.Fatalf("recovered placeholder status = %q, want retiring", settled.Status)
	}
}

func TestRecoveredRetirementFencesALateLegacyJobStarted(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	name := "billet-" + lease.ID
	retired, err := a.RetireRecoveredPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: "linux", LaunchRequestID: 11, RunnerName: name,
	})
	if err != nil {
		t.Fatalf("RetireRecoveredPoolRunner: %v", err)
	}
	if retired.Status != PoolRunnerRetiring || retired.RunnerID != 0 {
		t.Fatalf("recovery fence = %+v", retired)
	}

	// The delayed JobStarted path first registers the legacy name, then binds its
	// actual job. Registration is idempotent against the fence; binding must lose.
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 11, RunnerName: name}); err != nil {
		t.Fatalf("late legacy registration: %v", err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
		22, 202, "late-job"); !errors.Is(err, ErrConflict) {
		t.Fatalf("late JobStarted crossed recovery fence: %v", err)
	}
	got, err := a.PoolRunnerByLease(t.Context(), lease.ID)
	if err != nil || got.Status != PoolRunnerRetiring {
		t.Fatalf("late JobStarted changed recovery fence: %+v, err %v", got, err)
	}
}

func TestSettledPoolIdentitySurvivesUntilSourceAcknowledgement(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	name := "github-returned-name"
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 11, RunnerID: 71, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
		22, 202, "actual-job"); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if err := a.SettlePoolRunner(t.Context(), "linux", 11); err != nil {
		t.Fatalf("SettlePoolRunner: %v", err)
	}
	member, err := a.PoolRunnerByName(t.Context(), name)
	if err != nil || member.Status != PoolRunnerRetired || member.ActualRequestID != 22 {
		t.Fatalf("settled tombstone = %+v, err %v", member, err)
	}
	if err := a.AcknowledgePoolRunner(t.Context(), "linux", 11); err != nil {
		t.Fatalf("AcknowledgePoolRunner: %v", err)
	}
	if _, err := a.PoolRunnerByName(t.Context(), name); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("acknowledged pool identity still exists: %v", err)
	}
}

// RETIRING AN ALREADY-RETIRED MEMBER IS ACCEPTED AND MOVES NOTHING BACKWARDS.
//
// A retirement is idempotent on purpose -- it is reached from a scale-down that
// may be retried -- so the answer is nil rather than an error. What must not
// happen is the status going back: `retired` means compute settled and only
// GitHub's acknowledgement is outstanding, while `retiring` means teardown is
// still to come, so moving one to the other reopens work that has already
// finished.
//
// THE STATUS FILTER IN THE CLAIM IS WHAT DELIVERS THAT, and nothing tested it:
// measured, removing `AND status IN ('idle','busy')` left every other test in
// this package green while letting a settled member be re-marked retiring.
func TestRetiringAnAlreadyRetiredPoolMemberMovesNothingBackwards(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	name := "billet-retired"
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 11, RunnerID: 71, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
		22, 202, "actual-job"); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}

	if err := a.SettlePoolRunner(t.Context(), "linux", 11); err != nil {
		t.Fatalf("SettlePoolRunner: %v", err)
	}

	if err := a.RetirePoolRunner(t.Context(), lease.ID); err != nil {
		t.Fatalf("retiring a settled member: %v, want it accepted", err)
	}

	member, err := a.PoolRunnerByName(t.Context(), name)
	if err != nil {
		t.Fatalf("PoolRunnerByName: %v", err)
	}

	if member.Status != PoolRunnerRetired {
		t.Errorf("a repeated retirement moved the member from retired back to %q, "+
			"reopening a teardown that had already finished", member.Status)
	}
}

// AN ACKNOWLEDGEMENT THAT ARRIVES BEFORE SETTLEMENT STILL REMOVES THE IDENTITY.
//
// The two orders are both real: GitHub can acknowledge the completion before
// billet finishes tearing the compute down, or after. TestSettledPoolIdentity...
// covers settle-then-acknowledge; this is the other order, where the
// acknowledgement has to be RECORDED on a member that is still busy and read
// back by the settlement. Measured: writing source_acknowledged = 0 instead of 1
// left every other test in this package green, so nothing proved the record was
// ever made.
func TestAnAcknowledgementBeforeSettlementRemovesTheIdentity(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	lease := reserve(t, a, "linux")

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	name := "billet-early-ack"
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 11, RunnerID: 71, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 71, name,
		22, 202, "actual-job"); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}

	// ACKNOWLEDGED WHILE STILL BUSY: the row must survive, because the compute has
	// not settled and a later settlement still has to resolve to it.
	if err := a.AcknowledgePoolRunner(t.Context(), "linux", 11); err != nil {
		t.Fatalf("AcknowledgePoolRunner: %v", err)
	}

	member, err := a.PoolRunnerByName(t.Context(), name)
	if err != nil {
		t.Fatalf("an acknowledged busy member was removed before settling: %v", err)
	}

	if !member.SourceAcknowledged {
		t.Fatal("the acknowledgement was not recorded, so the settlement below cannot " +
			"know GitHub will not redeliver")
	}

	if err := a.SettlePoolRunner(t.Context(), "linux", 11); err != nil {
		t.Fatalf("SettlePoolRunner: %v", err)
	}

	if _, err := a.PoolRunnerByName(t.Context(), name); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("the identity survived an acknowledged settlement: %v", err)
	}
}

// JOB IDENTITIES AND POOL-SLOT IDENTITIES SHARE ONE NUMBER SPACE.
//
// They live in two tables, each with its own UNIQUE constraint, so nothing in the
// schema stops a job and a pool slot being handed the SAME negative id -- and
// billet keys concurrent work on that number. Both minting statements therefore
// take their minimum across BOTH tables.
//
// BOTH DIRECTIONS, because one was covered and the other was not: measured,
// making the pool-slot mint read only its own table left every other test in this
// package green.
func TestJobAndPoolSlotIdentitiesShareOneNumberSpace(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB}, nil)

	jobFirst, err := a.IdentifyDirectJob(t.Context(), "job-a")
	if err != nil {
		t.Fatalf("IdentifyDirectJob: %v", err)
	}

	slotAfter, err := a.IdentifyPoolSlot(t.Context(), "lease-a")
	if err != nil {
		t.Fatalf("IdentifyPoolSlot: %v", err)
	}

	if jobFirst == slotAfter {
		t.Errorf("a pool slot was minted the id %d a job already holds; the two "+
			"identity tables are not sharing one number space", slotAfter)
	}

	slotFirst, err := a.IdentifyPoolSlot(t.Context(), "lease-b")
	if err != nil {
		t.Fatalf("IdentifyPoolSlot: %v", err)
	}

	jobAfter, err := a.IdentifyDirectJob(t.Context(), "job-b")
	if err != nil {
		t.Fatalf("IdentifyDirectJob: %v", err)
	}

	if slotFirst == jobAfter {
		t.Errorf("a job was minted the id %d a pool slot already holds", jobAfter)
	}

	// AND EVERY ID IS DISTINCT AND NEGATIVE. Zero is never a scheduler identity,
	// and a positive one could collide with GitHub's own request ids.
	seen := map[int64]string{}
	for name, id := range map[string]int64{
		"job-a": jobFirst, "lease-a": slotAfter, "lease-b": slotFirst, "job-b": jobAfter,
	} {
		if id >= 0 {
			t.Errorf("%s was minted %d; a scheduler identity must be negative", name, id)
		}

		if other, ok := seen[id]; ok {
			t.Errorf("%s and %s were both minted %d", name, other, id)
		}

		seen[id] = name
	}
}
