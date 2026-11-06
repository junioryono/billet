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

// alertRecord answers GitHub's record of a run and its jobs from fields, counting
// reads and keeping what each asked for.
type alertRecord struct {
	*fakeProvisioner

	mu     sync.Mutex
	branch string
	// steps is each runner's job in every run a test reads, completed unless
	// unfinished says otherwise.
	steps      map[string][]JobStep
	unfinished bool
	err        error
	reads      int
	asked      []string
	remaining  int64
	limit      int64
}

func (g *alertRecord) WorkflowRun(_ context.Context, owner, repository string, runID int64) (WorkflowRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.reads++
	g.asked = append(g.asked, fmt.Sprintf("run %s/%s %d", owner, repository, runID))

	return WorkflowRun{ID: runID, HeadBranch: g.branch}, g.err
}

func (g *alertRecord) DefaultBranch(context.Context, string, string) (string, error) {
	return "main", nil
}

func (g *alertRecord) RunJobSteps(_ context.Context, owner, repository string, runID int64,
) (map[string]RunnerSteps, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.reads++
	g.asked = append(g.asked, fmt.Sprintf("jobs %s/%s %d", owner, repository, runID))

	out := map[string]RunnerSteps{}
	for runner, steps := range g.steps {
		out[runner] = RunnerSteps{Completed: !g.unfinished, Steps: steps}
	}

	return out, g.err
}

func (g *alertRecord) RESTHeadroom() (int64, int64, time.Time, bool) {
	return g.remaining, g.limit, time.Now().Add(time.Hour), g.limit > 0
}

