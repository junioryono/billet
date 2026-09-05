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

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
)

// writeTargetConfig writes a control-plane config whose github block is the
// given scope line, with an untrusted docker tier, and a throwaway App key.
func writeTargetConfig(t *testing.T, scope string, extra string) string {
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

	extraKey := filepath.Join(dir, "app-personal.pem")
	if err := os.WriteFile(extraKey, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the second key: %v", err)
	}

	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`server:
  listen: 127.0.0.1:7717
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
%s
  app_id: 7
  installation_id: 42
  private_key_path: %s
tiers:
  - label: billet-4vcpu
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: untrusted
%s`, filepath.Join(dir, "server"), scope, keyPath, strings.ReplaceAll(extra, "EXTRA_KEY", extraKey))
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return cfgPath
}

// installationAt answers the installation endpoint for one target and records
// every path asked, refusing every runner-group path outright.
func installationAt(t *testing.T, want map[string]string) *[]string {
	t.Helper()

	var asked []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)

		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"stub","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case strings.Contains(r.URL.Path, "/actions/runner-groups"):
			t.Errorf("a runner-group question reached GitHub for a repository target: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		default:
			body, ok := want[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)

				return
			}

			_, _ = fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(srv.Close)

	prev := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = prev })

	return &asked
}

// A repository-scoped deployment verifies its App at the repository's own
// endpoint against the repository permission set, and the runner-group probe
// is skipped by name rather than run against endpoints a repository lacks.
func TestCheckAtRepositoryScopeSkipsTheRunnerGroupProbeAndSaysSo(t *testing.T) {
	t.Setenv("BILLET_MAINTENANCE", "")

	cfgPath := writeTargetConfig(t, "  repository: acme/widgets", "")

	asked := installationAt(t, map[string]string{
		"/repos/acme/widgets/installation": `{"id": 42, "account": {"login": "acme", "type": "User"},
			"permissions": {"metadata": "read", "administration": "write"}}`,
	})

	var checkErr error

	out := capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })
	if checkErr != nil {
		t.Fatalf("check at repository scope failed: %v\n%s", checkErr, out)
	}

	for _, want := range []string{
		"repo     acme/widgets (app 7, installation 42)",
		"github   verified: installation 42 on acme/widgets, permissions exactly as requested for a repository",
		"runner group not probed: a repository target has no runner groups",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, `runner group "" verified`) {
		t.Errorf("the report claims a runner group was verified at repository scope:\n%s", out)
	}

	for _, path := range *asked {
		if strings.HasPrefix(path, "/orgs/") {
			t.Errorf("a repository target was asked about at an organization endpoint: %s", path)
		}
	}
}

