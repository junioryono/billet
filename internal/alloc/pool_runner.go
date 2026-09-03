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

// PoolRunner is one GitHub registration backed by one compute lease.
type PoolRunner struct {
	LeaseID            string
	Tier               string
	LaunchRequestID    int64
	RunnerID           int64
	RunnerName         string
	Status             string
	ActualRequestID    int64
	RunID              int64
	JobID              string
	SourceAcknowledged bool
}

const (
	PoolRunnerIdle     = "idle"
	PoolRunnerBusy     = "busy"
	PoolRunnerRetiring = "retiring"
	PoolRunnerRetired  = "retired"
)

// RegisterPoolRunner records the idle pool member created for a lease.
func (a *Allocator) RegisterPoolRunner(ctx context.Context, runner PoolRunner) error {
	if strings.TrimSpace(runner.LeaseID) == "" || strings.TrimSpace(runner.Tier) == "" ||
		runner.LaunchRequestID == 0 || strings.TrimSpace(runner.RunnerName) == "" || runner.RunnerID < 0 {
		return errors.New("alloc: a pool runner needs a lease, tier, launch request, name, and non-negative id")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		prior, found, err := poolRunnerByLease(ctx, q, runner.LeaseID)
		if err != nil {
			return err
		}
		if found {
			if prior.Tier != runner.Tier || prior.LaunchRequestID != runner.LaunchRequestID ||
				prior.RunnerName != runner.RunnerName ||
				(prior.RunnerID != 0 && runner.RunnerID != 0 && prior.RunnerID != runner.RunnerID) {
				return fmt.Errorf("alloc: %w: lease %s is already registered as runner %q for tier %q request %d",
					ErrConflict, runner.LeaseID, prior.RunnerName, prior.Tier, prior.LaunchRequestID)
			}

			return nil
		}

		if err := insertPoolRunner(ctx, q, runner, PoolRunnerIdle); err != nil {
			return fmt.Errorf("alloc: register pool runner %q: %w", runner.RunnerName, err)
		}

		return nil
	})
}

// StartPoolRunner durably binds a pool member to the job GitHub actually gave it.
func (a *Allocator) StartPoolRunner(ctx context.Context, leaseID, tier string, runnerID int64,
	runnerName string, requestID, runID int64, jobID string,
) (PoolRunner, error) {
	if strings.TrimSpace(leaseID) == "" || strings.TrimSpace(tier) == "" || runnerID <= 0 ||
		strings.TrimSpace(runnerName) == "" || requestID == 0 || runID < 0 || strings.TrimSpace(jobID) == "" {
		return PoolRunner{}, errors.New("alloc: a started pool runner needs complete runner and job identity")
	}

	var out PoolRunner
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		prior, found, err := poolRunnerByLease(ctx, q, leaseID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("alloc: %w: no pool runner is registered for lease %s", ErrLeaseNotFound, leaseID)
		}
		if prior.Tier != tier || prior.RunnerName != runnerName ||
			(prior.RunnerID != 0 && prior.RunnerID != runnerID) {
			return fmt.Errorf("alloc: %w: runner %q does not match lease %s", ErrConflict, runnerName, leaseID)
		}
		if prior.Status == PoolRunnerRetiring || prior.Status == PoolRunnerRetired {
			if prior.ActualRequestID == requestID && prior.RunID == runID && prior.JobID == jobID {
				out = prior
				return nil
			}
			return fmt.Errorf("alloc: %w: runner %q is already retiring", ErrConflict, runnerName)
		}
		if prior.Status == PoolRunnerBusy {
			// A legacy runner recovered from GitHub is journaled busy before its
			// delayed JobStarted arrives. That empty job identity is a reservation
			// for this exact physical runner, not a competing job binding.
			if prior.ActualRequestID == 0 && prior.RunID == 0 && prior.JobID == "" {
				if err := q.BindPoolRunnerJob(ctx, ledgerdb.BindPoolRunnerJobParams{
					ActualRequestID: requestID,
					RunID:           runID,
					JobID:           jobID,
					UpdatedAt:       nowStamp(),
					LeaseID:         leaseID,
				}); err != nil {
					return fmt.Errorf("alloc: bind recovered pool runner %q to job %q: %w",
						runnerName, jobID, err)
				}
				if err := a.recordJobStartTx(ctx, tx, leaseID, a.now()); err != nil {
					return err
				}
				prior.ActualRequestID, prior.RunID, prior.JobID = requestID, runID, jobID
				out = prior
				return nil
			}
			if prior.ActualRequestID != requestID || prior.RunID != runID || prior.JobID != jobID {
				return fmt.Errorf("alloc: %w: runner %q is already busy with another job", ErrConflict, runnerName)
			}
			// The same job reported started again: the start is already recorded,
			// and a reason that landed in between is repaired now.
			if err := a.recordJobStartTx(ctx, tx, leaseID, a.now()); err != nil {
				return err
			}
			out = prior
			return nil
		}

		if err := q.StartPoolRunner(ctx, ledgerdb.StartPoolRunnerParams{
			RunnerID:        runnerID,
			ActualRequestID: requestID,
			RunID:           runID,
			JobID:           jobID,
			UpdatedAt:       nowStamp(),
			LeaseID:         leaseID,
		}); err != nil {
			return fmt.Errorf("alloc: bind pool runner %q to job %q: %w", runnerName, jobID, err)
		}
		// A POOLED RUNNER'S JOB STARTS HERE, not on a lease phase: the lease of
		// a pool member stays where the launch left it while GitHub binds work
		// to the physical runner, so the ledger's evidence that a job ran (see
		// disruptionFor) is written on this edge as well as on `busy`.
		if err := a.recordJobStartTx(ctx, tx, leaseID, a.now()); err != nil {
			return err
		}
		prior.RunnerID, prior.Status = runnerID, PoolRunnerBusy
		prior.ActualRequestID, prior.RunID, prior.JobID = requestID, runID, jobID
		out = prior
		return nil
	})

	return out, err
}

