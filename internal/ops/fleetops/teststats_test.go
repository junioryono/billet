package fleetops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/guestreport"
)

func caseMillis(ms int64) *int64 { return &ms }

// aJobRow is one job's kept results as the ledger hands them over.
func aJobRow(t *testing.T, lease, commit string, suites ...guestreport.SuiteCases) *alloc.RecordedTestResults {
	t.Helper()

	_, data, err := guestreport.EncodeTestResults(guestreport.TestResults{Suites: suites}, guestreport.MaxTestResultsBytes)
	if err != nil {
		t.Fatalf("EncodeTestResults: %v", err)
	}

	// AN ATTEMPT OF ITS OWN FOR EACH LEASE, as re-runs of one run have.
	attempt := int64(1)
	for _, c := range lease {
		attempt = attempt*31 + int64(c)
	}

	return &alloc.RecordedTestResults{LeaseID: lease, Codec: guestreport.Codec, Data: data, Repo: "acme/api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", JobName: "test", HeadSHA: commit,
		RunID: 7, RunAttempt: attempt}
}

// aWholeSuite is a suite whose list names every case it counted.
func aWholeSuite(cases ...guestreport.CaseResult) guestreport.SuiteCases {
	s := guestreport.SuiteCases{Name: "pkg", Tests: int64(len(cases)), Complete: true, Cases: cases}
	for i := range cases {
		switch cases[i].Status {
		case guestreport.CaseFailed:
			s.Failures++
		case guestreport.CaseErrored:
			s.Errors++
		case guestreport.CaseSkipped:
			s.Skipped++
		}
	}

	return s
}

func passed(name string, ms int64) guestreport.CaseResult {
	return guestreport.CaseResult{Name: name, Status: guestreport.CasePassed, DurationMillis: caseMillis(ms)}
}

func failed(name, message string) guestreport.CaseResult {
	return guestreport.CaseResult{Name: name, Status: guestreport.CaseFailed, Message: message}
}

// statsOf ranks jobs, given newest first as the ledger reads them.
func statsOf(jobs ...*alloc.RecordedTestResults) testStats {
	b := testStatsBuilder{
		out:   testStats{Notice: guestBanner},
		tests: map[testKey]*testTally{}, flaky: map[flakyKey]*flakyTally{}, cleanSuite: map[cleanKey][]seenIn{},
		legs: map[jobKey]map[jobAttempt]string{}, ambiguous: map[jobKey]bool{},
	}
	for _, job := range jobs {
		b.addJob(job)
	}
	b.rank(20)

	return b.out
}

// THE SLOWEST ARE RANKED BY THE MEDIAN, SO ONE BAD RUN DOES NOT TOP THE LIST, each
// with the lease of its slowest run; a case with no duration is not ranked at zero.
func TestTheSlowestAreRankedByMedian(t *testing.T) {
	t.Parallel()

	noTime := guestreport.CaseResult{Name: "NoTime", Status: guestreport.CasePassed}
	got := statsOf(
		aJobRow(t, "l3", "c3", aWholeSuite(passed("Steady", 100), passed("Spike", 1), noTime)),
		aJobRow(t, "l2", "c2", aWholeSuite(passed("Steady", 110), passed("Spike", 9000))),
		aJobRow(t, "l1", "c1", aWholeSuite(passed("Steady", 90), passed("Spike", 2))),
	)

	if len(got.Slowest) != 2 || got.Slowest[0].Test != "Steady" || got.Slowest[1].Test != "Spike" {
		t.Fatalf("the ranking is %+v, want Steady above Spike", got.Slowest)
	}
	if s := got.Slowest[0]; s.MedianMillis != 100 || s.MaxMillis != 110 || s.Lease != "l2" || s.Runs != 3 {
		t.Errorf("Steady is %+v", s)
	}
	if s := got.Slowest[1]; s.MedianMillis != 2 || s.MaxMillis != 9000 || s.Lease != "l2" {
		t.Errorf("Spike is %+v", s)
	}
}

