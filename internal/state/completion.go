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
	Tier         string
	RequestID    int64
	RunID        int64
	Result       string
	LeaseID      string
	LeaseEpoch   int64
	LeaseNode    string
	Outcome      string
	ReleaseOnly  bool
	MessageID    int64
	Retired      bool
	Acknowledged bool
}

// PendingCompletionDisposition says whether an incoming delivery may perform teardown.
type PendingCompletionDisposition uint8

const (
	// PendingCompletionActionable is the current delivery and still needs settlement.
	PendingCompletionActionable PendingCompletionDisposition = iota
	// PendingCompletionRetired has already settled and exists only to stop replay.
	PendingCompletionRetired
	// PendingCompletionStale was superseded by a later delivery for the same request id.
	PendingCompletionStale
)

// PutPendingCompletion durably records a result-delivery obligation.
func (db *DB) PutPendingCompletion(
	ctx context.Context,
	completion PendingCompletion,
) (PendingCompletionDisposition, error) {
	if err := completion.valid(); err != nil {
		return PendingCompletionActionable, err
	}

	disposition := PendingCompletionActionable
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		var storedMessage int64
		var retired bool
		err := tx.QueryRowContext(ctx, `SELECT message_id, retired FROM pending_completions
			WHERE tier = ? AND request_id = ?`, completion.Tier, completion.RequestID).
			Scan(&storedMessage, &retired)
		switch {
		case err == nil && completion.MessageID < storedMessage:
			disposition = PendingCompletionStale

			return nil
		case err == nil && completion.MessageID == storedMessage && retired:
			disposition = PendingCompletionRetired

			return nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("state: inspect pending completion for %s request %d: %w",
				completion.Tier, completion.RequestID, err)
		}

		_, err = tx.ExecContext(ctx, `INSERT INTO pending_completions
			(tier, request_id, run_id, result, lease_id, lease_epoch, lease_node, outcome,
			 release_only, message_id, retired, acknowledged)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(tier, request_id) DO UPDATE SET
				run_id=excluded.run_id,
				result=excluded.result,
				lease_id=CASE WHEN excluded.message_id = pending_completions.message_id
					AND (pending_completions.retired = 1 OR
					     pending_completions.release_only > excluded.release_only)
					THEN pending_completions.lease_id ELSE excluded.lease_id END,
				lease_epoch=CASE WHEN excluded.message_id = pending_completions.message_id
					AND (pending_completions.retired = 1 OR
					     pending_completions.release_only > excluded.release_only)
					THEN pending_completions.lease_epoch ELSE excluded.lease_epoch END,
				lease_node=CASE WHEN excluded.message_id = pending_completions.message_id
					AND (pending_completions.retired = 1 OR
					     pending_completions.release_only > excluded.release_only)
					THEN pending_completions.lease_node ELSE excluded.lease_node END,
				outcome=CASE WHEN excluded.message_id = pending_completions.message_id
					AND (pending_completions.retired = 1 OR
					     pending_completions.release_only > excluded.release_only)
					THEN pending_completions.outcome ELSE excluded.outcome END,
				release_only=CASE
					WHEN excluded.message_id = pending_completions.message_id
					THEN max(pending_completions.release_only, excluded.release_only)
					ELSE excluded.release_only END,
				message_id=excluded.message_id,
				retired=CASE
					WHEN excluded.message_id = pending_completions.message_id
					THEN max(pending_completions.retired, excluded.retired)
					ELSE excluded.retired END,
				acknowledged=CASE
					WHEN excluded.message_id = pending_completions.message_id
					THEN max(pending_completions.acknowledged, excluded.acknowledged)
					ELSE excluded.acknowledged END
			WHERE excluded.message_id >= pending_completions.message_id`,
			completion.Tier, completion.RequestID, completion.RunID, completion.Result,
			completion.LeaseID, completion.LeaseEpoch, completion.LeaseNode, completion.Outcome,
			completion.ReleaseOnly, completion.MessageID, completion.Retired, completion.Acknowledged)
		if err != nil {
			return fmt.Errorf("state: record pending completion for %s request %d: %w",
				completion.Tier, completion.RequestID, err)
		}

		return nil
	})

	return disposition, err
}

// RetirePendingCompletion durably makes replay a no-op before deletion is tried.
func (db *DB) RetirePendingCompletion(
	ctx context.Context,
	tier string,
	requestID, messageID int64,
) error {
	if strings.TrimSpace(tier) == "" || requestID <= 0 || messageID < 0 {
		return errors.New("state: a pending completion retirement needs a tier and non-negative identity")
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE pending_completions SET retired = 1
			WHERE tier = ? AND request_id = ? AND message_id = ?`, tier, requestID, messageID)
		if err != nil {
			return fmt.Errorf("state: retire pending completion for %s request %d: %w",
				tier, requestID, err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_completions
			WHERE tier = ? AND request_id = ? AND message_id = ? AND acknowledged = 1`,
			tier, requestID, messageID); err != nil {
			return fmt.Errorf("state: remove acknowledged completion for %s request %d: %w",
				tier, requestID, err)
		}

		return nil
	})
}

// AcknowledgePendingCompletion records that GitHub will not redeliver a message.
func (db *DB) AcknowledgePendingCompletion(
	ctx context.Context,
	tier string,
	requestID, messageID int64,
) error {
	if strings.TrimSpace(tier) == "" || requestID <= 0 || messageID < 0 {
		return errors.New("state: a pending completion identity needs a tier and positive request id")
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE pending_completions SET acknowledged = 1
			WHERE tier = ? AND request_id = ? AND message_id = ?`,
			tier, requestID, messageID); err != nil {
			return fmt.Errorf("state: acknowledge pending completion for %s request %d: %w",
				tier, requestID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_completions
			WHERE tier = ? AND request_id = ? AND message_id = ? AND retired = 1`,
			tier, requestID, messageID); err != nil {
			return fmt.Errorf("state: remove retired completion for %s request %d: %w",
				tier, requestID, err)
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
			lease_id, lease_epoch, lease_node, outcome, release_only, message_id, retired,
			acknowledged
			FROM pending_completions WHERE tier = ? ORDER BY request_id`, tier)
		if err != nil {
			return fmt.Errorf("state: read pending completions for %s: %w", tier, err)
		}
		defer rows.Close()

		for rows.Next() {
			var completion PendingCompletion
			if err := rows.Scan(&completion.Tier, &completion.RequestID,
				&completion.RunID, &completion.Result, &completion.LeaseID,
				&completion.LeaseEpoch, &completion.LeaseNode, &completion.Outcome,
				&completion.ReleaseOnly, &completion.MessageID, &completion.Retired,
				&completion.Acknowledged); err != nil {
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
	if completion.MessageID < 0 {
		return errors.New("state: a pending completion cannot have a negative message id")
	}
	if completion.LeaseNode != "" && strings.TrimSpace(completion.LeaseNode) == "" {
		return errors.New("state: a pending completion lease node cannot be whitespace")
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
		if completion.LeaseEpoch != 0 || completion.LeaseNode != "" || completion.Outcome != "" ||
			completion.ReleaseOnly {
			return errors.New("state: pending completion release state needs a lease id")
		}
	} else if completion.Outcome == "" {
		return errors.New("state: a pending completion lease needs an outcome")
	}

	return nil
}
