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

// StrandedRecoveryState is where a recovery of a job GitHub stopped offering is
// (#911, migration 85): between its two requests, or what it came to.
type StrandedRecoveryState string

const (
	// RecoveryCancelling is a recovery whose cancel of the run may have been sent.
	RecoveryCancelling StrandedRecoveryState = "cancelling"
	// RecoveryRerunning is one whose attempt GitHub records completed, and whose
	// request to re-run the run's failed jobs may have been sent.
	RecoveryRerunning StrandedRecoveryState = "rerunning"
	// RecoveryRecovered is one whose re-run GitHub took.
	RecoveryRecovered StrandedRecoveryState = "recovered"
	// RecoverySuperseded is one whose run moved past its attempt by something other
	// than billet: a person re-ran it, or the run went on without the cancel.
	RecoverySuperseded StrandedRecoveryState = "superseded"
	// RecoveryFailed is one billet gave up on, its tries used.
	RecoveryFailed StrandedRecoveryState = "failed"
	// RecoveryExpired is one not finished within a day.
	RecoveryExpired StrandedRecoveryState = "expired"
)

// open says whether a state is between the two requests.
func (s StrandedRecoveryState) open() bool { return s == RecoveryCancelling || s == RecoveryRerunning }

// ErrMayBeOffered is a cancel the ledger refused because the tier has been assigned,
// since the stranded launch ended, what is or may be its job: GitHub may be offering
// it after all.
var ErrMayBeOffered = errors.New("alloc: the tier has since been assigned what may be the job")

// StrandedRecovery is one attempt of a run billet is recovering, or recovered.
type StrandedRecovery struct {
	// Repository is the run's repository, owner/name.
	Repository string
	RunID      int64
	Attempt    int64
	Tier       string
	LeaseID    string
	JobName    string
	// Ended is when the stranded launch's lease ended.
	Ended time.Time
	State StrandedRecoveryState
	// Tries is how many times the current request was sent, each counted before it
	// left, and Refused how many of the cancels GitHub definitely refused.
	Tries   int64
	Refused int64
	Created time.Time
}

// MayHaveCancelled says whether a cancel of billet's may have landed: one was sent
// that GitHub did not definitely refuse.
func (r *StrandedRecovery) MayHaveCancelled() bool { return r.Refused < r.Tries }

// StrandedLaunch is a launch decided stranded that a recovery may start from.
type StrandedLaunch struct {
	LeaseID string
	Tier    string
	// Repository is owner/name.
	Repository string
	RunID      int64
	JobName    string
	Ended      time.Time
	Decided    time.Time
}

// StrandedCursor is where a listing of StrandedToRecover resumes: just past the
// launch it names, or from the start for the zero value.
type StrandedCursor struct {
	Decided time.Time
	LeaseID string
}

// After is the cursor just past l.
func (l *StrandedLaunch) After() StrandedCursor {
	return StrandedCursor{Decided: l.Decided, LeaseID: l.LeaseID}
}

// AskStranded records that tiers ask for stranded jobs to be recovered, forgets
// every tier that no longer does, and says since when each asking tier has.
func (a *Allocator) AskStranded(ctx context.Context, tiers []string) (map[string]time.Time, error) {
	since := make(map[string]time.Time, len(tiers))

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		asking := make(map[string]bool, len(tiers))
		at := ts(a.now().UTC())

		for _, tier := range tiers {
			asking[tier] = true
			if err := q.AskStrandedForTier(ctx, ledgerdb.AskStrandedForTierParams{Tier: tier, AskedSince: at}); err != nil {
				return fmt.Errorf("alloc: record that tier %s recovers stranded jobs: %w", tier, err)
			}
		}

		rows, err := q.ListStrandedTiers(ctx)
		if err != nil {
			return fmt.Errorf("alloc: read the tiers that recover stranded jobs: %w", err)
		}

		clear(since)
		for _, row := range rows {
			if !asking[row.Tier] {
				if err := q.ForgetStrandedTier(ctx, row.Tier); err != nil {
					return fmt.Errorf("alloc: forget that tier %s recovered stranded jobs: %w", row.Tier, err)
				}

				continue
			}
			since[row.Tier] = parseStamp(row.AskedSince)
		}

		return nil
	})

	return since, err
}

