package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// resultSucceeded is the ONE completion result billet recognises, and it is
// measured rather than read: the vendored scale-set client enumerates none, so
// the only value this codebase has ever seen confirmed is the one node teardown
// already keys its cache settlement on.
//
// Everything else — "failed", "canceled", a spelling GitHub adds tomorrow — is
// treated as NOT succeeded. That direction is deliberate: an unknown result is
// "could not tell", and collapsing that into "fine" would hide exactly the job
// an operator came here looking for.
const resultSucceeded = "succeeded"

// RecordJobResult stores GitHub's own conclusion for the job a lease ran.
//
// SEPARATE FROM job_history.conclusion, which is the LEASE's terminal phase and
// answers a different question: whether billet's compute lifecycle finished
// tidily. A job GitHub reports as failed on a lease billet tore down perfectly
// is `done` there, and always was — so nothing in the ledger could tell an
// operator their build failed until this column existed.
//
// UNFENCED, for the same reason MarkDeregistered is: GitHub's conclusion for a
// job is monotonic and says nothing about who holds the lease. A reap that
// quarantined the row in the meantime does not make the job unfinished.
//
// A lease that never reached Assign has no history row and nothing to record —
// a promise GitHub cancelled before assigning it ran no job — so a write that
// matches nothing is success rather than an error.
//
// STORED VERBATIM, and the trim is only ever a blankness TEST. Normalising the
// stored value would decide the report: `" succeeded "` trimmed to `"succeeded"`
// vanishes from it, and the one thing this column must not do is turn a value
// billet does not recognise into one it does. An unknown result fails OPEN into
// the report, quoted, where a person can see the padding.
//
// FIRST OBSERVATION WINS, like MarkFailure and like a disruption. A lease runs
// exactly one job, so its result is immutable: a redelivery carries the same
// word, and re-writing would slide `result_at` forward and drag a days-old job
// back into every `--since` window while its teardown kept retrying. A
// CONTRADICTORY result is refused rather than allowed to replace the first,
// because one of the two is wrong and the earlier one is the one GitHub said
// first.
// THE WORKFLOW RUN IS FILLED IN HERE WHEN THE LEDGER HAS NONE, and only then.
// A pooled runner is launched before GitHub chooses its job, so assignPoolSlot
// records run 0 and the lease's own launch request id — and the run an operator
// needs is the one on the COMPLETION, which names the job that actually ran.
// Never an overwrite: a recorded run is the one this lease was assigned, and
// replacing it would let a swapped pool member rewrite another job's history.
func (a *Allocator) RecordJobResult(
	ctx context.Context, leaseID, result string, runID int64,
) error {
	if strings.TrimSpace(result) == "" {
		return errors.New("alloc: a job result must not be empty")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		recorded, err := q.ReadJobResult(ctx, leaseID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the recorded result of lease %s: %w", leaseID, err)
		}

		if recorded != "" && recorded != result {
			return fmt.Errorf("%w: lease %s already recorded result %q, cannot replace it with %q",
				ErrConflict, leaseID, recorded, result)
		}

		if recorded == "" {
			if err := q.RecordJobResult(ctx, ledgerdb.RecordJobResultParams{
				Result:   result,
				ResultAt: ts(a.now().UTC()),
				LeaseID:  leaseID,
			}); err != nil {
				return fmt.Errorf("alloc: record the result of lease %s: %w", leaseID, err)
			}
		}

		// THE RUN IS ATTEMPTED EVEN WHEN THE RESULT IS A REPEAT, because the two
		// are lost in different places. A crash between the two writes leaves the
		// result recorded and the run absent, and recovery redelivers the SAME
		// result — so returning early on the repeat would make the row that most
		// needs backfilling the one that never gets it.
		if runID <= 0 {
			return nil
		}

		if err := q.RecordJobRun(ctx, ledgerdb.RecordJobRunParams{
			RunID:   sql.NullInt64{Int64: runID, Valid: true},
			LeaseID: leaseID,
		}); err != nil {
			return fmt.Errorf("alloc: record the workflow run of lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// RecordedJobResult reports what GitHub concluded about the job a lease ran, or
// an empty string when nothing has been recorded.
//
// AN EMPTY ANSWER IS ONE OF THREE THINGS and the caller must not collapse them:
// the job has not finished, this lease never ran one, or it predates the column.
// Nothing here decides between them, which is why the report pairs the result
// with a disruption rather than reasoning from its absence.
func (a *Allocator) RecordedJobResult(ctx context.Context, leaseID string) (string, error) {
	var result string

	err := a.db.View(ctx, func(tx querier) error {
		var err error

		result, err = state.ReadQueries(tx).ReadJobResult(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s has no job history", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the recorded result of lease %s: %w", leaseID, err)
		}

		return nil
	})

	return result, err
}

// AttributedFailure is one job GitHub did not report as succeeded, on a lease
// billet's own infrastructure had disrupted.
//
// TWO FACTS, NOT A VERDICT. Nothing here says the disruption caused the failure;
// billet cannot tell a broken host from a broken build, and the report that
// renders this has to say so.
// NO REQUEST ID. A lease's request id is billet's SCHEDULER identity, and for a
// pooled runner it is a negative synthetic one issued before GitHub chose the
// job — so it names a different thing from the run beside it and no report should
// pair the two. The lease is billet's handle; the run is GitHub's.
type AttributedFailure struct {
	LeaseID string
	Tier    string
	Node    string
	RunID   int64
	// Result is GitHub's own word, stored verbatim. It reaches a report an
	// operator reads as billet's own output, so a renderer quotes it.
	Result string
	// Detail is the free-form failure reason recorded beside a reclaim, which is
	// text a NODE supplied. Quoted for the same reason.
	Detail      string
	Disruption  Disruption
	DisruptedAt string
	ResultAt    string
}

// AttributedFailures lists jobs that did not succeed while billet's own
// infrastructure was disrupted, newest first.
//
// ON THE READ-ONLY POOL. An operator asks this while the control plane is
// working, and a report must never reserve the single writer slot.
//
// WINDOWED ON result_at RATHER THAN finished_at, which is written when the LEASE
// terminalizes and stays empty for as long as a destroy is retrying — a job
// whose teardown is wedged is exactly one worth reporting.
//
// The provisional inventory marker is stripped from Detail: it is billet's own
// bookkeeping sentinel rather than an explanation, and the disruption token
// already says what it means.
func (a *Allocator) AttributedFailures(
	ctx context.Context, since time.Time, limit int,
) ([]AttributedFailure, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("alloc: a failure report needs a positive limit, got %d", limit)
	}

	var out []AttributedFailure

	err := a.db.View(ctx, func(tx querier) error {
		out = nil

		rows, err := state.ReadQueries(tx).ListAttributedFailures(ctx,
			ledgerdb.ListAttributedFailuresParams{
				Succeeded: resultSucceeded,
				Since:     ts(since.UTC()),
				MaxRows:   int64(limit),
			})
		if err != nil {
			return fmt.Errorf("alloc: list jobs that failed while billet was disrupted: %w", err)
		}

		for i := range rows {
			row := &rows[i]

			f := AttributedFailure{
				LeaseID:     row.LeaseID,
				Tier:        row.Tier,
				Node:        row.Node,
				RunID:       row.RunID,
				Result:      row.Result,
				Detail:      row.FailureReason,
				Disruption:  Disruption(row.Disruption),
				DisruptedAt: row.DisruptedAt,
				ResultAt:    row.ResultAt,
			}

			// THE PROVISIONAL MARKER IS BILLET'S OWN BOOKKEEPING SENTINEL rather
			// than an explanation, and the disruption token already says what it
			// means.
			if f.Detail == inventoryAbsenceFailureReason {
				f.Detail = ""
			}

			out = append(out, f)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
