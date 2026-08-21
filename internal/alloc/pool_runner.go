package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
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
		prior, found, err := poolRunnerByLease(ctx, tx, runner.LeaseID)
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

		_, err = tx.ExecContext(ctx, `INSERT INTO pool_runners
			(lease_id, tier, launch_request_id, runner_id, runner_name, status, updated_at)
			VALUES (?, ?, ?, ?, ?, 'idle', ?)`, runner.LeaseID, runner.Tier,
			runner.LaunchRequestID, runner.RunnerID, runner.RunnerName, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
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
		prior, found, err := poolRunnerByLease(ctx, tx, leaseID)
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
			if prior.ActualRequestID != requestID || prior.RunID != runID || prior.JobID != jobID {
				return fmt.Errorf("alloc: %w: runner %q is already busy with another job", ErrConflict, runnerName)
			}
			out = prior
			return nil
		}

		_, err = tx.ExecContext(ctx, `UPDATE pool_runners SET runner_id = ?, status = 'busy',
			actual_request_id = ?, run_id = ?, job_id = ?, updated_at = ? WHERE lease_id = ?`,
			runnerID, requestID, runID, jobID, time.Now().UTC().Format(time.RFC3339Nano), leaseID)
		if err != nil {
			return fmt.Errorf("alloc: bind pool runner %q to job %q: %w", runnerName, jobID, err)
		}
		prior.RunnerID, prior.Status = runnerID, PoolRunnerBusy
		prior.ActualRequestID, prior.RunID, prior.JobID = requestID, runID, jobID
		out = prior
		return nil
	})

	return out, err
}

// PoolRunnerByName resolves GitHub's runner identity to Billet's compute lease.
func (a *Allocator) PoolRunnerByName(ctx context.Context, name string) (PoolRunner, error) {
	var out PoolRunner
	err := a.db.View(ctx, func(q querier) error {
		return scanPoolRunner(q.QueryRowContext(ctx, `SELECT lease_id, tier, launch_request_id,
			runner_id, runner_name, status, actual_request_id, run_id, job_id, source_acknowledged
			FROM pool_runners WHERE runner_name = ?`, name), &out)
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
		out, found, err = poolRunnerByLease(ctx, q, leaseID)
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
		rows, err := q.QueryContext(ctx, `SELECT lease_id, tier, launch_request_id, runner_id,
			runner_name, status, actual_request_id, run_id, job_id, source_acknowledged FROM pool_runners
			WHERE tier = ? ORDER BY updated_at, lease_id`, tier)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var runner PoolRunner
			if err := scanPoolRunner(rows, &runner); err != nil {
				return err
			}
			out = append(out, runner)
		}
		return rows.Err()
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
		res, err := tx.ExecContext(ctx, `UPDATE pool_runners SET status = 'retiring', updated_at = ?
			WHERE lease_id = ? AND status IN ('idle','busy')`, time.Now().UTC().Format(time.RFC3339Nano), leaseID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			prior, found, err := poolRunnerByLease(ctx, tx, leaseID)
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
		var leaseID string
		var acknowledged bool
		err := tx.QueryRowContext(ctx, `SELECT lease_id, source_acknowledged FROM pool_runners
			WHERE tier = ? AND launch_request_id = ?`, tier, requestID).Scan(&leaseID, &acknowledged)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if acknowledged {
			_, err = tx.ExecContext(ctx, `DELETE FROM pool_runners WHERE lease_id = ?`, leaseID)
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE pool_runners SET status = 'retired', updated_at = ?
			WHERE lease_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), leaseID)
		return err
	})
}

// AcknowledgePoolRunner records that GitHub cannot redeliver the completion.
// The row is removed immediately only when physical settlement already landed.
func (a *Allocator) AcknowledgePoolRunner(ctx context.Context, tier string, requestID int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		var leaseID, status string
		err := tx.QueryRowContext(ctx, `SELECT lease_id, status FROM pool_runners
			WHERE tier = ? AND launch_request_id = ?`, tier, requestID).Scan(&leaseID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if status == PoolRunnerRetired {
			_, err = tx.ExecContext(ctx, `DELETE FROM pool_runners WHERE lease_id = ?`, leaseID)
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE pool_runners SET source_acknowledged = 1,
			updated_at = ? WHERE lease_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), leaseID)
		return err
	})
}

// ForgetPoolRunner removes settlement metadata after compute and capacity are gone.
func (a *Allocator) ForgetPoolRunner(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM pool_runners WHERE lease_id = ?`, leaseID)
		return err
	})
}

type rowScanner interface{ Scan(...any) error }

func scanPoolRunner(row rowScanner, out *PoolRunner) error {
	return row.Scan(&out.LeaseID, &out.Tier, &out.LaunchRequestID, &out.RunnerID,
		&out.RunnerName, &out.Status, &out.ActualRequestID, &out.RunID, &out.JobID,
		&out.SourceAcknowledged)
}

func poolRunnerByLease(ctx context.Context, q querier, leaseID string) (PoolRunner, bool, error) {
	var out PoolRunner
	err := scanPoolRunner(q.QueryRowContext(ctx, `SELECT lease_id, tier, launch_request_id,
		runner_id, runner_name, status, actual_request_id, run_id, job_id, source_acknowledged
		FROM pool_runners WHERE lease_id = ?`, leaseID), &out)
	if errors.Is(err, sql.ErrNoRows) {
		return PoolRunner{}, false, nil
	}
	return out, err == nil, err
}