func (g *alertRecord) readCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.reads
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
		got.Add(1)
		bodies <- readAll(t, r)
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
		sweep: newAlertSweep(), now: now, got: got, bodies: bodies}

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
	branch, tier              string
	run                       int64
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
	if job.run == 0 {
		job.run = 31
	}
	if job.tier == "" {
		job.tier = alertTier
	}

	started := r.now.Add(-job.age)
	resultAt := ""
	if job.result != "" {
		resultAt = stamp(started.Add(job.ran))
	}

	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, started_at, repo, workflow_ref, job_name, github_run_id, result, result_at,
			 github_queued_at, disruption, head_branch)
			VALUES ($1, $2, '', $3, $4, 'acme/api/.github/workflows/deploy.yml@refs/heads/main', $5, $6, $7, $8, $9, $10, $11)`,
			job.lease, job.tier, stamp(started), job.repo, job.name, job.run, job.result, resultAt, job.queued,
			job.disruption, job.branch)

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

// owedLeases is the leases owed a notice, in the order they were owed.
func (r *alertStack) owedLeases() []string {
	r.t.Helper()

	var out []string
	for _, n := range r.owed() {
		out = append(out, n.LeaseID+":"+n.Condition)
	}

	return out
}

// marked is how many judgements the ledger holds.
func (r *alertStack) marked() int {
	r.t.Helper()

	marks, err := r.a.AlertMarks(r.t.Context(), r.now.Add(-30*24*time.Hour))
	if err != nil {
		r.t.Fatal(err)
	}

	return len(marks)
}

// atRule is the rule the tests use unless they say otherwise.
func atRule(mutate ...func(*config.AlertRule)) config.AlertRule {
	rule := config.AlertRule{Name: "deploys", On: config.AlertConditions{Failed: true}, Notify: []string{"hook"}, Cooldown: "1h"}
	for _, m := range mutate {
		m(&rule)
	}

	return rule
}

// started is a stack whose rules have been first seen, a minute ago.
func started(t *testing.T, rules ...config.AlertRule) *alertStack {
	t.Helper()

	r := newAlertStack(t, rules...)
	r.pass()
	r.now = r.now.Add(time.Minute)

	return r
}

// A RULE FIRES ONCE AND THEN RESPECTS ITS COOLDOWN, through the real loop and a real
// destination, with the notice saying what happened. A later pass over the same job
// sends nothing, and neither does a second failure inside the cooldown.
func TestARuleFiresOnceThenRespectsItsCooldownThroughTheLoop(t *testing.T) {
	t.Parallel()

	r := started(t, atRule())

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

	r := started(t, atRule(func(c *config.AlertRule) { c.Cooldown = "0s" }))
	r.seed(jobRow{lease: "first", result: "failed", ran: time.Second, age: time.Second})
	r.now = r.now.Add(time.Minute)
	r.pass()
	r.deliver()

	r.sweep = newAlertSweep()
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

// EACH CONDITION OF A RULE COUNTS APART, as the docs promise: a failure that was not
// disrupted is a miss on `disrupted`, and must not break the failures' streak.
func TestEachConditionOfARuleCountsApart(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) {
		c.On = config.AlertConditions{Failed: true, Disrupted: true}
		c.InARow = 2
		c.Cooldown = "0s"
	}))

	r.now = r.now.Add(5 * time.Minute)
	r.seed(jobRow{lease: "one", result: "failed", ran: time.Second, age: 3 * time.Minute})
	r.seed(jobRow{lease: "two", result: "failed", ran: time.Second, age: 2 * time.Minute})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	if got := r.owedLeases(); len(got) != 1 || got[0] != "two:failed" {
		t.Errorf("owed = %v, want the second failure in a row", got)
	}

	// AND ONE JOB IS NOT TWO IN A ROW: a disrupted failure is one of each.
	both := started(t, atRule(func(c *config.AlertRule) {
		c.On = config.AlertConditions{Failed: true, Disrupted: true}
		c.InARow = 2
	}))
	both.seed(jobRow{lease: "once", result: "failed", disruption: "reclaimed", ran: time.Second, age: time.Second})
	both.now = both.now.Add(5 * time.Minute)
	both.pass()
	if got := both.owedLeases(); len(got) != 0 {
		t.Errorf("one job fired a rule asking for two: %v", got)
	}
}

// A STREAK COUNTS ITS JOBS IN THE ORDER THEY RAN, however the passes fell: a success
// between two failures breaks it even when both arrive between the same two passes.
func TestAStreakCountsItsJobsInTheOrderTheyRan(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) {
		c.InARow = 2
		c.Cooldown = "0s"
	}))

	r.seed(jobRow{lease: "first", result: "failed", ran: time.Second, age: 50 * time.Second})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	// BETWEEN TWO PASSES: a success, then a failure.
	r.seed(jobRow{lease: "fixed", result: "succeeded", ran: time.Second, age: 3 * time.Minute})
	r.seed(jobRow{lease: "broke", result: "failed", ran: time.Second, age: 2 * time.Minute})
	r.pass()

	if got := r.owedLeases(); len(got) != 0 {
		t.Errorf("owed = %v, want none: a success came between the failures", got)
	}
}

// A JOB PUT OFF HOLDS THE LATER JOBS OF ITS STREAK: a failure whose branch GitHub has
// not yet given waits, and so does the success after it, so the streak still counts
// the failure before the success once the read comes back.
func TestAJobPutOffHoldsTheLaterJobsOfItsStreak(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) {
		c.Match.Branch = "main"
		c.InARow = 2
		c.Cooldown = "0s"
	}))
	r.now = r.now.Add(5 * time.Minute)

	r.seed(jobRow{lease: "first", result: "failed", ran: time.Second, age: 4 * time.Minute, branch: "main"})
	r.seed(jobRow{lease: "unread", result: "failed", ran: time.Second, age: 3 * time.Minute, run: 51})
	r.seed(jobRow{lease: "fixed", result: "succeeded", ran: time.Second, age: 2 * time.Minute, branch: "main"})
	r.record.err = errors.New("connection reset")
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	if got := r.marked(); got != 1 {
		t.Fatalf("%d marks, want only the first judged: the later two wait for the unread one", got)
	}

	r.record.err = nil
	r.record.branch = "main"
	r.pass()

	if got := r.owedLeases(); len(got) != 1 || got[0] != "unread:failed" {
		t.Errorf("owed = %v, want the second failure in a row, counted before the success", got)
	}
}

// A JOB NOTHING COULD EVER READ ABOUT IS UNKNOWN AT ONCE, and holds nothing back: its
// tier is gone from the config, so no request for it can start a clock.
func TestAJobNothingCanReadIsUnknownAtOnce(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) {
		c.Match.Branch = "main"
		c.Cooldown = "0s"
	}))
	r.record.branch = "main"
	r.now = r.now.Add(5 * time.Minute)

	r.seed(jobRow{lease: "orphan", result: "failed", ran: time.Second, age: 2 * time.Minute, tier: "removed-tier"})
	r.seed(jobRow{lease: "later", result: "failed", ran: time.Second, age: time.Minute})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	if got := r.marked(); got != 2 {
		t.Errorf("%d marks, want the orphan judged unknown and the later job judged", got)
	}
	if got := r.owedLeases(); len(got) != 1 || got[0] != "later:failed" {
		t.Errorf("owed = %v, want the later job, held back by nothing", got)
	}
}

// A RUNNING JOB HOLDS NOTHING BACK: one still running cannot be out of order, and one
// on a branch the ledger shows the rule does not name is not its job at all. Neither
// keeps another's disruption from being told at once.
func TestARunningJobHoldsBackNoDisruption(t *testing.T) {
	t.Parallel()

	for name, earlier := range map[string]jobRow{
		"still running, undisrupted": {lease: "earlier", age: 20 * time.Second, branch: "main"},
		"on another branch":          {lease: "earlier", age: 20 * time.Second, branch: "feature"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := started(t, atRule(func(c *config.AlertRule) {
				c.On = config.AlertConditions{Disrupted: true}
				c.Match.Branch = "main"
			}))
			r.seed(earlier)
			r.seed(jobRow{lease: "disrupted", disruption: "guest-absent", age: 10 * time.Second, branch: "main"})
			r.pass()

			if got := r.owedLeases(); len(got) != 1 || got[0] != "disrupted:disrupted" {
				t.Errorf("owed = %v, want the disruption told at once", got)
			}
		})
	}
}

// A LEASE A DISRUPTION ARCHIVED BEFORE GITHUB SPOKE is told at once, and is not taken
// for a job still running long.
func TestAnArchivedDisruptionIsToldAtOnce(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) {
		c.On = config.AlertConditions{Disrupted: true, RunningLongerThan: "1m"}
	}))
	r.seed(jobRow{lease: "gone", disruption: "guest-absent", age: 30 * time.Second})
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE job_history SET finished_at = $1 WHERE lease_id = 'gone'`, stamp(r.now))

		return err
	}); err != nil {
		t.Fatal(err)
	}

	r.now = r.now.Add(time.Hour)
	r.pass()

	if got := r.owedLeases(); len(got) != 1 || got[0] != "gone:disrupted" {
		t.Errorf("owed = %v, want the disruption alone", got)
	}
}

