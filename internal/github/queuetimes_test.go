package github

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// EACH RUNNER'S QUEUE TIME IS ITS JOB'S created_at, across every page, and a
// runner GitHub lists more than once, a job with no runner and a job whose created_at is
// missing or unreadable give no time rather than a guess.
func TestARunsQueueTimesAreEachRunnersJobsCreatedAt(t *testing.T) {
	t.Parallel()

	job := func(id int64, runner, created string) string {
		return fmt.Sprintf(`{"id":%d,"name":"j","runner_name":%s,"created_at":%s,"steps":[]}`,
			id, runner, created)
	}
	c, requests := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		switch page {
		case 1:
			jobs := make([]string, 0, runJobsPerPage)
			jobs = append(jobs, job(1, `"billet-a"`, `"2026-10-18T21:37:09Z"`),
				job(2, `"billet-twice"`, `"2026-10-18T21:37:10Z"`),
				job(3, `null`, `"2026-10-18T21:37:11Z"`),
				job(4, `"billet-no-time"`, `null`),
				job(5, `"billet-bad-time"`, `"soon"`))
			for i := len(jobs); i < runJobsPerPage; i++ {
				jobs = append(jobs, job(int64(100+i), fmt.Sprintf(`"billet-other-%d"`, i), `"2026-10-18T21:00:00Z"`))
			}
			fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, runJobsPerPage+3, strings.Join(jobs, ","))
		case 2:
			fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s,%s,%s]}`, runJobsPerPage+3,
				job(7001, `"billet-b"`, `"2026-10-18T21:37:47Z"`), job(7002, `"billet-twice"`, `"2026-10-18T21:38:00Z"`),
				job(7003, `"billet-twice"`, `"2026-10-18T21:39:00Z"`))
		default:
			t.Errorf("page %d was asked for after the list was complete", page)
		}
	})

	times, err := c.RunQueueTimes(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("RunQueueTimes: %v", err)
	}
	if got, want := times["billet-a"], time.Date(2026, 10, 18, 21, 37, 9, 0, time.UTC); !got.Equal(want) {
		t.Errorf("billet-a = %s, want %s", got, want)
	}
	if got, want := times["billet-b"], time.Date(2026, 10, 18, 21, 37, 47, 0, time.UTC); !got.Equal(want) {
		t.Errorf("billet-b, on the second page, = %s, want %s", got, want)
	}
	for _, absent := range []string{"billet-twice", "", "billet-no-time", "billet-bad-time"} {
		if got, ok := times[absent]; ok {
			t.Errorf("%q has queue time %s, want none", absent, got)
		}
	}
	if requests.Load() != 3 {
		t.Errorf("requests = %d, want a token and two pages", requests.Load())
	}

	short, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		if page == 1 {
			fmt.Fprint(w, `{"total_count":2,"jobs":[`+job(1, `"billet-a"`, `"2026-10-18T21:37:09Z"`)+`]}`)
			return
		}
		fmt.Fprint(w, `{"total_count":2,"jobs":[]}`)
	})
	if _, err := short.RunQueueTimes(t.Context(), "acme", "api", 31); err == nil ||
		!strings.Contains(err.Error(), "ended after 1 of the 2") {
		t.Errorf("a list that ended short of its count = %v, want refused for ending early", err)
	}
}

// A RUN OF MORE THAN MaxQueueTimeRunJobs COSTS ONE PAGE AND IS REFUSED, because
// the read is made for every run the control plane starts a job of, from the
// installation's shared hourly budget; RunnerJob, read once per command, still
// reads such a run whole.
func TestAQueueTimeReadOfALargeRunCostsOnePage(t *testing.T) {
	t.Parallel()

	jobs := make([]string, 0, runJobsPerPage)
	for i := range runJobsPerPage {
		jobs = append(jobs, fmt.Sprintf(`{"id":%d,"name":"j","runner_name":"billet-%d","created_at":"2026-10-18T21:37:09Z","steps":[]}`,
			i+1, i))
	}
	c, requests := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, MaxQueueTimeRunJobs+1, strings.Join(jobs, ","))
	})
	if _, err := c.RunQueueTimes(t.Context(), "acme", "api", 31); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("more than the %d", MaxQueueTimeRunJobs)) {
		t.Errorf("a run of %d jobs = %v, want refused", MaxQueueTimeRunJobs+1, err)
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want a token and one page", requests.Load())
	}
}
