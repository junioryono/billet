package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alerts"
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

// alertRecord answers GitHub's record of a run and a job from fields, counting reads.
type alertRecord struct {
	*fakeProvisioner

	mu        sync.Mutex
	branch    string
	steps     []alerts.Step
	err       error
	reads     int
	remaining int64
	limit     int64
}

func (g *alertRecord) WorkflowRun(_ context.Context, _, _ string, runID int64) (WorkflowRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.reads++

	return WorkflowRun{ID: runID, HeadBranch: g.branch}, g.err
}

func (g *alertRecord) DefaultBranch(context.Context, string, string) (string, error) {
	return "main", nil
}

func (g *alertRecord) RunnerJobSteps(_ context.Context, _, _ string, _ int64, _ string) ([]alerts.Step, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.reads++

	return g.steps, g.err
}

func (g *alertRecord) RESTHeadroom() (int64, int64, time.Time, bool) {
	return g.remaining, g.limit, time.Now().Add(time.Hour), g.limit > 0
}

// alertStack is a real allocator over a real ledger, and the loop's own pass at a
// clock the test holds, which the ledger's writes read too, against a destination
// that records what it is sent.
type alertStack struct {
	t       *testing.T
	s       *Server
	a       *alloc.Allocator
	db      *state.DB
	record  *alertRecord
	targets map[string]Target
	sweep   *alertSweep
	now     time.Time
	got     *atomic.Int32
	bodies  chan string
}

const alertTier = "billet-4vcpu-a"

func newAlertStack(t *testing.T, rules ...config.AlertRule) *alertStack {
	t.Helper()

	db := openState(t)
	tiers := []config.Tier{tier(alertTier)}
	now := time.Now()

	// THE LEDGER'S CLOCK IS THE TEST'S, so a cooldown and a rule's first sighting are
	// decided on the clock the test moves.
	var r *alertStack
	clock := func() time.Time {
		if r == nil {
			return now
		}

		return r.now
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers, alloc.WithClock(clock))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	got := &atomic.Int32{}
	bodies := make(chan string, 64)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		if _, err := fmt.Fprint(buf, readAll(t, r)); err != nil {
			t.Errorf("read a notice: %v", err)
		}
		got.Add(1)
		bodies <- buf.String()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(destination.Close)

	set := &Alerts{Destinations: map[string]AlertDestination{"hook": {
		Sender: alerts.NewSender("hook", alerts.URL(destination.URL+"/secret")), Kind: config.AlertWebhook,
	}}}
	for i := range rules {
		rule, err := alerts.NewRule(&rules[i])
		if err != nil {
			t.Fatalf("NewRule: %v", err)
		}
		set.Rules = append(set.Rules, rule)
	}

	record := &alertRecord{fakeProvisioner: &fakeProvisioner{}}

	r = &alertStack{t: t, s: New(a, nil, tiers, "owner", nil, WithAlerts(set)), a: a, db: db, record: record,
		targets: map[string]Target{alertTier: {Config: config.GitHubTarget{Name: "default", Org: "acme", InstallationID: 7},
			Provisioner: record}},
		sweep: &alertSweep{judged: map[alloc.AlertMark]bool{}, branches: map[rerunRun]string{}}, now: now, got: got, bodies: bodies}

	return r
}

