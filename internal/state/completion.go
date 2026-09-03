package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/state/ledgerdb"
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
		q := WriteQueries(tx)

		stored, err := q.PendingCompletionMessage(ctx, ledgerdb.PendingCompletionMessageParams{
			Tier:      completion.Tier,
			RequestID: completion.RequestID,
		})

		switch {
		case err == nil && completion.MessageID < stored.MessageID:
			disposition = PendingCompletionStale

			return nil
		case err == nil && completion.MessageID == stored.MessageID && intBool(stored.Retired):
			disposition = PendingCompletionRetired

			return nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("state: inspect pending completion for %s request %d: %w",
				completion.Tier, completion.RequestID, err)
		}

		if err := q.UpsertPendingCompletion(ctx, ledgerdb.UpsertPendingCompletionParams{
			Tier:         completion.Tier,
			RequestID:    completion.RequestID,
			RunID:        completion.RunID,
			Result:       completion.Result,
			LeaseID:      completion.LeaseID,
			LeaseEpoch:   completion.LeaseEpoch,
			LeaseNode:    completion.LeaseNode,
			Outcome:      completion.Outcome,
			ReleaseOnly:  boolInt(completion.ReleaseOnly),
			MessageID:    completion.MessageID,
			Retired:      boolInt(completion.Retired),
			Acknowledged: boolInt(completion.Acknowledged),
		}); err != nil {
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
	if strings.TrimSpace(tier) == "" || requestID == 0 || messageID < 0 {
		return errors.New("state: a pending completion retirement needs a tier, non-zero request id, and non-negative message id")
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		q := WriteQueries(tx)

		if err := q.RetirePendingCompletion(ctx, ledgerdb.RetirePendingCompletionParams{
			Tier:      tier,
			RequestID: requestID,
			MessageID: messageID,
		}); err != nil {
			return fmt.Errorf("state: retire pending completion for %s request %d: %w",
				tier, requestID, err)
		}

		if err := q.DeleteAcknowledgedCompletion(ctx, ledgerdb.DeleteAcknowledgedCompletionParams{
			Tier:      tier,
			RequestID: requestID,
			MessageID: messageID,
		}); err != nil {
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
	if strings.TrimSpace(tier) == "" || requestID == 0 || messageID < 0 {
		return errors.New("state: a pending completion identity needs a tier, non-zero request id, and non-negative message id")
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		q := WriteQueries(tx)

		if err := q.AcknowledgePendingCompletion(ctx, ledgerdb.AcknowledgePendingCompletionParams{
			Tier:      tier,
			RequestID: requestID,
			MessageID: messageID,
		}); err != nil {
			return fmt.Errorf("state: acknowledge pending completion for %s request %d: %w",
				tier, requestID, err)
		}

		if err := q.DeleteRetiredCompletion(ctx, ledgerdb.DeleteRetiredCompletionParams{
			Tier:      tier,
			RequestID: requestID,
			MessageID: messageID,
		}); err != nil {
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
		rows, err := ReadQueries(q).ListPendingCompletions(ctx, tier)
		if err != nil {
			return fmt.Errorf("state: read pending completions for %s: %w", tier, err)
		}

		// Indexed rather than ranged by value: the generated row is 136 bytes and
		// this runs over every outstanding obligation on a tier.
		for i := range rows {
			r := &rows[i]

			completions = append(completions, PendingCompletion{
				Tier:         r.Tier,
				RequestID:    r.RequestID,
				RunID:        r.RunID,
				Result:       r.Result,
				LeaseID:      r.LeaseID,
				LeaseEpoch:   r.LeaseEpoch,
				LeaseNode:    r.LeaseNode,
				Outcome:      r.Outcome,
				ReleaseOnly:  intBool(r.ReleaseOnly),
				MessageID:    r.MessageID,
				Retired:      intBool(r.Retired),
				Acknowledged: intBool(r.Acknowledged),
			})
		}

		return nil
	})

	return completions, err
}

func (completion PendingCompletion) valid() error {
	if strings.TrimSpace(completion.Tier) == "" || completion.RequestID == 0 ||
		strings.TrimSpace(completion.Result) == "" {
		return errors.New("state: a pending completion needs a tier, non-zero request id, and result")
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