// THE MOST OFTEN FAILING NAME THE NEWEST FAILURE, what it said, and how many of its
// runs failed; a skip is not a run.
func TestTheMostOftenFailingNameTheirNewestFailure(t *testing.T) {
	t.Parallel()

	skipped := guestreport.CaseResult{Name: "Twice", Status: guestreport.CaseSkipped}
	got := statsOf(
		aJobRow(t, "l4", "c4", aWholeSuite(skipped)),
		aJobRow(t, "l3", "c3", aWholeSuite(failed("Twice", "newest"), failed("Once", "only"))),
		aJobRow(t, "l2", "c2", aWholeSuite(failed("Twice", "older"), passed("Once", 1))),
		aJobRow(t, "l1", "c1", aWholeSuite(passed("Twice", 1))),
	)

	if len(got.Failing) != 2 || got.Failing[0].Test != "Twice" || got.Failing[1].Test != "Once" {
		t.Fatalf("the ranking is %+v, want Twice above Once", got.Failing)
	}
	if f := got.Failing[0]; f.Failures != 2 || f.Runs != 3 || f.Lease != "l3" || f.Message != "newest" {
		t.Errorf("Twice is %+v", f)
	}
}

// FLAKY MEANS NOTHING BILLET CAN SEE CHANGED: THE SAME COMMIT, THE SAME JOB OF THE SAME
// WORKFLOW. A pass on another commit, or in another job of a matrix, proves nothing.
func TestFlakyNeedsOneCommitAndOneJob(t *testing.T) {
	t.Parallel()

	matrix := aJobRow(t, "l3", "same", aWholeSuite(passed("OtherJob", 1)))
	matrix.JobName = "test (linux)"
	got := statsOf(
		aJobRow(t, "l6", "same", aWholeSuite(failed("Flaky", "m"), failed("OtherJob", "m"), failed("OtherCommit", "m"))),
		aJobRow(t, "l5", "same", aWholeSuite(passed("Flaky", 1))),
		aJobRow(t, "l4", "other", aWholeSuite(passed("OtherCommit", 1))),
		matrix,
	)

	if len(got.Flaky) != 1 {
		t.Fatalf("flaky are %+v, want only the one that passed on the commit in the job", got.Flaky)
	}
	f := got.Flaky[0]
	if f.Test != "Flaky" || f.FailLease != "l6" || f.PassLease != "l5" || f.Evidence != "recorded" || f.Commit != "same" {
		t.Errorf("the flaky test is %+v", f)
	}
}

// A JOB WITH NO COMMIT IS LEFT OUT OF THE FLAKY RANKING AND SAID TO BE, and still
// counts in the other two: an empty commit is not one commit.
func TestAJobWithNoCommitIsNotFlakyAndIsCounted(t *testing.T) {
	t.Parallel()

	got := statsOf(
		aJobRow(t, "l2", "", aWholeSuite(failed("T", "m"))),
		aJobRow(t, "l1", "", aWholeSuite(passed("T", 1))),
	)

	if len(got.Flaky) != 0 || got.WithoutIdentity != 2 {
		t.Errorf("flaky %+v, %d without a commit; want none and 2", got.Flaky, got.WithoutIdentity)
	}
	if len(got.Failing) != 1 || len(got.Slowest) != 1 {
		t.Errorf("the other rankings are %+v and %+v, want the job in both", got.Failing, got.Slowest)
	}

	var out bytes.Buffer
	renderTestStats(&out, got)
	if !strings.Contains(out.String(), "2 jobs are left out of this ranking") {
		t.Errorf("the report does not say they were left out:\n%s", out.String())
	}
}

