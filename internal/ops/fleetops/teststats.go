package fleetops

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/guestreport"
)

// The bounds of one `billet tests stats`: the ledger is read a page of jobs at a time,
// at most testStatsMaxJobs of them, newest first; a test keeps its newest
// testStatsDurations durations, which its median is over; and at most
// testStatsMaxCases tests are ranked. A job writes its own results and its names, so
// without the last, jobs of many names could make the command hold what they chose.
const (
	testStatsPageRows  = 200
	testStatsMaxJobs   = 20_000
	testStatsDurations = 64
	testStatsMaxCases  = 200_000
)

// Tests is `billet tests`.
func Tests(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet tests stats [--repo R] [--since 14d] [--limit N] [--json]")
	}
	if args[0] == "stats" {
		return cmdTestsStats(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown tests command %q; try stats", args[0])
}

func cmdTestsStats(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet tests stats", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	repo := fs.String("repo", "", "report one repository, as owner/name; absent, every repository")
	sinceText := fs.String("since", "14d", "report the jobs recorded this far back, such as 14d or 36h")
	limit := fs.Int("limit", 20, "how many tests each ranking shows")
	asJSON := fs.Bool("json", false, "print the report as one JSON object")
	if err := cli.ParseWithArgs(fs, args, 0); err != nil {
		return err
	}
	since, err := cli.ParseSpan(*sinceText)
	if err != nil {
		return fmt.Errorf("billet tests stats: --since: %w", err)
	}
	if *limit < 1 {
		return fmt.Errorf("billet tests stats: --limit must be positive, got %d", *limit)
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	report, err := buildTestStats(ctx, a, *repo, time.Now().Add(-since), *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode the test report: %w", err)
		}
		if _, err := fmt.Fprintf(env.Stdout, "%s\n", body); err != nil {
			return fmt.Errorf("write the test report: %w", err)
		}

		return nil
	}
	renderTestStats(env.Stdout, report)

	return nil
}

// testStats is `billet tests stats`: three rankings of what jobs' VMs reported, each
// row naming a lease to look at.
type testStats struct {
	Notice string    `json:"notice"`
	Since  time.Time `json:"since"`
	// Jobs is how many jobs' results were ranked. Unreadable counts those whose row or
	// encoding could not be read, and WithoutIdentity those with no commit, attempt,
	// workflow or job name on record, which only the flaky ranking leaves out. Partial
	// says the bounds stopped the read: its oldest jobs, or the tests past the bound, are
	// left out.
	Jobs            int  `json:"jobs"`
	Unreadable      int  `json:"unreadable"`
	WithoutIdentity int  `json:"without_identity"`
	Partial         bool `json:"partial"`
	// Ambiguous counts the jobs on one commit whose display name covers several jobs of
	// one attempt, as a matrix's legs sharing a name do: the flaky ranking looks only
	// inside each of their jobs.
	Ambiguous int `json:"ambiguous"`
	// Incomplete counts the jobs whose results left out cases, a suite cut to fit its
	// bounds, so a test's counts may fall short.
	Incomplete int           `json:"incomplete"`
	Slowest    []slowestTest `json:"slowest"`
	Failing    []failingTest `json:"failing"`
	Flaky      []flakyTest   `json:"flaky"`
}

// slowestTest is one test by its median duration over its newest runs.
type slowestTest struct {
	Repository string `json:"repository"`
	Suite      string `json:"suite"`
	Test       string `json:"test"`
	Runs       int64  `json:"runs"`
	// MedianMillis is over the newest testStatsDurations durations, and MaxMillis over
	// all of them, in the job Lease.
	MedianMillis int64  `json:"median_ms"`
	MaxMillis    int64  `json:"max_ms"`
	Lease        string `json:"lease"`
}

// failingTest is one test by how many of its runs failed.
type failingTest struct {
	Repository string `json:"repository"`
	Suite      string `json:"suite"`
	Test       string `json:"test"`
	Runs       int64  `json:"runs"`
	Failures   int64  `json:"failures"`
	// Lease is its newest failure, and Message what that failure said, if anything.
	Lease   string `json:"lease"`
	Message string `json:"message,omitempty"`
}

// flakyTest is one test that failed and passed on one commit, in one job of one
// workflow, so nothing billet can see changed between the two: in two attempts of a
// run or two runs, since two jobs of one attempt ran side by side and may be two legs
// of a matrix sharing a display name.
//
// Evidence says how the pass is known: "recorded" is a case the agent kept as passed
// in PassLease; "suite" is the case's suite run there, as many tests as where it
// failed, reporting no failure, error or skip, with its cases cut to fit, so the case
// is inferred to have passed; "same job" is both in one job, a retry its test runner
// made or two cases of one name, which the report cannot tell apart.
type flakyTest struct {
	Repository  string `json:"repository"`
	Commit      string `json:"commit"`
	WorkflowRef string `json:"workflow_ref"`
	Job         string `json:"job"`
	Suite       string `json:"suite"`
	Test        string `json:"test"`
	Failures    int64  `json:"failures"`
	FailLease   string `json:"fail_lease"`
	PassLease   string `json:"pass_lease"`
	Evidence    string `json:"evidence"`
}

// testKey names one test across jobs: a repository's suite and case.
type testKey struct{ repo, suite, name string }

// jobKey names what a flaky test must have kept constant: one job of one workflow on
// one commit.
type jobKey struct{ repo, commit, workflow, job string }

// flakyKey is one test of one such job.
type flakyKey struct {
	job         jobKey
	suite, name string
}

// suiteKey is one suite of one such job.
type suiteKey struct {
	job   jobKey
	suite string
}

// jobAttempt is which attempt of which run a job was.
type jobAttempt struct{ run, attempt int64 }

// seenIn is one job a test was seen in, and how many tests its suite counted there.
type seenIn struct {
	lease      string
	attempt    jobAttempt
	suiteTests int64
}

// A test's sightings on one commit. Two distinct attempts are all a pair needs: at least
// one of two differs from any other, so a list keeps two. A failure is matched to a clean
// suite by what its suite counted, so each count keeps its own two, and flakyCounts
// bounds the counts a job can make it hold.
const (
	flakySightings = 2
	flakyCounts    = 16
)

// testTally is what was seen of one test.
type testTally struct {
	runs, failures    int64
	durations         []int64
	maxMillis         int64
	maxLease          string
	failLease, reason string
}

// flakyTally is what was seen of one test on one commit.
type flakyTally struct {
	failures      int64
	fails, passes []seenIn
	// failsByCount is the failures by what their suite counted.
	failsByCount map[int64][]seenIn
	// lastFail and lastPass are the jobs its latest failure and pass were in, and sameJob
	// is a job that did both: jobs arrive one at a time, so the two are adjacent.
	lastFail, lastPass string
	sameJob            string
}

// addSeen keeps one sighting to an attempt, within the bound.
func addSeen(list []seenIn, s seenIn) []seenIn {
	if len(list) >= flakySightings {
		return list
	}
	for i := range list {
		if list[i].attempt == s.attempt {
			return list
		}
	}

	return append(list, s)
}

// cleanKey is one suite of one job, at one count of tests: only a suite that counted as
// many tests stands for a case it does not name.
type cleanKey struct {
	suite suiteKey
	tests int64
}

// testStatsBuilder folds jobs' results, newest job first.
type testStatsBuilder struct {
	out        testStats
	tests      map[testKey]*testTally
	flaky      map[flakyKey]*flakyTally
	cleanSuite map[cleanKey][]seenIn
	// legs is each job's jobs by attempt, and ambiguous the jobs one attempt ran more
	// than one of: a display name covering several legs of a matrix, whose failure on one
	// and pass on another say nothing, in one attempt or across two.
	legs      map[jobKey]map[jobAttempt]string
	ambiguous map[jobKey]bool
}

// noteLeg records that a job ran in an attempt, and marks its name as covering several
// legs when the attempt ran another.
func (b *testStatsBuilder) noteLeg(job jobKey, attempt jobAttempt, lease string) {
	attempts := b.legs[job]
	if attempts == nil {
		if len(b.legs) >= testStatsMaxCases {
			b.out.Partial = true

			return
		}
		attempts = map[jobAttempt]string{}
		b.legs[job] = attempts
	}
	if first, seen := attempts[attempt]; !seen {
		attempts[attempt] = lease
	} else if first != lease && !b.ambiguous[job] {
		b.ambiguous[job] = true
		b.out.Ambiguous++
	}
}

// buildTestStats reads the jobs' results a page at a time, so no read holds more than
// a page's, and ranks them.
func buildTestStats(ctx context.Context, a *alloc.Allocator, repo string, since time.Time, limit int,
) (testStats, error) {
	b := testStatsBuilder{
		out:   testStats{Notice: guestBanner, Since: since},
		tests: map[testKey]*testTally{}, flaky: map[flakyKey]*flakyTally{}, cleanSuite: map[cleanKey][]seenIn{},
		legs: map[jobKey]map[jobAttempt]string{}, ambiguous: map[jobKey]bool{},
	}

	var cursor alloc.TestResultsCursor

	seen := 0
	for {
		page, err := a.TestResultsSince(ctx, since, repo, cursor, testStatsPageRows)
		if err != nil {
			return testStats{}, err
		}
		for i := range page {
			b.addJob(&page[i])
		}
		seen += len(page)
		if len(page) < testStatsPageRows {
			break
		}
		if seen >= testStatsMaxJobs {
			b.out.Partial = true
			break
		}
		cursor = page[len(page)-1].Cursor
	}
	b.rank(limit)

	return b.out, nil
}

// addJob folds one job's results.
func (b *testStatsBuilder) addJob(rec *alloc.RecordedTestResults) {
	if rec.Unreadable || rec.Codec != guestreport.Codec {
		b.out.Unreadable++

		return
	}
	// A REFUSAL IS NEVER QUOTED HERE, though it quotes nothing the guest wrote: a count
	// tells the operator there is something to look at, and which job is in the ledger.
	results, err := guestreport.DecodeTestResults(rec.Data)
	if err != nil {
		b.out.Unreadable++

		return
	}
	b.out.Jobs++

	// AN UNKNOWN IS NOT A VALUE TWO JOBS CAN SHARE: flaky says nothing changed between
	// two runs, and two jobs of no recorded name are not known to be one job.
	job := jobKey{repo: rec.Repo, commit: rec.HeadSHA, workflow: rec.WorkflowRef, job: rec.JobName}
	attempt := jobAttempt{rec.RunID, rec.RunAttempt}
	identified := job.commit != "" && job.workflow != "" && job.job != "" && job.repo != "" &&
		attempt.run > 0 && attempt.attempt > 0
	if !identified {
		b.out.WithoutIdentity++
	} else {
		b.noteLeg(job, attempt, rec.LeaseID)
	}

	incomplete := results.Dropped.Suites > 0 || results.Dropped.Cases > 0
	for i := range results.Suites {
		s := &results.Suites[i]
		incomplete = incomplete || !s.Complete
		// ONLY A LIST THAT WAS CUT can stand for a case it does not name: a Complete
		// list names every case that ran, so a name it does not hold did not run.
		if identified && !s.Complete && s.Tests > 0 && s.Failures == 0 && s.Errors == 0 && s.Skipped == 0 {
			b.addCleanSuite(cleanKey{suiteKey{job, s.Name}, s.Tests},
				seenIn{lease: rec.LeaseID, attempt: attempt, suiteTests: s.Tests})
		}
		for j := range s.Cases {
			b.addCase(rec, job, identified, s, &s.Cases[j])
		}
	}
	if incomplete {
		b.out.Incomplete++
	}
}

// addCleanSuite keeps a suite that reported nothing wrong, one sighting to an attempt.
func (b *testStatsBuilder) addCleanSuite(key cleanKey, seen seenIn) {
	list, known := b.cleanSuite[key]
	if !known && len(b.cleanSuite) >= testStatsMaxCases {
		b.out.Partial = true

		return
	}
	b.cleanSuite[key] = addSeen(list, seen)
}

// addCase folds one case, the newest of its test first because jobs arrive newest
// first.
func (b *testStatsBuilder) addCase(rec *alloc.RecordedTestResults, job jobKey, identified bool,
	suite *guestreport.SuiteCases, c *guestreport.CaseResult,
) {
	key := testKey{rec.Repo, suite.Name, c.Name}
	t := b.tests[key]
	if t == nil {
		if len(b.tests) >= testStatsMaxCases {
			b.out.Partial = true

			return
		}
		t = &testTally{}
		b.tests[key] = t
	}

	if c.Status != guestreport.CaseSkipped {
		t.runs++
		if c.DurationMillis != nil {
			if len(t.durations) < testStatsDurations {
				t.durations = append(t.durations, *c.DurationMillis)
			}
			if t.maxLease == "" || *c.DurationMillis > t.maxMillis {
				t.maxMillis, t.maxLease = *c.DurationMillis, rec.LeaseID
			}
		}
	}
	if c.Failing() {
		t.failures++
		if t.failLease == "" {
			t.failLease, t.reason = rec.LeaseID, c.Message
		}
	}

	if !identified || (!c.Failing() && c.Status != guestreport.CasePassed) {
		return
	}
	fk := flakyKey{job, suite.Name, c.Name}
	f := b.flaky[fk]
	if f == nil {
		if len(b.flaky) >= testStatsMaxCases {
			b.out.Partial = true

			return
		}
		f = &flakyTally{}
		b.flaky[fk] = f
	}
	seen := seenIn{lease: rec.LeaseID, attempt: jobAttempt{rec.RunID, rec.RunAttempt}, suiteTests: suite.Tests}
	if c.Failing() {
		f.failures++
		f.fails = addSeen(f.fails, seen)
		f.lastFail = rec.LeaseID
		b.addFailCount(f, seen)
	} else {
		f.passes = addSeen(f.passes, seen)
		f.lastPass = rec.LeaseID
	}
	if f.sameJob == "" && f.lastFail == f.lastPass {
		f.sameJob = rec.LeaseID
	}
}

// addFailCount keeps a failure by what its suite counted, saying so when a job has made
// more counts of one test than the bound keeps.
func (b *testStatsBuilder) addFailCount(f *flakyTally, seen seenIn) {
	if f.failsByCount == nil {
		f.failsByCount = map[int64][]seenIn{}
	}
	if _, known := f.failsByCount[seen.suiteTests]; !known && len(f.failsByCount) >= flakyCounts {
		b.out.Partial = true

		return
	}
	f.failsByCount[seen.suiteTests] = addSeen(f.failsByCount[seen.suiteTests], seen)
}

// flakyPair is the evidence that one test failed and passed with nothing changed: a pass
// kept from another attempt, or a suite of as many tests that reported nothing wrong in
// another attempt, or both in one job; "" for none. A name covering several legs of a
// matrix has only the last, since its failure on one leg and pass on another are no
// evidence.
func (b *testStatsBuilder) flakyPair(key flakyKey, f *flakyTally) (fail, pass, evidence string) {
	if !b.ambiguous[key.job] {
		for _, failed := range f.fails {
			for _, passed := range f.passes {
				if failed.attempt != passed.attempt {
					return failed.lease, passed.lease, "recorded"
				}
			}
		}
		for _, count := range slices.Sorted(maps.Keys(f.failsByCount)) {
			for _, failed := range f.failsByCount[count] {
				for _, clean := range b.cleanSuite[cleanKey{suiteKey{key.job, key.suite}, count}] {
					if failed.attempt != clean.attempt {
						return failed.lease, clean.lease, "suite"
					}
				}
			}
		}
	}
	if f.sameJob != "" {
		return f.sameJob, f.sameJob, "same job"
	}

	return "", "", ""
}

// rank fills the three rankings, each with a total order so one ledger always prints
// one report.
func (b *testStatsBuilder) rank(limit int) {
	byTest := func(a, c testKey) int {
		return cmp.Or(cmp.Compare(a.repo, c.repo), cmp.Compare(a.suite, c.suite), cmp.Compare(a.name, c.name))
	}

	slowest := []slowestTest{}

	type failing struct {
		key   testKey
		tally *testTally
	}

	var failingTests []failing

	for key, t := range b.tests {
		if len(t.durations) > 0 {
			slowest = append(slowest, slowestTest{Repository: key.repo, Suite: key.suite, Test: key.name, Runs: t.runs,
				MedianMillis: median(t.durations), MaxMillis: t.maxMillis, Lease: t.maxLease})
		}
		if t.failures > 0 {
			failingTests = append(failingTests, failing{key, t})
		}
	}
	slices.SortFunc(slowest, func(a, c slowestTest) int {
		return cmp.Or(cmp.Compare(c.MedianMillis, a.MedianMillis), byTest(testKey{a.Repository, a.Suite, a.Test},
			testKey{c.Repository, c.Suite, c.Test}))
	})
	slices.SortFunc(failingTests, func(a, c failing) int {
		return cmp.Or(cmp.Compare(c.tally.failures, a.tally.failures), byTest(a.key, c.key))
	})
	b.out.Slowest = slowest[:min(limit, len(slowest))]
	b.out.Failing = make([]failingTest, 0, min(limit, len(failingTests)))
	for _, f := range failingTests[:min(limit, len(failingTests))] {
		b.out.Failing = append(b.out.Failing, failingTest{Repository: f.key.repo, Suite: f.key.suite, Test: f.key.name,
			Runs: f.tally.runs, Failures: f.tally.failures, Lease: f.tally.failLease, Message: f.tally.reason})
	}

	flaky := []flakyTest{}

	for key, f := range b.flaky {
		if f.failures == 0 {
			continue
		}
		fail, pass, evidence := b.flakyPair(key, f)
		if evidence == "" {
			continue
		}
		flaky = append(flaky, flakyTest{Repository: key.job.repo, Commit: key.job.commit,
			WorkflowRef: key.job.workflow, Job: key.job.job, Suite: key.suite, Test: key.name,
			Failures: f.failures, FailLease: fail, PassLease: pass, Evidence: evidence})
	}
	slices.SortFunc(flaky, func(a, c flakyTest) int {
		return cmp.Or(cmp.Compare(c.Failures, a.Failures), byTest(testKey{a.Repository, a.Suite, a.Test},
			testKey{c.Repository, c.Suite, c.Test}), cmp.Compare(a.Commit, c.Commit),
			cmp.Compare(a.WorkflowRef, c.WorkflowRef), cmp.Compare(a.Job, c.Job))
	})
	b.out.Flaky = flaky[:min(limit, len(flaky))]
}

// median is the nearest-rank median of values, which it reorders.
func median(values []int64) int64 {
	slices.Sort(values)

	return values[(len(values)-1)/2]
}

// renderTestStats writes the report. EVERY STRING A JOB CHOSE IS QUOTED, so none can
// forge a line: a test's name is whatever its result file says.
func renderTestStats(w io.Writer, r testStats) {
	fmt.Fprintln(w, r.Notice)
	fmt.Fprintf(w, "jobs recorded since %s: %d read", r.Since.UTC().Format(time.RFC3339), r.Jobs)
	if r.Unreadable > 0 {
		fmt.Fprintf(w, ", %d could not be read", r.Unreadable)
	}
	if r.Incomplete > 0 {
		fmt.Fprintf(w, ", %d with cases cut to fit their bounds, so a test's counts may fall short", r.Incomplete)
	}
	fmt.Fprintln(w)
	if r.Partial {
		fmt.Fprintln(w, "partial: the report's bounds stopped it; its oldest jobs, or the tests past its bound, "+
			"are left out; shorten --since or name a --repo")
	}

	fmt.Fprintln(w, "\nslowest (median of its newest runs)")
	if len(r.Slowest) == 0 {
		fmt.Fprintln(w, "  no test reported a duration")
	}
	for _, t := range r.Slowest {
		fmt.Fprintf(w, "  median %s, max %s over %d runs  %s %s %s  lease %s\n",
			spanMillis(t.MedianMillis), spanMillis(t.MaxMillis), t.Runs,
			strconv.Quote(t.Repository), strconv.Quote(t.Suite), strconv.Quote(t.Test), t.Lease)
	}

	fmt.Fprintln(w, "\nmost often failing")
	if len(r.Failing) == 0 {
		fmt.Fprintln(w, "  no test failed")
	}
	for _, t := range r.Failing {
		fmt.Fprintf(w, "  %d of %d runs  %s %s %s  newest lease %s\n", t.Failures, t.Runs,
			strconv.Quote(t.Repository), strconv.Quote(t.Suite), strconv.Quote(t.Test), t.Lease)
		if t.Message != "" {
			fmt.Fprintf(w, "    %s\n", strconv.Quote(t.Message))
		}
	}

	fmt.Fprintln(w, "\nflaky (failed and passed on one commit, in one job of one workflow)")
	if r.WithoutIdentity > 0 {
		fmt.Fprintf(w, "  %d jobs are left out of this ranking, with no commit, attempt, workflow or job name on record "+
			"(the commit comes from GitHub's record of the run, which needs the App's actions: read and is read "+
			"for a day after a job starts)\n", r.WithoutIdentity)
	}
	if r.Ambiguous > 0 {
		fmt.Fprintf(w, "  %d jobs' names each cover several jobs of one attempt, as legs of a matrix sharing a "+
			"name do, so only a pass and a failure inside one of their jobs are ranked\n", r.Ambiguous)
	}
	if len(r.Flaky) == 0 {
		fmt.Fprintln(w, "  none seen")
	}
	for _, t := range r.Flaky {
		pass := "passed in lease " + t.PassLease
		switch t.Evidence {
		case "suite":
			pass = "inferred passed in lease " + t.PassLease + ": its suite reported no failure, error or skip, " +
				"with its cases cut to fit"
		case "same job":
			pass = "and passed in that job: a retry its test runner made, or two cases of one name"
		}
		fmt.Fprintf(w, "  %d failed  %s %s %s  commit %s, job %s of %s\n    failed in lease %s, %s\n", t.Failures,
			strconv.Quote(t.Repository), strconv.Quote(t.Suite), strconv.Quote(t.Test), strconv.Quote(shortCommit(t.Commit)),
			strconv.Quote(t.Job), strconv.Quote(t.WorkflowRef), t.FailLease, pass)
	}
}

// shortCommit is a commit as a person reads one.
func shortCommit(sha string) string { return sha[:min(len(sha), 12)] }

// jobTests is what a job's kept test results say, already worded, or why there are
// none.
type jobTests struct {
	Why     string
	Results *guestreport.TestResults
}

// readJobTests reads and decodes a lease's kept test results. No row and one that
// cannot be read each say so: neither is a job with no tests.
func readJobTests(ctx context.Context, a *alloc.Allocator, leaseID string) jobTests {
	rec, err := a.LeaseTestResults(ctx, leaseID)
	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		return jobTests{Why: "no test cases recorded (the agent found none, the node or its VM's image is older " +
			"than this, or they were deleted past server.test_results_retention)"}
	case err != nil:
		return jobTests{Why: "could not tell: the ledger's row could not be read: " + strconv.Quote(err.Error())}
	case rec.Codec != guestreport.Codec:
		return jobTests{Why: fmt.Sprintf("could not tell: they are in codec %d, which this billet does not read", rec.Codec)}
	}
	results, err := guestreport.DecodeTestResults(rec.Data)
	if err != nil {
		// A REFUSAL QUOTES NOTHING THE GUEST WROTE: guestreport builds its text from its
		// own field names.
		return jobTests{Why: "could not tell: they could not be decoded (" + err.Error() + ")"}
	}

	return jobTests{Results: &results}
}

