package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingObserver keeps every request it is told of.
type recordingObserver struct {
	mu       sync.Mutex
	requests []string
}

func (o *recordingObserver) GitHubRESTRequest(operation string, status int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.requests = append(o.requests, fmt.Sprintf("%s %d", operation, status))
}

// THE BUDGET IS WHAT THE LAST ANSWER WITH THE INSTALLATION'S TOKEN STATED, and
// every such request is told to the observer, refused or not. A 401 is told and
// states nothing: GitHub did not take the token, so a budget it carried would be
// another bucket's.
func TestTheBudgetIsWhatTheLastInstallationAnswerStated(t *testing.T) {
	t.Parallel()

	reset := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	answers := []struct {
		status                 int
		limit, remaining, used string
	}{
		{http.StatusOK, "5000", "4990", "10"},
		{http.StatusOK, "", "", ""},
		{http.StatusForbidden, "5000", "0", "5000"},
		{http.StatusUnauthorized, "60", "59", "1"},
		{http.StatusOK, "5000", "many", "4"},
	}
	var at int
	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		a := answers[at]
		at++
		if a.limit != "" {
			w.Header().Set("X-Ratelimit-Limit", a.limit)
			w.Header().Set("X-Ratelimit-Remaining", a.remaining)
			w.Header().Set("X-Ratelimit-Used", a.used)
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		}
		w.WriteHeader(a.status)
		fmt.Fprint(w, `{"total_count":0,"jobs":[]}`)
	})
	o := &recordingObserver{}
	c.ObserveREST(o)

	if _, ok := c.RESTBudget(); ok {
		t.Fatal("a client that has made no request states a budget")
	}

	want := []RESTBudget{
		{Limit: 5000, Remaining: 4990, Used: 10, Reset: reset},
		{Limit: 5000, Remaining: 4990, Used: 10, Reset: reset},
		{Limit: 5000, Remaining: 0, Used: 5000, Reset: reset},
		{Limit: 5000, Remaining: 0, Used: 5000, Reset: reset},
		{Limit: 5000, Remaining: 0, Used: 5000, Reset: reset},
	}
	for i, w := range want {
		if _, err := c.RunQueueTimes(t.Context(), "acme", "api", 31); (err != nil) != (answers[i].status != http.StatusOK) {
			t.Fatalf("answer %d (%d) read as %v", i, answers[i].status, err)
		}
		got, ok := c.RESTBudget()
		if !ok {
			t.Fatalf("after answer %d the client states no budget", i)
		}
		if got.Read.IsZero() {
			t.Errorf("after answer %d the budget has no read time", i)
		}
		got.Read = time.Time{}
		if got != w {
			t.Errorf("after answer %d the budget = %+v, want %+v", i, got, w)
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	wantRequests := []string{
		"list the run's jobs 200", "list the run's jobs 200", "list the run's jobs 403", "list the run's jobs 401",
		"list the run's jobs 200",
	}
	if fmt.Sprint(o.requests) != fmt.Sprint(wantRequests) {
		t.Errorf("observed %q, want %q", o.requests, wantRequests)
	}
}

// THE TOKEN EXCHANGE IS ANSWERED FROM THE APP'S OWN BUDGET, so what its answer
// states is not the installation's budget, and it is not one of the
// installation's requests.
func TestTheTokenExchangeIsNotTheInstallationsAccount(t *testing.T) {
	t.Parallel()

	key, _ := testKeyPKCS1(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Limit", "15000")
		w.Header().Set("X-Ratelimit-Remaining", "14999")
		w.Header().Set("X-Ratelimit-Used", "1")
		w.Header().Set("X-Ratelimit-Reset", "1792461600")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31/jobs", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":0,"jobs":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newRunnerGroupPolicyClient(defaultPolicyBounds, srv.URL, OrganizationTarget("acme"), 11, 22, key)
	o := &recordingObserver{}
	c.ObserveREST(o)

	if _, err := c.RunQueueTimes(t.Context(), "acme", "api", 31); err != nil {
		t.Fatalf("RunQueueTimes: %v", err)
	}
	if got, ok := c.RESTBudget(); ok {
		t.Errorf("the token exchange's answer stated the installation's budget as %+v", got)
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if fmt.Sprint(o.requests) != "[list the run's jobs 200]" {
		t.Errorf("observed %q, want only the installation's one request", o.requests)
	}
}

// A REDIRECT IS AN ANSWER OF ITS OWN: a renamed repository's old path is
// answered with a 301 that draws on the budget, and the request the client
// follows it with is a second, each told and each stating the budget.
func TestEachRedirectIsCountedAsAnAnswer(t *testing.T) {
	t.Parallel()

	key, _ := testKeyPKCS1(t)
	budget := func(w http.ResponseWriter, remaining string) {
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Remaining", remaining)
		w.Header().Set("X-Ratelimit-Used", "1")
		w.Header().Set("X-Ratelimit-Reset", "1792461600")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31/jobs", func(w http.ResponseWriter, r *http.Request) {
		budget(w, "4000")
		http.Redirect(w, r, "/repositories/7/actions/runs/31/jobs?"+r.URL.RawQuery, http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /repositories/7/actions/runs/31/jobs", func(w http.ResponseWriter, _ *http.Request) {
		budget(w, "3999")
		fmt.Fprint(w, `{"total_count":0,"jobs":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newRunnerGroupPolicyClient(defaultPolicyBounds, srv.URL, OrganizationTarget("acme"), 11, 22, key)
	o := &recordingObserver{}
	c.ObserveREST(o)

	if _, err := c.RunQueueTimes(t.Context(), "acme", "api", 31); err != nil {
		t.Fatalf("RunQueueTimes: %v", err)
	}
	if got, ok := c.RESTBudget(); !ok || got.Remaining != 3999 {
		t.Errorf("the budget after a redirect = %+v (%t), want the final answer's 3999", got, ok)
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if fmt.Sprint(o.requests) != "[list the run's jobs 301 list the run's jobs 200]" {
		t.Errorf("observed %q, want the redirect and the answer it led to", o.requests)
	}
}

// A REDIRECT THE CLIENT REFUSES TO FOLLOW IS STILL AN ANSWER: a loop is ten
// redirects, each counted and the last one's budget kept, not nine and a
// request no answer came to.
func TestTheRedirectPastTheLimitIsCountedAsAnAnswer(t *testing.T) {
	t.Parallel()

	key, _ := testKeyPKCS1(t)
	var hops atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31/jobs", func(w http.ResponseWriter, r *http.Request) {
		n := hops.Add(1)
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Remaining", strconv.FormatInt(5000-n, 10))
		w.Header().Set("X-Ratelimit-Used", strconv.FormatInt(n, 10))
		w.Header().Set("X-Ratelimit-Reset", "1792461600")
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newRunnerGroupPolicyClient(defaultPolicyBounds, srv.URL, OrganizationTarget("acme"), 11, 22, key)
	o := &recordingObserver{}
	c.ObserveREST(o)

	if _, err := c.RunQueueTimes(t.Context(), "acme", "api", 31); err == nil {
		t.Fatal("a redirect loop read as an answer")
	}
	if got, ok := c.RESTBudget(); !ok || got.Remaining != 5000-hops.Load() {
		t.Errorf("the budget after a redirect loop = %+v (%t), want the last answer's %d", got, ok, 5000-hops.Load())
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	want := strings.TrimSuffix(strings.Repeat("list the run's jobs 302 ", maxRESTRedirects), " ")
	if hops.Load() != maxRESTRedirects || fmt.Sprint(o.requests) != "["+want+"]" {
		t.Errorf("%d answers sent, observed %q; want %d, each a 302", hops.Load(), o.requests, maxRESTRedirects)
	}
}

// WITHIN ONE HOUR THE LOWEST REMAINING IS KEPT, so an answer that arrives after
// a later one cannot restore what that one spent; a new hour starts afresh, and
// an answer from an hour that has ended, arriving late, does not bring it back.
func TestAnAnswerArrivingLateDoesNotRestoreTheBudget(t *testing.T) {
	t.Parallel()

	stated := func(remaining string, reset int64) http.Header {
		h := http.Header{}
		h.Set("X-Ratelimit-Limit", "5000")
		h.Set("X-Ratelimit-Remaining", remaining)
		h.Set("X-Ratelimit-Used", "1")
		h.Set("X-Ratelimit-Reset", strconv.FormatInt(reset, 10))
		return h
	}
	var r restAccount
	now := time.Now()
	for _, step := range []struct {
		remaining string
		reset     int64
		want      int64
	}{
		{"999", 1792461600, 999},
		{"1000", 1792461600, 999},
		{"998", 1792461600, 998},
		{"4999", 1792465200, 4999},
		{"0", 1792461600, 4999},
	} {
		r.answered("list the run's jobs", http.StatusOK, stated(step.remaining, step.reset), now)
		if r.budget.Remaining != step.want {
			t.Errorf("after an answer stating %s left until %d the budget is %d, want %d",
				step.remaining, step.reset, r.budget.Remaining, step.want)
		}
	}
}

// AN ANSWER THAT NEVER CAME IS TOLD AS STATUS ZERO and leaves the budget as it
// was.
func TestARequestWithNoAnswerIsToldAsNone(t *testing.T) {
	t.Parallel()

	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot drop a connection")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	})
	o := &recordingObserver{}
	c.ObserveREST(o)

	if _, err := c.RunQueueTimes(t.Context(), "acme", "api", 31); err == nil {
		t.Fatal("a dropped connection read as an answer")
	}
	if _, ok := c.RESTBudget(); ok {
		t.Error("a request with no answer stated a budget")
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if fmt.Sprint(o.requests) != "[list the run's jobs 0]" {
		t.Errorf("observed %q, want the one request with no answer", o.requests)
	}
}

// AN ANSWER WHOSE BODY IS NOT READ WHOLE STILL STATES THE BUDGET its request
// drew on, and is told by its status: the body failing is not the answer
// failing to come.
func TestAnAnswerTooLargeToReadStillStatesTheBudget(t *testing.T) {
	t.Parallel()

	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Remaining", "12")
		w.Header().Set("X-Ratelimit-Used", "4988")
		w.Header().Set("X-Ratelimit-Reset", "1792461600")
		fmt.Fprint(w, `{"total_count":0,"jobs":[],"pad":"`+strings.Repeat("x", maxPolicyResponse)+`"}`)
	})
	o := &recordingObserver{}
	c.ObserveREST(o)

	if _, err := c.RunQueueTimes(t.Context(), "acme", "api", 31); err == nil {
		t.Fatal("an answer past the bound read as one")
	}
	got, ok := c.RESTBudget()
	if !ok || got.Remaining != 12 || got.Limit != 5000 {
		t.Errorf("the budget after an answer too large to read = %+v (%t), want 12 of 5000", got, ok)
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if fmt.Sprint(o.requests) != "[list the run's jobs 200]" {
		t.Errorf("observed %q, want the one request answered 200", o.requests)
	}
}

// A HEADER SET THAT IS MISSING A MEMBER, OR STATES A ZERO LIMIT OR A NEGATIVE
// COUNT, STATES NOTHING: could not tell is not a budget of zero.
func TestABudgetIsReadOnlyFromACompleteHeaderSet(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	full := func() http.Header {
		h := http.Header{}
		h.Set("X-Ratelimit-Limit", "12500")
		h.Set("X-Ratelimit-Remaining", "12000")
		h.Set("X-Ratelimit-Used", "500")
		h.Set("X-Ratelimit-Reset", "1792461600")
		return h
	}

	got, ok := budgetOf(full(), now)
	want := RESTBudget{Limit: 12500, Remaining: 12000, Used: 500, Reset: time.Unix(1792461600, 0).UTC(), Read: now}
	if !ok || got != want {
		t.Errorf("budgetOf(a complete set) = %+v, %t, want %+v", got, ok, want)
	}

	for name, change := range map[string]func(http.Header){
		"no limit":          func(h http.Header) { h.Del("X-Ratelimit-Limit") },
		"no remaining":      func(h http.Header) { h.Del("X-Ratelimit-Remaining") },
		"no used":           func(h http.Header) { h.Del("X-Ratelimit-Used") },
		"no reset":          func(h http.Header) { h.Del("X-Ratelimit-Reset") },
		"a zero limit":      func(h http.Header) { h.Set("X-Ratelimit-Limit", "0") },
		"a negative count":  func(h http.Header) { h.Set("X-Ratelimit-Remaining", "-1") },
		"a word for a time": func(h http.Header) { h.Set("X-Ratelimit-Reset", "soon") },
		"a reset past the window": func(h http.Header) {
			h.Set("X-Ratelimit-Reset", strconv.FormatInt(now.Add(maxRESTResetAhead+time.Second).Unix(), 10))
		},
	} {
		h := full()
		change(h)
		if got, ok := budgetOf(h, now); ok {
			t.Errorf("budgetOf(%s) = %+v, want no budget", name, got)
		}
	}
}