func readAll(t *testing.T, r *http.Request) string {
	t.Helper()

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

// jobRow is what a test says the ledger holds about one job.
type jobRow struct {
	lease, result, disruption string
	name, repo, queued        string
	age                       time.Duration
	ran                       time.Duration
}

// seed writes one job. Its start is age ago; a result arrives ran after that.
func (r *alertStack) seed(job jobRow) {
	r.t.Helper()

	if job.name == "" {
		job.name = "deploy"
	}
	if job.repo == "" {
		job.repo = "acme/api"
	}
	if job.age == 0 {
		job.age = 10 * time.Minute
	}

	started := r.now.Add(-job.age)
	resultAt := ""
	if job.result != "" {
		resultAt = stamp(started.Add(job.ran))
	}

	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, started_at, repo, workflow_ref, job_name, github_run_id, result, result_at,
			 github_queued_at, disruption)
			VALUES ($1, $2, '', $3, $4, 'acme/api/.github/workflows/deploy.yml@refs/heads/main', $5, 31, $6, $7, $8, $9)`,
			job.lease, alertTier, stamp(started), job.repo, job.name, job.result, resultAt, job.queued, job.disruption)

		return err
	}); err != nil {
		r.t.Fatalf("seed %s: %v", job.lease, err)
	}
}

// pass is one evaluation at the stack's clock. The first one of a stack records each
// rule's first sighting at that clock, so seed a job after it.
func (r *alertStack) pass() { r.s.evaluateAlertsOnce(r.t.Context(), r.targets, r.sweep, r.now) }

// deliver is one look at the destination's queue, for what is due at the clock.
func (r *alertStack) deliver() {
	dest := r.s.alerts.Destinations["hook"]
	r.s.deliverAlertsOnce(r.t.Context(), "hook", dest.Sender, r.now)
}

func (r *alertStack) owed() []alloc.DueAlertNotice {
	r.t.Helper()

	due, err := r.a.DueAlertNotices(r.t.Context(), "hook", r.now.Add(time.Hour), 100)
	if err != nil {
		r.t.Fatalf("DueAlertNotices: %v", err)
	}

	return due
}

// atRule is the rule the tests use unless they say otherwise.
func atRule(mutate ...func(*config.AlertRule)) config.AlertRule {
	rule := config.AlertRule{Name: "deploys", On: config.AlertConditions{Failed: true}, Notify: []string{"hook"}, Cooldown: "1h"}
	for _, m := range mutate {
		m(&rule)
	}

	return rule
}

// A RULE FIRES ONCE AND THEN RESPECTS ITS COOLDOWN, through the real loop and a real
// destination, with the notice saying what happened. A later pass over the same job
// sends nothing, and neither does a second failure inside the cooldown.
func TestARuleFiresOnceThenRespectsItsCooldownThroughTheLoop(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule())
	r.pass()
	r.now = r.now.Add(time.Minute)

	r.seed(jobRow{lease: "first", result: "failed", ran: 5 * time.Minute, age: time.Second})
	r.now = r.now.Add(time.Minute)
	r.pass()
	r.deliver()

	if got := r.got.Load(); got != 1 {
		t.Fatalf("the destination got %d notices, want 1", got)
	}

	var notice map[string]any
	if err := json.Unmarshal([]byte(<-r.bodies), &notice); err != nil {
		t.Fatalf("decode the notice: %v", err)
	}
	for key, want := range map[string]any{"rule": "deploys", "condition": "failed", "lease": "first", "job": "deploy",
		"repository": "acme/api", "tier": alertTier, "billet_alert": float64(1)} {
		if notice[key] != want {
			t.Errorf("%s = %v, want %v", key, notice[key], want)
		}
	}

	r.pass()
	r.deliver()
	if got := r.got.Load(); got != 1 {
		t.Errorf("a second pass over the job sent %d notices in all", got)
	}

	r.seed(jobRow{lease: "second", result: "failed", ran: time.Minute, age: time.Second})
	r.now = r.now.Add(10 * time.Minute)
	r.pass()
	r.deliver()
	if got := r.got.Load(); got != 1 {
		t.Errorf("a failure inside the cooldown sent %d notices in all", got)
	}

	r.seed(jobRow{lease: "third", result: "failed", ran: time.Minute, age: time.Second})
	r.now = r.now.Add(2 * time.Hour)
	r.pass()
	r.deliver()
	if got := r.got.Load(); got != 2 {
		t.Errorf("a failure past the cooldown left %d notices in all, want 2", got)
	}
}

// A RESTART REPEATS NOTHING: a new sweep over the same ledger reads what was judged.
func TestARestartDoesNotJudgeAJobAgain(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule(func(c *config.AlertRule) { c.Cooldown = "0s" }))
	r.pass()
	r.now = r.now.Add(time.Minute)
	r.seed(jobRow{lease: "first", result: "failed", ran: time.Second, age: time.Second})
	r.now = r.now.Add(time.Minute)
	r.pass()
	r.deliver()

	r.sweep = &alertSweep{judged: map[alloc.AlertMark]bool{}, branches: map[rerunRun]string{}}
	r.pass()
	r.deliver()

	if got := r.got.Load(); got != 1 {
		t.Errorf("after a restart the destination had %d notices, want 1", got)
	}
}

// A RULE IS ABOUT JOBS FROM WHEN IT WAS FIRST SEEN: adding one pages nobody about the
// last day's failures.
func TestANewRuleIgnoresHistory(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule())
	r.seed(jobRow{lease: "old", result: "failed", ran: time.Minute, age: time.Hour})

	r.pass()
	r.deliver()

	if got := r.got.Load(); got != 0 {
		t.Errorf("a job from before the rule sent %d notices", got)
	}
}

// A RUNNING JOB FIRES AS IT PASSES THE DURATION, while it runs, and once.
func TestARunningJobFiresWhileItRuns(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule(func(c *config.AlertRule) { c.On = config.AlertConditions{RunningLongerThan: "1h"} }))
	r.pass()
	r.now = r.now.Add(time.Minute)
	r.seed(jobRow{lease: "slow", age: 30 * time.Second})

	r.pass()
	if due := r.owed(); len(due) != 0 {
		t.Fatalf("owed %d before the duration", len(due))
	}

	r.now = r.now.Add(time.Hour)
	r.pass()
	r.pass()
	if due := r.owed(); len(due) != 1 || due[0].Condition != "running-long" || due[0].LeaseID != "slow" {
		t.Errorf("owed = %+v, want one notice", due)
	}
}

// A QUEUE TIME NOBODY HAS RECORDED IS NEITHER LONG NOR SHORT, until it is recorded.
func TestAWaitIsJudgedOnceItIsKnown(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule(func(c *config.AlertRule) { c.On = config.AlertConditions{WaitedLongerThan: "10m"} }))
	r.pass()
	r.now = r.now.Add(time.Minute)
	r.seed(jobRow{lease: "waited", result: "succeeded", ran: time.Second, age: time.Second})

	r.pass()
	if due := r.owed(); len(due) != 0 {
		t.Fatalf("owed %d with no queue time", len(due))
	}

	queued := stamp(r.now.Add(-time.Hour))
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE job_history SET github_queued_at = $1 WHERE lease_id = 'waited'`, queued)

		return err
	}); err != nil {
		t.Fatal(err)
	}

	r.pass()
	if due := r.owed(); len(due) != 1 || due[0].Condition != "waited-long" {
		t.Errorf("owed = %+v", due)
	}
}

