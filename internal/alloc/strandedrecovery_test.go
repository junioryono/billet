package alloc

import (
	"database/sql"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// A RUN BILLET HAS TRIED TO RECOVER IS NEVER TRIED AGAIN, on any attempt: the list a
// recovery begins from leaves it out, and the write that begins one refuses it on its
// own, each without the other.
func TestARunIsRecoveredOnceByTheListAndByTheWrite(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("billet-4vcpu", 4, 16*config.GiB)})
	decided := time.Date(2026, 11, 22, 7, 55, 45, 0, time.UTC)

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, lease := range []string{"first", "again"} {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
				(lease_id, tier, queued_at, finished_at, repo, run_id, job_name, failure_reason, stranded, stranded_at)
				VALUES ($1, 'billet-4vcpu', $2, $2, 'acme/app', 42, 'Lint App', 'billet:launch-failed', 'stranded', $2)`,
				lease, ts(decided)); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	listed, err := a.StrandedToRecover(t.Context(), decided.Add(-time.Hour), 10)
	if err != nil || len(listed) != 2 {
		t.Fatalf("StrandedToRecover = %+v, %v; want both launches before any recovery", listed, err)
	}

	first := StrandedRecovery{Repository: "acme/app", RunID: 42, Attempt: 1, Tier: "billet-4vcpu", LeaseID: "first",
		JobName: "Lint App"}
	if begun, err := a.BeginStrandedRecovery(t.Context(), first); err != nil || !begun {
		t.Fatalf("the first recovery = %v, %v; want begun", begun, err)
	}

	if listed, err := a.StrandedToRecover(t.Context(), decided.Add(-time.Hour), 10); err != nil || len(listed) != 0 {
		t.Fatalf("StrandedToRecover = %+v, %v; want no launch of a run billet has tried to recover", listed, err)
	}

	again := first
	again.Attempt, again.LeaseID = 2, "again"
	if begun, err := a.BeginStrandedRecovery(t.Context(), again); err != nil || begun {
		t.Fatalf("a second recovery of the run on another attempt = %v, %v; want it refused", begun, err)
	}
}

// A RECOVERY MOVES AND ENDS ONLY FROM THE STATE IT WAS READ IN, and only into the
// states each allows.
func TestARecoveryMovesOnlyFromTheStateItWasReadIn(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("billet-4vcpu", 4, 16*config.GiB)})

	r := StrandedRecovery{Repository: "acme/app", RunID: 42, Attempt: 1, Tier: "billet-4vcpu", LeaseID: "first",
		JobName: "Lint App", State: RecoveryCancelling}
	if begun, err := a.BeginStrandedRecovery(t.Context(), r); err != nil || !begun {
		t.Fatalf("begin = %v, %v", begun, err)
	}

	stale := r
	stale.State = RecoveryRerunning
	if moved, err := a.MoveStrandedRecovery(t.Context(), stale, RecoveryRerunning, 2); err != nil || moved {
		t.Fatalf("a move from a state the row is not in = %v, %v; want refused", moved, err)
	}
	if _, err := a.MoveStrandedRecovery(t.Context(), r, RecoveryRecovered, 1); err == nil {
		t.Fatal("a move into an end was accepted")
	}
	if moved, err := a.MoveStrandedRecovery(t.Context(), r, RecoveryRerunning, 1); err != nil || !moved {
		t.Fatalf("move = %v, %v; want moved", moved, err)
	}
	r.State = RecoveryRerunning

	if _, err := a.SettleStrandedRecovery(t.Context(), r, RecoveryCancelling, ""); err == nil {
		t.Fatal("an end into a state between the requests was accepted")
	}
	if settled, err := a.SettleStrandedRecovery(t.Context(), r, RecoveryRecovered, ""); err != nil || !settled {
		t.Fatalf("settle = %v, %v; want settled", settled, err)
	}
	if settled, err := a.SettleStrandedRecovery(t.Context(), r, RecoveryFailed, "late"); err != nil || settled {
		t.Fatalf("a second end = %v, %v; want the first to stand", settled, err)
	}

	open, err := a.OpenStrandedRecoveries(t.Context(), 10)
	if err != nil || len(open) != 0 {
		t.Fatalf("OpenStrandedRecoveries = %+v, %v; want none once it ended", open, err)
	}
}

// A TIER ASKS FROM THE FIRST TIME IT DID, AND ONE THAT STOPS IS FORGOTTEN, so asking
// again starts again.
func TestATierAsksForRecoveriesFromWhenItFirstDid(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	clock := now
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("billet-4vcpu", 4, 16*config.GiB)}, WithClock(func() time.Time { return clock }))

	asked, err := a.AskStranded(t.Context(), []string{"billet-4vcpu"})
	if err != nil || !asked["billet-4vcpu"].Equal(now) {
		t.Fatalf("AskStranded = %v, %v; want since now", asked, err)
	}

	clock = now.Add(time.Hour)
	if asked, err := a.AskStranded(t.Context(), []string{"billet-4vcpu"}); err != nil || !asked["billet-4vcpu"].Equal(now) {
		t.Fatalf("asking again = %v, %v; want the first time kept", asked, err)
	}

	if asked, err := a.AskStranded(t.Context(), nil); err != nil || len(asked) != 0 {
		t.Fatalf("no tier asking = %v, %v; want none", asked, err)
	}
	if asked, err := a.AskStranded(t.Context(), []string{"billet-4vcpu"}); err != nil ||
		!asked["billet-4vcpu"].Equal(clock) {
		t.Fatalf("asking after stopping = %v, %v; want since then", asked, err)
	}
}
