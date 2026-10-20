package github

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
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
// every such request is told to the observer, refused or not. The token
// exchange is answered from the App's own budget, so its headers state nothing
// about the installation's and it is not one of the installation's requests.
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
		"list the run's jobs 200", "list the run's jobs 200", "list the run's jobs 403", "list the run's jobs 200",
	}
	if fmt.Sprint(o.requests) != fmt.Sprint(wantRequests) {
		t.Errorf("observed %q, want %q", o.requests, wantRequests)
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
	} {
		h := full()
		change(h)
		if got, ok := budgetOf(h, now); ok {
			t.Errorf("budgetOf(%s) = %+v, want no budget", name, got)
		}
	}
}