// A RULE THAT NAMES NEITHER ASKS GITHUB NOTHING, and a rule naming a branch reads each
// run once and judges a job on another branch as not its job, which breaks no streak.
func TestABranchIsReadOncePerRunAndNarrowsTheRule(t *testing.T) {
	t.Parallel()

	plain := newAlertStack(t, atRule())
	plain.pass()
	plain.now = plain.now.Add(time.Minute)
	plain.seed(jobRow{lease: "any", result: "failed", ran: time.Second, age: time.Second})
	plain.pass()
	if plain.record.reads != 0 {
		t.Errorf("a rule naming neither read GitHub %d times", plain.record.reads)
	}

	r := newAlertStack(t, atRule(func(c *config.AlertRule) {
		c.Match.Branch = "main"
		c.InARow = 2
		c.Cooldown = "0s"
	}))
	r.pass()
	r.now = r.now.Add(time.Minute)
	r.record.branch = "main"

	r.seed(jobRow{lease: "one", result: "failed", ran: time.Second, age: time.Second})
	r.pass()
	if r.record.reads != 1 {
		t.Fatalf("read %d times", r.record.reads)
	}

	// THE SAME RUN, SO NO NEW READ, on another job, which completes the streak.
	r.seed(jobRow{lease: "two", result: "failed", ran: time.Second, age: time.Second, name: "deploy"})
	r.pass()
	if r.record.reads != 1 {
		t.Errorf("read %d times for one run", r.record.reads)
	}
	if due := r.owed(); len(due) != 1 || due[0].LeaseID != "two" {
		t.Errorf("owed = %+v, want the second in a row", due)
	}

	branch := newAlertStack(t, atRule(func(c *config.AlertRule) { c.Match.Branch = "main" }))
	branch.pass()
	branch.now = branch.now.Add(time.Minute)
	branch.record.branch = "feature"
	branch.seed(jobRow{lease: "elsewhere", result: "failed", ran: time.Second, age: time.Second})
	branch.pass()
	if due := branch.owed(); len(due) != 0 {
		t.Errorf("a job on another branch is owed: %+v", due)
	}
}