// A JOB ON A BRANCH THE LEDGER SHOWS THE RULE DOES NOT NAME is not its job, and holds
// nothing back even while it cannot be judged: a wait with no queue time yet on
// another branch does not keep a wait on the rule's own branch waiting with it.
func TestAJobOnAnotherBranchHoldsNothingBack(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) {
		c.On = config.AlertConditions{WaitedLongerThan: "10m"}
		c.Match.Branch = "main"
	}))
	r.now = r.now.Add(5 * time.Minute)
	r.seed(jobRow{lease: "feature", result: "succeeded", ran: time.Second, age: 2 * time.Minute, branch: "feature"})
	r.seed(jobRow{lease: "main", result: "succeeded", ran: time.Second, age: time.Minute, branch: "main",
		queued: stamp(r.now.Add(-time.Hour))})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	if got := r.owedLeases(); len(got) != 1 || got[0] != "main:waited-long" {
		t.Errorf("owed = %v, want the wait on main told now", got)
	}
}

// AN HOUR AFTER GITHUB WAS FIRST ASKED IT IS UNKNOWN, whatever the budget does after:
// an installation sitting at its reserve does not hold a job, or its streak, for ever.
func TestTheHourRunsOutAfterAnAskWhateverTheBudget(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.Match.Branch = "main" }))
	r.record.err = errors.New("connection reset")
	r.now = r.now.Add(5 * time.Minute)
	r.seed(jobRow{lease: "asked", result: "failed", ran: time.Second, age: time.Second})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()
	if got := r.record.readCount(); got != 1 {
		t.Fatalf("%d reads, want the one ask", got)
	}

	r.record.remaining, r.record.limit = 100, 5000
	r.now = r.now.Add(alertReadWindow + time.Minute)
	r.pass()

	if got := r.marked(); got != 1 {
		t.Errorf("%d marks, want the job judged unknown an hour after it was asked", got)
	}
}

