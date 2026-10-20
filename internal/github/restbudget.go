package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RESTBudget is an installation's hourly REST budget as GitHub stated it in the
// last answer to a request made with the installation's token.
//
// Every request made with the installation's token draws on it, whoever makes
// it: among billet's own, this client's runner-group checks, offline-runner
// inspection, cache-authority reads and queue-time sweep, the scale-set
// client's registration-token requests, and the CLI's reads. GitHub gives an
// installation 5,000 requests an hour, 50 more for each repository and each
// organization user past 20, at most 12,500 (15,000 on Enterprise Cloud), per
// its REST rate-limit reference as read on 2026-10-18 (#583).
type RESTBudget struct {
	Limit     int64
	Remaining int64
	Used      int64
	// Reset is when GitHub starts the next hour's budget.
	Reset time.Time
	// Read is when billet read the answer that stated it.
	Read time.Time
}

// RESTObserver is told of every request made with an installation's token, as
// it is answered, by the operation's fixed name and GitHub's status, zero when
// no answer came. It must return at once.
type RESTObserver interface {
	GitHubRESTRequest(operation string, status int)
}

// RESTAccount is what a REST client says about its installation's budget.
type RESTAccount interface {
	// RESTBudget is the budget as GitHub last stated it, false before any
	// answer stated one.
	RESTBudget() (RESTBudget, bool)
	// ObserveREST tells o of every request from now on.
	ObserveREST(o RESTObserver)
}

var _ RESTAccount = (*runnerGroupPolicyClient)(nil)

// restAccount keeps an installation's last stated budget and its observer.
type restAccount struct {
	mu       sync.Mutex
	budget   RESTBudget
	stated   bool
	observer RESTObserver
}

// answered records what an answer said about the budget, and tells the
// observer of the request. An answer missing a header, or carrying one that
// does not read as a count, leaves the last stated budget as it was: could
// not tell is not zero.
//
// WITHIN ONE HOUR THE LOWEST REMAINING IS KEPT: answers to concurrent requests
// can arrive out of order, and one that left before another was counted must
// not restore what that one spent.
//
// A 401 IS NOT THE INSTALLATION'S ANSWER: GitHub did not take the token, so any
// budget it stated would be another bucket's. Measured on 2026-10-20, a 401 to
// an invalid token carried no X-RateLimit values at all.
func (r *restAccount) answered(operation string, status int, header http.Header, now time.Time) {
	r.mu.Lock()
	observer := r.observer
	if budget, ok := budgetOf(header, now); ok && status != http.StatusUnauthorized {
		if !r.stated || !budget.Reset.Equal(r.budget.Reset) || budget.Remaining <= r.budget.Remaining {
			r.budget, r.stated = budget, true
		}
	}
	r.mu.Unlock()

	if observer != nil {
		observer.GitHubRESTRequest(operation, status)
	}
}

// restOperation keys the operation a request made with the installation's token
// is for, in its context, so a redirect it is answered with is counted under it.
type restOperation struct{}

func withRESTOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, restOperation{}, operation)
}

// maxRESTRedirects is how many redirects a request follows, as many as the
// standard client's default.
const maxRESTRedirects = 10

// redirected is the client's redirect policy. A REDIRECT IS AN ANSWER OF ITS
// OWN, drawing on the budget like any other, so each one a request made with
// the installation's token is answered with is recorded and told before the
// client follows it; the answer at the end of the chain, a redirect it refused
// to follow among them, is recorded by the request's own caller.
func (r *restAccount) redirected(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRESTRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRESTRedirects)
	}
	if operation, ok := req.Context().Value(restOperation{}).(string); ok && req.Response != nil {
		r.answered(operation, req.Response.StatusCode, req.Response.Header, time.Now())
	}

	return nil
}

// budgetOf reads GitHub's X-RateLimit headers.
func budgetOf(header http.Header, now time.Time) (RESTBudget, bool) {
	count := func(name string) (int64, bool) {
		v, err := strconv.ParseInt(header.Get(name), 10, 64)
		return v, err == nil && v >= 0
	}
	limit, ok1 := count("X-Ratelimit-Limit")
	remaining, ok2 := count("X-Ratelimit-Remaining")
	used, ok3 := count("X-Ratelimit-Used")
	reset, ok4 := count("X-Ratelimit-Reset")
	if !ok1 || !ok2 || !ok3 || !ok4 || limit == 0 {
		return RESTBudget{}, false
	}

	return RESTBudget{Limit: limit, Remaining: remaining, Used: used, Reset: time.Unix(reset, 0).UTC(), Read: now},
		true
}

// RESTBudget is the installation's budget as GitHub last stated it.
func (c *runnerGroupPolicyClient) RESTBudget() (RESTBudget, bool) {
	c.rest.mu.Lock()
	defer c.rest.mu.Unlock()

	return c.rest.budget, c.rest.stated
}

// ObserveREST tells o of every request made with the installation's token.
func (c *runnerGroupPolicyClient) ObserveREST(o RESTObserver) {
	c.rest.mu.Lock()
	defer c.rest.mu.Unlock()

	c.rest.observer = o
}
