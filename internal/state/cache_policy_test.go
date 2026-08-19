package state

import "testing"

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
