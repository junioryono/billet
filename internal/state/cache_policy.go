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

// AllCaches is the kind of a block that covers every cache.
const AllCaches = "*"

// ActionsCacheKind is the kind every block written before kinds existed
// covers.
const ActionsCacheKind = "actions"

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

// validCacheKind is the shape of a kind the ledger stores. Which kinds exist is
// config.CacheKind's to say; the ledger refuses only what no kind could be.
func validCacheKind(kind string) bool {
	if kind == AllCaches {
		return true
	}
	if kind == "" || len(kind) > 32 {
		return false
	}
	for _, character := range kind {
		if character < 'a' || character > 'z' {
			return false
		}
	}

	return true
}

// SetCacheEnabled updates one explicit kill-switch scope for one cache kind,
// or for every cache with AllCaches.
//
// Enabling removes exactly the block it names: enabling one repository leaves
// its organisation's block, and enabling one kind leaves an every-cache block.
func (db *DB) SetCacheEnabled(
	ctx context.Context,
	kind string,
	scope ActionsCacheScope,
	enabled bool,
) error {
	if !validCacheKind(kind) {
		return fmt.Errorf("cache policy kind %q is not a cache", kind)
	}
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
				Kind:       kind,
				ScopeType:  scopeType,
				Owner:      normalised.Owner,
				Repository: normalised.Repository,
			})
		}

		return q.UpsertCacheBlock(ctx, ledgerdb.UpsertCacheBlockParams{
			Kind:       kind,
			ScopeType:  scopeType,
			Owner:      normalised.Owner,
			Repository: normalised.Repository,
			DisabledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	})
}

// SetActionsCacheEnabled updates one explicit Actions-cache scope.
func (db *DB) SetActionsCacheEnabled(
	ctx context.Context,
	scope ActionsCacheScope,
	enabled bool,
) error {
	return db.SetCacheEnabled(ctx, ActionsCacheKind, scope, enabled)
}

// CacheAllowed reports whether no block covers one cache of one repository:
// neither its organisation's nor its own, for that cache or for every cache.
func (db *DB) CacheAllowed(ctx context.Context, kind, owner, repository string) (bool, error) {
	if kind == AllCaches || !validCacheKind(kind) {
		return false, fmt.Errorf("cache policy lookup names no cache kind (%q)", kind)
	}
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
			Kind:       kind,
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

// ActionsCacheAllowed reports whether the Actions cache is allowed for a
// repository.
func (db *DB) ActionsCacheAllowed(ctx context.Context, owner, repository string) (bool, error) {
	return db.CacheAllowed(ctx, ActionsCacheKind, owner, repository)
}

// CacheBlock is one kill-switch entry.
type CacheBlock struct {
	Kind       string
	Owner      string
	Repository string
	DisabledAt string
}

// CacheBlocks lists every kill-switch entry.
func (db *DB) CacheBlocks(ctx context.Context) ([]CacheBlock, error) {
	var blocks []CacheBlock
	err := db.View(ctx, func(q Querier) error {
		rows, err := ReadQueries(q).ListCacheBlocks(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			blocks = append(blocks, CacheBlock{Kind: row.Kind, Owner: row.Owner,
				Repository: row.Repository, DisabledAt: row.DisabledAt})
		}

		return nil
	})

	return blocks, err
}
