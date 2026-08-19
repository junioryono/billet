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

	var internalID int64
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO job_identities (job_id, internal_id)
			 SELECT ?, COALESCE(MIN(internal_id), 0) - 1 FROM job_identities WHERE true
			 ON CONFLICT(job_id) DO NOTHING`, jobID)
		if err != nil {
			return fmt.Errorf("alloc: identify direct job: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT internal_id FROM job_identities WHERE job_id = ?`, jobID).
			Scan(&internalID); err != nil {
			return fmt.Errorf("alloc: read direct job identity: %w", err)
		}

		return nil
	})

	return internalID, err
}
