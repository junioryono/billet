package alloc

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// aReportWithTests is a guest report carrying tests as the node encodes them, whose
// bytes are opaque here.
func aReportWithTests(tests ...byte) GuestReport {
	report := aGuestReport()
	report.TestsCodec, report.Tests = TestResultsCodec, tests

	return report
}

// A JOB'S TEST RESULTS ARE KEPT AS SENT, FENCED ON THE EPOCH THE REPORT IS, AND THE
// FIRST ONE WINS, because they are written in its transaction.
func TestALeasesTestResultsAreKeptOnceUnderItsReportsFence(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch-1, aReportWithTests(1)); !errors.Is(err, ErrFenced) {
		t.Fatalf("a stale epoch's report = %v, want ErrFenced", err)
	}
	if _, err := a.LeaseTestResults(t.Context(), lease.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("after a fenced report the row reads %v, want none", err)
	}

	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aReportWithTests(1, 2, 255)); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aReportWithTests(9)); err != nil {
		t.Fatalf("RecordGuestReport (retry): %v", err)
	}

	got, err := a.LeaseTestResults(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseTestResults: %v", err)
	}
	if got.Codec != TestResultsCodec || !reflect.DeepEqual(got.Data, []byte{1, 2, 255}) {
		t.Errorf("read back codec %d, data %v; want the first report's", got.Codec, got.Data)
	}
}

// A REPORT WITH NO TESTS WRITES NO ROW, so a reader can say none were kept, and an
// invalid codec is refused before it reaches the ledger.
func TestAReportWithNoTestsKeepsNoRow(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aGuestReport()); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	if _, err := a.LeaseTestResults(t.Context(), lease.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("a report with no tests reads %v, want ErrLeaseNotFound", err)
	}

	bad := aGuestReport()
	bad.TestsCodec, bad.Tests = 9, []byte{1}
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, bad); err == nil ||
		!strings.Contains(err.Error(), "codec 9") {
		t.Errorf("an unknown codec = %v, want refused", err)
	}
}

// testedJob is a busy lease of repository whose report carried tests, recorded at the
// fleet's clock, with commit on its history row.
func testedJob(t *testing.T, a *Allocator, repository, commit string) *Lease {
	t.Helper()

	lease := busyLease(t, a)
	owner, name, _ := strings.Cut(repository, "/")
	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{JobID: lease.ID, Owner: owner, Repository: name,
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", Name: "test", HeadSHA: commit,
		HeadBranch: "main"}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aReportWithTests(1)); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}

	return lease
}

// RESULTS ARE READ NEWEST FIRST A PAGE AT A TIME, EACH WITH THE JOB IT CAME FROM, within
// a window and a repository.
func TestTestResultsAreReadInPagesWithTheirJobs(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	oldest := testedJob(t, a, "acme/api", "aaa")
	now = now.Add(time.Minute)
	middle := testedJob(t, a, "acme/api", "bbb")
	now = now.Add(time.Minute)
	newest := testedJob(t, a, "acme/api", "ccc")
	now = now.Add(time.Minute)
	testedJob(t, a, "acme/other", "ddd")

	page, err := a.TestResultsSince(t.Context(), now.Add(-time.Hour), "acme/api", TestResultsCursor{}, 2)
	if err != nil {
		t.Fatalf("TestResultsSince: %v", err)
	}
	if len(page) != 2 || page[0].LeaseID != newest.ID || page[1].LeaseID != middle.ID {
		t.Fatalf("the first page is %+v, want the two newest of the repository", page)
	}
	got := page[0]
	if got.Repo != "acme/api" || got.HeadSHA != "ccc" || got.HeadBranch != "main" || got.JobName != "test" ||
		got.WorkflowRef != "acme/api/.github/workflows/ci.yml@refs/heads/main" || got.Unreadable ||
		!reflect.DeepEqual(got.Data, []byte{1}) {
		t.Errorf("the newest row is %+v", got)
	}

	page, err = a.TestResultsSince(t.Context(), now.Add(-time.Hour), "acme/api", page[1].Cursor, 2)
	if err != nil {
		t.Fatalf("TestResultsSince, the second page: %v", err)
	}
	if len(page) != 1 || page[0].LeaseID != oldest.ID {
		t.Fatalf("the second page is %+v, want the oldest", page)
	}

	all, err := a.TestResultsSince(t.Context(), now.Add(-time.Hour), "", TestResultsCursor{}, 10)
	if err != nil || len(all) != 4 {
		t.Fatalf("every repository is %d rows (%v), want 4", len(all), err)
	}

	recent, err := a.TestResultsSince(t.Context(), now.Add(-90*time.Second), "acme/api", TestResultsCursor{}, 10)
	if err != nil || len(recent) != 1 || recent[0].LeaseID != newest.ID {
		t.Fatalf("the last ninety seconds are %+v (%v), want the newest", recent, err)
	}
}

