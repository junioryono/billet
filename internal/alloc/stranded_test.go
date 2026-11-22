package alloc

import (
	"database/sql"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// A LAUNCH THAT STARTED NOTHING IS LISTED WITH WHAT ITS JOB IS READ BY, until its job
// is decided, and is decided once: a second decision changes nothing and says so.
func TestALaunchMissIsListedUntilItIsDecidedOnce(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("billet-4vcpu", 4, 16*config.GiB)})
	ended := time.Date(2026, 11, 20, 7, 55, 45, 0, time.UTC)

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, row := range []struct{ lease, reason string }{
			{"missed", "billet:launch-failed"}, {"ran", ""}, {"held", "billet:held-past-limit"},
		} {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
				(lease_id, tier, queued_at, finished_at, repo, run_id, github_job_id, job_name, failure_reason)
				VALUES ($1, 'billet-4vcpu', $2, $2, 'acme/app', 42, 'job-1', 'Lint App', $3)`,
				row.lease, ts(ended), row.reason); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	misses, err := a.LaunchMisses(t.Context(), ended.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("LaunchMisses: %v", err)
	}
	want := LaunchMiss{LeaseID: "missed", Tier: "billet-4vcpu", Owner: "acme", Repository: "app", RunID: 42,
		JobID: "job-1", JobName: "Lint App", Ended: ended}
	if len(misses) != 1 || misses[0] != want {
		t.Fatalf("LaunchMisses = %+v, want only %+v", misses, want)
	}

	if decided, err := a.DecideStranded(t.Context(), "missed", StrandedJob, ended.Add(20*time.Minute)); err != nil || !decided {
		t.Fatalf("the first decision = %v, %v; want decided", decided, err)
	}
	if decided, err := a.DecideStranded(t.Context(), "missed", StrandedOffered, ended.Add(30*time.Minute)); err != nil || decided {
		t.Fatalf("a second decision = %v, %v; want it refused", decided, err)
	}
	if _, err := a.DecideStranded(t.Context(), "missed", "lost", ended); err == nil {
		t.Fatal("a decision outside the closed set was accepted")
	}

	var got string
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT stranded FROM job_history WHERE lease_id = 'missed'`).Scan(&got)
	}); err != nil {
		t.Fatalf("read the decision: %v", err)
	}
	if got != string(StrandedJob) {
		t.Fatalf("the row says %q, want the first decision", got)
	}

	if misses, err := a.LaunchMisses(t.Context(), ended.Add(-time.Hour), 10); err != nil || len(misses) != 0 {
		t.Fatalf("a decided launch is still listed: %+v, %v", misses, err)
	}
}
