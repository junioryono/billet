package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
)

// restGitHub is a GitHub that answers an installation's token exchange and one
// run's jobs, stating the installation's budget as remaining.
func restGitHub(t *testing.T, installation int64, remaining *atomic.Int64, reset time.Time) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("POST /app/installations/%d/access_tokens", installation),
		func(w http.ResponseWriter, _ *http.Request) {
			// THE APP'S OWN BUDGET, which is not the installation's.
			w.Header().Set("X-Ratelimit-Limit", "1")
			w.Header().Set("X-Ratelimit-Remaining", "1")
			w.Header().Set("X-Ratelimit-Used", "0")
			w.Header().Set("X-Ratelimit-Reset", "1")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31/jobs", func(w http.ResponseWriter, _ *http.Request) {
		left := remaining.Load()
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Remaining", strconv.FormatInt(left, 10))
		w.Header().Set("X-Ratelimit-Used", strconv.FormatInt(5000-left, 10))
		w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		fmt.Fprint(w, `{"total_count":0,"jobs":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv.URL
}

// restClient is a target's client with the App's credentials, reading GitHub's
// REST API at api.
func restClient(t *testing.T, installation int64, api string) *scaleset.Client {
	t.Helper()

	c, err := scaleset.New(scaleset.Config{
		Target: billetgithub.OrganizationTarget("acme"), ClientID: "1", InstallationID: installation, AppID: 1,
		PrivateKey: testPrivateKey(t), APIURL: api,
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	return c
}

// EACH INSTALLATION'S BUDGET IS WHAT GITHUB LAST STATED TO ITS TARGETS' CLIENTS,
// served at the scrape, and every request made with its token is counted by
// GitHub's answer: through the assembled targets, the way the control plane
// builds them. Targets of one installation are one series, the latest
// statement's: of the latest hour, the lowest remaining, whichever client read
// it last; an installation that has made no request has none; and the
// headroom the sweep reads through a target is what that target's own client
// last saw.
func TestTheGitHubSourceServesEachInstallationsBudget(t *testing.T) {
	t.Parallel()

	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	var remaining22, remaining23 atomic.Int64
	api22 := restGitHub(t, 22, &remaining22, reset)
	api23 := restGitHub(t, 23, &remaining23, reset)

	servers, _, err := BuildTargets([]Target{
		{Config: config.GitHubTarget{Name: "first", Org: "acme", InstallationID: 22}, Client: restClient(t, 22, api22)},
		{Config: config.GitHubTarget{Name: "middle", Org: "acme", InstallationID: 22}, Client: restClient(t, 22, api22)},
		{Config: config.GitHubTarget{Name: "last", Org: "acme", InstallationID: 22}, Client: restClient(t, 22, api22)},
		{Config: config.GitHubTarget{Name: "late", Org: "acme", InstallationID: 22}, Client: restClient(t, 22, api22)},
		{Config: config.GitHubTarget{Name: "idle", Org: "acme", InstallationID: 23}, Client: restClient(t, 23, api23)},
	})
	if err != nil {
		t.Fatalf("BuildTargets: %v", err)
	}

	m, err := ServeMetrics(t.Context(), "server", &config.MetricsConfig{Listen: "127.0.0.1:0"},
		githubSource(servers))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(t.Context()) })

	read := func(target server.Target) {
		t.Helper()
		reader, ok := target.Provisioner.(server.QueueTimes)
		if !ok {
			t.Fatalf("target %s reads no queue times", target.Config.Name)
		}
		if _, err := reader.RunQueueTimes(t.Context(), "acme", "api", 31); err != nil {
			t.Fatalf("RunQueueTimes through %s: %v", target.Config.Name, err)
		}
	}

	// The latest statement is the middle target's, so neither the first listed
	// nor the last is what the series reports by position; and an answer of the
	// same hour read after it, stating more left, as one to a request that left
	// earlier would, does not restore what it spent.
	remaining22.Store(4000)
	read(servers[0])
	remaining22.Store(3995)
	read(servers[2])
	remaining22.Store(3990)
	read(servers[1])
	remaining22.Store(3998)
	read(servers[3])

	body := scrapeMetrics(t, m)
	for series, want := range map[string]float64{
		`billet_github_rest_limit{installation="22"}`:                                                       5000,
		`billet_github_rest_remaining{installation="22"}`:                                                   3990,
		`billet_github_rest_used{installation="22"}`:                                                        1010,
		`billet_github_rest_reset_timestamp_seconds{installation="22"}`:                                     float64(reset.Unix()),
		`billet_github_rest_requests_total{answer="2xx",installation="22",operation="list the run's jobs"}`: 4,
		`billet_scrape_up{source="github"}`:                                                                 1,
	} {
		if got := sampleValue(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if stated := sampleValue(t, body, `billet_github_rest_stated_timestamp_seconds{installation="22"}`); time.Since(
		time.Unix(int64(stated), 0)) > time.Minute {
		t.Errorf("the budget was stated at %v, want just now", stated)
	}
	if strings.Contains(body, `installation="23"`) {
		t.Error("an installation that has made no request has a series")
	}

	headroom, ok := servers[0].Provisioner.(server.RESTHeadroom)
	if !ok {
		t.Fatal("the listener's provisioner carries no REST headroom")
	}
	left, limit, at, stated := headroom.RESTHeadroom()
	if !stated || left != 4000 || limit != 5000 || !at.Equal(reset) {
		t.Errorf("the first target's headroom = %d of %d until %s (%t), want 4000 of 5000 until %s",
			left, limit, at, stated, reset)
	}
}

// scrapeMetrics reads the served endpoint once.
func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), metricsTestWait)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.srv.Addr().String()+"/metrics", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}