// A STEP THE RULE NAMES HAS TO BE THE ONE THAT FAILED. Only a hit asks: a job that
// succeeded has nothing to narrow.
func TestAStepNarrowsAHitAndOnlyAHitAsks(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule(func(c *config.AlertRule) { c.Match.Step = "Deploy*" }))
	r.pass()
	r.now = r.now.Add(time.Minute)

	r.seed(jobRow{lease: "fine", result: "succeeded", ran: time.Second, age: time.Second})
	r.pass()
	if r.record.reads != 0 {
		t.Errorf("a job that succeeded read its steps %d times", r.record.reads)
	}

	r.record.steps = []alerts.Step{{Name: "Build", Conclusion: "failure"}}
	r.seed(jobRow{lease: "build", result: "failed", ran: time.Second, age: time.Second, name: "other"})
	r.pass()
	if due := r.owed(); len(due) != 0 {
		t.Errorf("a failure in another step is owed: %+v", due)
	}

	r.record.steps = []alerts.Step{{Name: "Deploy to eu", Conclusion: "failure"}}
	r.seed(jobRow{lease: "deploy", result: "failed", ran: time.Second, age: time.Second, name: "third"})
	r.pass()
	if due := r.owed(); len(due) != 1 || due[0].LeaseID != "deploy" {
		t.Errorf("owed = %+v", due)
	}
}

// COULD NOT TELL WAITS, AND THEN SAYS SO. A read that fails is not a miss and not a
// hit: the job is asked about again, and past the window is judged unknown, which
// moves no streak and sends nothing, rather than a rule falling silent unnoticed.
func TestAReadThatFailsWaitsThenIsJudgedUnknown(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule(func(c *config.AlertRule) { c.Match.Branch = "main" }))
	r.pass()
	r.now = r.now.Add(time.Minute)
	r.record.err = errors.New("HTTP 403: Resource not accessible by integration")
	r.seed(jobRow{lease: "unread", result: "failed", ran: time.Second, age: time.Second})

	r.pass()
	reads := r.record.reads
	if reads == 0 {
		t.Fatal("GitHub was not asked")
	}
	marks, err := r.a.AlertMarks(t.Context(), r.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 0 {
		t.Fatalf("a read that failed recorded %+v", marks)
	}

	r.pass()
	if r.record.reads <= reads {
		t.Errorf("the job was not asked about again: %d reads", r.record.reads)
	}

	r.now = r.now.Add(alertReadWindow + time.Minute)
	r.pass()
	marks, err = r.a.AlertMarks(t.Context(), r.now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 1 {
		t.Errorf("past the window, marks = %+v", marks)
	}
	if due := r.owed(); len(due) != 0 {
		t.Errorf("an unknown job is owed: %+v", due)
	}
}

// THE RESERVE IS LEFT, AND EACH INSTALLATION HAS ITS SHARE OF A PASS, because this
// decides nothing about a runner.
func TestAlertReadsLeaveTheReserveAndTakeTheirShare(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule(func(c *config.AlertRule) { c.Match.Step = "Deploy*" }))
	r.pass()
	r.now = r.now.Add(time.Minute)
	r.record.remaining, r.record.limit = 100, 5000
	r.seed(jobRow{lease: "held", result: "failed", ran: time.Second, age: time.Second})
	r.pass()
	if r.record.reads != 0 {
		t.Errorf("read %d times on an installation down to its reserve", r.record.reads)
	}

	r = newAlertStack(t, atRule(func(c *config.AlertRule) { c.Match.Step = "Deploy*" }))
	r.pass()
	r.now = r.now.Add(time.Minute)
	for i := range alertRunsPerInstallation + 2 {
		r.seed(jobRow{lease: fmt.Sprintf("job-%d", i), result: "failed", ran: time.Second, age: time.Second,
			name: fmt.Sprintf("job-%d", i)})
	}
	r.pass()
	if r.record.reads != alertRunsPerInstallation {
		t.Errorf("%d reads in one pass, want the share's %d", r.record.reads, alertRunsPerInstallation)
	}
}

// A STEP IS READ FOR THE JOB BY ITS RUNNER'S NAME, which is its lease's.
func TestAStepsAreReadByTheRunnersName(t *testing.T) {
	t.Parallel()

	if got := provider.InstanceName("abc"); got != "billet-abc" {
		t.Errorf("the runner of lease abc is %q", got)
	}
}