// PreserveRecoveredBusyPoolRunner journals the exact identity of a legacy
// registration GitHub says is busy. Its empty actual-job fields are filled by a
// delayed JobStarted through StartPoolRunner.
func (a *Allocator) PreserveRecoveredBusyPoolRunner(ctx context.Context, runner PoolRunner) error {
	if strings.TrimSpace(runner.LeaseID) == "" || strings.TrimSpace(runner.Tier) == "" ||
		runner.LaunchRequestID == 0 || runner.RunnerID <= 0 ||
		strings.TrimSpace(runner.RunnerName) == "" {
		return errors.New("alloc: a recovered busy runner needs complete lease and runner identity")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		prior, found, err := poolRunnerByLease(ctx, q, runner.LeaseID)
		if err != nil {
			return err
		}
		if !found {
			return insertPoolRunner(ctx, q, runner, PoolRunnerBusy)
		}
		if prior.Tier != runner.Tier || prior.LaunchRequestID != runner.LaunchRequestID ||
			prior.RunnerName != runner.RunnerName ||
			(prior.RunnerID != 0 && prior.RunnerID != runner.RunnerID) {
			return fmt.Errorf("alloc: %w: recovered runner %q does not match lease %s",
				ErrConflict, runner.RunnerName, runner.LeaseID)
		}
		if prior.Status == PoolRunnerRetiring || prior.Status == PoolRunnerRetired {
			return fmt.Errorf("alloc: %w: recovered runner %q is already retiring",
				ErrConflict, runner.RunnerName)
		}
		if prior.Status == PoolRunnerBusy {
			return nil
		}
		return q.MarkPoolRunnerBusy(ctx, ledgerdb.MarkPoolRunnerBusyParams{
			RunnerID:  runner.RunnerID,
			UpdatedAt: nowStamp(),
			LeaseID:   runner.LeaseID,
		})
	})
}

