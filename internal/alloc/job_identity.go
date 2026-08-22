package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// IdentifyDirectJob returns a durable negative scheduler id for one GitHub job id.
//
// GitHub's direct JobAssigned path carries runnerRequestId 0. Zero cannot key
// concurrent work, while hashing jobId would make collisions a correctness
// property. Allocation under the database's immediate writer transaction gives
// every distinct job id one collision-free negative number and makes redelivery
// recover the same one after a restart.
func (a *Allocator) IdentifyDirectJob(ctx context.Context, jobID string) (int64, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return 0, errors.New("alloc: a direct assignment needs a job id")
	}

	return a.identifyJob(ctx, jobID, "direct job")
}

// IdentifyPoolSlot returns a durable negative scheduler id for one escrowed
// lease becoming a physical pool member. GitHub's desired-count signal names a
// quantity rather than a job, so the lease is the stable identity available on
// redelivery and after restart.
func (a *Allocator) IdentifyPoolSlot(ctx context.Context, leaseID string) (int64, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return 0, errors.New("alloc: a pool slot needs a lease id")
	}

	var internalID int64
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO pool_slot_identities (lease_id, internal_id)
			 SELECT ?, COALESCE(MIN(internal_id), 0) - 1
			 FROM (SELECT internal_id FROM job_identities
			       UNION ALL SELECT internal_id FROM pool_slot_identities)
			 WHERE true
			 ON CONFLICT(lease_id) DO NOTHING`, leaseID)
		if err != nil {
			return fmt.Errorf("alloc: identify pool slot: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT internal_id FROM pool_slot_identities WHERE lease_id = ?`, leaseID).
			Scan(&internalID); err != nil {
			return fmt.Errorf("alloc: read pool slot identity: %w", err)
		}

		return nil
	})

	return internalID, err
}

func (a *Allocator) identifyJob(ctx context.Context, jobID, kind string) (int64, error) {
	var internalID int64
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO job_identities (job_id, internal_id)
			 SELECT ?, COALESCE(MIN(internal_id), 0) - 1
			 FROM (SELECT internal_id FROM job_identities
			       UNION ALL SELECT internal_id FROM pool_slot_identities)
			 WHERE true
			 ON CONFLICT(job_id) DO NOTHING`, jobID)
		if err != nil {
			return fmt.Errorf("alloc: identify %s: %w", kind, err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT internal_id FROM job_identities WHERE job_id = ?`, jobID).
			Scan(&internalID); err != nil {
			return fmt.Errorf("alloc: read %s identity: %w", kind, err)
		}

		return nil
	})

	return internalID, err
}

// DirectJobIdentity reads an existing direct-assignment identity without creating one.
func (a *Allocator) DirectJobIdentity(ctx context.Context, jobID string) (int64, bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return 0, false, errors.New("alloc: a direct assignment lookup needs a job id")
	}

	var internalID int64
	err := a.db.View(ctx, func(tx querier) error {
		return tx.QueryRowContext(ctx,
			`SELECT internal_id FROM job_identities WHERE job_id = ?`, jobID).Scan(&internalID)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("alloc: read direct job identity: %w", err)
	default:
		return internalID, true, nil
	}
}