// A PASS THAT WAS CUT TO FIT IS INFERRED FROM ITS SUITE, and the report says it was
// inferred: only a suite whose list was cut, and that reports no failure, error or skip.
// A whole list that does not name the case says it did not run.
func TestAPassCutToFitIsInferredAndSaidToBe(t *testing.T) {
	t.Parallel()

	failing := aWholeSuite(failed("T", "m"))
	failing.Tests, failing.Complete = 100, false
	cut := guestreport.SuiteCases{Name: "pkg", Tests: 100}
	whole := aWholeSuite(passed("Different", 1))
	skipping := guestreport.SuiteCases{Name: "pkg", Tests: 100, Skipped: 1}
	smaller := guestreport.SuiteCases{Name: "pkg", Tests: 7}

	for name, tc := range map[string]struct {
		clean guestreport.SuiteCases
		want  bool
	}{
		"a cut list":              {cut, true},
		"a whole list without it": {whole, false},
		"a cut list with a skip":  {skipping, false},
		// A RUN THAT STOPPED EARLY RAN ANOTHER SET OF TESTS, and says nothing of this one.
		"a cut list of another size": {smaller, false},
	} {
		got := statsOf(
			aJobRow(t, "l2", "same", failing),
			aJobRow(t, "l1", "same", tc.clean),
		)
		if (len(got.Flaky) == 1) != tc.want {
			t.Errorf("%s: flaky are %+v, want flaky %t", name, got.Flaky, tc.want)
			continue
		}
		if !tc.want {
			continue
		}
		if f := got.Flaky[0]; f.Evidence != "suite" || f.PassLease != "l1" {
			t.Errorf("%s: the flaky test is %+v", name, f)
		}
		var out bytes.Buffer
		renderTestStats(&out, got)
		if !strings.Contains(out.String(), "inferred passed in lease l1") {
			t.Errorf("%s: the report does not say the pass was inferred:\n%s", name, out.String())
		}
	}
}

// A CASE THAT PASSES AND FAILS IN ONE JOB, a retry its test runner made, is flaky too.
func TestAPassAndAFailInOneJobIsFlaky(t *testing.T) {
	t.Parallel()

	// IN EITHER ORDER, since nothing says a retry is written after the failure it retried.
	if got := statsOf(aJobRow(t, "l1", "same", aWholeSuite(passed("T", 1), failed("T", "m")))); len(got.Flaky) != 1 ||
		got.Flaky[0].Evidence != "same job" || got.Flaky[0].FailLease != "l1" {
		t.Errorf("a pass before a failure in one job: flaky are %+v", got.Flaky)
	}

	got := statsOf(aJobRow(t, "l1", "same", aWholeSuite(failed("T", "m"), passed("T", 1))))
	if len(got.Flaky) != 1 || got.Flaky[0].FailLease != "l1" || got.Flaky[0].PassLease != "l1" ||
		got.Flaky[0].Evidence != "same job" {
		t.Fatalf("flaky are %+v", got.Flaky)
	}

	var out bytes.Buffer
	renderTestStats(&out, got)
	if !strings.Contains(out.String(), "a retry its test runner made, or two cases of one name") {
		t.Errorf("the report does not say what it cannot tell apart:\n%s", out.String())
	}
}

// TWO JOBS OF ONE ATTEMPT RAN SIDE BY SIDE, so a failure in one and a pass in the other
// is two legs of a matrix sharing a display name, not a flaky test. Another attempt of
// that run is, and so is another run.
func TestTwoJobsOfOneAttemptAreNotFlaky(t *testing.T) {
	t.Parallel()

	row := func(lease string, run, attempt int64, c guestreport.CaseResult) *alloc.RecordedTestResults {
		r := aJobRow(t, lease, "same", aWholeSuite(c))
		r.RunID, r.RunAttempt = run, attempt

		return r
	}

	if got := statsOf(row("l2", 7, 1, failed("T", "m")), row("l1", 7, 1, passed("T", 1))); len(got.Flaky) != 0 {
		t.Errorf("two legs of one attempt are flaky: %+v", got.Flaky)
	}
	if got := statsOf(row("l2", 7, 2, failed("T", "m")), row("l1", 7, 1, passed("T", 1))); len(got.Flaky) != 1 {
		t.Errorf("a re-run is not flaky: %+v", got.Flaky)
	}
	if got := statsOf(row("l2", 8, 1, failed("T", "m")), row("l1", 7, 1, passed("T", 1))); len(got.Flaky) != 1 {
		t.Errorf("another run of the commit is not flaky: %+v", got.Flaky)
	}
	// LEGS OF A MATRIX RE-RUN ARE STILL TWO LEGS: Linux passes and Windows fails in both
	// attempts, so a failure of the second and a pass of the first prove nothing.
	if got := statsOf(
		row("l4", 7, 2, failed("T", "m")), row("l3", 7, 2, passed("T", 1)),
		row("l2", 7, 1, failed("T", "m")), row("l1", 7, 1, passed("T", 1)),
	); len(got.Flaky) != 0 || got.Ambiguous != 1 {
		t.Errorf("a matrix re-run: flaky %+v, %d ambiguous; want none and 1", got.Flaky, got.Ambiguous)
	}
	// AND A JOB WITH NO ATTEMPT ON RECORD IS LEFT OUT, not one attempt with the rest.
	got := statsOf(row("l2", 7, 0, failed("T", "m")), row("l1", 7, 0, passed("T", 1)))
	if len(got.Flaky) != 0 || got.WithoutIdentity != 2 {
		t.Errorf("no attempt on record: flaky %+v, %d without identity", got.Flaky, got.WithoutIdentity)
	}
}

