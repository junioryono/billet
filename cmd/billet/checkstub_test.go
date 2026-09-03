package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubGitHubUnverifiable points cmdCheck's App verification at a fake that
// always answers 502, which classifies as UNVERIFIABLE — the advisory band —
// so a test about some other subsystem proceeds past the github line without
// ever reaching the real GitHub. A unit test must never depend on api.github.com.
func stubGitHubUnverifiable(t *testing.T) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	prev := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = prev })
}

// THE CHECK'S GITHUB LINE, both bands, through the real cmdCheck wiring: an
// exact installation prints verified; GitHub-down prints UNVERIFIED and the
// command still exits 0, because "could not reach a verdict" is not a verdict
// — while a definite mismatch is a hard failure.
func TestCheckReportsTheGitHubVerdictBands(t *testing.T) {
	// The ambient variable would skip all three GitHub exchanges and invert
	// every assertion below; pin it empty so the test means the same thing in
	// every environment.
	t.Setenv("BILLET_MAINTENANCE", "")

	cfgPath := writeCheckConfig(t)

	// Band 1: verified.
	exact := httptest.NewServer(withRunnerGroupEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id": 42, "permissions": {
			"metadata": "read", "organization_self_hosted_runners": "write"}}`)
		})))
	t.Cleanup(exact.Close)
	prev := githubAPIBase
	githubAPIBase = exact.URL
	t.Cleanup(func() { githubAPIBase = prev })

	var checkErr error
	out := capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })
	if checkErr != nil {
		t.Fatalf("check with an exact installation failed: %v\n%s", checkErr, out)
	}
	if !strings.Contains(out, "github   verified: installation 42") {
		t.Errorf("the verified band is not reported:\n%s", out)
	}

	// Band 2: unverifiable, still exit 0, said by name.
	stubGitHubUnverifiable(t)
	out = capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })
	if checkErr != nil {
		t.Fatalf("an unverifiable App failed the check: %v\n%s", checkErr, out)
	}
	if !strings.Contains(out, "github   UNVERIFIED") {
		t.Errorf("the advisory band is not reported by name:\n%s", out)
	}

	// Band 3: a definite mismatch is fatal.
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id": 99, "permissions": {
			"metadata": "read", "organization_self_hosted_runners": "write"}}`)
	}))
	t.Cleanup(wrong.Close)
	githubAPIBase = wrong.URL

	out = capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })
	if checkErr == nil {
		t.Fatal("a mismatched installation id passed the check")
	}
	for _, must := range []string{"42", "99"} {
		if !strings.Contains(checkErr.Error(), must) {
			t.Errorf("the verdict does not name %s: %v", must, checkErr)
		}
	}
	// FAILED is reported in place AND the local sections still ran — a broken
	// App must not hide the local diagnostics behind it.
	if !strings.Contains(out, "github   FAILED") {
		t.Errorf("the fatal band is not reported in place:\n%s", out)
	}
	if !strings.Contains(out, "tiers    ") {
		t.Errorf("a github verdict aborted the local sections:\n%s", out)
	}
}

// THE PROBE FLAG'S WIRING, through the real cmdCheck: with a fenced state dir,
// the probe opens through the fence and the github line reads skipped; without
// the flag, ErrMaintenance surfaces with the remedy naming the flag.
func TestCheckMaintenanceProbeCrossesTheFence(t *testing.T) {
	dir := t.TempDir()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	stateDir := filepath.Join(dir, "server")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("make state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "billet.maintenance"),
		[]byte("host upgrade\n"), 0o600); err != nil {
		t.Fatalf("fence: %v", err)
	}

	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`server:
  listen: 127.0.0.1:7717
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 7
  installation_id: 42
  private_key_path: %s
tiers:
  - label: billet-4vcpu
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: trusted
    runner_group: billet-trial
    workflows:
      - acme/repo/.github/workflows/ci.yml@refs/heads/main
`, stateDir, keyPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	// Without the flag: refused at the fence, remedy named.
	var checkErr error
	capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })
	if checkErr == nil {
		t.Fatal("a fenced ledger was opened without the probe flag")
	}
	if !strings.Contains(checkErr.Error(), "--maintenance-probe") {
		t.Errorf("the fence refusal does not name the flag: %v", checkErr)
	}

	// With the flag: crosses, and the network probes are skipped by name.
	out := capture(t, func() {
		checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath, "--maintenance-probe"})
	})
	if checkErr != nil {
		t.Fatalf("the typed probe could not cross the fence: %v\n%s", checkErr, out)
	}
	if !strings.Contains(out, "github   (verification skipped during maintenance)") {
		t.Errorf("the probe did not skip the App verification by name:\n%s", out)
	}
}

