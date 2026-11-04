package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RerunCauses is the closed list of disruptions that may lead to a re-run: each
// says the compute was taken or destroyed under the job.
//
// NOT DisruptionNodeForgotten. It says this control plane stopped hearing from a
// host, and a host cut off by a partition goes on running the job, which can then
// fail on its own. Not an operator's force-destroy either, which records no
// token: a person chose to end that job, and re-running it may undo why.
var RerunCauses = [...]Disruption{DisruptionHeldPastLimit, DisruptionReclaimed, DisruptionGuestAbsent}

// RerunDecision is what became of a job's chance of a re-run. Closed in Go, for
// the reason a Disruption is, and rendered verbatim where it is not recognised.
type RerunDecision string

const (
	// RerunRequested means GitHub accepted a request that covers this job.
	RerunRequested RerunDecision = "requested"
	// RerunUnproven means GitHub's record of the job does not prove a lost runner,
	// so it may have failed on its own.
	RerunUnproven RerunDecision = "unproven"
	// RerunMixed means more than one proved job sits beside jobs that failed on
	// their own, which neither GitHub request can run again without the others.
	RerunMixed RerunDecision = "mixed"
	// RerunLimit means the run already had every re-run its tiers allow.
	RerunLimit RerunDecision = "limit"
	// RerunSuperseded means the run moved past this job's attempt without billet.
	RerunSuperseded RerunDecision = "superseded"
	// RerunExpired means nothing could be decided inside the window.
	RerunExpired RerunDecision = "expired"
	// RerunFailed means GitHub never accepted the request, within its tries.
	RerunFailed RerunDecision = "failed"
)

// RerunKind is which of GitHub's two requests a re-run is.
type RerunKind string

const (
	// RerunFailedJobs asks for every failed job of the run.
	RerunFailedJobs RerunKind = "failed-jobs"
	// RerunOneJob asks for one job.
	RerunOneJob RerunKind = "job"
)

// The states of a request. A request is `asking` from before it leaves until
// GitHub's answer, or its record of the run, says how it ended.
const (
	rerunStateAsking    = "asking"
	rerunStateRequested = "requested"
	rerunStateFailed    = "failed"
)

// RerunCandidate is one red job a re-run may be decided for.
type RerunCandidate struct {
	LeaseID    string
	Tier       string
	Repository string
	RunID      int64
	Disruption Disruption
	ResultAt   time.Time
}

// RerunRequest names one attempt of a run being asked about.
type RerunRequest struct {
	Repository string
	RunID      int64
	Attempt    int64
	Kind       RerunKind
	// JobID is GitHub's id for the job when Kind is RerunOneJob.
	JobID int64
	// Tries is how many times it has been sent, this one included.
	Tries int64
}

// RerunCandidates lists the jobs awaiting a decision whose result arrived at or
// after since, newest first, at most limit.
func (a *Allocator) RerunCandidates(ctx context.Context, since time.Time, limit int) ([]RerunCandidate, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("alloc: a list of re-run candidates needs a positive limit, got %d", limit)
	}

	var out []RerunCandidate

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListRerunCandidates(ctx, ledgerdb.ListRerunCandidatesParams{
			CauseOne: string(RerunCauses[0]), CauseTwo: string(RerunCauses[1]), CauseThree: string(RerunCauses[2]),
			Succeeded: resultSucceeded, Since: ts(since.UTC()), MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the jobs awaiting a re-run decision: %w", err)
		}

		out = make([]RerunCandidate, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			out = append(out, RerunCandidate{LeaseID: r.LeaseID, Tier: r.Tier, Repository: r.Repo, RunID: r.RunID,
				Disruption: Disruption(r.Disruption), ResultAt: parseStamp(r.ResultAt)})
		}

		return nil
	})

	return out, err
}

// OpenRerun reads the request of a run that may have landed, false when none is
// in doubt.
func (a *Allocator) OpenRerun(ctx context.Context, repository string, runID int64) (RerunRequest, bool, error) {
	var (
		open  RerunRequest
		found bool
	)

	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadOpenRerun(ctx, ledgerdb.ReadOpenRerunParams{Repo: repository, RunID: runID})
		if errors.Is(err, sql.ErrNoRows) {
			open, found = RerunRequest{}, false

			return nil
		}
		if err != nil {
			return fmt.Errorf("alloc: read the open re-run of run %d: %w", runID, err)
		}

		open = RerunRequest{Repository: repository, RunID: runID, Attempt: row.RunAttempt,
			Kind: RerunKind(row.Kind), JobID: row.GithubJobID, Tries: row.Tries}
		found = true

		return nil
	})

	return open, found, err
}

// RerunOfAttempt says what the request about one attempt of a run came to, false
// when that attempt was never asked about.
func (a *Allocator) RerunOfAttempt(ctx context.Context, repository string, runID, attempt int64,
) (RerunDecision, bool, error) {
	var (
		decision RerunDecision
		asked    bool
	)

	err := a.db.View(ctx, func(q querier) error {
		settled, err := state.ReadQueries(q).ReadRerunOfAttempt(ctx, ledgerdb.ReadRerunOfAttemptParams{
			Repo: repository, RunID: runID, RunAttempt: attempt,
		})
		if errors.Is(err, sql.ErrNoRows) {
			decision, asked = "", false

			return nil
		}
		if err != nil {
			return fmt.Errorf("alloc: read the re-run of attempt %d of run %d: %w", attempt, runID, err)
		}

		decision, asked = RerunFailed, true
		if settled == rerunStateRequested {
			decision = RerunRequested
		}

		return nil
	})

	return decision, asked, err
}

