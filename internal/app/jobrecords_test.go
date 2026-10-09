package app

import (
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

// THE APP THAT READS A JOB IS THE ONE OF THE TARGET THAT SERVED IT, all the way
// to the request: with two targets, each tier's job is read with its own
// target's installation, and the other's is never asked.
func TestOpenJobRecordsAuthenticatesAsTheJobsTarget(t *testing.T) {
	dir := t.TempDir()
	keys := map[string]string{}
	for _, name := range []string{"acme", "beta"} {
		keys[name] = filepath.Join(dir, name+".pem")
		if err := os.WriteFile(keys[name], []byte(testPrivateKey(t)), 0o600); err != nil {
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
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
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
