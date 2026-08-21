package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerGroupPolicyClientRedactsEveryRenderingPath(t *testing.T) {
	key, _ := testKeyPKCS1(t)
	c := newRunnerGroupPolicyClient(http.DefaultClient, "https://api.example.test", "acme", 11, 22, key)
	c.token = "cached-installation-secret"
	c.expiresAt = time.Now().Add(time.Hour)

	render := func(format string) string {
		var out strings.Builder
		if _, err := fmt.Fprintf(&out, format, c); err != nil {
			t.Fatalf("format %q: %v", format, err)
		}
		return out.String()
	}
	rendered := []string{
		render("%v"), render("%+v"), render("%#v"),
		render("%s"), render("%q"), render("%d"),
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	rendered = append(rendered, string(encoded))
	for _, jsonHandler := range []bool{false, true} {
		var out bytes.Buffer
		var handler slog.Handler = slog.NewTextHandler(&out, nil)
		if jsonHandler {
			handler = slog.NewJSONHandler(&out, nil)
		}
		slog.New(handler).Info("client", "value", c)
		rendered = append(rendered, out.String())
	}
	for _, output := range rendered {
		if strings.Contains(output, string(key)) || strings.Contains(output, c.token) {
			t.Fatalf("credential leaked through rendering: %q", output)
		}
	}
}

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

func TestRunnerRecoveryPreservesOnlyAnExactBusyEphemeralRunner(t *testing.T) {
	key, _ := testKeyPKCS1(t)
	tests := []struct {
		name        string
		listed      string
		wantPresent bool
		wantBusy    bool
		wantID      int64
		wantErr     bool
		status      int
	}{
		{name: "absent", listed: `{"total_count":0,"runners":[]}`},
		{name: "busy", listed: `{"total_count":1,"runners":[{"id":71,"name":"billet-l1","status":"online","busy":true,"ephemeral":true}]}`, wantPresent: true, wantBusy: true, wantID: 71},
		{name: "idle", listed: `{"total_count":1,"runners":[{"id":72,"name":"billet-l1","status":"online","busy":false,"ephemeral":true}]}`, wantPresent: true, wantID: 72},
		{name: "offline", listed: `{"total_count":1,"runners":[{"id":73,"name":"billet-l1","status":"offline","busy":false,"ephemeral":true}]}`, wantPresent: true, wantID: 73},
		{name: "static", listed: `{"total_count":1,"runners":[{"id":74,"name":"billet-l1","status":"online","busy":false,"ephemeral":false}]}`, wantErr: true},
		{name: "busy offline", listed: `{"total_count":1,"runners":[{"id":75,"name":"billet-l1","status":"offline","busy":true,"ephemeral":true}]}`, wantErr: true},
		{name: "unknown status", listed: `{"total_count":1,"runners":[{"id":76,"name":"billet-l1","status":"paused","busy":false,"ephemeral":true}]}`, wantErr: true},
		{name: "duplicate", listed: `{"total_count":2,"runners":[{"id":77,"name":"billet-l1","status":"online","busy":true,"ephemeral":true},{"id":78,"name":"billet-l1","status":"online","busy":false,"ephemeral":true}]}`, wantErr: true},
		{name: "incomplete", listed: `{"total_count":2,"runners":[{"id":79,"name":"billet-l1","status":"online","busy":true,"ephemeral":true}]}`, wantErr: true},
		{name: "api failure", listed: `{"message":"unavailable"}`, status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /orgs/acme/actions/runners", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("name") != "billet-l1" || r.URL.Query().Get("per_page") != "100" {
					t.Errorf("runner recovery query = %q", r.URL.RawQuery)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer cached-token" {
					t.Errorf("Authorization = %q", got)
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				fmt.Fprint(w, tc.listed)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := newRunnerGroupPolicyClient(srv.Client(), srv.URL, "acme", 11, 22, key)
			c.token = "cached-token"
			c.expiresAt = time.Now().Add(time.Hour)
			got, err := c.InspectEphemeralRunner(t.Context(), "billet-l1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("InspectEphemeralRunner succeeded: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("InspectEphemeralRunner: %v", err)
			}
			if got.Present != tc.wantPresent || got.Busy != tc.wantBusy || got.RunnerID != tc.wantID {
				t.Errorf("recovery = %+v, want present %v busy %v id %d",
					got, tc.wantPresent, tc.wantBusy, tc.wantID)
			}
		})
	}
}
