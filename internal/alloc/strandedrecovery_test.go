package alloc

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// recoveryLedger is an allocator at a clock the test holds, whose tier billet-4vcpu
// started asking for recoveries an hour before decided, and whose tier billet-2vcpu
// never did.
func recoveryLedger(t *testing.T, decided time.Time) *Allocator {
	t.Helper()

	clock := decided.Add(-time.Hour)
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("billet-4vcpu", 4, 16*config.GiB), tier("billet-2vcpu", 2, 8*config.GiB)},
		WithClock(func() time.Time { return clock }))
	if _, err := a.AskStranded(t.Context(), []string{"billet-4vcpu"}); err != nil {
		t.Fatalf("AskStranded: %v", err)
	}
	clock = decided

	return a
}

// strandRow seeds a launch decided stranded at decided, on tier, whose lease ended at
// ended.
func strandRow(t *testing.T, a *Allocator, lease, tierLabel string, run int64, job string, ended, decided time.Time) {
	t.Helper()

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, finished_at, repo, run_id, job_name, failure_reason, stranded, stranded_at)
			VALUES ($1, $2, $3, $3, 'acme/app', $4, $5, 'billet:launch-failed', 'stranded', $6)`,
			lease, tierLabel, ts(ended), run, job, ts(decided))

		return err
	}); err != nil {
		t.Fatalf("seed %s: %v", lease, err)
	}
}

// A RUN BILLET HAS TRIED TO RECOVER IS NEVER TRIED AGAIN, on any attempt: the list a
// recovery begins from leaves it out, and the write that begins one refuses it on its
// own, each without the other.
func TestARunIsRecoveredOnceByTheListAndByTheWrite(t *testing.T) {
	t.Parallel()

	decided := time.Date(2026, 11, 22, 7, 55, 45, 0, time.UTC)
	a := recoveryLedger(t, decided)
	strandRow(t, a, "first", "billet-4vcpu", 42, "Lint App", decided.Add(-20*time.Minute), decided)
	strandRow(t, a, "again", "billet-4vcpu", 42, "Build iOS", decided.Add(-15*time.Minute), decided.Add(time.Second))

	listed, err := a.StrandedToRecover(t.Context(), decided.Add(-time.Hour), StrandedCursor{}, 10)
	if err != nil || len(listed) != 2 {
		t.Fatalf("StrandedToRecover = %+v, %v; want both launches before any recovery", listed, err)
	}

	first := StrandedRecovery{Repository: "acme/app", RunID: 42, Attempt: 1, Tier: "billet-4vcpu", LeaseID: "first",
		JobName: "Lint App", Ended: decided.Add(-20 * time.Minute)}
	if begun, err := a.BeginStrandedRecovery(t.Context(), first); err != nil || !begun {
		t.Fatalf("the first recovery = %v, %v; want begun", begun, err)
	}

	if listed, err := a.StrandedToRecover(t.Context(), decided.Add(-time.Hour), StrandedCursor{}, 10); err != nil ||
		len(listed) != 0 {
		t.Fatalf("StrandedToRecover = %+v, %v; want no launch of a run billet has tried to recover", listed, err)
	}

	again := first
	again.Attempt, again.LeaseID, again.Ended = 2, "again", decided.Add(-15*time.Minute)
	if begun, err := a.BeginStrandedRecovery(t.Context(), again); err != nil || begun {
		t.Fatalf("a second recovery of the run on another attempt = %v, %v; want it refused", begun, err)
	}
}

// ONLY WHAT A TIER ASKING MAY TAKE IS LISTED: a launch of a tier that does not ask,
// or one that ended before its tier asked, is left out, so it never fills a page.
// What is listed is paged from a cursor.
func TestOnlyWhatATierAskingMayTakeIsListedAndPaged(t *testing.T) {
	t.Parallel()

	decided := time.Date(2026, 11, 22, 7, 55, 45, 0, time.UTC)
	a := recoveryLedger(t, decided)
	strandRow(t, a, "not-asking", "billet-2vcpu", 41, "Lint App", decided.Add(-20*time.Minute), decided)
	strandRow(t, a, "before-asking", "billet-4vcpu", 40, "Lint App", decided.Add(-2*time.Hour), decided)
	for i := range 3 {
		strandRow(t, a, fmt.Sprintf("ok-%d", i), "billet-4vcpu", int64(50+i), "Lint App", decided.Add(-20*time.Minute),
			decided.Add(time.Duration(i)*time.Second))
	}

	page, err := a.StrandedToRecover(t.Context(), decided.Add(-time.Hour), StrandedCursor{}, 2)
	if err != nil || len(page) != 2 || page[0].LeaseID != "ok-0" || page[1].LeaseID != "ok-1" {
		t.Fatalf("the first page = %+v, %v; want ok-0 and ok-1 alone", page, err)
	}
	page, err = a.StrandedToRecover(t.Context(), decided.Add(-time.Hour), page[1].After(), 2)
	if err != nil || len(page) != 1 || page[0].LeaseID != "ok-2" {
		t.Fatalf("the page after = %+v, %v; want ok-2 alone", page, err)
	}
}

// EVERY CANCEL IS AUTHORISED IN A WRITE THAT ASKS THE LEDGER AGAIN: an assignment of
// the run since the launch ended that is, or may be, its job refuses the first cancel
// and a cancel sent again alike.
func TestEveryCancelAsksTheLedgerAgainInItsOwnWrite(t *testing.T) {
	t.Parallel()

	decided := time.Date(2026, 11, 22, 7, 55, 45, 0, time.UTC)
	a := recoveryLedger(t, decided)
	ended := decided.Add(-20 * time.Minute)

	r := StrandedRecovery{Repository: "acme/app", RunID: 42, Attempt: 1, Tier: "billet-4vcpu", LeaseID: "first",
		JobName: "Lint App", Ended: ended, State: RecoveryCancelling}
	if begun, err := a.BeginStrandedRecovery(t.Context(), r); err != nil || !begun {
		t.Fatalf("begin = %v, %v", begun, err)
	}

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, run_id)
			VALUES ('booting', 'billet-4vcpu', $1, 42)`, ts(ended.Add(5*time.Minute)))

		return err
	}); err != nil {
		t.Fatalf("seed an assignment: %v", err)
	}

	if moved, err := a.MoveStrandedRecovery(t.Context(), r, RecoveryCancelling, 2); !errors.Is(err, ErrMayBeOffered) ||
		moved {
		t.Fatalf("a cancel sent again = %v, %v; want ErrMayBeOffered", moved, err)
	}
	// THE RE-RUN IS NOT A CANCEL: once the attempt is over, the run is re-run whatever
	// was assigned since.
	if moved, err := a.MoveStrandedRecovery(t.Context(), r, RecoveryRerunning, 1); err != nil || !moved {
		t.Fatalf("the move to the re-run = %v, %v; want moved", moved, err)
	}

	other := r
	other.RunID, other.LeaseID = 43, "other"
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, run_id, job_name)
			VALUES ('again', 'billet-4vcpu', $1, 43, 'Lint App')`, ts(ended.Add(time.Minute)))

		return err
	}); err != nil {
		t.Fatalf("seed an assignment: %v", err)
	}
	if begun, err := a.BeginStrandedRecovery(t.Context(), other); !errors.Is(err, ErrMayBeOffered) || begun {
		t.Fatalf("a first cancel after the job was assigned again = %v, %v; want ErrMayBeOffered", begun, err)
	}
}

// A RECOVERY MOVES AND ENDS ONLY FROM THE STATE IT WAS READ IN, and only into the
// states each allows.
func TestARecoveryMovesOnlyFromTheStateItWasReadIn(t *testing.T) {
	t.Parallel()

	decided := time.Date(2026, 11, 22, 7, 55, 45, 0, time.UTC)
	a := recoveryLedger(t, decided)

	r := StrandedRecovery{Repository: "acme/app", RunID: 42, Attempt: 1, Tier: "billet-4vcpu", LeaseID: "first",
		JobName: "Lint App", Ended: decided.Add(-20 * time.Minute), State: RecoveryCancelling}
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

	open, err := a.OpenStrandedRecoveries(t.Context(), RecoveryCursor{}, 10)
	if err != nil || len(open) != 1 || !open[0].Ended.Equal(r.Ended) || open[0].JobName != r.JobName {
		t.Fatalf("OpenStrandedRecoveries = %+v, %v; want the one, with its launch's end and job", open, err)
	}

	if _, err := a.SettleStrandedRecovery(t.Context(), r, RecoveryCancelling, ""); err == nil {
		t.Fatal("an end into a state between the requests was accepted")
	}
	if settled, err := a.SettleStrandedRecovery(t.Context(), r, RecoveryRecovered, ""); err != nil || !settled {
		t.Fatalf("settle = %v, %v; want settled", settled, err)
	}
	if settled, err := a.SettleStrandedRecovery(t.Context(), r, RecoveryFailed, "late"); err != nil || settled {
		t.Fatalf("a second end = %v, %v; want the first to stand", settled, err)
	}

	if open, err := a.OpenStrandedRecoveries(t.Context(), RecoveryCursor{}, 10); err != nil || len(open) != 0 {
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
