package github

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
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

	jobs, err := c.RunJobs(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if got, want := jobs["billet-a"].QueuedAt, time.Date(2026, 10, 18, 21, 37, 9, 0, time.UTC); !got.Equal(want) {
		t.Errorf("billet-a = %s, want %s", got, want)
	}
	if got, want := jobs["billet-b"].QueuedAt, time.Date(2026, 10, 18, 21, 37, 47, 0, time.UTC); !got.Equal(want) {
		t.Errorf("billet-b, on the second page, = %s, want %s", got, want)
	}
	for _, absent := range []string{"billet-twice", "", "billet-no-time", "billet-bad-time"} {
		if got, ok := jobs[absent]; ok {
			t.Errorf("%q has queue time %s, want none", absent, got.QueuedAt)
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
	if _, err := short.RunJobs(t.Context(), "acme", "api", 31); err == nil ||
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
	if _, err := c.RunJobs(t.Context(), "acme", "api", 31); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("more than the %d", MaxQueueTimeRunJobs)) {
		t.Errorf("a run of %d jobs = %v, want refused", MaxQueueTimeRunJobs+1, err)
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want a token and one page", requests.Load())
	}
}

// A JOB'S COMMIT IS READ FROM THE RECORD THAT GIVES ITS QUEUE TIME. The shape is the
// measured one, 2026-11-03 on the test organization: head_sha and head_branch beside
// created_at. A branch GitHub gives as null, and a record with neither, are empty,
// which is could-not-tell; the queue time still stands.
func TestARunsJobsNameTheirCommit(t *testing.T) {
	t.Parallel()

	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprint(w, `{"total_count":3,"jobs":[
			{"id":1,"runner_name":"billet-a","created_at":"2026-10-18T21:37:09Z",
			 "head_sha":"20f462ad39fee8e29587ae6a2a317c68992469ae","head_branch":"main","run_attempt":2},
			{"id":2,"runner_name":"billet-b","created_at":"2026-10-18T21:37:09Z",
			 "head_sha":"abc","head_branch":null},
			{"id":3,"runner_name":"billet-c","created_at":"2026-10-18T21:37:09Z"}]}`)
	})

	jobs, err := c.RunJobs(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	queued := time.Date(2026, 10, 18, 21, 37, 9, 0, time.UTC)
	for name, want := range map[string]RunJob{
		"billet-a": {QueuedAt: queued, HeadSHA: "20f462ad39fee8e29587ae6a2a317c68992469ae", HeadBranch: "main", RunAttempt: 2},
		"billet-b": {QueuedAt: queued, HeadSHA: "abc"},
		"billet-c": {QueuedAt: queued},
	} {
		got := jobs[name]
		if !got.QueuedAt.Equal(want.QueuedAt) || got.HeadSHA != want.HeadSHA || got.HeadBranch != want.HeadBranch ||
			got.RunAttempt != want.RunAttempt {
			t.Errorf("%s = %+v, want %+v", name, got, want)
		}
	}
}

// EACH RUNNER'S ATTEMPT IS ITS JOB'S run_attempt, read from the same list as its
// queue time: a re-run lists every attempt's jobs under one run id, on runners of
// their own, and a record that does not say, or says zero, gives no attempt rather
// than a first one.
func TestARunsRecordsCarryEachRunnersAttempt(t *testing.T) {
	t.Parallel()

	const created = `"2026-10-18T21:37:09Z"`
	job := func(id int64, runner, created, attempt string) string {
		return fmt.Sprintf(`{"id":%d,"name":"j","runner_name":%q,"created_at":%s%s,"steps":[]}`,
			id, runner, created, attempt)
	}
	jobs := []string{
		job(1, "billet-first", created, `,"run_attempt":1`),
		job(2, "billet-rerun", created, `,"run_attempt":2`),
		job(3, "billet-silent", created, ""),
		job(4, "billet-zero", created, `,"run_attempt":0`),
		job(5, "billet-negative", created, `,"run_attempt":-1`),
		job(6, "billet-no-time", "null", `,"run_attempt":3`),
		job(7, "billet-nothing", "null", ""),
	}
	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, len(jobs), strings.Join(jobs, ","))
	})

	records, err := c.RunJobs(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	queued := time.Date(2026, 10, 18, 21, 37, 9, 0, time.UTC)
	for runner, want := range map[string]RunJob{
		"billet-first":    {QueuedAt: queued, RunAttempt: 1},
		"billet-rerun":    {QueuedAt: queued, RunAttempt: 2},
		"billet-silent":   {QueuedAt: queued},
		"billet-zero":     {QueuedAt: queued},
		"billet-negative": {QueuedAt: queued},
	} {
		got, ok := records[runner]
		if !ok || !got.QueuedAt.Equal(want.QueuedAt) || got.RunAttempt != want.RunAttempt {
			t.Errorf("%s = %+v (%t), want %+v", runner, got, ok, want)
		}
	}
	for _, runner := range []string{"billet-no-time", "billet-nothing"} {
		if got, ok := records[runner]; ok {
			t.Errorf("%s, whose created_at is null, has record %+v, want none", runner, got)
		}
	}
}

