package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ActionsCacheScope names one organisation or one repository below it.
type ActionsCacheScope struct {
	Owner      string
	Repository string
}

func (s ActionsCacheScope) normalised() (ActionsCacheScope, error) {
	s.Owner = strings.ToLower(strings.TrimSpace(s.Owner))
	s.Repository = strings.ToLower(strings.TrimSpace(s.Repository))
	if !validGitHubComponent(s.Owner) || s.Repository != "" && !validGitHubComponent(s.Repository) {
		return ActionsCacheScope{}, errors.New("cache policy needs a GitHub organisation or owner/repository")
	}

	return s, nil
}

func validGitHubComponent(value string) bool {
	return value != "" && len(value) <= 100 && !strings.ContainsAny(value, "/\x00\r\n\t ")
}

// SetActionsCacheEnabled updates one explicit interception policy scope.
func (db *DB) SetActionsCacheEnabled(
	ctx context.Context,
	scope ActionsCacheScope,
	enabled bool,
) error {
	normalised, err := scope.normalised()
	if err != nil {
		return err
	}
	scopeType := "org"
	if normalised.Repository != "" {
		scopeType = "repository"
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		if enabled {
			_, err := tx.ExecContext(ctx,
				`DELETE FROM cache_interception_blocks
				  WHERE scope_type = ? AND owner = ? AND repository = ?`,
				scopeType, normalised.Owner, normalised.Repository)

			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO cache_interception_blocks (scope_type, owner, repository, disabled_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(scope_type, owner, repository) DO UPDATE SET disabled_at = excluded.disabled_at`,
			scopeType, normalised.Owner, normalised.Repository, time.Now().UTC().Format(time.RFC3339Nano))

		return err
	})
}

// ActionsCacheAllowed reports whether neither the organisation nor repository is blocked.
func (db *DB) ActionsCacheAllowed(ctx context.Context, owner, repository string) (bool, error) {
	scope, err := (ActionsCacheScope{Owner: owner, Repository: repository}).normalised()
	if err != nil {
		return false, fmt.Errorf("cache policy scope: %w", err)
	}
	if scope.Repository == "" {
		return false, errors.New("cache policy lookup needs an owner and repository")
	}

	blocked := 0
	err = db.View(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cache_interception_blocks
			  WHERE owner = ? AND ((scope_type = 'org' AND repository = '') OR
			                       (scope_type = 'repository' AND repository = ?))`,
			scope.Owner, scope.Repository).Scan(&blocked)
	})
	if err != nil {
		return false, err
	}

	return blocked == 0, nil
}
