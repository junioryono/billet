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
	"testing"
)

// checkConfigFixture writes a control-plane config and its App key, and returns
// the config path. The App ids match the "exact installation" stub below.
func checkConfigFixture(t *testing.T) string {
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

// pointGitHubAt serves the given handler as GitHub for the duration of a test.
func pointGitHubAt(t *testing.T, handler http.HandlerFunc) {
	t.Helper()

	srv := httptest.NewServer(withRunnerGroupEndpoints(handler))
	t.Cleanup(srv.Close)

	prev := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = prev })
}

func exactInstallation(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, `{"id": 42, "permissions": {
		"metadata": "read", "organization_self_hosted_runners": "write"}}`)
}

// THE VERDICT IS A VALUE, not a line of text a caller has to recognise.
//
// `billet check` prints all four of these bands and exits 0 for three of them,
// so a caller that reads only the error cannot tell them apart — which is the
// whole reason runCheck reports what it established.
func TestRunCheckReportsWhatTheGitHubProbeEstablished(t *testing.T) {
	// The ambient variable skips the exchange and would invert every case here;
	// pin it empty so the test means the same thing in every environment.
	t.Setenv("BILLET_MAINTENANCE", "")

	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    githubVerdict
		wantErr bool
	}{
		{
			name:    "an exact installation is verified",
			handler: exactInstallation,
			want:    githubVerified,
		},
		{
			name: "an unreachable GitHub reaches no verdict",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			},
			want: githubUnverifiable,
		},
		{
			name: "a mismatched installation is a refusal",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, `{"id": 99, "permissions": {
					"metadata": "read", "organization_self_hosted_runners": "write"}}`)
			},
			want:    githubFailed,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := checkConfigFixture(t)
			pointGitHubAt(t, tc.handler)

			var (
				report checkReport
				err    error
			)

			out := capture(t, func() {
				report, err = runCheck(t.Context(), checkOptions{configPath: cfgPath})
			})

			if tc.wantErr && err == nil {
				t.Fatalf("the run succeeded where it should have failed:\n%s", out)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("the run failed: %v\n%s", err, out)
			}
			if report.github != tc.want {
				t.Errorf("the GitHub probe reports %s, want %s:\n%s", report.github, tc.want, out)
			}
		})
	}
}

// A SKIP IS NOT A PASS, and this is the property the whole type exists for.
//
// BILLET_MAINTENANCE=1 makes the check skip the App verification entirely and
// still exit 0 — so a caller gating on the error alone would treat a deployment
// whose credential was NEVER CHECKED as one whose credential is good. That
// caller is `billet local up`, which starts a control plane.
func TestAMaintenanceSkipIsNotAPass(t *testing.T) {
	t.Setenv("BILLET_MAINTENANCE", "1")

	cfgPath := checkConfigFixture(t)

	// Served, and deliberately answering the VERIFIED body: if the skip ever
	// stopped applying, this fixture would report verified and the assertion
	// below would be satisfied by a real exchange rather than by the skip.
	// Nothing may reach it.
	reached := false
	pointGitHubAt(t, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		exactInstallation(w, r)
	})

	var (
		report checkReport
		err    error
	)

	out := capture(t, func() {
		report, err = runCheck(t.Context(), checkOptions{configPath: cfgPath})
	})
	if err != nil {
		t.Fatalf("a maintenance run failed: %v\n%s", err, out)
	}

	if reached {
		t.Error("the App verification ran during maintenance; this test can no longer prove the skip")
	}
	if report.github == githubVerified {
		t.Fatalf("a skipped verification reports as verified:\n%s", out)
	}
	if report.github != githubSkipped {
		t.Errorf("a skipped verification reports %s, want %s:\n%s", report.github, githubSkipped, out)
	}
}

// A NODE-ONLY HOST HAS NOTHING TO VERIFY, and that is its own answer rather
// than a quiet zero that could be mistaken for one. Config validation requires
// a github section for the server role, so this state means exactly "this host
// runs no control plane".
func TestANodeOnlyHostHasNoGitHubVerdict(t *testing.T) {
	t.Setenv("BILLET_MAINTENANCE", "")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`node:
  name: probe-node
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: %s
`, filepath.Join(dir, "node"))
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	var (
		report checkReport
		err    error
	)

	out := capture(t, func() {
		report, err = runCheck(t.Context(), checkOptions{configPath: cfgPath})
	})
	if err != nil {
		t.Fatalf("a node-only check failed: %v\n%s", err, out)
	}
	if report.github != githubNotConfigured {
		t.Errorf("a node-only host reports %s, want %s:\n%s", report.github, githubNotConfigured, out)
	}
}
