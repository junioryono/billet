package metrics

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// EACH REQUEST IS COUNTED UNDER ITS INSTALLATION, ITS OPERATION AND ITS ANSWER'S
// CLASS, and a request no answer came to, or one with a status outside HTTP's,
// under none rather than as an answer.
func TestTheGitHubRequestsAreCountedByAnswer(t *testing.T) {
	t.Parallel()

	reg, err := New("server")
	if err != nil {
		t.Fatal(err)
	}
	g, err := reg.GitHubRESTRequests()
	if err != nil {
		t.Fatal(err)
	}
	o22, o23 := g.For("22"), g.For("23")
	o22.GitHubRESTRequest("list the run's jobs", 200)
	o22.GitHubRESTRequest("list the run's jobs", 204)
	o22.GitHubRESTRequest("list the run's jobs", 403)
	o22.GitHubRESTRequest("list the run's jobs", 0)
	o22.GitHubRESTRequest("list the run's jobs", 999)
	o23.GitHubRESTRequest("get runner group", 502)
	o23.GitHubRESTRequest("get runner group", 304)

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
		`billet_github_rest_requests_total{answer="2xx",installation="22",operation="list the run's jobs"} 2`,
		`billet_github_rest_requests_total{answer="4xx",installation="22",operation="list the run's jobs"} 1`,
		`billet_github_rest_requests_total{answer="none",installation="22",operation="list the run's jobs"} 2`,
		`billet_github_rest_requests_total{answer="5xx",installation="23",operation="get runner group"} 1`,
		`billet_github_rest_requests_total{answer="3xx",installation="23",operation="get runner group"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics does not carry %q", want)
		}
	}
	if strings.Contains(body, `answer="9xx"`) || strings.Contains(body, `answer="0xx"`) {
		t.Error("a status outside HTTP's became an answer class")
	}
}