// RetireRecoveredPoolRunner claims only an idle or placeholder-busy recovery
// row. An authoritative JobStarted binding wins the same transaction race and
// is returned unchanged so recovery preserves its compute.
func (a *Allocator) RetireRecoveredPoolRunner(
	ctx context.Context, runner PoolRunner,
) (PoolRunner, error) {
	if strings.TrimSpace(runner.LeaseID) == "" || strings.TrimSpace(runner.Tier) == "" ||
		runner.LaunchRequestID == 0 || runner.RunnerID < 0 ||
		strings.TrimSpace(runner.RunnerName) == "" {
		return PoolRunner{}, errors.New("alloc: a recovered retirement needs complete lease and runner identity")
	}
	var out PoolRunner
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		prior, found, err := poolRunnerByLease(ctx, q, runner.LeaseID)
		if err != nil {
			return err
		}
		if !found {
			err = insertPoolRunner(ctx, q, runner, PoolRunnerRetiring)
			runner.Status = PoolRunnerRetiring
			out = runner
			return err
		}
		if prior.Tier != runner.Tier || prior.LaunchRequestID != runner.LaunchRequestID ||
			prior.RunnerName != runner.RunnerName ||
			(prior.RunnerID != 0 && runner.RunnerID != 0 && prior.RunnerID != runner.RunnerID) {
			return fmt.Errorf("alloc: %w: recovered retirement for %q does not match lease %s",
				ErrConflict, runner.RunnerName, runner.LeaseID)
		}
		out = prior
		if prior.Status == PoolRunnerBusy &&
			(prior.ActualRequestID != 0 || prior.RunID != 0 || prior.JobID != "") {
			return nil
		}
		if prior.Status == PoolRunnerRetiring || prior.Status == PoolRunnerRetired {
			return nil
		}
		if prior.Status != PoolRunnerIdle && prior.Status != PoolRunnerBusy {
			return ErrConflict
		}
		err = q.MarkPoolRunnerRetiring(ctx, ledgerdb.MarkPoolRunnerRetiringParams{
			UpdatedAt: nowStamp(),
			LeaseID:   runner.LeaseID,
		})
		if err == nil {
			out.Status = PoolRunnerRetiring
		}
		return err
	})
	return out, err
}

// PoolRunnerByName resolves GitHub's runner identity to Billet's compute lease.
func (a *Allocator) PoolRunnerByName(ctx context.Context, name string) (PoolRunner, error) {
	var out PoolRunner
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadPoolRunnerByName(ctx, name)
		if err != nil {
			return err
		}

		out = poolRunnerFrom(&row)

		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PoolRunner{}, ErrLeaseNotFound
	}
	return out, err
}

// PoolRunnerByLease resolves the compute authority when no message carries a runner name.
func (a *Allocator) PoolRunnerByLease(ctx context.Context, leaseID string) (PoolRunner, error) {
	var out PoolRunner
	err := a.db.View(ctx, func(q querier) error {
		var found bool
		var err error
		out, found, err = poolRunnerByLease(ctx, state.ReadQueries(q), leaseID)
		if err == nil && !found {
			return sql.ErrNoRows
		}
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PoolRunner{}, ErrLeaseNotFound
	}
	return out, err
}

// PoolRunners reports every durable member of one tier's GitHub runner pool.
func (a *Allocator) PoolRunners(ctx context.Context, tier string) ([]PoolRunner, error) {
	var out []PoolRunner
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListPoolRunnersInTier(ctx, tier)
		if err != nil {
			return err
		}

		for i := range rows {
			out = append(out, poolRunnerFrom(&rows[i]))
		}

		return nil
	})
	return out, err
}

// IdlePoolRunners reports pool members that can be considered for scale-down.
func (a *Allocator) IdlePoolRunners(ctx context.Context, tier string) ([]PoolRunner, error) {
	runners, err := a.PoolRunners(ctx, tier)
	if err != nil {
		return nil, err
	}
	idle := runners[:0]
	for i := range runners {
		if runners[i].Status == PoolRunnerIdle {
			idle = append(idle, runners[i])
		}
	}
	return idle, nil
}

// RetirePoolRunner claims one member for teardown.
func (a *Allocator) RetirePoolRunner(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		res, err := q.ClaimPoolRunnerForRetirement(ctx,
			ledgerdb.ClaimPoolRunnerForRetirementParams{
				UpdatedAt: nowStamp(),
				LeaseID:   leaseID,
			})
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			prior, found, err := poolRunnerByLease(ctx, q, leaseID)
			if err != nil {
				return err
			}
			if !found {
				return ErrLeaseNotFound
			}
			if prior.Status != PoolRunnerRetiring && prior.Status != PoolRunnerRetired {
				return ErrConflict
			}
		}
		return nil
	})
}