// Every target is verified on its own lines, and a failure on one fails the
// check whatever the others said.
func TestCheckReportsEveryTargetAndTheWorstVerdictWins(t *testing.T) {
	t.Setenv("BILLET_MAINTENANCE", "")

	cfgPath := writeTargetConfig(t, "  org: acme", `    target: default
  - label: billet-4vcpu-personal
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: untrusted
    target: personal
targets:
  - name: personal
    repository: someone/widgets
    app_id: 8
    installation_id: 43
    private_key_path: EXTRA_KEY
`)

	// Only the organization answers; the repository's installation is a 404,
	// which is a definite verdict.
	_ = installationAt(t, map[string]string{
		"/orgs/acme/installation": `{"id": 42, "account": {"login": "acme", "type": "Organization"},
			"permissions": {"metadata": "read", "organization_self_hosted_runners": "write"}}`,
	})

	var checkErr error

	out := capture(t, func() { checkErr = cmdCheck(t.Context(), []string{"--config", cfgPath}) })
	if checkErr == nil {
		t.Fatalf("a deployment with one uninstalled target passed the check:\n%s", out)
	}

	for _, want := range []string{
		"target   default: org acme (app 7, installation 42)",
		"target   personal: repository someone/widgets (app 8, installation 43)",
		"github   verified: installation 42 on acme",
		"github   FAILED",
		`not installed on repository "someone/widgets"`,
		"target:default",
		"target:personal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// `github-app create --repository ... --target NAME` writes a targets entry,
// leaves the github block alone, and defaults the key beside the config under
// the target's own name.
func TestGitHubAppCreateWritesANamedRepositoryTarget(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	seed := "# my config\ngithub:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: /etc/billet/app.pem\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	calls := stubOnboard(t, testKey(t), nil)

	var err error

	out := capture(t, func() {
		err = githubAppCreate(t.Context(), []string{
			"--repository", "someone/widgets", "--target", "personal", "--config", cfgPath, "--no-browser",
		})
	})
	if err != nil {
		t.Fatalf("githubAppCreate: %v\n%s", err, out)
	}

	if *calls != 1 {
		t.Fatalf("onboarding ran %d times, want once", *calls)
	}

	// THE WIDER GRANT IS DISCLOSED for a repository target.
	for _, want := range []string{"administration", "ONLY permission GitHub offers"} {
		if !strings.Contains(out, want) {
			t.Errorf("the disclosure does not say %q:\n%s", want, out)
		}
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}

	var doc struct {
		GitHub struct {
			Org   string `yaml:"org"`
			AppID int64  `yaml:"app_id"`
		} `yaml:"github"`
		Targets []struct {
			Name           string `yaml:"name"`
			Org            string `yaml:"org"`
			Repository     string `yaml:"repository"`
			AppID          int64  `yaml:"app_id"`
			InstallationID int64  `yaml:"installation_id"`
			ClientID       string `yaml:"client_id"`
			PrivateKeyPath string `yaml:"private_key_path"`
		} `yaml:"targets"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the config no longer parses: %v\n%s", err, raw)
	}

	if doc.GitHub.Org != "acme" || doc.GitHub.AppID != 1 {
		t.Errorf("the github block was disturbed: %+v", doc.GitHub)
	}

	if !strings.HasPrefix(string(raw), "# my config\n") {
		t.Errorf("the operator's comment was lost:\n%s", raw)
	}

	if len(doc.Targets) != 1 {
		t.Fatalf("targets = %+v, want one entry", doc.Targets)
	}

	got := doc.Targets[0]
	wantKey := filepath.Join(dir, "app-private-key-personal.pem")

	if got.Name != "personal" || got.Repository != "someone/widgets" || got.Org != "" ||
		got.AppID != stubAppID || got.InstallationID != stubInstallationID ||
		got.ClientID != "Iv1.stub" || got.PrivateKeyPath != wantKey {
		t.Errorf("the targets entry is %+v, want personal someone/widgets app %d installation %d at %s",
			got, stubAppID, stubInstallationID, wantKey)
	}

	if _, err := os.Stat(wantKey); err != nil {
		t.Errorf("the key was not saved under the target's own name: %v", err)
	}

	// AND THE RESULT LOADS as a two-target config once the tiers name theirs.
	loadable := string(raw) + "server:\n  listen: 127.0.0.1:7717\n  state_dir: " + dir +
		"\n  max_vcpu: 8\n  max_memory: 32GiB\ntiers:\n  - label: t\n    provider: docker\n" +
		"    vcpu: 2\n    memory: 4GiB\n    image: ghcr.io/x/y\n    target: personal\n"

	if _, err := config.Parse("test", []byte(loadable)); err != nil {
		t.Errorf("the written config does not load: %v", err)
	}
}

func TestGitHubAppCreateRefusesTwoScopesAndNone(t *testing.T) {
	calls := stubOnboard(t, testKey(t), nil)

	for name, args := range map[string][]string{
		"both":         {"--org", "acme", "--repository", "acme/widgets"},
		"neither":      {"--no-browser"},
		"a bad repo":   {"--repository", "acme"},
		"an empty tgt": {"--org", "acme", "--target", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := githubAppCreate(t.Context(), append(args, "--no-browser")); err == nil {
				t.Errorf("githubAppCreate accepted %s", name)
			}
		})
	}

	if *calls != 0 {
		t.Errorf("onboarding ran %d times for refused flags", *calls)
	}
}

// --target names where an UNDECLARED tier lives; a declared tier's target is
// the config's to say.
func TestTeardownTargetFlagAppliesOnlyToAnUndeclaredTier(t *testing.T) {
	cfgPath := writeTargetConfig(t, "  org: acme", "")

	err := cmdTeardown(t.Context(), []string{
		"--config", cfgPath, "--tier", "billet-4vcpu", "--target", "default", "--yes",
	})
	if err == nil || !strings.Contains(err.Error(), "--target names where to look") {
		t.Fatalf("a declared tier with --target: %v", err)
	}
}

// `billet init --repository` writes a repository-scoped github block, and the
// two scope flags together are refused.
func TestInitWritesARepositoryScopedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := cmdInit(t.Context(), []string{
		"--config", path, "--repository", "someone/widgets", "--provider", "firecracker",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the generation: %v", err)
	}

	if !strings.Contains(string(raw), "\n  repository: someone/widgets\n") || strings.Contains(string(raw), "\n  org:") {
		t.Errorf("the generation does not carry the repository scope alone:\n%s", raw)
	}

	// Filled in the way `github-app create --config` fills them for THIS scope,
	// so what is under test is everything init decided.
	if err := writeGitHubBlock(path, githubBlock{
		Repository: "someone/widgets", AppID: 1, InstallationID: 2,
		PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
	}); err != nil {
		t.Fatalf("filling in the app: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the config billet generated does not load: %v", err)
	}

	targets := cfg.GitHubTargets()
	if len(targets) != 1 || !targets[0].IsRepository() || targets[0].Path() != "someone/widgets" {
		t.Errorf("the generated config's target is %+v", targets)
	}

	for _, tier := range cfg.Tiers {
		if tier.Trust.Effective() != config.WorkloadUntrusted {
			t.Errorf("tier %s is %q under a repository target", tier.Label, tier.Trust)
		}
	}

	if err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--org", "acme", "--repository", "someone/widgets", "--provider", "firecracker",
	}); err == nil {
		t.Error("init accepted --org and --repository together")
	}

	if err := cmdInit(t.Context(), []string{
		"--config", filepath.Join(t.TempDir(), "billet.yaml"),
		"--repository", "widgets", "--provider", "firecracker",
	}); err == nil || !strings.Contains(err.Error(), "--repository") {
		t.Errorf("init accepted a repository with no owner: %v", err)
	}
}