// A JOB'S COMMIT IS WRITTEN ONCE, like the rest of its identity, and a job keeps being
// listed for GitHub's record until it has both its queue time and its commit.
func TestAJobIsAwaitingUntilItHasItsCommitAndItsQueueTime(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)
	lease := busyLease(t, a)

	awaiting := func() bool {
		t.Helper()

		jobs, err := a.JobsAwaitingQueueTime(t.Context(), now.Add(-time.Hour), QueueTimeCursor{}, 10)
		if err != nil {
			t.Fatalf("JobsAwaitingQueueTime: %v", err)
		}

		return len(jobs) == 1 && jobs[0].LeaseID == lease.ID
	}
	identity := HistoryJob{JobID: lease.ID, Owner: "acme", Repository: "api", RunID: 7}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, identity); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}
	if !awaiting() {
		t.Fatal("a job with neither is not listed")
	}

	queued := now.Add(-time.Minute)
	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{JobID: lease.ID, QueuedAt: queued}); err != nil {
		t.Fatalf("RecordJobIdentity (queue time): %v", err)
	}
	if !awaiting() {
		t.Error("a job with its queue time and no commit is not listed")
	}

	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{JobID: lease.ID, HeadSHA: "aaa", HeadBranch: "main"}); err != nil {
		t.Fatalf("RecordJobIdentity (commit): %v", err)
	}
	if awaiting() {
		t.Error("a job with both is still listed")
	}

	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{JobID: lease.ID, HeadSHA: "bbb", HeadBranch: "other"}); err != nil {
		t.Fatalf("RecordJobIdentity (a second commit): %v", err)
	}
	rec, err := a.Job(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if rec.Job.HeadSHA != "aaa" || rec.Job.HeadBranch != "main" {
		t.Errorf("the commit is %q on %q, want the first written", rec.Job.HeadSHA, rec.Job.HeadBranch)
	}
}

// TEST RESULTS GO BY THEIR OWN AGE AND THEIR OWN BOUND, THE OLDEST FIRST, and neither
// they nor a job's detail take the other with them: the point of the row is to outlast
// the guest report that carried it.
func TestTestResultsPastTheirRetentionOrBoundGoAlone(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	first := testedJob(t, a, "acme/api", "aaa")
	now = now.Add(time.Hour)
	second := testedJob(t, a, "acme/api", "bbb")
	now = now.Add(time.Hour)
	third := testedJob(t, a, "acme/api", "ccc")

	// aReportWithTests(1) is one byte, four of base64.
	past, err := a.TestResultsPastBound(t.Context(), 8, 10)
	if err != nil {
		t.Fatalf("TestResultsPastBound: %v", err)
	}
	if !reflect.DeepEqual(past, []string{first.ID}) {
		t.Errorf("past a bound of two rows are %v, want the oldest", past)
	}

	expired, err := a.TestResultsPastRetention(t.Context(), now.Add(-90*time.Minute), 10)
	if err != nil {
		t.Fatalf("TestResultsPastRetention: %v", err)
	}
	if !reflect.DeepEqual(expired, []string{first.ID}) {
		t.Errorf("past the retention are %v, want the oldest", expired)
	}

	n, err := a.DeleteTestResults(t.Context(), expired)
	if err != nil || n != 1 {
		t.Fatalf("DeleteTestResults = %d, %v", n, err)
	}
	if _, err := a.LeaseTestResults(t.Context(), first.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("the deleted row reads %v", err)
	}
	if _, err := a.LeaseGuestReport(t.Context(), first.ID); err != nil {
		t.Errorf("deleting test results took the guest report: %v", err)
	}
	for _, kept := range []*Lease{second, third} {
		if _, err := a.LeaseTestResults(t.Context(), kept.ID); err != nil {
			t.Errorf("a row inside the retention reads %v", err)
		}
	}

	if n, err := a.DeleteTestResults(t.Context(), nil); err != nil || n != 0 {
		t.Errorf("deleting nothing = %d, %v", n, err)
	}
	if _, err := a.DeleteTestResults(t.Context(), []string{"short"}); err == nil {
		t.Error("a lease id of another length was deleted by")
	}
}

// RETIRING A JOB'S DETAIL LEAVES ITS TEST RESULTS, which is why they have a row.
func TestRetiringAJobsDetailLeavesItsTestResults(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	lease := detailedJob(t, a, detail{usageAt: now})
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aReportWithTests(1)); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	if _, err := a.LeaseTestResults(t.Context(), lease.ID); err != nil {
		t.Fatalf("LeaseTestResults: %v", err)
	}
	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got, err := retire(t, a, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got.GuestReports != 1 {
		t.Fatalf("the retirement deleted %d guest reports, want 1", got.GuestReports)
	}
	if _, err := a.LeaseTestResults(t.Context(), lease.ID); err != nil {
		t.Errorf("after the retirement the test results read %v", err)
	}
}

// AN IGNORED REPORT SUPPLIES NOTHING. A first report without tests is the report, so a
// retry cannot add results to it; and a retry after a retention deleted the results
// cannot bring them back with a fresh time.
func TestAnIgnoredReportSuppliesNoTestResults(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	without := busyLease(t, a)
	if err := a.RecordGuestReport(t.Context(), without.ID, without.Epoch, aGuestReport()); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	if err := a.RecordGuestReport(t.Context(), without.ID, without.Epoch, aReportWithTests(1)); err != nil {
		t.Fatalf("RecordGuestReport (retry): %v", err)
	}
	if _, err := a.LeaseTestResults(t.Context(), without.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("a retry added test results to the report that was kept: %v", err)
	}

	// A FLEET OF ITS OWN: the first holds its one slot.
	a = quarantineFleet(t, &now)

	with := busyLease(t, a)
	if err := a.RecordGuestReport(t.Context(), with.ID, with.Epoch, aReportWithTests(1)); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	if _, err := a.DeleteTestResults(t.Context(), []string{with.ID}); err != nil {
		t.Fatalf("DeleteTestResults: %v", err)
	}
	if err := a.RecordGuestReport(t.Context(), with.ID, with.Epoch, aReportWithTests(1)); err != nil {
		t.Fatalf("RecordGuestReport (retry): %v", err)
	}
	if _, err := a.LeaseTestResults(t.Context(), with.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("a retry brought back test results a retention deleted: %v", err)
	}
}
