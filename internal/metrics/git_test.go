package metrics

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// THE GIT PROXY'S METRICS COUNT WHAT THEY ARE TOLD: every label value from the
// start at zero, each request under its route, reason and status class, each
// refresh and lookup under its outcome, and a value outside the lists under
// other rather than as a label of its own.
func TestTheGitProxysMetricsCountWhatTheyAreTold(t *testing.T) {
	t.Parallel()

	reg, err := New("node")
	if err != nil {
		t.Fatal(err)
	}
	o, err := reg.GitObserver(GitLabels{
		Routes: []string{"mirror", "github"}, Reasons: []string{"served", "no_mirror"},
		Statuses: []string{"2xx", "5xx"}, RefreshOutcomes: []string{"ok", "unverified"},
		LookupOutcomes: []string{"ok", "rate_limited"},
	})
	if err != nil {
		t.Fatal(err)
	}
	o.GitRequest("mirror", "served", "2xx", 40*time.Millisecond)
	o.GitRequest("mirror", "served", "2xx", 2*time.Second)
	o.GitRequest("github", "no_mirror", "2xx", time.Second)
	o.GitRequest("github", "a reason from somewhere else", "418", time.Second)
	o.GitRefresh("unverified", 3*time.Second)
	o.GitLookup("rate_limited")
	o.GitLookup("rate_limited")

	srv, err := Listen(t.Context(), "127.0.0.1:0", reg.Handler(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	code, body := get(t, srv, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics answered %d", code)
	}
	for _, want := range []string{
		`billet_node_git_requests_total{reason="served",route="mirror",status="2xx"} 2`,
		`billet_node_git_requests_total{reason="no_mirror",route="github",status="2xx"} 1`,
		`billet_node_git_requests_total{reason="no_mirror",route="mirror",status="5xx"} 0`,
		`billet_node_git_requests_total{reason="other",route="github",status="other"} 1`,
		`billet_node_git_request_seconds_count{route="mirror"} 2`,
		`billet_node_git_refreshes_total{outcome="unverified"} 1`,
		`billet_node_git_refreshes_total{outcome="ok"} 0`,
		"billet_node_git_refresh_seconds_count 1",
		`billet_node_git_repository_lookups_total{outcome="rate_limited"} 2`,
		`billet_node_git_repository_lookups_total{outcome="ok"} 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics does not carry %q", want)
		}
	}
	if strings.Contains(body, "somewhere else") || strings.Contains(body, `"418"`) {
		t.Error("a value outside the lists became a label")
	}
}
