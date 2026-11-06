package alloc

import (
	"database/sql"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// alertClock is a clock a test moves.
type alertClock struct{ now time.Time }

func (c *alertClock) read() time.Time { return c.now }

func newAlertAllocator(t *testing.T) (*Allocator, *alertClock) {
	t.Helper()

	clock := &alertClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}

	return newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}}, WithClock(clock.read)), clock
}

// judge is one judgement of a new job of one key under one rule.
// jobKey is a streak's key as the evaluation writes one: a JSON array of the condition
// and the job, quotes and all, which PostgreSQL's text must hold as SQLite's does.
const jobKey = `["failed","acme/api",".github/workflows/deploy.yml","deploy \"eu\" (prod)"]`

func judge(t *testing.T, a *Allocator, lease string, outcome AlertOutcome, inARow int64, cooldown time.Duration) AlertDecision {
	t.Helper()

	decision, err := a.RecordAlertJudgement(t.Context(), &AlertJudgement{Rule: "r", LeaseID: lease, Condition: "failed",
		Key: jobKey, Outcome: outcome, InARow: inARow, Cooldown: cooldown,
		Notices: []AlertNotice{{Destination: "oncall", Payload: []byte(`{"lease":"` + lease + `"}`)}}})
	if err != nil {
		t.Fatalf("judge %s: %v", lease, err)
	}

	return decision
}

func owed(t *testing.T, a *Allocator, destination string, clock *alertClock) []DueAlertNotice {
	t.Helper()

	due, err := a.DueAlertNotices(t.Context(), destination, clock.now, 100)
	if err != nil {
		t.Fatalf("DueAlertNotices: %v", err)
	}

	return due
}

// A RULE FIRES ONCE, THEN RESPECTS ITS COOLDOWN, and fires again when it ends. The
// hits in between still count, and what fired is owed exactly once.
func TestARuleFiresOnceThenRespectsItsCooldown(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)

	first := judge(t, a, "one", AlertHit, 1, time.Hour)
	if !first.Recorded || !first.Fired || first.Streak != 1 {
		t.Fatalf("the first hit = %+v, want it fired", first)
	}
	if due := owed(t, a, "oncall", clock); len(due) != 1 || due[0].Rule != "r" || due[0].LeaseID != "one" ||
		due[0].Condition != "failed" || string(due[0].Payload) != `{"lease":"one"}` || due[0].Attempts != 0 {
		t.Fatalf("owed = %+v", due)
	}

	clock.now = clock.now.Add(59 * time.Minute)
	second := judge(t, a, "two", AlertHit, 1, time.Hour)
	if second.Fired || !second.Recorded || second.Streak != 2 {
		t.Errorf("a hit inside the cooldown = %+v", second)
	}
	if due := owed(t, a, "oncall", clock); len(due) != 1 {
		t.Errorf("owed %d notices inside the cooldown, want the first alone", len(due))
	}

	clock.now = clock.now.Add(time.Minute)
	third := judge(t, a, "three", AlertHit, 1, time.Hour)
	if !third.Fired {
		t.Errorf("a hit as the cooldown ends = %+v, want it fired", third)
	}
	if due := owed(t, a, "oncall", clock); len(due) != 2 {
		t.Errorf("owed %d notices, want 2", len(due))
	}
}

// N IN A ROW IS IN A ROW: a run that does not hit starts it again.
func TestAStreakMustBeUnbroken(t *testing.T) {
	t.Parallel()

	a, _ := newAlertAllocator(t)

	if d := judge(t, a, "1", AlertHit, 3, 0); d.Fired || d.Streak != 1 {
		t.Errorf("first hit = %+v", d)
	}
	if d := judge(t, a, "2", AlertHit, 3, 0); d.Fired || d.Streak != 2 {
		t.Errorf("second hit = %+v", d)
	}
	if d := judge(t, a, "3", AlertMiss, 3, 0); d.Fired || d.Streak != 0 {
		t.Errorf("a miss = %+v", d)
	}
	if d := judge(t, a, "4", AlertHit, 3, 0); d.Fired || d.Streak != 1 {
		t.Errorf("a hit after a miss = %+v", d)
	}
	judge(t, a, "5", AlertHit, 3, 0)
	if d := judge(t, a, "6", AlertHit, 3, 0); !d.Fired || d.Streak != 3 {
		t.Errorf("the third in a row = %+v, want it fired", d)
	}
	// PAST N, WITH NO COOLDOWN, EACH HIT FIRES.
	if d := judge(t, a, "7", AlertHit, 3, 0); !d.Fired || d.Streak != 4 {
		t.Errorf("the fourth in a row = %+v, want it fired", d)
	}
}