// A ROW THAT CANNOT BE READ IS COUNTED AND SAID TO BE, never ranked and never a job with
// no tests, and what it holds is not quoted.
func TestAnUnreadableRowIsCountedAndNotQuoted(t *testing.T) {
	t.Parallel()

	got := statsOf(
		&alloc.RecordedTestResults{LeaseID: "l1", Unreadable: true},
		&alloc.RecordedTestResults{LeaseID: "l2", Codec: 9, Data: []byte("hunter2")},
		&alloc.RecordedTestResults{LeaseID: "l3", Codec: guestreport.Codec, Data: []byte("hunter2")},
		aJobRow(t, "l4", "c", aWholeSuite(passed("T", 1))),
	)
	if got.Jobs != 1 || got.Unreadable != 3 {
		t.Fatalf("read %d jobs and %d unreadable, want 1 and 3", got.Jobs, got.Unreadable)
	}

	var out bytes.Buffer
	renderTestStats(&out, got)
	if !strings.Contains(out.String(), "3 could not be read") || strings.Contains(out.String(), "hunter2") {
		t.Errorf("the report is:\n%s", out.String())
	}
}

// EVERY STRING A JOB CHOSE IS QUOTED AND THE REPORT IS LABELLED AS THE GUEST'S: a test's
// name is whatever its result file says, and no name can forge a line of the report.
func TestTheReportQuotesWhatAJobChoseAndSaysWhose(t *testing.T) {
	t.Parallel()

	evil := `T"` + "\u202e"
	message := `wanted "1"`
	row := aJobRow(t, "l2", "same", aWholeSuite(failed(evil, message)))
	row.Repo = `acme/"forged"`
	got := statsOf(row, aJobRow(t, "l1", "same", aWholeSuite(passed(evil, 5))))

	var out bytes.Buffer
	renderTestStats(&out, got)
	text := out.String()
	if !strings.HasPrefix(text, guestBanner) {
		t.Errorf("the report does not begin by saying whose it is:\n%s", text)
	}
	for _, want := range []string{strconv.Quote(evil), strconv.Quote(message), strconv.Quote(row.Repo)} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not hold %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\u202e") {
		t.Errorf("an invisible character reached the report unescaped:\n%s", text)
	}
}

// A TEST'S DURATIONS ARE BOUNDED, its median over the newest, whatever a run reports.
func TestAMedianIsOverTheNewestDurations(t *testing.T) {
	t.Parallel()

	var jobs []*alloc.RecordedTestResults
	for i := range testStatsDurations {
		jobs = append(jobs, aJobRow(t, fmt.Sprintf("new%d", i), "c", aWholeSuite(passed("T", 10))))
	}
	for i := range testStatsDurations {
		jobs = append(jobs, aJobRow(t, fmt.Sprintf("old%d", i), "c", aWholeSuite(passed("T", 5000))))
	}

	got := statsOf(jobs...)
	if s := got.Slowest[0]; s.MedianMillis != 10 || s.MaxMillis != 5000 || s.Runs != 2*testStatsDurations {
		t.Errorf("T is %+v, want the newest runs' median and every run's maximum", s)
	}
}

