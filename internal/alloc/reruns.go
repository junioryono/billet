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
// says the compute was taken from under the job by something other than the job
// and other than a person's choice.
//
// NOT DisruptionNodeForgotten. It says this control plane stopped hearing from a
// host, and a host cut off by a partition goes on running the job, which can then
// fail on its own. NOT DisruptionHeldPastLimit, the bound an operator set on how
// long held compute may live: a job that hangs or outlives it is ended by that
// choice, and would be re-run to hang again. Not a force-destroy either, which
// records no token: a person chose to end that job, and re-running it may undo why.
var RerunCauses = [...]Disruption{DisruptionReclaimed, DisruptionGuestAbsent}

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
	// Bound is the attempt a request in doubt covers this job for, zero for none.
	Bound int64
	// Cursor is where the page after this row begins.
	Cursor RerunCursor
}

// RerunCursor is a place in the list of candidates; the zero value is its start.
type RerunCursor struct {
	resultAt, leaseID string
}

// OpenRerun is a request that may have landed.
type OpenRerun struct {
	RerunRequest
	// Cursor is where the page after this row begins.
	Cursor OpenRerunCursor
}

// OpenRerunCursor is a place in the list of requests in doubt; the zero value is its
// start.
type OpenRerunCursor struct {
	createdAt, repo string
	run             int64
}

// rerunCandidates is a page of rows as RerunCandidate.
func rerunCandidates[R ledgerdb.ListRerunCandidatesRow | ledgerdb.ListRerunCandidatesOfRunRow](rows []R) []RerunCandidate {
	out := make([]RerunCandidate, 0, len(rows))
	for i := range rows {
		r := ledgerdb.ListRerunCandidatesRow(rows[i])
		out = append(out, RerunCandidate{LeaseID: r.LeaseID, Tier: r.Tier, Repository: r.Repo, RunID: r.RunID,
			Disruption: Disruption(r.Disruption), ResultAt: parseStamp(r.ResultAt), Bound: r.RerunAttempt,
			Cursor: RerunCursor{resultAt: r.ResultAt, leaseID: r.LeaseID}})
	}

	return out
}

// RunRerunCandidates lists every candidate of one run, newest first, at most limit.
func (a *Allocator) RunRerunCandidates(ctx context.Context, repository string, runID int64, limit int,
) ([]RerunCandidate, error) {
	var out []RerunCandidate

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListRerunCandidatesOfRun(ctx, ledgerdb.ListRerunCandidatesOfRunParams{
			RunID: runID, Repo: repository, CauseOne: string(RerunCauses[0]), CauseTwo: string(RerunCauses[1]),
			Succeeded: resultSucceeded, MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the candidates of run %d: %w", runID, err)
		}
		out = rerunCandidates(rows)

		return nil
	})

	return out, err
}

// BoundRerunTiers lists the tiers of the jobs a request covers and has not decided.
func (a *Allocator) BoundRerunTiers(ctx context.Context, request RerunRequest) ([]string, error) {
	var out []string

	err := a.db.View(ctx, func(q querier) error {
		var err error

		out, err = state.ReadQueries(q).ListBoundRerunTiers(ctx, ledgerdb.ListBoundRerunTiersParams{
			Repo: request.Repository, RunAttempt: request.Attempt, RunID: request.RunID,
		})
		if err != nil {
			return fmt.Errorf("alloc: list the tiers the re-run of run %d covers: %w", request.RunID, err)
		}

		return nil
	})

	return out, err
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
// after since, newest first from after, at most limit.
func (a *Allocator) RerunCandidates(ctx context.Context, since time.Time, after RerunCursor, limit int,
) ([]RerunCandidate, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("alloc: a list of re-run candidates needs a positive limit, got %d", limit)
	}

	var out []RerunCandidate

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListRerunCandidates(ctx, ledgerdb.ListRerunCandidatesParams{
			CauseOne: string(RerunCauses[0]), CauseTwo: string(RerunCauses[1]),
			Succeeded: resultSucceeded, Since: ts(since.UTC()),
			BeforeResult: after.resultAt, BeforeLease: after.leaseID, MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the jobs awaiting a re-run decision: %w", err)
		}

		out = rerunCandidates(rows)

		return nil
	})

	return out, err
}