// A JOB NEVER ASKED ABOUT IS JUDGED UNKNOWN BEFORE IT LEAVES THE READ, and logged, so
// an installation that never has a read to spare cannot let it age out unseen.
func TestAJobNeverAskedIsUnknownBeforeItLeavesTheRead(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.Match.Branch = "main" }))
	r.record.remaining, r.record.limit = 100, 5000
	r.now = r.now.Add(5 * time.Minute)
	r.seed(jobRow{lease: "starved", result: "failed", ran: time.Second, age: time.Second})

	r.now = r.now.Add(time.Hour)
	r.pass()
	if got := r.marked(); got != 0 {
		t.Fatalf("%d marks an hour in, want it still waiting for a read", got)
	}

	r.now = r.now.Add(alertEndedWindow - alertReadWindow)
	r.pass()
	if got := r.marked(); got != 1 || r.record.readCount() != 0 {
		t.Errorf("%d marks and %d reads near the end of the read, want it judged unknown unread", got,
			r.record.readCount())
	}
}

// A WAIT GITHUB NEVER GAVE A QUEUE TIME FOR IS RECORDED AS UNKNOWN once none can be
// expected, so a queue time that arrives later cannot judge it, out of order.
func TestAWaitNeverKnownIsRecordedUnknown(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.On = config.AlertConditions{WaitedLongerThan: "10m"} }))
	r.now = r.now.Add(5 * time.Minute)
	r.seed(jobRow{lease: "unwaited", result: "succeeded", ran: time.Second, age: time.Second})

	r.now = r.now.Add(alertReadWindow + time.Minute)
	r.pass()
	if got := r.marked(); got != 1 {
		t.Errorf("%d marks, want the unknown wait recorded", got)
	}
}

// A RUNNING JOB WAITS FOR A PASS THAT READ EVERY ENDED JOB: one not yet read could come
// before it and break the streak it would add to.
func TestARunningJobWaitsForTheEndedJobsToBeRead(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.On = config.AlertConditions{RunningLongerThan: "1m"} }))
	r.sweep.rows = 1
	r.now = r.now.Add(5 * time.Minute)
	r.seed(jobRow{lease: "ended-a", result: "succeeded", ran: time.Second, age: 4 * time.Minute, name: "a"})
	r.seed(jobRow{lease: "ended-b", result: "succeeded", ran: time.Second, age: 3 * time.Minute, name: "b"})
	r.seed(jobRow{lease: "running", age: 2 * time.Minute})
	r.now = r.now.Add(5 * time.Minute)

	r.pass()
	if got := r.owedLeases(); len(got) != 0 {
		t.Fatalf("owed %v with an ended page still full", got)
	}

	for range 3 {
		r.pass()
	}
	if got := r.owedLeases(); len(got) != 1 || got[0] != "running:running-long" {
		t.Errorf("owed = %v, want the running job once the ended jobs were read", got)
	}
}

// THE CURSOR PASSES ONLY A RESULT OLD ENOUGH that no earlier one can still land: a
// result stamped before another's but committed after it is still read.
func TestTheCursorWaitsForResultsStillLanding(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.Cooldown = "0s" }))
	r.now = r.now.Add(5 * time.Minute)

	r.seed(jobRow{lease: "newer", result: "failed", ran: 0, age: 30 * time.Second, name: "newer"})
	r.pass()

	r.seed(jobRow{lease: "older", result: "failed", ran: 0, age: 40 * time.Second, name: "older"})
	r.pass()

	if got := r.marked(); got != 2 {
		t.Errorf("%d marks, want the result that landed late judged too", got)
	}
}

// A RUNNING JOB FIRES AS IT PASSES THE DURATION, while it runs, and once.
func TestARunningJobFiresWhileItRuns(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.On = config.AlertConditions{RunningLongerThan: "1h"} }))
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

	r := started(t, atRule(func(c *config.AlertRule) { c.On = config.AlertConditions{WaitedLongerThan: "10m"} }))
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

