package state

import (
	"database/sql"
	"fmt"
	"testing"
)

func TestActionsCachePolicyBlocksAnOrganisationOrOneRepository(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	allowed, err := db.ActionsCacheAllowed(t.Context(), "Acme", "API")
	if err != nil || !allowed {
		t.Fatalf("fresh policy allowed=%t error=%v", allowed, err)
	}
	if err := db.SetActionsCacheEnabled(t.Context(), ActionsCacheScope{
		Owner: "Acme", Repository: "API",
	}, false); err != nil {
		t.Fatalf("disable repository: %v", err)
	}
	for repository, want := range map[string]bool{"api": false, "web": true} {
		allowed, err := db.ActionsCacheAllowed(t.Context(), "acme", repository)
		if err != nil || allowed != want {
			t.Errorf("repository %s allowed=%t error=%v, want %t", repository, allowed, err, want)
		}
	}
	if err := db.SetActionsCacheEnabled(t.Context(), ActionsCacheScope{Owner: "ACME"}, false); err != nil {
		t.Fatalf("disable organisation: %v", err)
	}
	if allowed, err := db.ActionsCacheAllowed(t.Context(), "acme", "web"); err != nil || allowed {
		t.Fatalf("organisation block allowed=%t error=%v", allowed, err)
	}
	if err := db.SetActionsCacheEnabled(t.Context(), ActionsCacheScope{Owner: "acme"}, true); err != nil {
		t.Fatalf("enable organisation: %v", err)
	}
	if allowed, err := db.ActionsCacheAllowed(t.Context(), "acme", "web"); err != nil || !allowed {
		t.Fatalf("re-enabled organisation allowed=%t error=%v", allowed, err)
	}
	if allowed, err := db.ActionsCacheAllowed(t.Context(), "acme", "api"); err != nil || allowed {
		t.Fatalf("repository block survived organisation enable allowed=%t error=%v", allowed, err)
	}
}

// A BLOCK COVERS THE CACHE IT NAMES, AND '*' COVERS EVERY CACHE. Enabling one
// kind removes only that kind's block, so an every-cache block still stands.
func TestCacheBlocksCoverTheKindTheyName(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := ActionsCacheScope{Owner: "acme", Repository: "api"}
	if err := db.SetCacheEnabled(t.Context(), "docker", repo, false); err != nil {
		t.Fatalf("block docker: %v", err)
	}
	for kind, want := range map[string]bool{"docker": false, "sticky": true, "actions": true} {
		if allowed, err := db.CacheAllowed(t.Context(), kind, "acme", "api"); err != nil || allowed != want {
			t.Errorf("after a docker block, %s allowed=%t err=%v, want %t", kind, allowed, err, want)
		}
	}

	if err := db.SetCacheEnabled(t.Context(), AllCaches, ActionsCacheScope{Owner: "acme"}, false); err != nil {
		t.Fatalf("block everything: %v", err)
	}
	if err := db.SetCacheEnabled(t.Context(), "go", ActionsCacheScope{Owner: "acme"}, true); err != nil {
		t.Fatalf("enable go: %v", err)
	}
	for _, kind := range []string{"docker", "sticky", "actions", "go"} {
		if allowed, err := db.CacheAllowed(t.Context(), kind, "acme", "web"); err != nil || allowed {
			t.Errorf("under an every-cache block, %s allowed=%t err=%v", kind, allowed, err)
		}
	}

	if _, err := db.CacheAllowed(t.Context(), AllCaches, "acme", "api"); err == nil {
		t.Error("a lookup for every cache at once was answered")
	}
	blocks, err := db.CacheBlocks(t.Context())
	if err != nil || len(blocks) != 2 {
		t.Fatalf("blocks = %+v, %v, want the docker and every-cache blocks", blocks, err)
	}
}

// A BLOCK WRITTEN BEFORE KINDS EXISTED STILL COVERS THE ACTIONS CACHE, and only
// it, after the upgrade that added them.
func TestAnInterceptionBlockSurvivesAsAnActionsBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP TABLE cache_blocks`,
			`CREATE TABLE cache_interception_blocks (
				scope_type  TEXT NOT NULL CHECK (scope_type IN ('org','repository')),
				owner       TEXT NOT NULL CHECK (length(trim(owner)) > 0 AND owner = lower(owner)),
				repository  TEXT NOT NULL DEFAULT '' CHECK (repository = lower(repository)),
				disabled_at TEXT NOT NULL,
				PRIMARY KEY (scope_type, owner, repository),
				CHECK ((scope_type = 'org' AND repository = '') OR
				       (scope_type = 'repository' AND length(trim(repository)) > 0))
			) STRICT`,
			`INSERT INTO cache_interception_blocks (scope_type, owner, repository, disabled_at)
			 VALUES ('repository', 'acme', 'api', '2026-09-01T00:00:00Z')`,
			`DELETE FROM schema_migrations WHERE version = 54`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version 53: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	upgraded, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer upgraded.Close()
	for kind, want := range map[string]bool{"actions": false, "docker": true} {
		if allowed, err := upgraded.CacheAllowed(t.Context(), kind, "acme", "api"); err != nil || allowed != want {
			t.Errorf("after the upgrade, %s allowed=%t err=%v, want %t", kind, allowed, err, want)
		}
	}
}

func TestActionsCachePolicyRejectsAmbiguousScopes(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, scope := range []ActionsCacheScope{
		{}, {Owner: "acme/api"}, {Owner: "acme", Repository: "api/other"},
	} {
		if err := db.SetActionsCacheEnabled(t.Context(), scope, false); err == nil {
			t.Errorf("accepted invalid scope %+v", scope)
		}
	}
}