// OpenReruns lists the requests that may have landed, oldest first from after, at
// most limit.
// They are settled from here whatever became of the jobs that made them: a job past
// its window, or a tier that stopped asking, must not strand a request GitHub may
// have accepted.
func (a *Allocator) OpenReruns(ctx context.Context, after OpenRerunCursor, limit int) ([]OpenRerun, error) {
	var out []OpenRerun

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListOpenReruns(ctx, ledgerdb.ListOpenRerunsParams{
			AfterCreated: after.createdAt, AfterRepo: after.repo, AfterRun: after.run, MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the re-runs in doubt: %w", err)
		}

		out = make([]OpenRerun, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			out = append(out, OpenRerun{
				RerunRequest: RerunRequest{Repository: r.Repo, RunID: r.RunID, Attempt: r.RunAttempt,
					Kind: RerunKind(r.Kind), JobID: r.GithubJobID, Tries: r.Tries},
				Cursor: OpenRerunCursor{createdAt: r.CreatedAt, repo: r.Repo, run: r.RunID},
			})
		}

		return nil
	})

	return out, err
}

// AskReruns records that these tiers ask for re-runs, forgets every other, and
// says since when each has asked. A tier asking again after it stopped starts again.
func (a *Allocator) AskReruns(ctx context.Context, tiers []string) (map[string]time.Time, error) {
	since := make(map[string]time.Time, len(tiers))

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		asking := make(map[string]bool, len(tiers))
		at := ts(a.now().UTC())

		for _, tier := range tiers {
			asking[tier] = true
			if err := q.AskRerunsForTier(ctx, ledgerdb.AskRerunsForTierParams{Tier: tier, AskedSince: at}); err != nil {
				return fmt.Errorf("alloc: record that tier %s asks for re-runs: %w", tier, err)
			}
		}

		rows, err := q.ListRerunTiers(ctx)
		if err != nil {
			return fmt.Errorf("alloc: read the tiers that ask for re-runs: %w", err)
		}

		clear(since)
		for _, row := range rows {
			if !asking[row.Tier] {
				if err := q.ForgetRerunTier(ctx, row.Tier); err != nil {
					return fmt.Errorf("alloc: forget that tier %s asked for re-runs: %w", row.Tier, err)
				}

				continue
			}
			since[row.Tier] = parseStamp(row.AskedSince)
		}

		return nil
	})

	return since, err
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

// RerunOfAttempt says what the request about one attempt of a run came to and what
// it asked for, false when that attempt was never asked about.
func (a *Allocator) RerunOfAttempt(ctx context.Context, repository string, runID, attempt int64,
) (RerunDecision, RerunKind, bool, error) {
	var (
		decision RerunDecision
		kind     RerunKind
		asked    bool
	)

	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadRerunOfAttempt(ctx, ledgerdb.ReadRerunOfAttemptParams{
			Repo: repository, RunID: runID, RunAttempt: attempt,
		})
		if errors.Is(err, sql.ErrNoRows) {
			decision, kind, asked = "", "", false

			return nil
		}
		if err != nil {
			return fmt.Errorf("alloc: read the re-run of attempt %d of run %d: %w", attempt, runID, err)
		}

		decision, kind, asked = RerunFailed, RerunKind(row.Kind), true
		if row.State == rerunStateRequested {
			decision = RerunRequested
		}

		return nil
	})

	return decision, kind, asked, err
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

// SettleRerun ends a request in doubt with what it came to, on every job bound to
// it: RerunRequested for one GitHub accepted, which is the one counted against a
// limit, and for one never accepted the reason, RerunFailed or RerunLimit.
func (a *Allocator) SettleRerun(ctx context.Context, request RerunRequest, decision RerunDecision, detail string) error {
	settled := rerunStateFailed
	if decision == RerunRequested {
		settled = rerunStateRequested
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
