package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// jobsServer answers the token exchange and the run's job list, handing each
// page request to pages. It counts every request it is sent, on any path.
func jobsServer(t *testing.T, pages func(w http.ResponseWriter, r *http.Request, page int)) (
	*runnerGroupPolicyClient, *atomic.Int32,
) {
	t.Helper()

	key, _ := testKeyPKCS1(t)
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31/jobs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer installation-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.URL.Query().Get("filter"); got != "all" {
			t.Errorf("filter = %q, want every attempt", got)
		}
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Errorf("page = %q", r.URL.Query().Get("page"))
		}
		pages(w, r, page)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return newRunnerGroupPolicyClient(defaultPolicyBounds, srv.URL, OrganizationTarget("acme"), 11, 22, key),
		&requests
}

// jobJSON is one job of the list, on runner.
func jobJSON(id int64, runner, steps string) string {
	return fmt.Sprintf(`{"id":%d,"name":"build (%d)","run_attempt":1,"runner_name":%q,"steps":[%s]}`,
		id, id, runner, steps)
}

const twoSteps = `{"number":1,"name":"Set up job","status":"completed","conclusion":"success",
		"started_at":"2026-10-09T12:00:01Z","completed_at":"2026-10-09T12:00:04Z"},
	{"number":2,"name":"Run make\ntest","status":"completed","conclusion":"failure",
		"started_at":"2026-10-09T12:00:04Z","completed_at":"2026-10-09T12:01:00Z"}`

// THE JOB IS FOUND BY ITS RUNNER ACROSS EVERY PAGE, and read as GitHub records
// it, steps and times included.
func TestARunnersJobIsFoundAcrossPages(t *testing.T) {
	t.Parallel()

	c, requests := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		switch page {
		case 1:
			jobs := make([]string, 0, runJobsPerPage)
			for i := range runJobsPerPage {
				jobs = append(jobs, jobJSON(int64(100+i), fmt.Sprintf("billet-other-%d", i), ""))
			}
			fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, runJobsPerPage+1, strings.Join(jobs, ","))
		case 2:
			fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, runJobsPerPage+1,
				jobJSON(7001, "billet-lease-1", twoSteps))
		default:
			t.Errorf("page %d was asked for after the list was complete", page)
		}
	})

	job, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1")
	if err != nil {
		t.Fatalf("RunnerJob: %v", err)
	}
	if job.ID != 7001 || job.Name != "build (7001)" || job.RunAttempt != 1 || len(job.Steps) != 2 {
		t.Fatalf("job = %+v", job)
	}
	want := JobStep{Number: 2, Name: "Run make\ntest", Status: "completed", Conclusion: "failure",
		StartedAt:   time.Date(2026, 10, 9, 12, 0, 4, 0, time.UTC),
		CompletedAt: time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC)}
	if got := job.Steps[1]; got != want {
		t.Errorf("step 2 = %+v, want %+v", got, want)
	}
	if requests.Load() != 3 {
		t.Errorf("requests = %d, want a token and two pages", requests.Load())
	}
}

// A STEP WITHOUT A TIME IS LISTED WITH A ZERO TIME, never refused and never
// given one; a time that is there and unreadable is refused.
func TestAStepsMissingTimeIsZeroAndAMalformedOneIsRefused(t *testing.T) {
	t.Parallel()

	pending := `{"number":3,"name":"Post","status":"pending","conclusion":null,
		"started_at":null}`
	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"total_count":1,"jobs":[%s]}`, jobJSON(7001, "billet-lease-1", pending))
	})
	job, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1")
	if err != nil {
		t.Fatalf("RunnerJob: %v", err)
	}
	if s := job.Steps[0]; !s.StartedAt.IsZero() || !s.CompletedAt.IsZero() || s.Conclusion != "" {
		t.Errorf("a step without times = %+v", s)
	}

	malformed := strings.Replace(twoSteps, `"2026-10-09T12:00:04Z","completed_at":"2026-10-09T12:01:00Z"`,
		`"yesterday","completed_at":"2026-10-09T12:01:00Z"`, 1)
	c, _ = jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"total_count":1,"jobs":[%s]}`, jobJSON(7001, "billet-lease-1", malformed))
	})
	if _, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1"); err == nil ||
		!strings.Contains(err.Error(), "step 2: started_at") {
		t.Errorf("a malformed step time = %v, want refused naming the step", err)
	}
}

// NO MATCH IS ITS OWN ANSWER, AND TWO ARE NOT AN ANSWER.
func TestARunnerWithNoJobOrTwoIsNotAnAnswer(t *testing.T) {
	t.Parallel()

	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"total_count":1,"jobs":[%s]}`, jobJSON(7001, "billet-lease-2", twoSteps))
	})
	if _, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1"); !errors.Is(err, ErrNoRunnerJob) {
		t.Errorf("no job on the runner = %v, want ErrNoRunnerJob", err)
	}

	c, _ = jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"total_count":2,"jobs":[%s,%s]}`, jobJSON(7001, "billet-lease-1", twoSteps),
			jobJSON(7002, "billet-lease-1", twoSteps))
	})
	_, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1")
	if err == nil || errors.Is(err, ErrNoRunnerJob) || !strings.Contains(err.Error(), "refusing to guess") {
		t.Errorf("two jobs on one runner = %v, want refused", err)
	}
}

