package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RecordLeaseGitHubTarget records the GitHub owner a lease's runner is about to
// be registered on, by its path (`owner` or `owner/name`).
//
// BEFORE THE REGISTRATION IS MINTED, by the caller: a mint whose answer is lost
// leaves a runner on GitHub and no binding naming it, and its recovery has only
// this to find the owner by. Fenced on the epoch and written once: the same
// path again is a retry, and another is ErrConflict, because a lease's runner is
// registered on one owner and a second answer would send its removal elsewhere.
func (a *Allocator) RecordLeaseGitHubTarget(ctx context.Context, leaseID string, epoch int64, path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("alloc: a lease's GitHub target must not be empty")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := a.load(ctx, tx, leaseID, epoch); err != nil {
			return err
		}

		q := state.WriteQueries(tx)

		recorded, err := q.ReadLeaseGitHubTarget(ctx, leaseID)
		if err != nil {
			return fmt.Errorf("alloc: read the GitHub target of lease %s: %w", leaseID, err)
		}

		switch recorded {
		case path:
			return nil
		case "":
		default:
			return fmt.Errorf("%w: lease %s's runner is registered on GitHub target %q, not %q",
				ErrConflict, leaseID, recorded, path)
		}

		if err := q.RecordLeaseGitHubTarget(ctx, ledgerdb.RecordLeaseGitHubTargetParams{
			GithubTarget: path,
			ID:           leaseID,
			Epoch:        epoch,
		}); err != nil {
			return fmt.Errorf("alloc: record the GitHub target of lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// LeaseGitHubTarget reports the GitHub owner a lease's runner was registered
// on, whatever phase the lease is in. Empty when none was recorded; a lease the
// ledger has no row for is ErrLeaseNotFound.
func (a *Allocator) LeaseGitHubTarget(ctx context.Context, leaseID string) (string, error) {
	var path string

	err := a.db.View(ctx, func(q querier) error {
		var err error

		path, err = state.ReadQueries(q).ReadLeaseGitHubTarget(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the GitHub target of lease %s: %w", leaseID, err)
		}

		return nil
	})

	return path, err
}