// THE COMMAND READS THE LEDGER AS THE CLI BUILDS IT, reaches the three rankings through
// the real allocator, and its JSON says the same; --repo narrows it and a span it
// cannot read is refused.
func TestTestsStatsReadsTheLedger(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)

	seed := func(request int64, commit string, cases ...guestreport.CaseResult) string {
		t.Helper()
		_, data, err := guestreport.EncodeTestResults(guestreport.TestResults{
			Suites: []guestreport.SuiteCases{aWholeSuite(cases...)}}, guestreport.MaxTestResultsBytes)
		if err != nil {
			t.Fatalf("EncodeTestResults: %v", err)
		}

		return seedJob(t, stateDir, jobSeed{requestID: request, tests: data,
			job: &alloc.HistoryJob{JobID: strconv.FormatInt(request, 10), Owner: "acme", Repository: "api",
				WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", Name: "test", HeadSHA: commit,
				RunAttempt: request}})
	}
	failing := seed(1, "same", failed("Flaky", "wanted 1"))
	passing := seed(2, "same", passed("Flaky", 40))

	env, out := bufferedEnv()
	if err := Tests(t.Context(), env, []string{"stats", "--config", cfg, "--json"}); err != nil {
		t.Fatalf("billet tests stats: %v", err)
	}
	var got testStats
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if got.Jobs != 2 || got.Notice != guestBanner {
		t.Errorf("the report read %d jobs under the notice %q", got.Jobs, got.Notice)
	}
	if len(got.Flaky) != 1 || got.Flaky[0].FailLease != failing || got.Flaky[0].PassLease != passing {
		t.Errorf("flaky are %+v, want failed in %s and passed in %s", got.Flaky, failing, passing)
	}
	if len(got.Failing) != 1 || got.Failing[0].Message != "wanted 1" {
		t.Errorf("failing are %+v", got.Failing)
	}
	if len(got.Slowest) != 1 || got.Slowest[0].MedianMillis != 40 {
		t.Errorf("slowest are %+v", got.Slowest)
	}

	env, out = bufferedEnv()
	if err := Tests(t.Context(), env, []string{"stats", "--config", cfg, "--repo", "acme/other"}); err != nil {
		t.Fatalf("billet tests stats --repo: %v", err)
	}
	if !strings.Contains(out.String(), ": 0 read") {
		t.Errorf("another repository's report is:\n%s", out.String())
	}

	env, _ = bufferedEnv()
	if err := Tests(t.Context(), env, []string{"stats", "--config", cfg, "--since", "soon"}); err == nil ||
		!strings.Contains(err.Error(), "--since") {
		t.Errorf("a span it cannot read = %v", err)
	}
	if err := Tests(t.Context(), env, []string{"nothing"}); err == nil || !strings.Contains(err.Error(), "try stats") {
		t.Errorf("an unknown command = %v", err)
	}
}

// `billet jobs show` SAYS WHICH OF THE JOB'S TESTS FAILED, WHY, AND WHICH WERE SLOWEST,
// and that a job with no row has none recorded, never that it ran none.
func TestJobsShowPrintsAJobsTestCases(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)

	_, data, err := guestreport.EncodeTestResults(guestreport.TestResults{Suites: []guestreport.SuiteCases{
		aWholeSuite(failed("pkg.Fails", "wanted 1"), passed("pkg.Slow", 1200), passed("pkg.Fast", 1)),
	}}, guestreport.MaxTestResultsBytes)
	if err != nil {
		t.Fatalf("EncodeTestResults: %v", err)
	}
	lease := seedJob(t, stateDir, jobSeed{requestID: 1, tests: data})
	none := seedJob(t, stateDir, jobSeed{requestID: 2})

	env, out := bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, lease}); err != nil {
		t.Fatalf("billet jobs show: %v", err)
	}
	for _, want := range []string{
		guestBanner, `suite      "pkg": 3 tests, 1 failures, 0 errors, 0 skipped`,
		`failed "pkg.Fails"`, `"wanted 1"`, `slowest: "pkg.Slow" 1.2s, "pkg.Fast" 1ms`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}

	env, out = bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, none}); err != nil {
		t.Fatalf("billet jobs show (none): %v", err)
	}
	if !strings.Contains(out.String(), "no test cases recorded") {
		t.Errorf("a job with no row does not say so:\n%s", out.String())
	}
}

// AN UNKNOWN NAME IS NOT A NAME TWO JOBS SHARE: two jobs of no recorded workflow or job
// are not known to be one job, so a pass in one and a failure in the other is not flaky.
func TestAJobWithNoNameIsNotFlaky(t *testing.T) {
	t.Parallel()

	for name, clear := range map[string]func(*alloc.RecordedTestResults){
		"no job name": func(r *alloc.RecordedTestResults) { r.JobName = "" },
		"no workflow": func(r *alloc.RecordedTestResults) { r.WorkflowRef = "" },
		"no repo":     func(r *alloc.RecordedTestResults) { r.Repo = "" },
	} {
		failing, passing := aJobRow(t, "l2", "same", aWholeSuite(failed("T", "m"))),
			aJobRow(t, "l1", "same", aWholeSuite(passed("T", 1)))
		clear(failing)
		clear(passing)

		got := statsOf(failing, passing)
		if len(got.Flaky) != 0 || got.WithoutIdentity != 2 {
			t.Errorf("%s: flaky %+v, %d without identity; want none and 2", name, got.Flaky, got.WithoutIdentity)
		}
	}
}

