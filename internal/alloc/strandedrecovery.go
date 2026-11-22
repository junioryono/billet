package alloc

import (
	"context"
	"database/sql"
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

// StrandedRecovery is one attempt of a run billet is recovering, or recovered.
type StrandedRecovery struct {
	// Repository is the run's repository, owner/name.
	Repository string
	RunID      int64
	Attempt    int64
	Tier       string
	LeaseID    string
	JobName    string
	State      StrandedRecoveryState
	// Tries is how many times the current request was sent.
	Tries   int64
	Created time.Time
}

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

// StrandedToRecover is the launches decided stranded since since whose run billet
// has never tried to recover, oldest decision first, at most maxRows.
func (a *Allocator) StrandedToRecover(ctx context.Context, since time.Time, maxRows int) ([]StrandedLaunch, error) {
	var out []StrandedLaunch
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListStrandedToRecover(ctx, ledgerdb.ListStrandedToRecoverParams{
			Since: ts(since), MaxRows: int64(maxRows),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the stranded jobs to recover: %w", err)
		}

		for _, row := range rows {
			out = append(out, StrandedLaunch{LeaseID: row.LeaseID, Tier: row.Tier, Repository: row.Repo, RunID: row.RunID,
				JobName: row.JobName, Ended: parseStamp(row.FinishedAt), Decided: parseStamp(row.StrandedAt)})
		}

		return nil
	})

	return out, err
}

// OpenStrandedRecoveries is every recovery between its two requests, oldest first,
// at most maxRows.
func (a *Allocator) OpenStrandedRecoveries(ctx context.Context, maxRows int) ([]StrandedRecovery, error) {
	var out []StrandedRecovery
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListOpenStrandedRecoveries(ctx, int64(maxRows))
		if err != nil {
			return fmt.Errorf("alloc: list the stranded-job recoveries in progress: %w", err)
		}

		for _, row := range rows {
			out = append(out, StrandedRecovery{Repository: row.Repo, RunID: row.RunID, Attempt: row.RunAttempt,
				Tier: row.Tier, LeaseID: row.LeaseID, JobName: row.JobName, State: StrandedRecoveryState(row.State), Tries: row.Tries,
				Created: parseStamp(row.CreatedAt)})
		}

		return nil
	})

	return out, err
}

// BeginStrandedRecovery writes that billet is about to cancel r's attempt, before
// the cancel leaves, and says whether this call began it: false for a run billet
// has tried to recover before, on any attempt.
func (a *Allocator) BeginStrandedRecovery(ctx context.Context, r StrandedRecovery) (bool, error) {
	if r.RunID <= 0 || r.Attempt <= 0 || r.Repository == "" {
		return false, fmt.Errorf("alloc: a recovery needs a repository, a run and an attempt, not %q %d %d",
			r.Repository, r.RunID, r.Attempt)
	}

	var begun bool
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := state.WriteQueries(tx).BeginStrandedRecovery(ctx, ledgerdb.BeginStrandedRecoveryParams{
			Repo: r.Repository, RunID: r.RunID, RunAttempt: r.Attempt, Tier: r.Tier, LeaseID: r.LeaseID,
			JobName: r.JobName, CreatedAt: ts(a.now().UTC()),
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
// sends of that request, and says whether it moved.
func (a *Allocator) MoveStrandedRecovery(ctx context.Context, r StrandedRecovery, to StrandedRecoveryState,
	tries int64,
) (bool, error) {
	if !to.open() {
		return false, fmt.Errorf("alloc: %q is not a state between a recovery's requests", to)
	}

	var moved bool
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
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