// A RULE THAT NAMES NEITHER ASKS GITHUB NOTHING. A rule naming a branch takes it from
// the ledger where the queue-time sweep recorded it, reads each run once where it did
// not, and counts a job on another branch as not its job, which breaks no streak.
func TestABranchNarrowsTheRuleFromTheLedgerOrOneReadARun(t *testing.T) {
	t.Parallel()

	plain := started(t, atRule())
	plain.seed(jobRow{lease: "any", result: "failed", ran: time.Second, age: time.Second})
	plain.pass()
	if got := plain.record.readCount(); got != 0 {
		t.Errorf("a rule naming neither read GitHub %d times", got)
	}

	branchRule := atRule(func(c *config.AlertRule) {
		c.Match.Branch = "main"
		c.InARow = 2
		c.Cooldown = "0s"
	})

	ledger := started(t, branchRule)
	ledger.now = ledger.now.Add(5 * time.Minute)
	ledger.seed(jobRow{lease: "recorded", result: "failed", ran: time.Second, age: 2 * time.Minute, branch: "main"})
	ledger.seed(jobRow{lease: "also", result: "failed", ran: time.Second, age: time.Minute, branch: "main"})
	ledger.now = ledger.now.Add(5 * time.Minute)
	ledger.pass()
	if got := ledger.record.readCount(); got != 0 {
		t.Errorf("a branch the ledger holds was read from GitHub %d times", got)
	}
	if got := ledger.owedLeases(); len(got) != 1 || got[0] != "also:failed" {
		t.Errorf("owed = %v", got)
	}

	r := started(t, branchRule)
	r.record.branch = "main"
	r.now = r.now.Add(5 * time.Minute)
	r.seed(jobRow{lease: "one", result: "failed", ran: time.Second, age: 3 * time.Minute})
	r.seed(jobRow{lease: "elsewhere", result: "failed", ran: time.Second, age: 2 * time.Minute, branch: "feature"})
	r.seed(jobRow{lease: "two", result: "failed", ran: time.Second, age: time.Minute})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	if got := r.record.readCount(); got != 1 {
		t.Errorf("read GitHub %d times for one run", got)
	}
	if len(r.record.asked) != 1 || r.record.asked[0] != "run acme/api 31" {
		t.Errorf("asked %v, want the job's own run", r.record.asked)
	}
	// THE JOB ON ANOTHER BRANCH BETWEEN THEM BROKE NOTHING.
	if got := r.owedLeases(); len(got) != 1 || got[0] != "two:failed" {
		t.Errorf("owed = %v, want the second on main in a row", got)
	}
}

// A STEP THE RULE NAMES HAS TO BE THE ONE THAT FAILED, read from one read of the run,
// for the job's own runner. Only a hit asks, and a job GitHub has not concluded waits:
// a step still running has no conclusion yet.
func TestAStepNarrowsAHitAndOnlyAConcludedJobDecides(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.Match.Step = "Deploy*" }))

	r.seed(jobRow{lease: "fine", result: "succeeded", ran: time.Second, age: time.Second})
	r.pass()
	if got := r.record.readCount(); got != 0 {
		t.Errorf("a job that succeeded read its steps %d times", got)
	}

	r.record.steps = map[string][]JobStep{
		provider.InstanceName("build"):  {{Name: "Build", Conclusion: "failure"}},
		provider.InstanceName("deploy"): {{Name: "Deploy to eu", Conclusion: ""}},
		"billet-another-lease":          {{Name: "Deploy to eu", Conclusion: "failure"}},
	}
	r.record.unfinished = true
	r.seed(jobRow{lease: "build", result: "failed", ran: time.Second, age: time.Second, name: "other", run: 41})
	r.seed(jobRow{lease: "deploy", result: "failed", ran: time.Second, age: time.Second, name: "third", run: 42})
	r.pass()
	if got := r.owedLeases(); len(got) != 0 || r.marked() != 1 {
		t.Fatalf("owed %v, %d marks, want nothing decided while GitHub has not concluded the jobs", got, r.marked())
	}

	// CONCLUDED: the job that failed in another step is a miss, and the one that
	// failed in the step the rule names fires.
	r.record.unfinished = false
	r.record.steps[provider.InstanceName("deploy")] = []JobStep{{Name: "Deploy to eu", Conclusion: "failure"}}
	r.now = r.now.Add(time.Minute)
	r.pass()
	if got := r.owedLeases(); len(got) != 1 || got[0] != "deploy:failed" {
		t.Errorf("owed = %v, want the failure in the step the rule names", got)
	}
	for _, want := range []string{"jobs acme/api 41", "jobs acme/api 42"} {
		found := false
		for _, asked := range r.record.asked {
			found = found || asked == want
		}
		if !found {
			t.Errorf("never asked %q; asked %v", want, r.record.asked)
		}
	}
}