// WHAT THE AGENT LEFT OUT MAKES A JOB INCOMPLETE, though every suite it sent is whole.
func TestAJobMissingSuitesIsIncomplete(t *testing.T) {
	t.Parallel()

	_, data, err := guestreport.EncodeTestResults(guestreport.TestResults{
		Suites:  []guestreport.SuiteCases{aWholeSuite(passed("T", 1))},
		Dropped: guestreport.DroppedCases{Suites: 1, Cases: 3},
	}, guestreport.MaxTestResultsBytes)
	if err != nil {
		t.Fatalf("EncodeTestResults: %v", err)
	}

	got := statsOf(&alloc.RecordedTestResults{LeaseID: "l1", Codec: guestreport.Codec, Data: data})
	if got.Incomplete != 1 {
		t.Errorf("%d jobs read as incomplete, want the one that lost a suite", got.Incomplete)
	}
}

// THE BOUNDS KEEP WHAT DECIDES: however many attempts failed, a pass in one more is found,
// and a clean suite of another count does not hide one of the same.
func TestTheBoundsKeepWhatDecides(t *testing.T) {
	t.Parallel()

	row := func(lease string, attempt int64, s guestreport.SuiteCases) *alloc.RecordedTestResults {
		r := aJobRow(t, lease, "same", s)
		r.RunAttempt = attempt

		return r
	}

	var jobs []*alloc.RecordedTestResults
	for i := range 2 * flakySightings {
		jobs = append(jobs, row(fmt.Sprintf("f%d", i), int64(100+i), aWholeSuite(failed("T", "m"))))
	}
	jobs = append(jobs, row("p", 1, aWholeSuite(passed("T", 1))))
	if got := statsOf(jobs...); len(got.Flaky) != 1 || got.Flaky[0].PassLease != "p" {
		t.Errorf("a pass past the bound on failures was lost: %+v", got.Flaky)
	}

	failing := aWholeSuite(failed("T", "m"))
	failing.Tests, failing.Complete = 100, false
	clean := func(tests int64) guestreport.SuiteCases {
		return guestreport.SuiteCases{Name: "pkg", Tests: tests}
	}
	got := statsOf(
		row("l3", 3, failing),
		row("l2", 4, clean(7)),
		row("l1", 2, clean(100)),
	)
	if len(got.Flaky) != 1 || got.Flaky[0].PassLease != "l1" || got.Flaky[0].Evidence != "suite" {
		t.Errorf("a clean suite of the same count was hidden: %+v", got.Flaky)
	}

	// A FAILURE'S COUNT IS KEPT PAST ANY NUMBER OF OTHERS, or the failure a clean suite
	// matches is the one thrown away.
	jobs = nil
	for i := range 2 * flakySightings {
		small := aWholeSuite(failed("T", "m"))
		jobs = append(jobs, row(fmt.Sprintf("f%d", i), int64(200+i), small))
	}
	jobs = append(jobs, row("big", 300, failing), row("clean", 301, clean(100)))
	if got := statsOf(jobs...); len(got.Flaky) != 1 || got.Flaky[0].FailLease != "big" ||
		got.Flaky[0].PassLease != "clean" {
		t.Errorf("a failure past the bound on sightings was lost: %+v", got.Flaky)
	}

	// PAST THE BOUND ON COUNTS IT SAYS SO, rather than quietly finding nothing.
	jobs = nil
	for i := range flakyCounts + 1 {
		s := aWholeSuite(failed("T", "m"))
		s.Tests, s.Complete = int64(100+i), false
		jobs = append(jobs, row(fmt.Sprintf("c%d", i), int64(400+i), s))
	}
	if got := statsOf(jobs...); !got.Partial {
		t.Error("a test of more counts than the bound keeps reads as complete")
	}
}
