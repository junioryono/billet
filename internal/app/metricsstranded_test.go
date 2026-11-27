package app

import (
	"database/sql"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// A JOB GITHUB STOPPED OFFERING IS COUNTED FROM THE LEDGER, and each recovery by its
// state: one under way however old, one settled within the day. A launch decided
// anything but stranded is not counted, nor a recovery settled before the day, and
// nothing at all is no sample. Every sample names a family the source declares.
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

	scrape := func(at time.Time) map[string][]metrics.Sample {
		t.Helper()

		snap, err := ledgerSnapshot(a, []config.Tier{tier}, func() time.Time { return at })(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		declared := map[string]int{}
		for _, f := range ledgerFamilies {
			declared[f.Name] = len(f.Labels)
		}

		for family, samples := range snap {
			n, ok := declared[family]
			if !ok {
				t.Errorf("the snapshot reports %s, which the source does not declare", family)
			}

			for _, s := range samples {
				if len(s.Labels) != n {
					t.Errorf("%s sample %v has %d labels, the family declares %d", family, s.Labels, len(s.Labels), n)
				}
			}
		}

		return snap
	}

	stranded := func(snap map[string][]metrics.Sample) float64 {
		t.Helper()

		got := snap[familyJobsStranded]
		if len(got) == 0 {
			return 0
		}
		if len(got) != 1 || got[0].Labels[0] != tier.Label {
			t.Fatalf("stranded samples = %+v", got)
		}

		return got[0].Value
	}

	byState := func(snap map[string][]metrics.Sample) map[string]float64 {
		t.Helper()

		out := map[string]float64{}
		for _, s := range snap[familyStrandedRecov] {
			if s.Labels[0] != tier.Label {
				t.Fatalf("a recovery sample for tier %q", s.Labels[0])
			}

			out[s.Labels[1]] = s.Value
		}

		return out
	}

	snap := scrape(now)
	if n := len(snap[familyJobsStranded]) + len(snap[familyStrandedRecov]); n != 0 {
		t.Fatalf("%d samples with nothing stranded: %+v %+v", n, snap[familyJobsStranded], snap[familyStrandedRecov])
	}

	// The ledger's own timestamp format, which alloc writes.
	stamp := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }
	twoDaysAgo, dayAndAnHourAgo := stamp(now.Add(-48*time.Hour)), stamp(now.Add(-25*time.Hour))

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, stranded, stranded_at)
			VALUES ('stranded-a', $1, '', 'stranded', $2), ('stranded-b', $1, '', 'stranded', $2),
			       ('offered', $1, '', 'offered', $2), ('expired', $1, '', 'expired', $2),
			       ('undecided', $1, '', '', '')`, tier.Label, stamp(now)); err != nil {
			return err
		}

		_, err := tx.ExecContext(t.Context(), `INSERT INTO stranded_recoveries
			(repo, run_id, run_attempt, tier, lease_id, job_name, ended_at, state, created_at, settled_at)
			VALUES ('o/r', 1, 1, $1, 'a', 'lint', $2, 'recovered', $2, $2),
			       ('o/r', 2, 1, $1, 'b', 'lint', $2, 'recovered', $2, $2),
			       ('o/r', 3, 1, $1, 'c', 'lint', $3, 'cancelling', $3, ''),
			       ('o/r', 4, 1, $1, 'd', 'lint', $4, 'expired', $4, $2),
			       ('o/r', 5, 1, $1, 'e', 'lint', $3, 'failed', $3, $3)`,
			tier.Label, stamp(now), twoDaysAgo, dayAndAnHourAgo)

		return err
	}); err != nil {
		t.Fatal(err)
	}

	snap = scrape(now)
	if got := stranded(snap); got != 2 {
		t.Errorf("stranded = %v, want the two decided stranded and neither the offered nor the expired", got)
	}

	want := map[string]float64{"recovered": 2, "cancelling": 1, "expired": 1}
	if got := byState(snap); len(got) != len(want) || got["recovered"] != 2 || got["cancelling"] != 1 ||
		got["expired"] != 1 {
		t.Errorf("recoveries by state = %v, want %v: the one under way for two days, the one expired a day "+
			"after it began, and not the one that failed two days ago", got, want)
	}

	// THE WINDOW RUNS BACK FROM THE SCRAPE: an hour before it closes everything settled
	// now still counts, an hour after it only the recovery still under way does.
	snap = scrape(now.Add(jobRerunWindow - time.Hour))
	if got := stranded(snap); got != 2 {
		t.Errorf("stranded an hour before the day ends = %v", got)
	}
	if got := byState(snap); len(got) != 3 {
		t.Errorf("recoveries an hour before the day ends = %v", got)
	}

	snap = scrape(now.Add(jobRerunWindow + time.Hour))
	if got := stranded(snap); got != 0 {
		t.Errorf("stranded past the day = %v", got)
	}
	if got := byState(snap); len(got) != 1 || got["cancelling"] != 1 {
		t.Errorf("recoveries past the day = %v, want only the one still under way", got)
	}
}