// COULD NOT TELL WAITS, AND THEN SAYS SO. A read that fails is not a miss and not a
// hit: the job is asked about again, and an hour after GitHub was first asked it is
// judged unknown, which moves no streak and sends nothing.
func TestAReadThatFailsWaitsThenIsJudgedUnknownAndBreaksNoStreak(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) {
		c.Match.Branch = "main"
		c.InARow = 2
		c.Cooldown = "0s"
	}))

	r.seed(jobRow{lease: "before", result: "failed", ran: time.Second, age: time.Second, branch: "main"})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	r.record.err = errors.New("HTTP 403: Resource not accessible by integration")
	r.seed(jobRow{lease: "unread", result: "failed", ran: time.Second, age: time.Second})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()
	reads := r.record.readCount()
	if reads == 0 {
		t.Fatal("GitHub was not asked")
	}
	if got := r.marked(); got != 1 {
		t.Fatalf("a read that failed was recorded: %d marks", got)
	}

	r.pass()
	if got := r.record.readCount(); got <= reads {
		t.Errorf("the job was not asked about again: %d reads", got)
	}

	r.now = r.now.Add(alertReadWindow + time.Minute)
	r.pass()
	if got := r.marked(); got != 2 {
		t.Fatalf("past the window, %d marks, want the unknown recorded", got)
	}
	if due := r.owed(); len(due) != 0 {
		t.Errorf("an unknown job is owed: %+v", due)
	}

	// UNKNOWN IS NOT A MISS: the next failure on main is the second in a row.
	r.record.err = nil
	r.seed(jobRow{lease: "after", result: "failed", ran: time.Second, age: time.Second, branch: "main"})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()
	if got := r.owedLeases(); len(got) != 1 || got[0] != "after:failed" {
		t.Errorf("owed = %v, want the unknown to have broken nothing", got)
	}
}

// A PASS WITH NO READ LEFT ASKED NOTHING, AND COSTS A JOB NO TIME: its window starts
// with the first request actually made for it, however long it waited for one.
func TestTheReadWindowStartsWithTheFirstRequest(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.Match.Branch = "main" }))
	r.record.branch = "main"
	r.record.remaining, r.record.limit = 100, 5000

	r.seed(jobRow{lease: "waiting", result: "failed", ran: time.Second, age: time.Second})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()

	// WELL PAST THE WINDOW, STILL WITH NO READ LEFT: nothing is decided.
	r.now = r.now.Add(3 * alertReadWindow)
	r.pass()
	if got := r.marked(); got != 0 || r.record.readCount() != 0 {
		t.Fatalf("%d marks and %d reads with no read left, want neither", got, r.record.readCount())
	}

	// THE BUDGET COMES BACK: the job is read and judged, not written off.
	r.record.remaining = 5000
	r.pass()
	if got := r.owedLeases(); len(got) != 1 || got[0] != "waiting:failed" {
		t.Errorf("owed = %v, want the job judged on its read", got)
	}
}

