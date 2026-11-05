package host

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/tokenbroker"
)

// A BROKER'S ANSWER IS WHAT `billet check` REPORTS, and could-not-tell stays that: a verdict
// from a broker that did not answer is what would stop `local up` for no reason, or worse,
// let it through.
func TestCheckReadsWhatTheBrokerAnswers(t *testing.T) {
	t.Parallel()

	org := map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"}
	withActions := map[string]string{"metadata": "read", "organization_self_hosted_runners": "write", "actions": "read"}

	for name, tc := range map[string]struct {
		permissions map[string]string
		status      int
		verdict     githubVerdict
		says        string
		warns       bool
	}{
		"what billet asks for": {permissions: withActions, verdict: githubVerified, says: "github   verified"},
		"no job records": {
			permissions: org, verdict: githubVerified, says: "github   verified", warns: true,
		},
		"more than billet asks for": {
			permissions: map[string]string{"metadata": "read", "organization_self_hosted_runners": "write", "contents": "write"},
			verdict:     githubFailed, says: "github   FAILED",
		},
		"a broker that cannot tell": {status: http.StatusServiceUnavailable, verdict: githubUnverifiable, says: "UNVERIFIED"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			deployment, err := state.DeploymentID(dir)
			if err != nil {
				t.Fatalf("DeploymentID: %v", err)
			}

			clientKey := filepath.Join(t.TempDir(), "client.key")
			if _, err := tokenbroker.CreateClientKey(clientKey); err != nil {
				t.Fatalf("CreateClientKey: %v", err)
			}

			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)

					if err := json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "message": "not now"}); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
					}

					return
				}

				if err := json.NewEncoder(w).Encode(map[string]any{
					"deployment": deployment, "token": "a-token",
					"expires_at":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					"permissions": tc.permissions,
				}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			}))
			srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
			srv.StartTLS()
			t.Cleanup(srv.Close)

			roots := filepath.Join(t.TempDir(), "roots.pem")
			if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{
				Type: "CERTIFICATE", Bytes: srv.Certificate().Raw,
			}), 0o600); err != nil {
				t.Fatalf("write the roots: %v", err)
			}

			cfg := &config.Config{Server: &config.ServerConfig{
				IdentityDir: dir,
				TokenBroker: &config.TokenBrokerConfig{URL: srv.URL, CAFile: roots, ClientKeyPath: clientKey},
			}}

			var out, errs bytes.Buffer

			env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}

			verdict, err := checkBrokerTokens(t.Context(), env, cfg,
				config.GitHubTarget{Name: "default", Org: "acme", AppID: 12345, InstallationID: 67890}, false)

			// AN ERROR ONLY FOR A VERDICT: could-not-tell is a report, and stops nothing.
			if (err != nil) != (tc.verdict == githubFailed) {
				t.Errorf("verdict %v with error %v", tc.verdict, err)
			}

			if verdict != tc.verdict {
				t.Errorf("verdict %v, want %v:\n%s", verdict, tc.verdict, out.String())
			}
			if !strings.Contains(out.String(), tc.says) {
				t.Errorf("printed:\n%s\nwant it to say %q", out.String(), tc.says)
			}
			if got := strings.Contains(out.String(), "cannot read GitHub's job records"); got != tc.warns {
				t.Errorf("warned about job records: %v, want %v:\n%s", got, tc.warns, out.String())
			}
		})
	}
}

// AN ARCHIVE OF A DEPLOYMENT ON A BROKER CLAIMS IT HOLDS NO KEY, and the claim is the
// command's to make: a missing file is not what decides that a restore wants no key.
func TestABackupOnABrokerClaimsItHoldsNoKey(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)

	clientKey := filepath.Join(t.TempDir(), "client.key")
	if _, err := tokenbroker.CreateClientKey(clientKey); err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	body, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}

	broker := strings.Replace(string(body), "  private_key_path: "+f.keyPath+"\n", "", 1)
	broker = strings.Replace(broker, "  max_memory: 32GiB\n", "  max_memory: 32GiB\n  token_broker:\n"+
		"    url: https://broker.internal:8443\n    client_key_path: "+clientKey+"\n", 1)
	if broker == string(body) {
		t.Fatal("the fixture did not change")
	}

	if err := os.WriteFile(f.configPath, []byte(broker), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backup")
	if err := cmdLocalBackup(t.Context(), processEnv(), []string{"--config", f.configPath, "--out", dest}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	a, err := deployarchive.Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("the archive it wrote does not open: %v", err)
	}

	if !a.Manifest.KeysWithBroker {
		t.Error("the archive does not claim its keys are with a broker")
	}

	if _, ok := a.Entry(deployarchive.EntryAppKey); ok {
		t.Error("the archive carries an App key")
	}

	// AND THE RESTORE'S SIDE OF THE SAME CLAIM, from the config it is restoring under.
	cfg, err := config.Load(f.configPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var target deployarchive.Target

	archiveTargets(cfg, &target)

	if !target.KeysWithBroker {
		t.Error("a restore under this config would not know its keys are with a broker")
	}
}
