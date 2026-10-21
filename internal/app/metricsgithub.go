package app

import (
	"context"
	"strconv"

	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/metrics"
	"github.com/junioryono/billet/internal/server"
)

// A target's client tells the request counter of each request through
// billetgithub.RESTObserver, which internal/metrics satisfies without importing
// the client.
var _ billetgithub.RESTObserver = (*metrics.GitHubRESTObserver)(nil)

// Each App installation's REST budget, as GitHub last stated it.
const (
	familyGitHubRESTLimit     = "billet_github_rest_limit"
	familyGitHubRESTRemaining = "billet_github_rest_remaining"
	familyGitHubRESTUsed      = "billet_github_rest_used"
	familyGitHubRESTReset     = "billet_github_rest_reset_timestamp_seconds"
	familyGitHubRESTStated    = "billet_github_rest_stated_timestamp_seconds"
)

var githubFamilies = []metrics.Family{
	{Name: familyGitHubRESTLimit, Labels: []string{"installation"},
		Help: "The App installation's hourly REST budget, as GitHub last stated it."},
	{Name: familyGitHubRESTRemaining, Labels: []string{"installation"},
		Help: "What was left of the installation's hourly REST budget when GitHub last stated it. Every " +
			"client of the installation draws on it, billet's and any other's."},
	{Name: familyGitHubRESTUsed, Labels: []string{"installation"},
		Help: "How much of the installation's hourly REST budget had been used when GitHub last stated it."},
	{Name: familyGitHubRESTReset, Labels: []string{"installation"},
		Help: "When GitHub starts the installation's next hourly REST budget, in Unix seconds."},
	{Name: familyGitHubRESTStated, Labels: []string{"installation"},
		Help: "When billet read the answer that last stated the installation's budget, in Unix seconds; " +
			"the other values are as old as this."},
}

// restAccount is one target's App client and the installation it counts
// against.
type restAccount struct {
	installation string
	account      billetgithub.RESTAccount
}

// GitHubMetrics is each App installation's REST budget as metrics: the budget
// GitHub stated in its last answer, read from the targets' clients at every
// scrape, and the requests themselves, counted as they are answered.
func (cp *ControlPlane) GitHubMetrics() MetricsSource {
	return githubSource(cp.targets)
}

func githubSource(targets []server.Target) MetricsSource {
	var accounts []restAccount

	for _, t := range targets {
		if p, ok := t.Provisioner.(Provisioner); ok && p.Client != nil {
			accounts = append(accounts, restAccount{
				installation: strconv.FormatInt(t.Config.InstallationID, 10), account: p.Client,
			})
		}
	}

	return MetricsSource{
		name:     "github",
		families: githubFamilies,
		read:     githubSnapshot(accounts),
		attach: func(reg *metrics.Registry) error {
			requests, err := reg.GitHubRESTRequests()
			if err != nil {
				return err
			}

			for _, a := range accounts {
				a.account.ObserveREST(requests.For(a.installation))
			}

			return nil
		},
	}
}

func githubSnapshot(accounts []restAccount) func(context.Context) (metrics.Snapshot, error) {
	return func(context.Context) (metrics.Snapshot, error) {
		// SEVERAL TARGETS MAY SHARE AN INSTALLATION, each with a client of its
		// own: the latest statement is the installation's, by the same order a
		// client keeps its own, and one series each.
		newest := map[string]billetgithub.RESTBudget{}

		for _, a := range accounts {
			b, ok := a.account.RESTBudget()
			if !ok {
				continue
			}

			if seen, ok := newest[a.installation]; !ok || b.Supersedes(seen) {
				newest[a.installation] = b
			}
		}

		snap := metrics.Snapshot{}

		add := func(family string, value float64, installation string) {
			snap[family] = append(snap[family], metrics.Sample{Labels: []string{installation}, Value: value})
		}

		// AN INSTALLATION NO ANSWER HAS STATED A BUDGET FOR IS LEFT OUT, never
		// reported as a budget of zero.
		for installation, b := range newest {
			add(familyGitHubRESTLimit, float64(b.Limit), installation)
			add(familyGitHubRESTRemaining, float64(b.Remaining), installation)
			add(familyGitHubRESTUsed, float64(b.Used), installation)
			add(familyGitHubRESTReset, float64(b.Reset.Unix()), installation)
			add(familyGitHubRESTStated, float64(b.Read.UnixNano())/1e9, installation)
		}

		return snap, nil
	}
}