// THE RESERVE IS LEFT, AND EACH INSTALLATION HAS ITS SHARE OF A PASS, because this
// decides nothing about a runner; one read of a run serves every job of it.
func TestAlertReadsLeaveTheReserveAndTakeTheirShare(t *testing.T) {
	t.Parallel()

	stepRule := atRule(func(c *config.AlertRule) { c.Match.Step = "Deploy*" })

	r := started(t, stepRule)
	r.record.remaining, r.record.limit = 100, 5000
	r.seed(jobRow{lease: "held", result: "failed", ran: time.Second, age: time.Second})
	r.pass()
	if got := r.record.readCount(); got != 0 {
		t.Errorf("read %d times on an installation down to its reserve", got)
	}

	r = started(t, stepRule)
	for i := range alertRunsPerInstallation + 2 {
		r.seed(jobRow{lease: fmt.Sprintf("job-%d", i), result: "failed", ran: time.Second, age: time.Second,
			name: fmt.Sprintf("job-%d", i), run: int64(100 + i)})
	}
	r.pass()
	if got := r.record.readCount(); got != alertRunsPerInstallation {
		t.Errorf("%d reads in one pass, want the share's %d", got, alertRunsPerInstallation)
	}

	// A READ IS ADMITTED BY WHAT IT CAN COST: a thousand and five left of five thousand
	// leaves a one-page read above the reserve and an eight-page one below it.
	pages := started(t, stepRule)
	pages.record.remaining, pages.record.limit = 1005, 5000
	pages.seed(jobRow{lease: "steps", result: "failed", ran: time.Second, age: time.Second})
	pages.pass()
	if got := pages.record.readCount(); got != 0 {
		t.Errorf("an eight-page read was made %d times into the reserve", got)
	}

	branch := started(t, atRule(func(c *config.AlertRule) { c.Match.Branch = "main" }))
	branch.record.remaining, branch.record.limit, branch.record.branch = 1005, 5000, "main"
	branch.seed(jobRow{lease: "branch", result: "failed", ran: time.Second, age: time.Second})
	branch.pass()
	if got := branch.record.readCount(); got != 1 {
		t.Errorf("a one-page read above the reserve was made %d times, want 1", got)
	}

	same := started(t, stepRule)
	for i := range alertRunsPerInstallation + 2 {
		same.seed(jobRow{lease: fmt.Sprintf("job-%d", i), result: "failed", ran: time.Second, age: time.Second,
			name: fmt.Sprintf("job-%d", i)})
	}
	same.pass()
	if got := same.record.readCount(); got != 1 {
		t.Errorf("%d reads for one run's jobs, want one", got)
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

		// A SECOND PAST THE WAIT, which is counted from when the try ended.
		r.now = r.now.Add(wait + time.Second)
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

// A TRY'S WAIT IS COUNTED FROM WHEN IT ENDED, not from when the look at the queue
// began: a slow look does not make the next try come early.
func TestARetryWaitsFromWhenItsTryEnded(t *testing.T) {
	t.Parallel()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(slow.Close)

	r := newAlertStack(t, atRule())
	dest := alerts.NewSender("hook", alerts.URL(slow.URL))
	if _, err := r.a.RecordAlertJudgement(t.Context(), &alloc.AlertJudgement{Rule: "deploys", LeaseID: "job", Condition: "failed",
		Key: "k", Outcome: alloc.AlertHit, InARow: 1, Notices: []alloc.AlertNotice{{Destination: "hook", Payload: []byte("{}")}}}); err != nil {
		t.Fatal(err)
	}

	r.s.deliverAlertsOnce(t.Context(), "hook", dest, r.now)

	due, err := r.a.DueAlertNotices(t.Context(), "hook", r.now.Add(alertRetryFirst+100*time.Millisecond), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("due %d notices a wait after the look began, before a wait after the try ended", len(due))
	}
}

// AN ANSWER TRYING AGAIN CANNOT HELP ENDS AT ONCE, and is reported.
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

	// A DAY APART, so a retry it was wrongly given would have come round.
	for day := 1; day <= 3; day++ {
		r.s.deliverAlertsOnce(t.Context(), "hook", dest, r.now.Add(time.Duration(day)*24*time.Hour))
	}
	if got := tries.Load(); got != 1 {
		t.Errorf("%d requests, want 1", got)
	}

	report, err := r.a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Failure, "403") {
		t.Errorf("failures = %+v, want the 403 given up on", report.Failures)
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
		r.s.deliverAlertsOnce(ctx, "stuck", alerts.NewSender("stuck", alerts.URL(hung.URL)), r.now)
	}()

	<-entered

	r.deliver()
	select {
	case body := <-r.bodies:
		if body != `{"to":"hook"}` {
			t.Errorf("the other destination got %q", body)
		}
	default:
		t.Error("the other destination got nothing while one hung")
	}

	// AND A STOP ENDS THE HUNG REQUEST AT ONCE, well inside the sender's own bound,
	// with its try uncounted: it was not an answer.
	stopped := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stop did not end a hung delivery")
	}
	if took := time.Since(stopped); took > 3*time.Second {
		t.Errorf("a stop took %s to end a hung delivery", took)
	}

	due, err := r.a.DueAlertNotices(t.Context(), "stuck", r.now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Attempts != 0 {
		t.Errorf("after a stop = %+v, want it still owed with no try counted", due)
	}
}