// renderJobTests writes a job's test cases: every failing one and the slowest of the
// rest. The guest's text is quoted, so none can forge a line.
func renderJobTests(w io.Writer, t jobTests) {
	const slowestShown = 10

	fmt.Fprintln(w)
	if t.Results == nil {
		fmt.Fprintf(w, "%-11s%s\n", "tests", t.Why)

		return
	}
	fmt.Fprintf(w, "%-11s%s\n", "tests", guestBanner)
	for i := range t.Results.Suites {
		s := &t.Results.Suites[i]
		cut := ""
		if !s.Complete {
			cut = fmt.Sprintf("; %d of its cases are kept", len(s.Cases))
		}
		fmt.Fprintf(w, "%-11s%s: %s tests, %s failures, %s errors, %s skipped%s\n", "suite", strconv.Quote(s.Name),
			groupDigits(s.Tests), groupDigits(s.Failures), groupDigits(s.Errors), groupDigits(s.Skipped), cut)

		var rest []*guestreport.CaseResult

		for j := range s.Cases {
			c := &s.Cases[j]
			if !c.Failing() {
				if c.DurationMillis != nil {
					rest = append(rest, c)
				}

				continue
			}
			fmt.Fprintf(w, "%-11s  %s %s%s\n", "", c.Status, strconv.Quote(c.Name), caseTime(c))
			if c.Message != "" {
				fmt.Fprintf(w, "%-11s    %s\n", "", strconv.Quote(c.Message))
			}
		}
		slices.SortStableFunc(rest, func(a, b *guestreport.CaseResult) int {
			return cmp.Compare(*b.DurationMillis, *a.DurationMillis)
		})
		if n := min(slowestShown, len(rest)); n > 0 {
			names := make([]string, 0, n)
			for _, c := range rest[:n] {
				names = append(names, strconv.Quote(c.Name)+caseTime(c))
			}
			fmt.Fprintf(w, "%-11s  slowest: %s\n", "", strings.Join(names, ", "))
		}
	}
	if d := t.Results.Dropped; d.Suites > 0 || d.Cases > 0 {
		fmt.Fprintf(w, "%-11s%d suites and %d cases past the bounds are left out\n", "", d.Suites, d.Cases)
	}
}

// caseTime is how long a case took as the result file says, or nothing where it does
// not say.
func caseTime(c *guestreport.CaseResult) string {
	if c.DurationMillis == nil {
		return ""
	}

	return " " + spanMillis(*c.DurationMillis)
}

// testsJSON is the tests member of `billet jobs show --json`: Results is null with Why
// saying why, and Notice labels them as the guest's.
type testsJSON struct {
	Notice  string                   `json:"notice"`
	Why     string                   `json:"why,omitempty"`
	Results *guestreport.TestResults `json:"results"`
}

// testsJSONOf is a job's tests member.
func testsJSONOf(t jobTests) testsJSON {
	return testsJSON{Notice: guestBanner, Why: t.Why, Results: t.Results}
}