// RunReruns says how many re-runs of a run GitHub accepted from billet.
func (a *Allocator) RunReruns(ctx context.Context, repository string, runID int64) (int64, error) {
	var count int64

	err := a.db.View(ctx, func(q querier) error {
		var err error

		count, err = state.ReadQueries(q).CountRunReruns(ctx, ledgerdb.CountRunRerunsParams{Repo: repository, RunID: runID})
		if err != nil {
			return fmt.Errorf("alloc: count the re-runs of run %d: %w", runID, err)
		}

		return nil
	})

	return count, err
}

// BeginRerun records a request about to leave, and binds the jobs it covers to it.
//
// IN ONE TRANSACTION, BEFORE THE REQUEST, so a crash at any point leaves a row
// that says a request may have landed and which jobs it was for.
func (a *Allocator) BeginRerun(ctx context.Context, request RerunRequest, leases []string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		if err := q.InsertRerunIntent(ctx, ledgerdb.InsertRerunIntentParams{
			Repo: request.Repository, RunID: request.RunID, RunAttempt: request.Attempt,
			Kind: string(request.Kind), GithubJobID: request.JobID, CreatedAt: ts(a.now().UTC()),
		}); err != nil {
			return fmt.Errorf("alloc: record the re-run of attempt %d of run %d: %w",
				request.Attempt, request.RunID, err)
		}

		for _, lease := range leases {
			if err := q.BindJobToRerun(ctx, ledgerdb.BindJobToRerunParams{
				RunAttempt: request.Attempt, LeaseID: lease,
			}); err != nil {
				return fmt.Errorf("alloc: bind lease %s to the re-run of run %d: %w", lease, request.RunID, err)
			}
		}

		return nil
	})
}

// RetryRerun counts one more try of a request still in doubt.
func (a *Allocator) RetryRerun(ctx context.Context, request RerunRequest) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).AddRerunTry(ctx, ledgerdb.AddRerunTryParams{
			Repo: request.Repository, RunID: request.RunID, RunAttempt: request.Attempt,
		}); err != nil {
			return fmt.Errorf("alloc: count a try of the re-run of run %d: %w", request.RunID, err)
		}

		return nil
	})
}

// SettleRerun ends a request in doubt: accepted, or never accepted and why, with
// the outcome on every job bound to it.
func (a *Allocator) SettleRerun(ctx context.Context, request RerunRequest, accepted bool, detail string) error {
	settled, decision := rerunStateFailed, RerunFailed
	if accepted {
		settled, decision = rerunStateRequested, RerunRequested
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		at := ts(a.now().UTC())

		if err := q.SettleRerun(ctx, ledgerdb.SettleRerunParams{
			State: settled, SettledAt: at, Detail: detail,
			Repo: request.Repository, RunID: request.RunID, RunAttempt: request.Attempt,
		}); err != nil {
			return fmt.Errorf("alloc: settle the re-run of run %d: %w", request.RunID, err)
		}

		if err := q.SettleJobsOfRerun(ctx, ledgerdb.SettleJobsOfRerunParams{
			Decision: string(decision), DecidedAt: at, RunAttempt: request.Attempt,
			Repo: request.Repository, RunID: request.RunID,
		}); err != nil {
			return fmt.Errorf("alloc: record the re-run of run %d on its jobs: %w", request.RunID, err)
		}

		return nil
	})
}

// RecordRerunDecision records why a job was not asked about, once. attempt is the
// attempt it was decided against, zero when GitHub was never read.
func (a *Allocator) RecordRerunDecision(ctx context.Context, leaseID string, decision RerunDecision, attempt int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).RecordJobRerunDecision(ctx, ledgerdb.RecordJobRerunDecisionParams{
			Decision: string(decision), DecidedAt: ts(a.now().UTC()), RunAttempt: attempt, LeaseID: leaseID,
		}); err != nil {
			return fmt.Errorf("alloc: record the re-run decision of lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// RerunDecisionCount is how many jobs of a tier reached a decision.
type RerunDecisionCount struct {
	Tier     string
	Decision RerunDecision
	Jobs     int64
}

// RerunDecisions counts each tier's decisions made at or after since.
func (a *Allocator) RerunDecisions(ctx context.Context, since time.Time) ([]RerunDecisionCount, error) {
	var out []RerunDecisionCount

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListRerunDecisions(ctx, ts(since.UTC()))
		if err != nil {
			return fmt.Errorf("alloc: count the re-run decisions: %w", err)
		}

		out = make([]RerunDecisionCount, 0, len(rows))
		for _, r := range rows {
			out = append(out, RerunDecisionCount{Tier: r.Tier, Decision: RerunDecision(r.RerunDecision), Jobs: r.Jobs})
		}

		return nil
	})

	return out, err
}
