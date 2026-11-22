package alloc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// StrandedDecision is whether GitHub kept offering the job of a launch that started
// nothing (#911), decided once on that launch's row (migration 84).
type StrandedDecision string

const (
	// StrandedOffered is a job GitHub offered again, or whose run is over.
	StrandedOffered StrandedDecision = "offered"
	// StrandedJob is a job still queued with no runner long after the failure, while
	// its tier advertised and waited for nothing: one GitHub stopped offering.
	StrandedJob StrandedDecision = "stranded"
	// StrandedExpired is a launch nobody decided about within a day, or one whose row
	// names too little to ask GitHub about.
	StrandedExpired StrandedDecision = "expired"
)

// LaunchMiss is a launch that started nothing, with what its job is read by.
type LaunchMiss struct {
	LeaseID string
	Tier    string
	// Owner and Repository are the repository the job was assigned from, both empty
	// where the row did not record it.
	Owner      string
	Repository string
	RunID      int64
	JobID      string
	JobName    string
	// Ended is when the lease ended, as the failure released it.
	Ended time.Time
}

// LaunchMissCursor is where a listing of LaunchMisses resumes: just past the launch
// it names, or from the start for the zero value.
type LaunchMissCursor struct {
	Ended   time.Time
	LeaseID string
}

// After is the cursor just past miss.
func (m *LaunchMiss) After() LaunchMissCursor {
	return LaunchMissCursor{Ended: m.Ended, LeaseID: m.LeaseID}
}

// LaunchMisses is the launches that started nothing and still wait for a decision,
// whose leases ended since since, oldest first from just past after, at most maxRows.
func (a *Allocator) LaunchMisses(ctx context.Context, since time.Time, after LaunchMissCursor, maxRows int,
) ([]LaunchMiss, error) {
	// THE START IS AN EMPTY STAMP, which every recorded one sorts after.
	afterEnded := ""
	if !after.Ended.IsZero() {
		afterEnded = ts(after.Ended)
	}

	var out []LaunchMiss
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListLaunchMisses(ctx, ledgerdb.ListLaunchMissesParams{
			Since:      sql.NullString{String: ts(since), Valid: true},
			AfterEnded: sql.NullString{String: afterEnded, Valid: true},
			AfterLease: after.LeaseID,
			MaxRows:    int64(maxRows),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the launches that started nothing: %w", err)
		}

		for _, row := range rows {
			owner, repository, _ := strings.Cut(row.Repo, "/")
			out = append(out, LaunchMiss{LeaseID: row.LeaseID, Tier: row.Tier, Owner: owner,
				Repository: repository, RunID: row.RunID, JobID: row.GithubJobID, JobName: row.JobName,
				Ended: parseStamp(row.FinishedAt)})
		}

		return nil
	})

	return out, err
}

// LaterLeases counts the leases a tier was assigned for a run after a moment.
type LaterLeases struct {
	// Same is those whose job is the one named: GitHub offering it again.
	Same int64
	// Unnamed is those whose job is not named yet, any of which may be the one named.
	Unnamed int64
}

// LaterLeasesOfJob is what tier was assigned for run after after, as it bears on its
// job named jobName.
func (a *Allocator) LaterLeasesOfJob(ctx context.Context, tier string, runID int64, jobName string,
	after time.Time,
) (LaterLeases, error) {
	var out LaterLeases
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).CountLaterLeasesOfJob(ctx, ledgerdb.CountLaterLeasesOfJobParams{
			Tier: tier, RunID: sql.NullInt64{Int64: runID, Valid: true}, JobName: jobName, After: ts(after),
		})
		if err != nil {
			return fmt.Errorf("alloc: count tier %s's leases of run %d's job %q: %w", tier, runID, jobName, err)
		}
		out = LaterLeases{Same: row.Same, Unnamed: row.Unnamed}

		return nil
	})

	return out, err
}

// DecideStranded records whether GitHub kept offering a launch's job, once, and
// reports whether this call decided it: false for a row already decided, which keeps
// its decision.
func (a *Allocator) DecideStranded(ctx context.Context, leaseID string, decision StrandedDecision,
	at time.Time,
) (bool, error) {
	switch decision {
	case StrandedOffered, StrandedJob, StrandedExpired:
	default:
		return false, fmt.Errorf("alloc: %q is not a stranded decision", decision)
	}

	var decided bool
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var n int64
		var err error
		// STRANDED ONLY WHILE NOTHING SINCE MAY BE THE JOB, said again in the write: an
		// assignment recorded while GitHub was being read may be the job offered again.
		if decision == StrandedJob {
			n, err = state.WriteQueries(tx).DecideJobStranded(ctx, ledgerdb.DecideJobStrandedParams{
				StrandedAt: ts(at), LeaseID: leaseID,
			})
		} else {
			n, err = state.WriteQueries(tx).DecideStranded(ctx, ledgerdb.DecideStrandedParams{
				Stranded: string(decision), StrandedAt: ts(at), LeaseID: leaseID,
			})
		}
		if err != nil {
			return fmt.Errorf("alloc: decide whether lease %s's job is stranded: %w", leaseID, err)
		}
		decided = n > 0

		return nil
	})

	return decided, err
}
