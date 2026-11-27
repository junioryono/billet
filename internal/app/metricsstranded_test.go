package app

import (
	"database/sql"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// A JOB GITHUB STOPPED OFFERING IS COUNTED FROM THE LEDGER, and so is each recovery by
// its state, with the labels their families declare; outside the day neither is, a
// launch not decided stranded is not, and nothing decided is no sample.
func TestTheStrandedGaugesReadTheDecisionsAndTheRecoveries(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tier := metricsTestTier()
	now := time.Now()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatal(err)
	}

	read := ledgerSnapshot(a, []config.Tier{tier}, func() time.Time { return now })

	snap, err := read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(snap[familyJobsStranded]) + len(snap[familyStrandedRecov]); n != 0 {
		t.Fatalf("%d samples with nothing stranded: %+v %+v", n, snap[familyJobsStranded], snap[familyStrandedRecov])
	}

	// The ledger's own timestamp format, which alloc writes.
	stamp := now.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, stranded, stranded_at)
			VALUES ('stranded-a', $1, '', 'stranded', $2), ('stranded-b', $1, '', 'stranded', $2),
			       ('undecided', $1, '', '', '')`, tier.Label, stamp); err != nil {
			return err
		}

		_, err := tx.ExecContext(t.Context(), `INSERT INTO stranded_recoveries
			(repo, run_id, run_attempt, tier, lease_id, job_name, ended_at, state, created_at)
			VALUES ('o/r', 1, 1, $1, 'stranded-a', 'lint', $2, 'recovered', $2),
			       ('o/r', 2, 1, $1, 'stranded-b', 'lint', $2, 'cancelling', $2)`, tier.Label, stamp)

		return err
	}); err != nil {
		t.Fatal(err)
	}

	snap, err = read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	got := snap[familyJobsStranded]
	if len(got) != 1 || got[0].Value != 2 || len(got[0].Labels) != 1 || got[0].Labels[0] != tier.Label {
		t.Errorf("stranded samples = %+v, want 2 for %s", got, tier.Label)
	}

	states := map[string]float64{}
	for _, s := range snap[familyStrandedRecov] {
		if len(s.Labels) != 2 || s.Labels[0] != tier.Label {
			t.Fatalf("a recovery sample with labels %v", s.Labels)
		}

		states[s.Labels[1]] = s.Value
	}
	if len(states) != 2 || states[string(alloc.RecoveryRecovered)] != 1 || states[string(alloc.RecoveryCancelling)] != 1 {
		t.Errorf("recovery samples by state = %v, want one recovered and one cancelling", states)
	}

	// THE WINDOW RUNS BACK FROM THE SCRAPE: an hour before it closes both still count,
	// an hour after it neither does.
	closing := ledgerSnapshot(a, []config.Tier{tier}, func() time.Time { return now.Add(jobRerunWindow - time.Hour) })

	snap, err = closing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap[familyJobsStranded]; len(got) != 1 || got[0].Value != 2 {
		t.Errorf("an hour before the day ends = %+v", got)
	}
	if got := snap[familyStrandedRecov]; len(got) != 2 {
		t.Errorf("recoveries an hour before the day ends = %+v", got)
	}

	later := ledgerSnapshot(a, []config.Tier{tier}, func() time.Time { return now.Add(jobRerunWindow + time.Hour) })

	snap, err = later(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(snap[familyJobsStranded]) + len(snap[familyStrandedRecov]); n != 0 {
		t.Errorf("%d samples past the day: %+v %+v", n, snap[familyJobsStranded], snap[familyStrandedRecov])
	}
}
