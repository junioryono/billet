package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrustedRunnerGroupRequiresTheExactWorkflowRestriction(t *testing.T) {
	key, _ := testKeyPKCS1(t)
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /orgs/acme/actions/runner-groups/7", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer installation-secret" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"restricted_to_workflows":true,"selected_workflows":["acme/api/.github/workflows/release.yml@refs/heads/main","acme/api/.github/workflows/ci.yml@refs/heads/main"]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newRunnerGroupPolicyClient(srv.Client(), srv.URL, "acme", 11, 22, key)
	want := []string{"acme/api/.github/workflows/ci.yml@refs/heads/main",
		"acme/api/.github/workflows/release.yml@refs/heads/main"}
	if err := c.ValidateTrustedRunnerGroup(t.Context(), 7, want); err != nil {
		t.Fatalf("ValidateTrustedRunnerGroup: %v", err)
	}
	if err := c.ValidateTrustedRunnerGroup(t.Context(), 7, want); err != nil {
		t.Fatalf("cached ValidateTrustedRunnerGroup: %v", err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("installation token calls = %d, want 1", tokenCalls.Load())
	}
}

func TestTrustedRunnerGroupRefusesPolicyDrift(t *testing.T) {
	key, _ := testKeyPKCS1(t)
	policy := `{"restricted_to_workflows":false,"selected_workflows":[]}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /orgs/acme/actions/runner-groups/7", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, policy)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newRunnerGroupPolicyClient(srv.Client(), srv.URL, "acme", 11, 22, key)
	if err := c.ValidateTrustedRunnerGroup(t.Context(), 7, []string{"acme/api/.github/workflows/ci.yml@refs/heads/main"}); err == nil {
		t.Fatal("accepted a runner group without workflow restriction")
	}
	policy = `{"restricted_to_workflows":true,"selected_workflows":["acme/api/.github/workflows/other.yml@refs/heads/main"]}`
	if err := c.ValidateTrustedRunnerGroup(t.Context(), 7, []string{"acme/api/.github/workflows/ci.yml@refs/heads/main"}); err == nil {
		t.Fatal("accepted a different workflow allowlist")
	}
}