// WHAT IS OWED TO A DESTINATION THE CONFIG NO LONGER NAMES IS GIVEN UP ON, with why,
// and what is owed to one it does name is left to its loop.
func TestNoticesOwedToARemovedDestinationAreGivenUpOn(t *testing.T) {
	t.Parallel()

	r := newAlertStack(t, atRule())
	if _, err := r.a.RecordAlertJudgement(t.Context(), &alloc.AlertJudgement{Rule: "deploys", LeaseID: "job", Condition: "failed",
		Key: "k", Outcome: alloc.AlertHit, InARow: 1, Notices: []alloc.AlertNotice{
			{Destination: "removed", Payload: []byte("{}")}, {Destination: "hook", Payload: []byte("{}")},
		}}); err != nil {
		t.Fatal(err)
	}

	r.s.settleRemovedDestinations(t.Context())

	report, err := r.a.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 1 || report.Failures[0].Destination != "removed" ||
		!strings.Contains(report.Failures[0].Failure, "no longer in server.alerts") {
		t.Errorf("failures = %+v", report.Failures)
	}
	if due := r.owed(); len(due) != 1 {
		t.Errorf("the configured destination is owed %d, want its one", len(due))
	}
}

// THE PASS'S MEMORY OF WHAT IT JUDGED IS THE LEDGER'S, read again at each prune, so it
// does not grow with the process's life: a judgement the ledger has let go is let go
// here too.
func TestTheJudgedSetIsReadAgainAtEachPrune(t *testing.T) {
	t.Parallel()

	r := started(t, atRule(func(c *config.AlertRule) { c.Cooldown = "0s" }))
	r.seed(jobRow{lease: "one", result: "failed", ran: time.Second, age: time.Second})
	r.now = r.now.Add(5 * time.Minute)
	r.pass()
	if len(r.sweep.judged) != 1 {
		t.Fatalf("judged = %v", r.sweep.judged)
	}

	r.sweep.judged[alloc.AlertMark{Rule: "deploys", LeaseID: "long-gone", Condition: "failed"}] = true
	r.now = r.now.Add(alertPruneEvery + time.Minute)
	r.pass()

	if _, kept := r.sweep.judged[alloc.AlertMark{Rule: "deploys", LeaseID: "long-gone", Condition: "failed"}]; kept {
		t.Error("a judgement the ledger does not hold was kept")
	}
	if !r.sweep.judged[alloc.AlertMark{Rule: "deploys", LeaseID: "one", Condition: "failed"}] {
		t.Error("a judgement the ledger holds was forgotten")
	}
}

// THE OPTION CARRIES WHAT THERE IS: nothing for an empty set, and destinations with no
// rule, which still deliver what they are owed.
func TestAlertsCarryRulesOrDestinations(t *testing.T) {
	t.Parallel()

	if s := New(nil, nil, nil, "owner", nil, WithAlerts(&Alerts{})); s.alerts != nil {
		t.Error("an empty set was kept")
	}
	if s := New(nil, nil, nil, "owner", nil, WithAlerts(nil)); s.alerts != nil {
		t.Error("nil was kept")
	}

	only := &Alerts{Destinations: map[string]AlertDestination{"hook": {Sender: alerts.NewSender("hook", "https://x")}}}
	if s := New(nil, nil, nil, "owner", nil, WithAlerts(only)); s.alerts != only {
		t.Error("destinations with no rule were dropped, and what they are owed with them")
	}
}
