package app

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
)

// jobTargetsConfig serves two owners: acme by organization and one of
// someone's repositories, with a tier on each.
func jobTargetsConfig() *config.Config {
	return &config.Config{
		GitHub:  &config.GitHubConfig{Org: "acme"},
		Targets: []config.GitHubConfig{{Name: "personal", Repository: "someone/tool"}},
		Tiers: []config.Tier{
			{Label: "acme-2vcpu", Target: config.DefaultTargetName},
			{Label: "tool-2vcpu", Target: "personal"},
		},
	}
}

// THE TARGET THAT READS A JOB IS ITS TIER'S, and only if that target holds the
// job's repository; a tier no longer declared falls back to the one target
// holding the repository, and none or two is refused, never guessed.
func TestAJobIsReadWithTheAppThatServedIt(t *testing.T) {
	t.Parallel()

	two := jobTargetsConfig()
	two.Targets = append(two.Targets, config.GitHubConfig{Name: "acme-too", Org: "ACME"})

	for name, tc := range map[string]struct {
		cfg              *config.Config
		tier, repository string
		want, refusal    string
	}{
		"the tier's organization":      {jobTargetsConfig(), "acme-2vcpu", "Acme/api", config.DefaultTargetName, ""},
		"the tier's repository":        {jobTargetsConfig(), "tool-2vcpu", "SOMEONE/Tool", "personal", ""},
		"a repository the tier lacks":  {jobTargetsConfig(), "tool-2vcpu", "someone/other", "", "does not hold"},
		"another owner's repository":   {jobTargetsConfig(), "acme-2vcpu", "evil/api", "", "does not hold"},
		"a removed tier, one holder":   {jobTargetsConfig(), "gone", "acme/api", config.DefaultTargetName, ""},
		"a removed tier, no holder":    {jobTargetsConfig(), "gone", "evil/api", "", "no target holds"},
		"a removed tier, two holders":  {two, "gone", "acme/api", "", "refusing to guess"},
		"a repository with no owner":   {jobTargetsConfig(), "acme-2vcpu", "api", "", "not owner/name"},
		"a tier naming no real target": {&config.Config{GitHub: &config.GitHubConfig{Org: "acme"}, Tiers: []config.Tier{{Label: "x", Target: "nowhere"}}}, "x", "acme/api", "", "names no target"},
	} {
		got, err := jobTarget(tc.cfg, tc.tier, tc.repository)
		switch {
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: %v, %v, want refused with %q", name, got.Name, err, tc.refusal)
		case tc.refusal == "" && (err != nil || got.Name != tc.want):
			t.Errorf("%s: %q, %v, want %q", name, got.Name, err, tc.want)
		}
	}
}

// appJWT checks a token exchange's App JWT as GitHub would: signed by the
// App's key, and issued by the App, for the installation it asks for. It
// returns what is wrong, or "".
func appJWT(r *http.Request, apps map[string]struct {
	issuer string
	key    *rsa.PublicKey
},
) string {
	installation := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
	app, ok := apps[installation]
	if !ok {
		return "an exchange for installation " + installation + ", which no App has"
	}
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		return "a bearer that is not a JWT"
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "an unreadable signature"
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(app.key, crypto.SHA256, digest[:], signature) != nil {
		return "installation " + installation + " asked with another App's key"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "an unreadable payload"
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Iss != app.issuer {
		return "installation " + installation + " asked by issuer " + claims.Iss + ", not " + app.issuer
	}

	return ""
}

// THE APP THAT READS A JOB IS THE ONE OF THE TARGET THAT SERVED IT, all the way
// to the request: with two targets, each tier's job is read with its own
// target's installation, issuer and key, and the other's is never asked.
func TestOpenJobRecordsAuthenticatesAsTheJobsTarget(t *testing.T) {
	dir := t.TempDir()
	keys := map[string]string{}
	apps := map[string]struct {
		issuer string
		key    *rsa.PublicKey
	}{}
	for name, app := range map[string]struct{ installation, issuer string }{
		"acme": {"2", "1"}, "beta": {"3", "5"},
	} {
		pemKey := testPrivateKey(t)
		block, _ := pem.Decode([]byte(pemKey))
		if block == nil {
			t.Fatalf("the %s key is not PEM", name)
		}
		private, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("parse the %s key: %v", name, err)
		}
		apps[app.installation] = struct {
			issuer string
			key    *rsa.PublicKey
		}{app.issuer, &private.PublicKey}
		keys[name] = filepath.Join(dir, name+".pem")
		if err := os.WriteFile(keys[name], []byte(pemKey), 0o600); err != nil {
			t.Fatalf("write the %s key: %v", name, err)
		}
	}
	cfgPath := filepath.Join(dir, "billet.yaml")
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + filepath.Join(dir, "state") + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + keys["acme"] + `
targets:
  - name: beta
    org: beta
    app_id: 5
    installation_id: 3
    private_key_path: ` + keys["beta"] + `
tiers:
  - label: acme-2vcpu
    target: default
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
  - label: beta-2vcpu
    target: beta
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	var mu sync.Mutex
	var exchanged []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			mu.Lock()
			exchanged = append(exchanged, r.URL.Path)
			mu.Unlock()
			if wrong := appJWT(r, apps); wrong != "" {
				t.Errorf("the token exchange was refused as GitHub would refuse it: %s", wrong)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_jobrecordstesttoken","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		fmt.Fprint(w, `{"total_count":0,"jobs":[]}`)
	}))
	t.Cleanup(srv.Close)
	prev := GitHubAPIBase
	GitHubAPIBase = srv.URL
	t.Cleanup(func() { GitHubAPIBase = prev })

	for _, tc := range []struct {
		tier, owner, want string
	}{
		{"beta-2vcpu", "beta", "/app/installations/3/access_tokens"},
		{"acme-2vcpu", "acme", "/app/installations/2/access_tokens"},
	} {
		mu.Lock()
		exchanged = nil
		mu.Unlock()
		records, err := OpenJobRecords(t.Context(), cfgPath, tc.tier, tc.owner+"/api")
		if err != nil {
			t.Fatalf("OpenJobRecords for %s: %v", tc.tier, err)
		}
		if _, err := records.RunnerJob(t.Context(), tc.owner, "api", 31, "billet-l1"); !errors.Is(err,
			billetgithub.ErrNoRunnerJob) {
			t.Fatalf("RunnerJob for %s = %v, want the fake's empty list", tc.tier, err)
		}
		mu.Lock()
		got := slices.Clone(exchanged)
		mu.Unlock()
		if !slices.Equal(got, []string{tc.want}) {
			t.Errorf("a %s job exchanged tokens at %v, want only %s", tc.tier, got, tc.want)
		}
	}
}