// A JOB COUNTS ONCE, however many passes and controllers see it, and a second look
// changes nothing else.
func TestAJobIsJudgedOnce(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)

	if d := judge(t, a, "one", AlertHit, 1, 0); !d.Fired {
		t.Fatalf("first = %+v", d)
	}
	if d := judge(t, a, "one", AlertHit, 1, 0); d.Recorded || d.Fired {
		t.Errorf("the same job again = %+v, want nothing recorded", d)
	}
	if due := owed(t, a, "oncall", clock); len(due) != 1 {
		t.Errorf("owed %d, want 1", len(due))
	}

	// ANOTHER CONDITION, OR ANOTHER RULE, IS ANOTHER JUDGEMENT.
	for _, j := range []AlertJudgement{
		{Rule: "r", LeaseID: "one", Condition: "disrupted", Key: jobKey, Outcome: AlertMiss, InARow: 1},
		{Rule: "other", LeaseID: "one", Condition: "failed", Key: jobKey, Outcome: AlertMiss, InARow: 1},
	} {
		if d, err := a.RecordAlertJudgement(t.Context(), &j); err != nil || !d.Recorded {
			t.Errorf("%+v = %+v, %v", j, d, err)
		}
	}

	marks, err := a.AlertMarks(t.Context(), clock.now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("AlertMarks: %v", err)
	}
	if len(marks) != 3 {
		t.Errorf("marks = %+v", marks)
	}
}

// COULD-NOT-TELL COUNTS NEITHER WAY: a job nobody could read about does not break a
// streak, or add to one. So does a job the rule turned out not to be about.
func TestAnUnknownOrOtherJobLeavesTheStreakAlone(t *testing.T) {
	t.Parallel()

	a, _ := newAlertAllocator(t)

	judge(t, a, "1", AlertHit, 2, 0)

	for lease, outcome := range map[string]AlertOutcome{"2": AlertUnknown, "3": AlertOther} {
		if d := judge(t, a, lease, outcome, 2, 0); !d.Recorded || d.Fired || d.Streak != 0 {
			t.Errorf("%s = %+v", outcome, d)
		}
	}

	if d := judge(t, a, "4", AlertHit, 2, 0); !d.Fired || d.Streak != 2 {
		t.Errorf("the next hit = %+v, want the streak to have held", d)
	}
}

// EACH JOB OF A WORKFLOW COUNTS FOR ITSELF, under each rule: one job's cooldown and
// streak are not another's.
func TestAStreakAndACooldownAreOneJobsUnderOneRule(t *testing.T) {
	t.Parallel()

	a, _ := newAlertAllocator(t)

	record := func(rule, lease, key string) AlertDecision {
		t.Helper()

		d, err := a.RecordAlertJudgement(t.Context(), &AlertJudgement{Rule: rule, LeaseID: lease, Condition: "failed",
			Key: key, Outcome: AlertHit, InARow: 1, Cooldown: time.Hour})
		if err != nil {
			t.Fatal(err)
		}

		return d
	}

	if !record("r", "1", "job-a").Fired {
		t.Error("job a did not fire")
	}
	if !record("r", "2", "job-b").Fired {
		t.Error("job a's cooldown held job b")
	}
	if !record("other", "3", "job-a").Fired {
		t.Error("rule r's cooldown held rule other")
	}
	if record("r", "4", "job-a").Fired {
		t.Error("job a fired inside its own cooldown")
	}
}