// withRunnerGroupEndpoints wraps an installation-verification handler so the
// trusted-tier runner-group check can also reach a verdict.
//
// `billet check` validates a trusted tier's group against GitHub, which the
// server has always done at startup and check used to pass over. These stubs
// answered every path with the installation JSON, so once check started asking
// about groups it got that JSON for a token request and failed on the shape.
// Serving the endpoints keeps the existing bands meaningful instead of moving
// the group check behind a flag to keep old tests green.
//
// The group is deliberately visible to ALL repositories: these tests are about
// the installation bands, and `visibility: all` needs no repository grant, so
// nothing here depends on the grant rule that has its own test in
// internal/github.
func withRunnerGroupEndpoints(installation http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"stub","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

		case strings.HasSuffix(r.URL.Path, "/actions/runner-groups"):
			_, _ = fmt.Fprint(w, `{"runner_groups":[{"id":9,"name":"billet-trial","default":false}]}`)

		case strings.Contains(r.URL.Path, "/actions/runner-groups/"):
			_, _ = fmt.Fprint(w, `{"restricted_to_workflows":true,"visibility":"all",
				"selected_workflows":["acme/repo/.github/workflows/ci.yml@refs/heads/main"]}`)

		default:
			installation.ServeHTTP(w, r)
		}
	})
}

// THE GAP THAT MADE A GREEN CHECK MEANINGLESS.
//
// A trusted tier's runner group is validated by the server at startup. `billet
// check` did not look at it, so a deployment whose group was missing got a green
// check and then a server that refused to start — while `billet init` had
// printed that this very step confirms "any runner-group policy". Two operators
// hit exactly that as their first failure on a fresh host.
//
// The unverifiable case is the other half of the contract and is the one a
// naive fix breaks: an unreachable GitHub is ADVISORY for the App probe, and a
// group lookup against the same unreachable API cannot reach a verdict either,
// so it must not turn "could not tell" into "failed".
func TestCheckValidatesATrustedTiersRunnerGroup(t *testing.T) {
	t.Run("a missing group fails the check and names the tier", func(t *testing.T) {
		cfgPath := writeCheckConfig(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/access_tokens"):
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"token":"stub","expires_at":%q}`,
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			case strings.HasSuffix(r.URL.Path, "/actions/runner-groups"):
				// The organization has groups; none of them is the tier's.
				_, _ = fmt.Fprint(w, `{"runner_groups":[{"id":1,"name":"Default","default":true}]}`)
			default:
				_, _ = fmt.Fprint(w, `{"id": 42, "permissions": {
					"metadata": "read", "organization_self_hosted_runners": "write"}}`)
			}
		}))
		t.Cleanup(srv.Close)

		prev := githubAPIBase
		githubAPIBase = srv.URL
		t.Cleanup(func() { githubAPIBase = prev })

		var checkErr error
		out := capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })

		if checkErr == nil {
			t.Fatalf("a missing runner group passed the check:\n%s", out)
		}
		if !strings.Contains(out, "runner group FAILED") {
			t.Errorf("the report does not show the group failing:\n%s", out)
		}
		if !strings.Contains(out, "billet-trial") {
			t.Errorf("the report does not name the group:\n%s", out)
		}
	})

	t.Run("an unreachable GitHub stays advisory", func(t *testing.T) {
		cfgPath := writeCheckConfig(t)
		stubGitHubUnverifiable(t)

		var checkErr error
		out := capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })

		if checkErr != nil {
			t.Fatalf("an unreachable GitHub failed the check: %v\n%s", checkErr, out)
		}
		if strings.Contains(out, "runner group FAILED") {
			t.Errorf("a group verdict was reported against an unreachable GitHub:\n%s", out)
		}
		if !strings.Contains(out, "UNVERIFIED") {
			t.Errorf("the advisory band is not reported:\n%s", out)
		}
	})
}

// writeCheckConfig writes a config `billet check` accepts, with one TRUSTED
// tier — the shape whose runner group check must verify against GitHub.
//
// Extracted rather than copied: two tests now need it, and a second spelling of
// a fixture is how the two drift until one of them stops testing what its name
// says.
func writeCheckConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`server:
  listen: 127.0.0.1:7717
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 7
  installation_id: 42
  private_key_path: %s
tiers:
  - label: billet-4vcpu
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: trusted
    runner_group: billet-trial
    workflows:
      - acme/repo/.github/workflows/ci.yml@refs/heads/main
`, filepath.Join(dir, "server"), keyPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return cfgPath
}