// StrandedToRecover is the launches decided stranded since since, of a tier that
// asks, that ended after it asked, and whose run billet has never tried to recover:
// oldest decision first from just past after, at most maxRows.
func (a *Allocator) StrandedToRecover(ctx context.Context, since time.Time, after StrandedCursor, maxRows int,
) ([]StrandedLaunch, error) {
	// THE START IS AN EMPTY STAMP, which every recorded one sorts after.
	afterDecided := ""
	if !after.Decided.IsZero() {
		afterDecided = ts(after.Decided)
	}

	var out []StrandedLaunch
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListStrandedToRecover(ctx, ledgerdb.ListStrandedToRecoverParams{
			Since: ts(since), AfterDecided: afterDecided, AfterLease: after.LeaseID, MaxRows: int64(maxRows),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the stranded jobs to recover: %w", err)
		}

		for _, row := range rows {
			out = append(out, StrandedLaunch{LeaseID: row.LeaseID, Tier: row.Tier, Repository: row.Repo,
				RunID: row.RunID, JobName: row.JobName, Ended: parseStamp(row.FinishedAt),
				Decided: parseStamp(row.StrandedAt)})
		}

		return nil
	})

	return out, err
}

// RecoveryCursor is where a listing of OpenStrandedRecoveries resumes: just past the
// recovery it names, or from the start for the zero value.
type RecoveryCursor struct {
	Created    time.Time
	Repository string
	RunID      int64
}

// After is the cursor just past r.
func (r *StrandedRecovery) After() RecoveryCursor {
	return RecoveryCursor{Created: r.Created, Repository: r.Repository, RunID: r.RunID}
}

// OpenStrandedRecoveries is the recoveries between their two requests, oldest first
// from just past after, at most maxRows.
func (a *Allocator) OpenStrandedRecoveries(ctx context.Context, after RecoveryCursor, maxRows int,
) ([]StrandedRecovery, error) {
	// THE START IS AN EMPTY STAMP, which every recorded one sorts after.
	afterCreated := ""
	if !after.Created.IsZero() {
		afterCreated = ts(after.Created)
	}

	var out []StrandedRecovery
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListOpenStrandedRecoveries(ctx, ledgerdb.ListOpenStrandedRecoveriesParams{
			AfterCreated: afterCreated, AfterRepo: after.Repository, AfterRun: after.RunID, MaxRows: int64(maxRows),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the stranded-job recoveries in progress: %w", err)
		}

		for i := range rows {
			row := &rows[i]
			out = append(out, StrandedRecovery{Repository: row.Repo, RunID: row.RunID, Attempt: row.RunAttempt,
				Tier: row.Tier, LeaseID: row.LeaseID, JobName: row.JobName, Ended: parseStamp(row.EndedAt),
				State: StrandedRecoveryState(row.State), Tries: row.Tries, Refused: row.Refused,
				Created: parseStamp(row.CreatedAt)})
		}

		return nil
	})

	return out, err
}

// mayBeOffered asks, inside the transaction that authorises a cancel, whether the
// tier has been assigned since r's launch ended what is or may be its job.
func mayBeOffered(ctx context.Context, tx *sql.Tx, r *StrandedRecovery) (bool, error) {
	later, err := state.ReadQueries(tx).CountLaterLeasesOfJob(ctx, ledgerdb.CountLaterLeasesOfJobParams{
		Tier: r.Tier, RunID: sql.NullInt64{Int64: r.RunID, Valid: true}, JobName: r.JobName, After: ts(r.Ended),
	})
	if err != nil {
		return false, fmt.Errorf("alloc: count tier %s's leases of run %d since its stranded launch: %w",
			r.Tier, r.RunID, err)
	}

	return later.Same > 0 || later.Unnamed > 0, nil
}