// A MUTE SWALLOWS WHAT WOULD HAVE FIRED, FOR AS LONG AS IT LASTS, AND ONLY THEN. It
// costs no cooldown and owes nothing later, and the job goes on being counted.
func TestAMuteSilencesARuleAndLiftingItRestoresIt(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)

	if err := a.MuteAlertRule(t.Context(), "r", clock.now.Add(2*time.Hour), "alex"); err != nil {
		t.Fatalf("MuteAlertRule: %v", err)
	}

	if d := judge(t, a, "muted", AlertHit, 1, time.Hour); d.Fired || !d.Muted || d.Streak != 1 {
		t.Errorf("a hit while muted = %+v", d)
	}
	if due := owed(t, a, "oncall", clock); len(due) != 0 {
		t.Errorf("owed %d while muted", len(due))
	}

	report, err := a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatalf("AlertReport: %v", err)
	}
	if len(report.Rules) != 1 || report.Rules[0].MutedBy != "alex" || !report.Rules[0].MutedUntil.Equal(clock.now.Add(2*time.Hour)) {
		t.Errorf("the report's rule = %+v", report.Rules)
	}

	known, err := a.UnmuteAlertRule(t.Context(), "r")
	if err != nil || !known {
		t.Fatalf("UnmuteAlertRule = %v, %v", known, err)
	}
	if d := judge(t, a, "lifted", AlertHit, 1, time.Hour); !d.Fired || d.Muted {
		t.Errorf("a hit after the mute was lifted = %+v", d)
	}
	if due := owed(t, a, "oncall", clock); len(due) != 1 || due[0].LeaseID != "lifted" {
		t.Errorf("owed = %+v, want the later job alone", due)
	}

	if known, err := a.UnmuteAlertRule(t.Context(), "never-seen"); err != nil || known {
		t.Errorf("a rule nobody knows = %v, %v", known, err)
	}
}

// A MUTE ENDS BY ITSELF.
func TestAMuteEndsWhenItSays(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)

	if err := a.MuteAlertRule(t.Context(), "r", clock.now.Add(time.Hour), "alex"); err != nil {
		t.Fatal(err)
	}

	clock.now = clock.now.Add(time.Hour)
	if d := judge(t, a, "after", AlertHit, 1, 0); !d.Fired {
		t.Errorf("a hit as the mute ends = %+v", d)
	}
}

// A RULE'S FIRST SIGHTING STANDS: a restart does not move it, or a rule added long ago
// would start again and an operator's mute would lose its row.
func TestARulesFirstSightingStands(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)
	first := clock.now

	seen, err := a.EnsureAlertRules(t.Context(), []string{"r"})
	if err != nil || !seen["r"].Equal(first) {
		t.Fatalf("seen = %v, %v", seen, err)
	}

	clock.now = clock.now.Add(time.Hour)
	seen, err = a.EnsureAlertRules(t.Context(), []string{"r", "new"})
	if err != nil {
		t.Fatalf("EnsureAlertRules: %v", err)
	}
	if !seen["r"].Equal(first) || !seen["new"].Equal(clock.now) {
		t.Errorf("seen = %v", seen)
	}

	// A MUTE WRITTEN BEFORE ANY CONTROLLER SAW THE RULE SURVIVES ONE SEEING IT.
	if err := a.MuteAlertRule(t.Context(), "muted-first", clock.now.Add(time.Hour), "alex"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnsureAlertRules(t.Context(), []string{"muted-first"}); err != nil {
		t.Fatal(err)
	}
	if d, err := a.RecordAlertJudgement(t.Context(), &AlertJudgement{Rule: "muted-first", LeaseID: "1", Condition: "failed",
		Key: jobKey, Outcome: AlertHit, InARow: 1}); err != nil || d.Fired || !d.Muted {
		t.Errorf("after the first sighting = %+v, %v", d, err)
	}
}

