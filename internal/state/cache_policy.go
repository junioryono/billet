package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
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
		q := WriteQueries(tx)

		if enabled {
			return q.DeleteCacheBlock(ctx, ledgerdb.DeleteCacheBlockParams{
				ScopeType:  scopeType,
				Owner:      normalised.Owner,
				Repository: normalised.Repository,
			})
		}

		return q.UpsertCacheBlock(ctx, ledgerdb.UpsertCacheBlockParams{
			ScopeType:  scopeType,
			Owner:      normalised.Owner,
			Repository: normalised.Repository,
			DisabledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
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

	var blocked int64

	err = db.View(ctx, func(q Querier) error {
		var err error
		blocked, err = ReadQueries(q).CountCacheBlocks(ctx, ledgerdb.CountCacheBlocksParams{
			Owner:      scope.Owner,
			Repository: scope.Repository,
		})

		return err
	})
	if err != nil {
		return false, err
	}

	return blocked == 0, nil
}