// A LIST THAT DOES NOT ADD UP IS COULD-NOT-TELL: incomplete, too long, changed
// under the read, ended early, or a job too long to read.
func TestAJobListThatDoesNotAddUpIsRefused(t *testing.T) {
	t.Parallel()

	manySteps := strings.TrimSuffix(strings.Repeat(`{"number":1,"name":"s"},`, MaxJobSteps+1), ",")
	for name, tc := range map[string]struct {
		page func(page int) string
		want string
	}{
		"no total": {func(int) string { return `{"jobs":[]}` }, "incomplete"},
		"no jobs":  {func(int) string { return `{"total_count":0}` }, "incomplete"},
		"too long": {func(int) string {
			return fmt.Sprintf(`{"total_count":%d,"jobs":[]}`, MaxRunJobs+1)
		}, "more than the 1000"},
		"ended early": {func(page int) string {
			if page == 1 {
				return fmt.Sprintf(`{"total_count":3,"jobs":[%s]}`, jobJSON(1, "x", ""))
			}
			return `{"total_count":3,"jobs":[]}`
		}, "ended after 1 of the 3"},
		"count changed": {func(page int) string {
			return fmt.Sprintf(`{"total_count":%d,"jobs":[%s]}`, 2+page, jobJSON(int64(page), "x", ""))
		}, "changed from 3 to 4"},
		"more than counted": {func(int) string {
			return fmt.Sprintf(`{"total_count":1,"jobs":[%s,%s]}`, jobJSON(1, "x", ""), jobJSON(2, "y", ""))
		}, "more jobs than the 1"},
		"a page at a time": {func(page int) string {
			return fmt.Sprintf(`{"total_count":%d,"jobs":[%s]}`, MaxRunJobs, jobJSON(int64(page), "x", ""))
		}, "did not arrive in 40 pages"},
		"too many steps": {func(int) string {
			return fmt.Sprintf(`{"total_count":1,"jobs":[%s]}`, jobJSON(1, "billet-lease-1", manySteps))
		}, "more than the 1000 billet reads"},
		"a step with no name": {func(int) string {
			return fmt.Sprintf(`{"total_count":1,"jobs":[%s]}`, jobJSON(1, "billet-lease-1", `{"number":1}`))
		}, "incomplete step"},
		"a job with no steps": {func(int) string {
			return `{"total_count":1,"jobs":[{"id":1,"name":"j","runner_name":"billet-lease-1"}]}`
		}, "job record was incomplete"},
	} {
		c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, page int) {
			fmt.Fprint(w, tc.page(page))
		})
		_, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1")
		if err == nil || errors.Is(err, ErrNoRunnerJob) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error saying %q", name, err, tc.want)
		}
	}
}

// GITHUB'S REFUSALS COME BACK TYPED, an oversized answer is no answer, and the
// token is in none of them.
func TestAJobListGitHubDidNotGiveIsAnError(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusBadGateway} {
		c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
		})
		_, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1")
		api, ok := errors.AsType[*APIError](err)
		if !ok || api.Status != status {
			t.Errorf("HTTP %d = %v, want an APIError with that status", status, err)
		}
		if err != nil && strings.Contains(err.Error(), "installation-secret") {
			t.Errorf("HTTP %d: the error carries the token: %v", status, err)
		}
	}

	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"total_count":1,"jobs":[],"pad":%q}`, strings.Repeat("x", maxPolicyResponse))
	})
	if _, err := c.RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1"); !errors.Is(err, errNoAnswer) {
		t.Errorf("an oversized page = %v, want errNoAnswer", err)
	}
}

// A SLOW GITHUB IS CUT OFF AT THE CALLER'S DEADLINE.
func TestASlowJobListEndsAtTheDeadline(t *testing.T) {
	t.Parallel()

	c, _ := jobsServer(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := c.RunnerJob(ctx, "acme", "api", 31, "billet-lease-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a silent server = %v, want the deadline", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("the read took %s past a 200ms deadline", took)
	}
}

// WHAT A CALLER PASSES IS CHECKED BEFORE ANYTHING IS ASKED.
func TestARunnerJobNeedsARunARunnerAndARepository(t *testing.T) {
	t.Parallel()

	c, requests := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprint(w, `{"total_count":0,"jobs":[]}`)
	})
	for name, call := range map[string]func() error{
		"no run": func() error {
			_, err := c.RunnerJob(t.Context(), "acme", "api", 0, "billet-lease-1")
			return err
		},
		"no runner": func() error {
			_, err := c.RunnerJob(t.Context(), "acme", "api", 31, " ")
			return err
		},
		"a path for a repository": func() error {
			_, err := c.RunnerJob(t.Context(), "acme", "api/../x", 31, "billet-lease-1")
			return err
		},
		"no credentials": func() error {
			_, err := (&runnerGroupPolicyClient{}).RunnerJob(t.Context(), "acme", "api", 31, "billet-lease-1")
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if requests.Load() != 0 {
		t.Errorf("%d requests were made for calls that should have been refused", requests.Load())
	}
}
