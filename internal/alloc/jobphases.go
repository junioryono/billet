package alloc

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// recordJobPhaseTx records, once, when a lease entered a phase the history
// keeps a time for, in the transaction that moves it there. incarnation is the
// process launching it, read only for the launch. A phase the history keeps no
// time for, and a lease with no history row, write nothing.
func recordJobPhaseTx(ctx context.Context, tx *sql.Tx, leaseID string, to Phase, now time.Time,
	incarnation string,
) error {
	switch to {
	case PhaseLaunching, PhaseCustody, PhaseTeardown, PhaseQuarantine:
	default:
		return nil
	}

	if err := state.WriteQueries(tx).RecordJobPhase(ctx, ledgerdb.RecordJobPhaseParams{
		Phase:       string(to),
		At:          ts(now),
		Incarnation: incarnation,
		LeaseID:     leaseID,
	}); err != nil {
		return fmt.Errorf("alloc: record when lease %s entered %s: %w", leaseID, to, err)
	}

	return nil
}

// JobsOfRunLimit bounds how many leases one run resolves to. A workflow run
// has at most 256 jobs a matrix can make, and a re-run adds its attempt's.
const JobsOfRunLimit = 1000

// jobsOfRunLimit is the bound JobsOfRun applies: JobsOfRunLimit, outside a
// test that lowers it to reach the bound with a handful of leases.
var jobsOfRunLimit = JobsOfRunLimit

// JobsOfRun lists the leases whose jobs belong to one of GitHub's workflow
// runs, oldest assignment first: the run that started, or for a lease whose
// job never started, the run it was assigned. The second result reports that
// the run has leases past JobsOfRunLimit, which are left out.
func (a *Allocator) JobsOfRun(ctx context.Context, runID int64) ([]string, bool, error) {
	if runID <= 0 {
		return nil, false, fmt.Errorf("alloc: a workflow run id is positive, not %d", runID)
	}

	var leases []string
	err := a.db.View(ctx, func(q querier) error {
		var err error

		leases, err = state.ReadQueries(q).ListJobsOfRun(ctx, ledgerdb.ListJobsOfRunParams{
			RunID:   runID,
			MaxRows: int64(jobsOfRunLimit) + 1,
		})
		if err != nil {
			return fmt.Errorf("alloc: list the jobs of run %d: %w", runID, err)
		}

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	if len(leases) > jobsOfRunLimit {
		return leases[:jobsOfRunLimit], true, nil
	}

	return leases, false, nil
}

// JobPhase is one part of a job's life and when it began, on billet's clock.
type JobPhase struct {
	Name string
	At   string
}

// Phases are the parts of the job's life the history recorded a time for, in
// the order they happened, and in the order a lease lives them where two share
// a time: bought, assigned, launched, registered, started, custody, teardown,
// quarantined, finished. The start is GitHub's word, arriving independently of
// the lease's phase, so a node that took custody of a launch whose answer was
// lost can record custody before the start. A part with no time is left out
// rather than shown as never entered, because an empty time is also what a
// row written before the time was kept holds.
func (r JobRecord) Phases() []JobPhase {
	var out []JobPhase

	for _, p := range []JobPhase{
		{"bought", r.BoughtAt},
		{"assigned", r.AssignedAt},
		{"launched", r.LaunchedAt},
		{"registered", r.RegisteredAt},
		{"started", r.StartedAt},
		{"custody", r.CustodyAt},
		{"teardown", r.TeardownAt},
		{"quarantined", r.QuarantinedAt},
		{"finished", r.FinishedAt},
	} {
		if p.At != "" {
			out = append(out, p)
		}
	}

	// billet's stamps are fixed-width (see timestampFormat), so their string
	// order is their time order.
	slices.SortStableFunc(out, func(a, b JobPhase) int { return strings.Compare(a.At, b.At) })

	return out
}

// StartedRun is the workflow run of the job that started on the lease's
// runner, which for a pooled runner GitHub gave another run's job is that other
// run; for a job that never started, the run the lease was assigned. Zero when
// neither is recorded.
func (r JobRecord) StartedRun() int64 {
	if r.GitHubRunID > 0 {
		return r.GitHubRunID
	}

	return r.RunID
}