// WHAT A DESTINATION IS OWED IS ITS OWN, comes due when it says, and ends once.
func TestANoticeIsOwedUntilItSettles(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)

	if _, err := a.RecordAlertJudgement(t.Context(), &AlertJudgement{Rule: "r", LeaseID: "one", Condition: "failed", Key: jobKey,
		Outcome: AlertHit, InARow: 1, Notices: []AlertNotice{
			{Destination: "oncall", Payload: []byte("a")}, {Destination: "pager", Payload: []byte("b")},
		}}); err != nil {
		t.Fatal(err)
	}

	due := owed(t, a, "oncall", clock)
	if len(due) != 1 || string(due[0].Payload) != "a" {
		t.Fatalf("oncall's = %+v", due)
	}
	if other := owed(t, a, "pager", clock); len(other) != 1 || string(other[0].Payload) != "b" {
		t.Fatalf("pager's = %+v", other)
	}

	if err := a.RetryAlertNotice(t.Context(), due[0].ID, clock.now.Add(time.Minute), "it timed out"); err != nil {
		t.Fatal(err)
	}
	if got := owed(t, a, "oncall", clock); len(got) != 0 {
		t.Errorf("a notice waiting for its next try is due: %+v", got)
	}

	clock.now = clock.now.Add(time.Minute)
	got := owed(t, a, "oncall", clock)
	if len(got) != 1 || got[0].Attempts != 1 {
		t.Fatalf("after its wait = %+v, want one try counted", got)
	}

	if err := a.SettleAlertNotice(t.Context(), got[0].ID, false, "the destination answered 403"); err != nil {
		t.Fatal(err)
	}
	if err := a.SettleAlertNotice(t.Context(), other(t, a, clock)[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := owed(t, a, "oncall", clock); len(got) != 0 {
		t.Errorf("a settled notice is due: %+v", got)
	}

	report, err := a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 1 || report.Failures[0].Destination != "oncall" ||
		report.Failures[0].Failure != "the destination answered 403" {
		t.Errorf("failures = %+v", report.Failures)
	}

	counts := map[string]int64{}
	for _, c := range report.Notices {
		counts[c.Destination+"/"+c.State] = c.Notices
	}
	if counts["oncall/"+AlertNoticeFailed] != 1 || counts["pager/"+AlertNoticeDelivered] != 1 || len(counts) != 2 {
		t.Errorf("counts = %v", counts)
	}

	// AN ANSWER STANDS: a failed notice is not delivered by a late report.
	if err := a.SettleAlertNotice(t.Context(), got[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	report, err = a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 1 {
		t.Errorf("a settled notice moved: %+v", report.Failures)
	}
}

func other(t *testing.T, a *Allocator, clock *alertClock) []DueAlertNotice {
	t.Helper()

	return owed(t, a, "pager", clock)
}

// IF THE NOTICE CANNOT BE OWED, THE JUDGEMENT DID NOT HAPPEN. A mark without its notice
// would be a job that can never fire again, so all of it lands or none.
func TestAJudgementLandsWholeOrNotAtAll(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE alert_outbox`)

		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.RecordAlertJudgement(t.Context(), &AlertJudgement{Rule: "r", LeaseID: "one", Condition: "failed", Key: jobKey,
		Outcome: AlertHit, InARow: 1, Notices: []AlertNotice{{Destination: "oncall", Payload: []byte("a")}}}); err == nil {
		t.Fatal("a notice that could not be owed was accepted")
	}

	marks, err := a.AlertMarks(t.Context(), clock.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 0 {
		t.Errorf("the mark outlived its notice: %+v", marks)
	}

	report, err := a.alertStreakCount(t)
	if err != nil {
		t.Fatal(err)
	}
	if report != 0 {
		t.Errorf("%d streaks outlived the notice", report)
	}
}

// alertStreakCount counts every streak row.
func (a *Allocator) alertStreakCount(t *testing.T) (int, error) {
	t.Helper()

	var count int
	err := a.db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM alert_streaks`).Scan(&count)

	return count, err
}

// WHAT IS OWED SURVIVES PRUNING, however old, and what is finished with does not.
func TestPruningKeepsWhatIsStillOwed(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)

	judge(t, a, "settled", AlertHit, 1, 0)
	settled := owed(t, a, "oncall", clock)
	if err := a.SettleAlertNotice(t.Context(), settled[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	judge(t, a, "pending", AlertHit, 1, 0)

	clock.now = clock.now.Add(30 * 24 * time.Hour)
	if err := a.PruneAlerts(t.Context(), clock.now.Add(-time.Hour), clock.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	due := owed(t, a, "oncall", clock)
	if len(due) != 1 || due[0].LeaseID != "pending" {
		t.Errorf("after pruning, owed = %+v", due)
	}

	marks, err := a.AlertMarks(t.Context(), clock.now.Add(-365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 0 {
		t.Errorf("marks past their retention = %+v", marks)
	}

	report, err := a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Streaks) != 0 {
		t.Errorf("streaks past their retention = %+v", report.Streaks)
	}
	for _, n := range report.Notices {
		if n.State != AlertNoticePending {
			t.Errorf("a settled notice survived: %+v", n)
		}
	}
}

// THE TWO READS ARE EACH THEIR OWN: the running by when they started, the ended by when
// their result came and after a cursor, both oldest first and bounded, and neither
// lists a job that never started or an ended job that started before the running
// window. A lease archived with no result is in the running read, saying so.
func TestAlertJobsAreTheRunningAndTheRecentlyEnded(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)
	now := clock.now

	insert := func(lease, started, result, resultAt, finished string) {
		t.Helper()

		var startedAt, finishedAt any
		if started != "" {
			startedAt = started
		}
		if finished != "" {
			finishedAt = finished
		}

		if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
				(lease_id, tier, queued_at, started_at, repo, workflow_ref, job_name, github_run_id, result, result_at,
				 github_queued_at, disruption, rerun_decision, node, head_branch, finished_at)
				VALUES ($1, 'billet-2vcpu', '', $2, 'acme/api', 'acme/api/.github/workflows/ci.yml@refs/heads/main', 'build', 31,
				        $3, $4, '', 'reclaimed', 'unproven', 'epyc-1', 'main', $5)`, lease, startedAt, result, resultAt, finishedAt)

			return err
		}); err != nil {
			t.Fatalf("insert %s: %v", lease, err)
		}
	}

	hour := func(n int) string { return ts(now.Add(time.Duration(n) * time.Hour)) }

	insert("running-older", hour(-3), "", "", "")
	insert("running", hour(-2), "", "", "")
	insert("running-too-long", hour(-200), "", "", "")
	insert("finished-without-a-result", hour(-2), "", "", hour(-1))
	insert("archived-too-long-ago", hour(-9), "", "", hour(-8))
	insert("ended-older", hour(-4), "failed", hour(-2), "")
	insert("ended", hour(-3), "failed", hour(-1), "")
	insert("ended-long-ago", hour(-30), "failed", hour(-20), "")
	insert("ended-but-started-before-the-window", hour(-200), "failed", hour(-1), "")
	insert("never-started", "", "canceled", hour(-1), "")
	insert("never-started-running", "", "", "", "")

	runningSince, endedSince := now.Add(-7*24*time.Hour), now.Add(-6*time.Hour)

	got, err := a.AlertJobs(t.Context(), runningSince, endedSince, AlertCursor{}, 100, true)
	if err != nil {
		t.Fatalf("AlertJobs: %v", err)
	}

	leases := func(jobs []AlertJob) []string {
		out := make([]string, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, j.LeaseID)
		}

		return out
	}
	if r := leases(got.Running); len(r) != 3 || r[0] != "running-older" || r[1] != "finished-without-a-result" ||
		r[2] != "running" {
		t.Errorf("running = %v, want the three, oldest first", r)
	}
	if archived := got.Running[1]; !archived.FinishedAt.Equal(now.Add(-time.Hour)) || !got.Running[2].FinishedAt.IsZero() {
		t.Errorf("finished = %s and %s, want only the archived one's", archived.FinishedAt, got.Running[2].FinishedAt)
	}
	if e := leases(got.Ended); len(e) != 2 || e[0] != "ended-older" || e[1] != "ended" {
		t.Fatalf("ended = %v, want the two, oldest first", e)
	}

	ended := got.Ended[1]
	if ended.Tier != "billet-2vcpu" || ended.Node != "epyc-1" || ended.Repository != "acme/api" || ended.JobName != "build" ||
		ended.WorkflowRef == "" || ended.RunID != 31 || ended.Result != "failed" || ended.Disruption != "reclaimed" ||
		ended.Rerun != "unproven" || ended.HeadBranch != "main" || !ended.StartedAt.Equal(now.Add(-3*time.Hour)) ||
		!ended.ResultAt.Equal(now.Add(-time.Hour)) || ended.Cursor.LeaseID != "ended" {
		t.Errorf("the ended job = %+v", ended)
	}
	if !ended.QueuedAt.IsZero() {
		t.Errorf("a queue time nobody recorded = %s", ended.QueuedAt)
	}
	if running := got.Running[2]; running.Result != "" || !running.ResultAt.IsZero() || running.HeadBranch != "main" {
		t.Errorf("the running job = %+v", running)
	}

	// A LIMIT BINDS EACH READ, keeping the oldest, and the cursor goes on from there.
	bounded, err := a.AlertJobs(t.Context(), runningSince, endedSince, AlertCursor{}, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if r, e := leases(bounded.Running), leases(bounded.Ended); len(r) != 1 || r[0] != "running-older" ||
		len(e) != 1 || e[0] != "ended-older" {
		t.Errorf("a limit of 1 = running %v, ended %v", r, e)
	}

	next, err := a.AlertJobs(t.Context(), runningSince, endedSince, bounded.Ended[0].Cursor, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if e := leases(next.Ended); len(e) != 1 || e[0] != "ended" {
		t.Errorf("after the cursor = %v, want the next one", e)
	}
	if len(next.Running) != 0 {
		t.Errorf("a read without the running jobs read %d", len(next.Running))
	}

	// TWO RESULTS STAMPED THE SAME INSTANT are told apart by lease, so a cursor on the
	// first reads the second and not the first again.
	insert("tie-a", hour(-2), "failed", hour(-1), "")
	insert("tie-b", hour(-2), "failed", hour(-1), "")
	tied, err := a.AlertJobs(t.Context(), runningSince, endedSince, AlertCursor{ResultAt: hour(-1), LeaseID: "tie-a"}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if e := leases(tied.Ended); len(e) != 1 || e[0] != "tie-b" {
		t.Errorf("after a cursor on tie-a = %v, want tie-b alone", e)
	}
}

// A NOTICE OWED TO A DESTINATION THE CONFIG NO LONGER NAMES IS GIVEN UP ON, with why;
// one owed to a destination it names is left to that destination's loop.
func TestNoticesOutsideTheConfiguredDestinationsAreGivenUpOn(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)
	if _, err := a.RecordAlertJudgement(t.Context(), &AlertJudgement{Rule: "r", LeaseID: "one", Condition: "failed",
		Key: jobKey, Outcome: AlertHit, InARow: 1, Notices: []AlertNotice{
			{Destination: "oncall", Payload: []byte("{}")}, {Destination: "removed", Payload: []byte("{}")},
		}}); err != nil {
		t.Fatal(err)
	}

	failed, err := a.FailAlertNoticesOutside(t.Context(), []string{"oncall"}, "its destination is gone")
	if err != nil || failed != 1 {
		t.Fatalf("FailAlertNoticesOutside = %d, %v, want the one notice", failed, err)
	}
	if due := owed(t, a, "oncall", clock); len(due) != 1 {
		t.Errorf("the configured destination is owed %d", len(due))
	}
	if due := owed(t, a, "removed", clock); len(due) != 0 {
		t.Errorf("the removed destination is still owed %d", len(due))
	}

	report, err := a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 1 || report.Failures[0].Destination != "removed" ||
		report.Failures[0].Failure != "its destination is gone" {
		t.Errorf("failures = %+v", report.Failures)
	}

	if again, err := a.FailAlertNoticesOutside(t.Context(), []string{"oncall"}, "x"); err != nil || again != 0 {
		t.Errorf("a second settlement = %d, %v", again, err)
	}
}

// A COOLDOWN STILL RUNNING OUTLIVES THE STREAK'S RETENTION, however long it was asked
// to be: pruning it would let the rule fire inside the quiet it promised.
func TestPruningKeepsACooldownStillRunning(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)
	judge(t, a, "one", AlertHit, 1, 45*24*time.Hour)

	clock.now = clock.now.Add(31 * 24 * time.Hour)
	if err := a.PruneAlerts(t.Context(), clock.now.Add(-8*24*time.Hour), clock.now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if d := judge(t, a, "two", AlertHit, 1, time.Hour); d.Fired {
		t.Errorf("a hit inside a 45-day cooldown fired after 31 days: %+v", d)
	}

	clock.now = clock.now.Add(15 * 24 * time.Hour)
	if err := a.PruneAlerts(t.Context(), clock.now.Add(-8*24*time.Hour), clock.now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d := judge(t, a, "three", AlertHit, 1, time.Hour); !d.Fired {
		t.Errorf("a hit after the cooldown ended did not fire: %+v", d)
	}
}

// UNMUTING SAYS WHETHER THERE WAS A MUTE, not whether the rule was ever seen.
func TestUnmutingSaysWhetherThereWasAMute(t *testing.T) {
	t.Parallel()

	a, clock := newAlertAllocator(t)
	if _, err := a.EnsureAlertRules(t.Context(), []string{"r"}); err != nil {
		t.Fatal(err)
	}

	if lifted, err := a.UnmuteAlertRule(t.Context(), "r"); err != nil || lifted {
		t.Errorf("unmuting a rule never muted = %v, %v", lifted, err)
	}
	if err := a.MuteAlertRule(t.Context(), "r", clock.now.Add(time.Hour), "someone"); err != nil {
		t.Fatal(err)
	}
	if lifted, err := a.UnmuteAlertRule(t.Context(), "r"); err != nil || !lifted {
		t.Errorf("unmuting a muted rule = %v, %v", lifted, err)
	}
}