// A JOB A RE-RUN CARRIED FORWARD KEEPS THE ATTEMPT THAT RAN IT, in whichever order the
// list names them: that attempt's whole entry, its queue time and its commit, and not
// the later created_at GitHub gives the copy or a field of another entry. A copy
// repeats the job's start; entries the attempts cannot separate, entries that do not
// give one start, and a name an entry of which has no queue time are left out.
func TestARunsRecordsKeepTheAttemptThatRanACarriedJob(t *testing.T) {
	t.Parallel()

	const first, copied = `"2026-10-22T17:09:06Z"`, `"2026-10-22T20:03:23Z"`
	const started = `,"started_at":"2026-10-22T17:22:38Z"`
	// The entry that ran and its copies are told apart by their commits, so a record
	// spliced from two entries is seen.
	const ranRest, copyRest = started + `,"head_sha":"aaa","head_branch":"main"`, started + `,"head_sha":"bbb","head_branch":"copy"`
	job := func(id int64, runner, created, attempt, rest string) string {
		return fmt.Sprintf(`{"id":%d,"name":"j","runner_name":%q,"created_at":%s%s%s,"steps":[]}`,
			id, runner, created, attempt, rest)
	}
	jobs := []string{
		job(1, "carried", first, `,"run_attempt":2`, ranRest), job(2, "carried", copied, `,"run_attempt":3`, copyRest),
		job(3, "carried-backwards", copied, `,"run_attempt":3`, copyRest), job(4, "carried-backwards", first, `,"run_attempt":2`, ranRest),
		job(5, "carried-twice", first, `,"run_attempt":1`, ranRest), job(6, "carried-twice", copied, `,"run_attempt":2`, copyRest),
		job(7, "carried-twice", copied, `,"run_attempt":3`, copyRest),
		job(22, "ran-without-a-commit", first, `,"run_attempt":1`, started), job(23, "ran-without-a-commit", copied, `,"run_attempt":2`, copyRest),
		job(8, "one-attempt-twice", first, `,"run_attempt":2`, ranRest), job(9, "one-attempt-twice", copied, `,"run_attempt":2`, copyRest),
		job(10, "a-copy-says-nothing", first, `,"run_attempt":2`, ranRest), job(11, "a-copy-says-nothing", copied, "", copyRest),
		job(12, "the-first-says-nothing", first, "", ranRest), job(13, "the-first-says-nothing", copied, `,"run_attempt":2`, copyRest),
		job(14, "shared-then-more", first, `,"run_attempt":2`, ranRest), job(15, "shared-then-more", first, `,"run_attempt":2`, ranRest),
		job(16, "shared-then-more", first, `,"run_attempt":1`, ranRest),
		job(19, "more-then-shared", first, `,"run_attempt":1`, ranRest), job(20, "more-then-shared", copied, `,"run_attempt":2`, copyRest),
		job(21, "more-then-shared", copied, `,"run_attempt":2`, copyRest),
		job(17, "no-time-on-the-first", "null", `,"run_attempt":2`, ranRest), job(18, "no-time-on-the-first", copied, `,"run_attempt":3`, copyRest),
		job(24, "two-runs", first, `,"run_attempt":1`, ranRest),
		job(25, "two-runs", copied, `,"run_attempt":2`, `,"started_at":"2026-10-22T20:04:00Z","head_sha":"bbb"`),
		job(26, "a-start-not-given", first, `,"run_attempt":1`, ranRest), job(27, "a-start-not-given", copied, `,"run_attempt":2`, ""),
		job(28, "no-start-given", first, `,"run_attempt":1`, ""), job(29, "no-start-given", copied, `,"run_attempt":2`, ""),
	}
	c, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		from, to := (page-1)*runJobsPerPage, min(page*runJobsPerPage, len(jobs))
		fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, len(jobs), strings.Join(jobs[from:to], ","))
	})

	records, err := c.RunJobs(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}

	same := func(x, y RunJob) bool {
		return x.QueuedAt.Equal(y.QueuedAt) && x.HeadSHA == y.HeadSHA && x.HeadBranch == y.HeadBranch &&
			x.RunAttempt == y.RunAttempt
	}
	// The same entries in the reverse order settle every name the same way.
	reversed := slices.Clone(jobs)
	slices.Reverse(reversed)
	back, _ := jobsServer(t, func(w http.ResponseWriter, _ *http.Request, page int) {
		from, to := (page-1)*runJobsPerPage, min(page*runJobsPerPage, len(reversed))
		fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, len(reversed), strings.Join(reversed[from:to], ","))
	})
	again, err := back.RunJobs(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("RunJobs in reverse: %v", err)
	}
	if !maps.EqualFunc(records, again, same) {
		t.Errorf("the order of the list decided the answer: %v against %v", records, again)
	}

	queued := time.Date(2026, 10, 22, 17, 9, 6, 0, time.UTC)
	for runner, want := range map[string]RunJob{
		"carried":              {QueuedAt: queued, HeadSHA: "aaa", HeadBranch: "main", RunAttempt: 2},
		"carried-backwards":    {QueuedAt: queued, HeadSHA: "aaa", HeadBranch: "main", RunAttempt: 2},
		"carried-twice":        {QueuedAt: queued, HeadSHA: "aaa", HeadBranch: "main", RunAttempt: 1},
		"ran-without-a-commit": {QueuedAt: queued, RunAttempt: 1},
	} {
		if got, ok := records[runner]; !ok || !same(got, want) {
			t.Errorf("%s = %+v (%t), want %+v", runner, got, ok, want)
		}
	}
	for _, runner := range []string{"one-attempt-twice", "a-copy-says-nothing", "the-first-says-nothing", "shared-then-more",
		"more-then-shared", "no-time-on-the-first", "two-runs", "a-start-not-given", "no-start-given"} {
		if got, ok := records[runner]; ok {
			t.Errorf("%s = %+v; these entries are not one job and its copies, so want none", runner, got)
		}
	}
}