// SettlePoolRunner preserves the physical identity until GitHub acknowledges
// the completion that used it. A redelivery must resolve to the same compute
// even after teardown and capacity release have both completed.
func (a *Allocator) SettlePoolRunner(ctx context.Context, tier string, requestID int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		row, err := q.ReadPoolRunnerSettlementByRequest(ctx,
			ledgerdb.ReadPoolRunnerSettlementByRequestParams{
				Tier:            tier,
				LaunchRequestID: requestID,
			})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.SourceAcknowledged == 1 {
			return q.DeletePoolRunner(ctx, row.LeaseID)
		}
		return q.MarkPoolRunnerRetired(ctx, ledgerdb.MarkPoolRunnerRetiredParams{
			UpdatedAt: nowStamp(),
			LeaseID:   row.LeaseID,
		})
	})
}

// AcknowledgePoolRunner records that GitHub cannot redeliver the completion.
// The row is removed immediately only when physical settlement already landed.
func (a *Allocator) AcknowledgePoolRunner(ctx context.Context, tier string, requestID int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		row, err := q.ReadPoolRunnerSettlementByRequest(ctx,
			ledgerdb.ReadPoolRunnerSettlementByRequestParams{
				Tier:            tier,
				LaunchRequestID: requestID,
			})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Status == PoolRunnerRetired {
			return q.DeletePoolRunner(ctx, row.LeaseID)
		}
		return q.AcknowledgePoolRunnerSource(ctx, ledgerdb.AcknowledgePoolRunnerSourceParams{
			UpdatedAt: nowStamp(),
			LeaseID:   row.LeaseID,
		})
	})
}

// ForgetPoolRunner removes settlement metadata after compute and capacity are gone.
func (a *Allocator) ForgetPoolRunner(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		return state.WriteQueries(tx).DeletePoolRunner(ctx, leaseID)
	})
}

// nowStamp is the timestamp every mutation here writes to updated_at.
//
// NOT a.now(): this table's updated_at is a bookkeeping order, read only by
// ListPoolRunnersInTier's ORDER BY, and it has always come from the wall clock
// rather than the allocator's injectable one. Changing that here would silently
// alter what a test with a frozen clock observes.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// poolRunnerFrom maps one ledger row onto billet's own type.
//
// SHARED BY ALL THREE READS, which is why their projections are identical and in
// table order: sqlc returns the model struct for exactly that shape, so there is
// one mapping rather than three.
func poolRunnerFrom(row *ledgerdb.PoolRunner) PoolRunner {
	return PoolRunner{
		LeaseID:            row.LeaseID,
		Tier:               row.Tier,
		LaunchRequestID:    row.LaunchRequestID,
		RunnerID:           row.RunnerID,
		RunnerName:         row.RunnerName,
		Status:             row.Status,
		ActualRequestID:    row.ActualRequestID,
		RunID:              row.RunID,
		JobID:              row.JobID,
		SourceAcknowledged: row.SourceAcknowledged == 1,
	}
}

// insertPoolRunner journals one member at the status the caller decided on.
//
// ONE STATEMENT FOR THREE CALLERS, and the status is what differs: idle when
// billet registers what it launched, busy when recovery finds a legacy
// registration GitHub says is working, retiring when recovery is claiming one and
// no row existed.
func insertPoolRunner(
	ctx context.Context, q state.WriteOps, runner PoolRunner, status string,
) error {
	return q.InsertPoolRunner(ctx, ledgerdb.InsertPoolRunnerParams{
		LeaseID:         runner.LeaseID,
		Tier:            runner.Tier,
		LaunchRequestID: runner.LaunchRequestID,
		RunnerID:        runner.RunnerID,
		RunnerName:      runner.RunnerName,
		Status:          status,
		UpdatedAt:       nowStamp(),
	})
}

func poolRunnerByLease(
	ctx context.Context, q state.ReadOps, leaseID string,
) (PoolRunner, bool, error) {
	row, err := q.ReadPoolRunnerByLease(ctx, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return PoolRunner{}, false, nil
	}
	if err != nil {
		return PoolRunner{}, false, err
	}
	return poolRunnerFrom(&row), true, nil
}
