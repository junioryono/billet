package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PendingCompletion is a GitHub result and capacity obligation the control
// plane must settle before the source message can be forgotten.
type PendingCompletion struct {
	Tier        string
	RequestID   int64
	RunID       int64
	Result      string
	LeaseID     string
	LeaseEpoch  int64
	Outcome     string
	ReleaseOnly bool
}

// PutPendingCompletion durably records a result-delivery obligation.
func (db *DB) PutPendingCompletion(ctx context.Context, completion PendingCompletion) error {
	if err := completion.valid(); err != nil {
		return err
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO pending_completions
			(tier, request_id, run_id, result, lease_id, lease_epoch, outcome, release_only)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(tier, request_id) DO UPDATE SET
				run_id=excluded.run_id,
				result=excluded.result,
				lease_id=excluded.lease_id,
				lease_epoch=excluded.lease_epoch,
				outcome=excluded.outcome,
				release_only=excluded.release_only`,
			completion.Tier, completion.RequestID, completion.RunID, completion.Result,
			completion.LeaseID, completion.LeaseEpoch, completion.Outcome, completion.ReleaseOnly)
		if err != nil {
			return fmt.Errorf("state: record pending completion for %s request %d: %w",
				completion.Tier, completion.RequestID, err)
		}

		return nil
	})
}

// DeletePendingCompletion forgets an obligation only after the node accepted it.
func (db *DB) DeletePendingCompletion(ctx context.Context, tier string, requestID int64) error {
	if strings.TrimSpace(tier) == "" || requestID <= 0 {
		return errors.New("state: a pending completion identity needs a tier and positive request id")
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM pending_completions WHERE tier = ? AND request_id = ?`, tier, requestID); err != nil {
			return fmt.Errorf("state: delete pending completion for %s request %d: %w", tier, requestID, err)
		}

		return nil
	})
}

// PendingCompletions returns one tier's obligations from the read-only pool.
func (db *DB) PendingCompletions(ctx context.Context, tier string) ([]PendingCompletion, error) {
	if strings.TrimSpace(tier) == "" {
		return nil, errors.New("state: pending completions need a tier")
	}

	var completions []PendingCompletion
	err := db.View(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT tier, request_id, run_id, result,
			lease_id, lease_epoch, outcome, release_only
			FROM pending_completions WHERE tier = ? ORDER BY request_id`, tier)
		if err != nil {
			return fmt.Errorf("state: read pending completions for %s: %w", tier, err)
		}
		defer rows.Close()

		for rows.Next() {
			var completion PendingCompletion
			if err := rows.Scan(&completion.Tier, &completion.RequestID,
				&completion.RunID, &completion.Result, &completion.LeaseID,
				&completion.LeaseEpoch, &completion.Outcome, &completion.ReleaseOnly); err != nil {
				return fmt.Errorf("state: scan pending completion for %s: %w", tier, err)
			}
			completions = append(completions, completion)
		}

		return rows.Err()
	})

	return completions, err
}

func (completion PendingCompletion) valid() error {
	if strings.TrimSpace(completion.Tier) == "" || completion.RequestID <= 0 ||
		strings.TrimSpace(completion.Result) == "" {
		return errors.New("state: a pending completion needs a tier, positive request id, and result")
	}
	if completion.RunID < 0 {
		return errors.New("state: a pending completion cannot have a negative run id")
	}
	if completion.LeaseEpoch < 0 {
		return errors.New("state: a pending completion cannot have a negative lease epoch")
	}
	if completion.Outcome != "" && completion.Outcome != "done" && completion.Outcome != "failed" {
		return errors.New("state: a pending completion outcome must be done or failed")
	}
	if completion.LeaseID != "" && strings.TrimSpace(completion.LeaseID) == "" {
		return errors.New("state: a pending completion lease id cannot be whitespace")
	}
	if strings.TrimSpace(completion.LeaseID) == "" {
		if completion.LeaseEpoch != 0 || completion.Outcome != "" || completion.ReleaseOnly {
			return errors.New("state: pending completion release state needs a lease id")
		}
	} else if completion.Outcome == "" {
		return errors.New("state: a pending completion lease needs an outcome")
	}

	return nil
}