// BeginStrandedRecovery writes that billet is about to cancel r's attempt, before
// the cancel leaves, and says whether this call began it: false for a run billet
// has tried to recover before, on any attempt, and ErrMayBeOffered for one whose
// tier has since been assigned what may be the job.
func (a *Allocator) BeginStrandedRecovery(ctx context.Context, r StrandedRecovery) (bool, error) {
	if r.RunID <= 0 || r.Attempt <= 0 || r.Repository == "" || r.JobName == "" || r.Ended.IsZero() {
		return false, fmt.Errorf("alloc: a recovery needs a repository, a run, an attempt, a job and when its "+
			"launch ended, not %q %d %d %q %v", r.Repository, r.RunID, r.Attempt, r.JobName, r.Ended)
	}

	var begun bool
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		offered, err := mayBeOffered(ctx, tx, &r)
		if err != nil {
			return err
		}
		if offered {
			return ErrMayBeOffered
		}

		n, err := state.WriteQueries(tx).BeginStrandedRecovery(ctx, ledgerdb.BeginStrandedRecoveryParams{
			Repo: r.Repository, RunID: r.RunID, RunAttempt: r.Attempt, Tier: r.Tier, LeaseID: r.LeaseID,
			JobName: r.JobName, EndedAt: ts(r.Ended), CreatedAt: ts(a.now().UTC()),
		})
		if err != nil {
			return fmt.Errorf("alloc: begin recovering run %d of %s: %w", r.RunID, r.Repository, err)
		}
		begun = n > 0

		return nil
	})

	return begun, err
}

// MoveStrandedRecovery moves r from the state it was read in to to, with tries
// sends of that request, and says whether it moved. A move to `cancelling`
// authorises a cancel sent again, and is refused with ErrMayBeOffered where the
// tier has since been assigned what may be the job.
func (a *Allocator) MoveStrandedRecovery(ctx context.Context, r StrandedRecovery, to StrandedRecoveryState,
	tries int64,
) (bool, error) {
	if !to.open() {
		return false, fmt.Errorf("alloc: %q is not a state between a recovery's requests", to)
	}

	var moved bool
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		if to == RecoveryCancelling {
			offered, err := mayBeOffered(ctx, tx, &r)
			if err != nil {
				return err
			}
			if offered {
				return ErrMayBeOffered
			}
		}

		n, err := state.WriteQueries(tx).MoveStrandedRecovery(ctx, ledgerdb.MoveStrandedRecoveryParams{
			ToState: string(to), Tries: tries, Repo: r.Repository, RunID: r.RunID, RunAttempt: r.Attempt,
			FromState: string(r.State),
		})
		if err != nil {
			return fmt.Errorf("alloc: move the recovery of run %d to %s: %w", r.RunID, to, err)
		}
		moved = n > 0

		return nil
	})

	return moved, err
}

// RefuseStrandedCancel counts that GitHub definitely refused r's most recent cancel,
// the one its Tries counts, and says whether it was counted.
func (a *Allocator) RefuseStrandedCancel(ctx context.Context, r StrandedRecovery) (bool, error) {
	var counted bool
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := state.WriteQueries(tx).RefuseStrandedCancel(ctx, ledgerdb.RefuseStrandedCancelParams{
			Repo: r.Repository, RunID: r.RunID, RunAttempt: r.Attempt, Tries: r.Tries,
		})
		if err != nil {
			return fmt.Errorf("alloc: count a refused cancel of run %d: %w", r.RunID, err)
		}
		counted = n > 0

		return nil
	})

	return counted, err
}

// SettleStrandedRecovery ends r from the state it was read in, and says whether
// this call ended it.
func (a *Allocator) SettleStrandedRecovery(ctx context.Context, r StrandedRecovery, to StrandedRecoveryState,
	detail string,
) (bool, error) {
	switch to {
	case RecoveryRecovered, RecoverySuperseded, RecoveryFailed, RecoveryExpired:
	default:
		return false, fmt.Errorf("alloc: %q is not what a recovery comes to", to)
	}

	var settled bool
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := state.WriteQueries(tx).SettleStrandedRecovery(ctx, ledgerdb.SettleStrandedRecoveryParams{
			ToState: string(to), SettledAt: ts(a.now().UTC()), Detail: detail, Repo: r.Repository, RunID: r.RunID,
			RunAttempt: r.Attempt, FromState: string(r.State),
		})
		if err != nil {
			return fmt.Errorf("alloc: settle the recovery of run %d as %s: %w", r.RunID, to, err)
		}
		settled = n > 0

		return nil
	})

	return settled, err
}
