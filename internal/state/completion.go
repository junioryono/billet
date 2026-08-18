package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PendingCompletion is a GitHub result the control plane must still deliver to
// the node before the source message can be forgotten.
type PendingCompletion struct {
	Tier      string
	RequestID int64
	RunID     int64
	Result    string
}

// PutPendingCompletion durably records a result-delivery obligation.
func (db *DB) PutPendingCompletion(ctx context.Context, completion PendingCompletion) error {
	if err := completion.valid(); err != nil {
		return err
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO pending_completions (tier, request_id, run_id, result)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(tier, request_id) DO UPDATE SET run_id=excluded.run_id, result=excluded.result`,
			completion.Tier, completion.RequestID, completion.RunID, completion.Result)
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
		rows, err := q.QueryContext(ctx, `SELECT tier, request_id, run_id, result
			FROM pending_completions WHERE tier = ? ORDER BY request_id`, tier)
		if err != nil {
			return fmt.Errorf("state: read pending completions for %s: %w", tier, err)
		}
		defer rows.Close()

		for rows.Next() {
			var completion PendingCompletion
			if err := rows.Scan(&completion.Tier, &completion.RequestID,
				&completion.RunID, &completion.Result); err != nil {
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

	return nil
}