// A DESTINATION THAT FAILS GETS ITS TRIES, BOUNDED, spaced and reported, and never more.
func TestADeliveryIsBoundedAndSpaced(t *testing.T) {
	t.Parallel()

	var tries atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(failing.Close)

	r := newAlertStack(t, atRule())
	dest := alerts.NewSender("hook", alerts.URL(failing.URL+"/secret-path"))
	if _, err := r.a.RecordAlertJudgement(t.Context(), &alloc.AlertJudgement{Rule: "deploys", LeaseID: "job", Condition: "failed",
		Key: "k", Outcome: alloc.AlertHit, InARow: 1, Notices: []alloc.AlertNotice{{Destination: "hook", Payload: []byte("{}")}}}); err != nil {
		t.Fatal(err)
	}

	// THE LEDGER'S CLOCK IS REAL, so the notice is due from now.
	r.now = time.Now().Add(time.Second)

	wait := alertRetryFirst
	for try := int32(1); try <= alertMaxAttempts; try++ {
		r.s.deliverAlertsOnce(t.Context(), "hook", dest, r.now)
		if got := tries.Load(); got != try {
			t.Fatalf("after try %d: %d requests", try, got)
		}

		// NOT AGAIN UNTIL ITS WAIT HAS PASSED.
		if try < alertMaxAttempts {
			r.s.deliverAlertsOnce(t.Context(), "hook", dest, r.now.Add(wait-time.Second))
			if got := tries.Load(); got != try {
				t.Errorf("after try %d a second request came inside its wait of %s", try, wait)
			}
		}

		r.now = r.now.Add(wait)
		wait *= 2
	}

	r.s.deliverAlertsOnce(t.Context(), "hook", dest, r.now.Add(24*time.Hour))
	if got := tries.Load(); got != alertMaxAttempts {
		t.Errorf("%d requests, want the limit's %d", got, alertMaxAttempts)
	}

	report, err := r.a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Failure, "503") {
		t.Fatalf("failures = %+v", report.Failures)
	}
	if strings.Contains(report.Failures[0].Failure, "secret-path") {
		t.Errorf("the ledger holds the address: %+v", report.Failures[0])
	}
}

// AN ANSWER TRYING AGAIN CANNOT HELP ENDS AT ONCE.
func TestAPermanentAnswerIsNotTriedAgain(t *testing.T) {
	t.Parallel()

	var tries atomic.Int32
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(gone.Close)

	r := newAlertStack(t, atRule())
	dest := alerts.NewSender("hook", alerts.URL(gone.URL))
	if _, err := r.a.RecordAlertJudgement(t.Context(), &alloc.AlertJudgement{Rule: "deploys", LeaseID: "job", Condition: "failed",
		Key: "k", Outcome: alloc.AlertHit, InARow: 1, Notices: []alloc.AlertNotice{{Destination: "hook", Payload: []byte("{}")}}}); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		r.s.deliverAlertsOnce(t.Context(), "hook", dest, time.Now().Add(24*time.Hour))
	}
	if got := tries.Load(); got != 1 {
		t.Errorf("%d requests, want 1", got)
	}
}

// A DESTINATION THAT HANGS HOLDS UP ONLY ITSELF. Two real queues side by side: the one
// whose request is demonstrably inside its hang does not stop the other delivering.
func TestADestinationThatHangsHoldsUpOnlyItsOwnQueue(t *testing.T) {
	t.Parallel()

	// RELEASED BEFORE THE SERVER CLOSES, which waits for its handlers: cleanups run
	// last first, so this one is registered after the server's.
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) })

	r := newAlertStack(t, atRule())
	if _, err := r.a.RecordAlertJudgement(t.Context(), &alloc.AlertJudgement{Rule: "deploys", LeaseID: "job", Condition: "failed",
		Key: "k", Outcome: alloc.AlertHit, InARow: 1, Notices: []alloc.AlertNotice{
			{Destination: "stuck", Payload: []byte("{}")}, {Destination: "hook", Payload: []byte(`{"to":"hook"}`)},
		}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.s.deliverAlertsOnce(ctx, "stuck", alerts.NewSender("stuck", alerts.URL(hung.URL)), time.Now().Add(time.Second))
	}()

	<-entered

	r.now = time.Now().Add(time.Second)
	r.deliver()
	select {
	case body := <-r.bodies:
		if body != `{"to":"hook"}` {
			t.Errorf("the other destination got %q", body)
		}
	default:
		t.Error("the other destination got nothing while one hung")
	}

	// AND A STOP ENDS THE HUNG REQUEST, with its try uncounted: it was not an answer.
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a stop did not end a hung delivery")
	}

	due, err := r.a.DueAlertNotices(t.Context(), "stuck", time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Attempts != 0 {
		t.Errorf("after a stop = %+v, want it still owed with no try counted", due)
	}
}

// ONLY RULES START THE LOOPS, and the option carries the rules.
func TestAlertsStartOnlyWithRules(t *testing.T) {
	t.Parallel()

	s := New(nil, nil, nil, "owner", nil)
	if s.alerts != nil {
		t.Error("a control plane given no alerts holds some")
	}
}
