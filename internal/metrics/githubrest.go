package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// GitHubRESTRequests counts the requests billet made with each App
// installation's token.
type GitHubRESTRequests struct {
	requests *prometheus.CounterVec
}

// GitHubRESTRequests registers and returns the request counter.
func (r *Registry) GitHubRESTRequests() (*GitHubRESTRequests, error) {
	g := &GitHubRESTRequests{requests: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "billet_github_rest_requests_total",
		Help: "Requests billet's REST client made with an App installation's token, by installation, by " +
			"what the request was for (the client's fixed operation names), and by GitHub's answer: 2xx, " +
			"3xx, 4xx, 5xx, or none when no answer came. Every one draws on the installation's hourly " +
			"budget, which billet_github_rest_remaining reports.",
	}, []string{"installation", "operation", "answer"})}

	if err := r.reg.Register(g.requests); err != nil {
		return nil, fmt.Errorf("metrics: register the GitHub REST requests: %w", err)
	}

	return g, nil
}

// For is the observer one installation's client tells of its requests. It
// satisfies internal/github's RESTObserver by its method alone.
func (g *GitHubRESTRequests) For(installation string) *GitHubRESTObserver {
	return &GitHubRESTObserver{requests: g.requests, installation: installation}
}

// GitHubRESTObserver counts one installation's requests.
type GitHubRESTObserver struct {
	requests     *prometheus.CounterVec
	installation string
}

// GitHubRESTRequest counts one request by its operation and GitHub's answer.
func (o *GitHubRESTObserver) GitHubRESTRequest(operation string, status int) {
	answer := "none"
	if status >= 200 && status < 600 {
		answer = fmt.Sprintf("%dxx", status/100)
	}
	o.requests.WithLabelValues(o.installation, operation, answer).Inc()
}
