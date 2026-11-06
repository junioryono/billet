package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

const (
	rerunTier  = "billet-4vcpu-a"
	rerunTierB = "billet-4vcpu-b"
)

// rerunGitHub answers what a test says GitHub would, and keeps every request with
// the repository it named.
type rerunGitHub struct {
	*fakeProvisioner

	mu        sync.Mutex
	attempt   RunAttempt
	readErr   error
	askErr    error
	reads     int
	readRuns  []int64
	failedAsk []int64
	jobAsk    []int64
	asks      []string
	// remaining and limit are the installation's REST budget, when limit is set, and
	// reset when it is refilled, an hour from the wall clock when zero.
	remaining, limit int64
	reset            time.Time
	// onAsk runs inside a request, for a run whose state a request changes.
	onAsk func(*rerunGitHub)
}

func (g *rerunGitHub) LatestAttempt(_ context.Context, owner, repository string, runID int64) (RunAttempt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.reads++
	g.readRuns = append(g.readRuns, runID)
	if owner != "acme" || repository != "api" {
		return RunAttempt{}, fmt.Errorf("read %s/%s", owner, repository)
	}

	return g.attempt, g.readErr
}

func (g *rerunGitHub) RerunFailedJobs(_ context.Context, owner, repository string, runID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.failedAsk = append(g.failedAsk, runID)
	g.asks = append(g.asks, owner+"/"+repository)
	if g.onAsk != nil {
		g.onAsk(g)
	}

	return g.askErr
}

func (g *rerunGitHub) RerunJob(_ context.Context, owner, repository string, jobID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.jobAsk = append(g.jobAsk, jobID)
	g.asks = append(g.asks, owner+"/"+repository)
	if g.onAsk != nil {
		g.onAsk(g)
	}

	return g.askErr
}

func (g *rerunGitHub) RESTHeadroom() (int64, int64, time.Time, bool) {
	reset := g.reset
	if reset.IsZero() {
		reset = time.Now().Add(time.Hour)
	}

	return g.remaining, g.limit, reset, g.limit > 0
}

func (g *rerunGitHub) asked() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.failedAsk) + len(g.jobAsk)
}

// rerunStack is a real allocator over a real ledger, seeded with red jobs, and the
// loop's own pass driven at a clock the test holds. Its tiers asked for re-runs a
// few days before the clock, so the jobs a test seeds are theirs.
type rerunStack struct {
	t       *testing.T
	s       *Server
	a       *alloc.Allocator
	db      *state.DB
	github  *rerunGitHub
	targets map[string]Target
	limits  map[string]int64
	sweep   *rerunSweep
	now     time.Time
	ledger  *time.Time
	// onClock, when set, runs each time the allocator reads its clock, which is inside
	// every ledger write: what moves time on in the middle of a decision.
	onClock *func()
}

func newRerunStack(t *testing.T) *rerunStack {
	t.Helper()

	db := openState(t)
	tiers := []config.Tier{tier(rerunTier), tier(rerunTierB)}
	now := time.Now()
	ledger := now.Add(-72 * time.Hour)
	onClock := new(func())

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithClock(func() time.Time {
			if *onClock != nil {
				(*onClock)()
			}

			return ledger
		}))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	github := &rerunGitHub{fakeProvisioner: &fakeProvisioner{}, attempt: RunAttempt{Completed: true, Attempt: 1}}
	target := Target{Config: config.GitHubTarget{Name: "default", Org: "acme", InstallationID: 7}, Provisioner: github}

	r := &rerunStack{t: t, s: New(a, nil, tiers, "owner", nil), a: a, db: db, github: github,
		targets: map[string]Target{rerunTier: target, rerunTierB: target},
		limits:  map[string]int64{rerunTier: 1}, sweep: newRerunSweep(), now: now, ledger: &ledger,
		onClock: onClock}

	asked, err := a.AskReruns(t.Context(), []string{rerunTier, rerunTierB})
	if err != nil {
		t.Fatalf("AskReruns: %v", err)
	}
	r.sweep.asked = asked
	ledger = now

	return r
}

// job seeds one red job on rerunTier whose lease a cause on the list disrupted, its
// result age before the clock.
func (r *rerunStack) job(lease string, run int64, age time.Duration) {
	r.t.Helper()

	r.jobOn(rerunTier, lease, run, age)
}

func (r *rerunStack) jobOn(tier, lease string, run int64, age time.Duration) {
	r.t.Helper()

	at := stamp(r.now.Add(-age))
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, started_at, repo, github_job_id, github_run_id,
			 disruption, disrupted_at, result, result_at)
			VALUES ($1, $2, $3, $3, 'acme/api', $4, $5, 'reclaimed', $3, 'failed', $3)`,
			lease, tier, at, "guid-"+lease, run)

		return err
	}); err != nil {
		r.t.Fatalf("seed %s: %v", lease, err)
	}
}

// listed is a failed job of the attempt, on the runner a lease's name gives.
func listed(lease string, id int64, lost bool) AttemptJob {
	return AttemptJob{ID: id, RunnerName: provider.InstanceName(lease), RunnerLost: lost, Failed: true}
}

// lists is what GitHub says the run is now: completed on attempt, with these jobs,
// each of the attempt unless it says otherwise.
func (r *rerunStack) lists(attempt int64, jobs ...AttemptJob) {
	for i := range jobs {
		if jobs[i].Attempt == 0 {
			jobs[i].Attempt = attempt
		}
	}

	r.github.mu.Lock()
	r.github.attempt = RunAttempt{Completed: true, Attempt: attempt, Jobs: jobs}
	r.github.mu.Unlock()
}

func (r *rerunStack) pass() { r.s.rerunOnce(r.t.Context(), r.targets, r.limits, r.sweep, r.now) }

// after moves the test's clock, and the ledger's with it.
func (r *rerunStack) after(d time.Duration) *rerunStack {
	r.now = r.now.Add(d)
	*r.ledger = r.now

	return r
}

func (r *rerunStack) decision(lease string) alloc.RerunDecision {
	r.t.Helper()

	rec, err := r.a.Job(r.t.Context(), lease)
	if err != nil {
		r.t.Fatalf("read %s: %v", lease, err)
	}

	return rec.RerunDecision
}

func (r *rerunStack) request(attempt int64) (settled string, tries int64, detail string) {
	r.t.Helper()

	if err := r.db.Reader().QueryRowContext(r.t.Context(),
		`SELECT state, tries, detail FROM job_reruns WHERE repo = 'acme/api' AND run_id = 31 AND run_attempt = $1`,
		attempt).Scan(&settled, &tries, &detail); err != nil {
		r.t.Fatalf("read the request of attempt %d: %v", attempt, err)
	}

	return settled, tries, detail
}

func (r *rerunStack) reads() int {
	r.github.mu.Lock()
	defer r.github.mu.Unlock()

	return r.github.reads
}

// A JOB BILLET PROVED IT ENDED IS RE-RUN ONCE, in its own repository. The run's only
// failed job is proved, so the whole request is exact; the job carries the outcome,
// and the next pass, with nothing left undecided, asks nothing.
func TestAProvedJobIsRerunOnce(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.lists(1, listed("lost", 77, true))

	r.pass()

	if len(r.github.failedAsk) != 1 || r.github.failedAsk[0] != 31 || len(r.github.jobAsk) != 0 {
		t.Fatalf("asked failed-jobs %v, jobs %v, want run 31's failed jobs once", r.github.failedAsk, r.github.jobAsk)
	}
	if r.github.asks[0] != "acme/api" {
		t.Errorf("asked about %q", r.github.asks[0])
	}
	if got := r.decision("lost"); got != alloc.RerunRequested {
		t.Errorf("decision = %q", got)
	}
	if got, tries, _ := r.request(1); got != "requested" || tries != 1 {
		t.Errorf("request = %s, %d tries", got, tries)
	}

	r.after(time.Hour).pass()

	if got := r.github.asked(); got != 1 {
		t.Errorf("a second pass asked again: %d requests", got)
	}
}

// A JOB THAT FAILED ON ITS OWN IS LEFT ALONE, even on a lease with a disruption: the
// disruption is what made it a candidate, and GitHub's record says it was not the cause.
func TestAJobWhoseRecordShowsItsOwnFailureIsLeftAlone(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("own", 31, 10*time.Minute)
	r.lists(1, listed("own", 77, false))

	r.pass()

	if got := r.github.asked(); got != 0 {
		t.Errorf("a job that failed on its own was re-run: %d requests", got)
	}
	if got := r.decision("own"); got != alloc.RerunUnproven {
		t.Errorf("decision = %q", got)
	}
}

// THE LIMIT HOLDS ACROSS ATTEMPTS. Attempt 1 was re-run; attempt 2 lost its runner
// too, and a tier allowing one re-run leaves it red and says why. A tier allowing two
// asks again, so the refusal is the limit's.
func TestTheLimitHoldsAcrossAttempts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		limit    int64
		requests int
		second   alloc.RerunDecision
	}{
		{1, 1, alloc.RerunLimit},
		{2, 2, alloc.RerunRequested},
	} {
		t.Run(fmt.Sprint(tc.limit), func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.limits[rerunTier] = tc.limit
			r.job("first", 31, 10*time.Minute)
			r.lists(1, listed("first", 77, true))
			r.pass()

			r.job("second", 31, 10*time.Minute)
			r.lists(2, listed("second", 88, true))
			r.after(time.Hour).pass()

			if got := r.github.asked(); got != tc.requests {
				t.Errorf("%d requests, want %d", got, tc.requests)
			}
			if got := r.decision("second"); got != tc.second {
				t.Errorf("decision = %q, want %q", got, tc.second)
			}
		})
	}
}

// THE LIMIT IS THE SMALLEST OF THE TIERS WHOSE JOBS A REQUEST WOULD RUN AGAIN. A run
// already re-run once has room on a tier allowing two and none on one allowing one.
func TestTheLimitIsThatOfTheJobsRerun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		jobs     map[string]string
		requests int
		want     alloc.RerunDecision
	}{
		{"a job of the tier allowing two", map[string]string{"b": rerunTierB}, 2, alloc.RerunRequested},
		{"jobs of both tiers", map[string]string{"a": rerunTier, "b": rerunTierB}, 1, alloc.RerunLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.limits[rerunTierB] = 2
			r.job("first", 31, 10*time.Minute)
			r.lists(1, listed("first", 77, true))
			r.pass()

			var jobs []AttemptJob
			id := int64(100)
			for lease, tier := range tc.jobs {
				r.jobOn(tier, lease, 31, 10*time.Minute)
				jobs = append(jobs, listed(lease, id, true))
				id++
			}
			r.lists(2, jobs...)
			r.after(time.Hour).pass()

			if got := r.github.asked(); got != tc.requests {
				t.Errorf("%d requests, want %d", got, tc.requests)
			}
			for lease := range tc.jobs {
				if got := r.decision(lease); got != tc.want {
					t.Errorf("%s = %q, want %q", lease, got, tc.want)
				}
			}
		})
	}
}

// THE REQUEST IS EXACT, or there is none. `rerun-failed-jobs` re-runs every failed job
// of the run, so a job that failed on its own beside the proved one rules it out.
func TestTheRequestRunsExactlyTheProvedJobs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		jobs      []AttemptJob
		wantRun   int
		wantJob   []int64
		decisions map[string]alloc.RerunDecision
	}{
		{
			name: "every failed job proved is one request for them all",
			jobs: []AttemptJob{
				listed("one", 77, true), listed("two", 78, true),
				{ID: 90, RunnerName: "hosted", Failed: false},
			},
			wantRun:   1,
			decisions: map[string]alloc.RerunDecision{"one": alloc.RerunRequested, "two": alloc.RerunRequested},
		},
		{
			name: "one proved beside a job that failed on its own is that job alone",
			jobs: []AttemptJob{
				listed("one", 77, true), listed("two", 78, false),
				{ID: 91, RunnerName: "hosted", Failed: true},
			},
			wantJob:   []int64{77},
			decisions: map[string]alloc.RerunDecision{"one": alloc.RerunRequested, "two": alloc.RerunUnproven},
		},
		{
			name: "two proved beside one that failed on its own is left for a person",
			jobs: []AttemptJob{
				listed("one", 77, true), listed("two", 78, true),
				{ID: 91, RunnerName: "hosted", Failed: true},
			},
			decisions: map[string]alloc.RerunDecision{"one": alloc.RerunMixed, "two": alloc.RerunMixed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("one", 31, 10*time.Minute)
			r.job("two", 31, 10*time.Minute)
			r.lists(1, tc.jobs...)

			r.pass()

			if len(r.github.failedAsk) != tc.wantRun {
				t.Errorf("failed-jobs requests = %v, want %d", r.github.failedAsk, tc.wantRun)
			}
			if fmt.Sprint(r.github.jobAsk) != fmt.Sprint(append([]int64{}, tc.wantJob...)) {
				t.Errorf("job requests = %v, want %v", r.github.jobAsk, tc.wantJob)
			}
			for lease, want := range tc.decisions {
				if got := r.decision(lease); got != want {
					t.Errorf("%s = %q, want %q", lease, got, want)
				}
			}
		})
	}
}

// A RUN WITH JOBS LEFT IS NOT DECIDED, and is read again on the unfinished pace rather
// than on every pass. GitHub refuses a re-run of one anyway, and its sibling jobs may
// yet fail.
func TestARunNotFinishedWaits(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.github.attempt = RunAttempt{Completed: false, Attempt: 1}

	r.pass()
	if got := r.decision("lost"); got != "" {
		t.Errorf("decided %q while the run has jobs left", got)
	}
	if r.github.asked() != 0 {
		t.Error("asked about a run with jobs left")
	}

	r.after(rerunUnfinishedEvery - time.Second).pass()
	if r.reads() != 1 {
		t.Errorf("read %d times inside its wait", r.reads())
	}

	r.after(2 * time.Second)
	r.lists(1, listed("lost", 77, true))
	r.pass()
	if r.reads() != 2 || r.github.asked() != 1 {
		t.Errorf("reads %d, requests %d, want it decided once the run finished", r.reads(), r.github.asked())
	}
}

// THE RUN MOVED PAST THE JOB WITHOUT BILLET: a person re-ran it, and billet does not
// add an attempt of its own on top of theirs. It moved past when its latest attempt
// does not list the job, or lists it from an earlier attempt.
func TestARunThatMovedOnIsSuperseded(t *testing.T) {
	t.Parallel()

	for name, jobs := range map[string][]AttemptJob{
		"not listed":                   {listed("another-lease", 88, true)},
		"listed from an older attempt": {{ID: 77, RunnerName: provider.InstanceName("lost"), Attempt: 1, RunnerLost: true, Failed: true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("lost", 31, 10*time.Minute)
			r.lists(2, jobs...)

			r.pass()

			if got := r.decision("lost"); got != alloc.RerunSuperseded {
				t.Errorf("decision = %q", got)
			}
			if r.github.asked() != 0 {
				t.Error("asked about a run a person already re-ran")
			}
		})
	}
}

// NOTHING CAN HAVE SUPERSEDED THE FIRST ATTEMPT, so a job its list does not show is
// could-not-tell: nothing is decided, and it is read again after a backoff. Neither is
// anything decided from a list naming one runner twice.
func TestAListThatCannotBeTrustedDecidesNothing(t *testing.T) {
	t.Parallel()

	for name, jobs := range map[string][]AttemptJob{
		"the first attempt without the job": {listed("another-lease", 88, true)},
		"a runner named twice":              {listed("lost", 77, true), listed("lost", 78, false)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("lost", 31, 10*time.Minute)
			r.lists(1, jobs...)

			r.pass()
			if got := r.decision("lost"); got != "" {
				t.Errorf("decided %q", got)
			}
			if r.github.asked() != 0 {
				t.Error("asked")
			}

			r.after(rerunRetryFirst - time.Second).pass()
			if r.reads() != 1 {
				t.Errorf("read %d times inside the backoff", r.reads())
			}
		})
	}
}

// RESULTS ARRIVE ONE BY ONE, SO A RUN WAITS FOR THE REST. Deciding on the first would
// read its sibling's lost job, not yet a candidate, as one that failed on its own.
func TestARunWithAResultStillSettlingWaits(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("older", 31, time.Hour)
	r.job("newest", 31, rerunSettle/2)
	r.lists(1, listed("older", 77, true), listed("newest", 78, true))

	r.pass()
	if r.reads() != 0 {
		t.Fatalf("read a run whose newest result is still settling: %d", r.reads())
	}

	r.after(rerunSettle).pass()
	if len(r.github.failedAsk) != 1 {
		t.Errorf("requests = %v, want one covering both", r.github.failedAsk)
	}
	if r.decision("older") != alloc.RerunRequested || r.decision("newest") != alloc.RerunRequested {
		t.Errorf("decisions = %q, %q", r.decision("older"), r.decision("newest"))
	}
}

// A SIBLING BILLET HAS NOT HEARD FROM HOLDS THE RUN: its completion may be on its way,
// and it may be one more job billet proves. Another deployment's runner does not.
func TestASiblingNotYetHeardFromHoldsTheRun(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, started_at, repo)
			VALUES ('unheard', $1, '', $2, 'acme/api')`, rerunTier, stamp(r.now))

		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.lists(1, listed("lost", 77, true), listed("unheard", 78, true), listed("another-deployments", 79, false))

	r.pass()
	if r.github.asked() != 0 || r.decision("lost") != "" {
		t.Fatalf("decided with a sibling unheard: %d requests, %q", r.github.asked(), r.decision("lost"))
	}

	if err := r.a.RecordJobResult(t.Context(), "unheard", "succeeded", 31); err != nil {
		t.Fatal(err)
	}
	r.lists(1, listed("lost", 77, true), AttemptJob{ID: 78, RunnerName: provider.InstanceName("unheard")},
		listed("another-deployments", 79, false))
	r.after(rerunUnfinishedEvery + time.Second).pass()
	if got := r.decision("lost"); got != alloc.RerunRequested || len(r.github.jobAsk) != 1 || r.github.jobAsk[0] != 77 {
		t.Errorf("once heard: %q, job requests %v", got, r.github.jobAsk)
	}
}

// NOTHING WAITS FOREVER: a job past the window is decided as expired, with GitHub
// not asked, and a tier that did not opt in is not touched at all.
func TestAJobPastItsWindowExpiresAndATierNotOptedInIsLeftAlone(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("old", 31, rerunWindow+time.Minute)
	r.pass()
	if got := r.decision("old"); got != alloc.RerunExpired {
		t.Errorf("decision = %q", got)
	}
	if r.reads() != 0 {
		t.Errorf("GitHub was read %d times about an expired job", r.reads())
	}

	r = newRerunStack(t)
	delete(r.limits, rerunTier)
	r.job("not-opted-in", 31, 10*time.Minute)
	r.lists(1, listed("not-opted-in", 77, true))
	r.pass()
	if got := r.decision("not-opted-in"); got != "" {
		t.Errorf("a tier that did not opt in was decided: %q", got)
	}
	if r.reads() != 0 || r.github.asked() != 0 {
		t.Errorf("reads %d, requests %d about a tier that did not opt in", r.reads(), r.github.asked())
	}
}

// A JOB THAT WENT RED BEFORE ITS TIER ASKED IS NOT RE-RUN BECAUSE THE CONFIG CHANGED,
// through the loop's own first pass, which records when each tier started asking.
func TestAJobFromBeforeItsTierAskedIsLeftAlone(t *testing.T) {
	t.Parallel()

	// TIER A HAS ASKED FOR DAYS AND TIER B STARTS NOW, so the ledger is read from A's
	// start and only the per-tier rule keeps B's older job out.
	r := newRerunStack(t)
	r.limits[rerunTierB] = 1
	r.sweep = newRerunSweep()
	if _, err := r.a.AskReruns(t.Context(), []string{rerunTier}); err != nil {
		t.Fatal(err)
	}
	r.jobOn(rerunTierB, "before", 31, 10*time.Minute)
	r.lists(1, listed("before", 77, true))

	r.pass()

	if r.sweep.asked[rerunTierB].IsZero() || !r.sweep.asked[rerunTier].Before(r.sweep.asked[rerunTierB]) {
		t.Fatalf("the first pass recorded %v", r.sweep.asked)
	}
	if r.reads() != 0 || r.decision("before") != "" {
		t.Errorf("a job from before the tier asked: %d reads, %q", r.reads(), r.decision("before"))
	}

	r.jobOn(rerunTierB, "after", 32, -time.Minute)
	r.after(rerunSettle + time.Minute).pass()
	if r.reads() != 1 {
		t.Errorf("a job after the tier asked was read %d times", r.reads())
	}
}

// A FAILURE TO ASK IS REPORTED, AND NEVER RETRIED FOREVER. Five sends, each in doubt
// until the run is read again; the run, still on its attempt, is then given up on
// with GitHub's words, and nothing is sent after.
func TestAFailedRequestIsBoundedAndReported(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.lists(1, listed("lost", 77, true))
	r.github.askErr = errors.New("HTTP 500: GitHub had a bad moment")

	for try := 1; try <= 5; try++ {
		r.pass()
		if got := r.github.asked(); got != try {
			t.Fatalf("after pass %d: %d requests", try, got)
		}
		if got, tries, _ := r.request(1); got != "asking" || tries != int64(try) {
			t.Errorf("try %d: request = %s, %d tries", try, got, tries)
		}
		if got := r.decision("lost"); got != "" {
			t.Errorf("try %d: decided %q while the run may yet show it landed", try, got)
		}

		r.after(rerunRetryMost + time.Minute)
	}

	r.pass()

	got, tries, detail := r.request(1)
	if got != "failed" || tries != 5 || detail != "HTTP 500: GitHub had a bad moment" {
		t.Errorf("request = %s, %d tries, detail %q", got, tries, detail)
	}
	if got := r.decision("lost"); got != alloc.RerunFailed {
		t.Errorf("decision = %q", got)
	}

	r.after(rerunRetryMost + time.Minute).pass()
	if got := r.github.asked(); got != 5 {
		t.Errorf("%d requests after it settled", got)
	}

	// IT DOES NOT COUNT AGAINST THE LIMIT: it ran nothing.
	if used, err := r.a.RunReruns(t.Context(), "acme/api", 31); err != nil || used != 0 {
		t.Errorf("RunReruns = %d, %v", used, err)
	}
}

// AN ANSWER LOST ON THE WAY BACK IS NOT A REFUSAL, on the fifth try as on the first:
// the run read next says it landed, and it is counted.
func TestAnAnswerLostOnTheLastTryIsSettledByTheRun(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.lists(1, listed("lost", 77, true))
	r.github.askErr = errors.New("connection reset")

	for range 4 {
		r.pass()
		r.after(rerunRetryMost + time.Minute)
	}

	r.github.onAsk = func(g *rerunGitHub) { g.attempt = RunAttempt{Completed: false, Attempt: 1} }
	r.pass()
	if got, tries, _ := r.request(1); got != "asking" || tries != 5 {
		t.Fatalf("after the fifth = %s, %d tries", got, tries)
	}

	r.after(rerunRetryMost + time.Minute).pass()
	if got, _, _ := r.request(1); got != "requested" {
		t.Errorf("request = %s, want it settled as landed", got)
	}
	if got := r.decision("lost"); got != alloc.RerunRequested {
		t.Errorf("decision = %q", got)
	}
	if used, err := r.a.RunReruns(t.Context(), "acme/api", 31); err != nil || used != 1 {
		t.Errorf("RunReruns = %d, %v, want it counted", used, err)
	}
}

// A REFUSAL PAUSES THE WHOLE APP INSTALLATION and says what the App needs, and a
// throttle pauses it for less: neither spends a request per run on an answer that will
// not change, and each pause ends.
func TestARefusalOrAThrottlePausesTheInstallation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		wait time.Duration
	}{
		{"refused", fmt.Errorf("no actions: write: %w", ErrRerunRefused), rerunRefused},
		{"throttled", fmt.Errorf("slow down: %w", ErrRerunThrottled), rerunThrottled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("one", 31, 10*time.Minute)
			r.job("other", 32, 10*time.Minute)
			r.lists(1, listed("one", 77, true), listed("other", 78, true))
			r.github.askErr = tc.err

			r.pass()
			if got := r.github.asked(); got != 1 {
				t.Fatalf("%d requests in the pass GitHub refused, want it to stop at the first", got)
			}

			r.after(tc.wait - time.Second).pass()
			if got := r.github.asked(); got != 1 {
				t.Errorf("%d requests inside the pause", got)
			}

			r.after(2 * time.Second).pass()
			if got := r.reads(); got <= 1 {
				t.Errorf("the pause did not end: %d reads", got)
			}
		})
	}
}

// A REQUEST IN DOUBT IS SETTLED BY GITHUB'S RECORD OF THE RUN, NEVER BY ASKING AGAIN
// WHILE IT MAY HAVE LANDED. Measured: straight after a 201 the run is `queued` on the
// same attempt, so the status says it landed before the attempt does.
func TestARequestInDoubtIsSettledByTheRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		attempt RunAttempt
		asked   int
	}{
		{"the run is going, so it landed", RunAttempt{Completed: false, Attempt: 1}, 0},
		{"the run finished a later attempt, so it landed", RunAttempt{Completed: true, Attempt: 2}, 0},
		{"the run finished the same attempt, so it did not, and is sent again", RunAttempt{Completed: true, Attempt: 1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("lost", 31, 10*time.Minute)
			if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{
				Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs,
			}, []string{"lost"}); err != nil {
				t.Fatalf("BeginRerun: %v", err)
			}
			r.github.attempt = tc.attempt

			r.pass()

			if got := r.github.asked(); got != tc.asked {
				t.Errorf("%d requests, want %d", got, tc.asked)
			}
			if got, _, _ := r.request(1); got != "requested" {
				t.Errorf("request = %s", got)
			}
			if got := r.decision("lost"); got != alloc.RerunRequested {
				t.Errorf("decision = %q", got)
			}
		})
	}
}

// A RUN ON AN EARLIER ATTEMPT THAN ONE BILLET ASKED ABOUT IS NOT A RUN BILLET
// UNDERSTANDS: nothing is sent or settled.
func TestARunBehindItsRequestDecidesNothing(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 2,
		Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	r.lists(1)

	r.pass()

	if r.github.asked() != 0 {
		t.Error("asked about a run behind its request")
	}
	if got, tries, _ := r.request(2); got != "asking" || tries != 1 {
		t.Errorf("request = %s, %d tries", got, tries)
	}
}

// A REQUEST IN DOUBT IS SETTLED WHEN NOTHING ELSE BRINGS ITS RUN UP: its job is past
// its window, and its tier no longer asks. The job is not expired while it waits.
func TestARequestInDoubtIsSettledOnItsOwn(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("old", 31, rerunWindow+time.Hour)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
		Kind: alloc.RerunFailedJobs}, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	r.limits = map[string]int64{rerunTierB: 1}
	r.github.attempt = RunAttempt{Completed: false, Attempt: 1}

	r.pass()

	if got, _, _ := r.request(1); got != "requested" {
		t.Errorf("request = %s, want it settled", got)
	}
	if got := r.decision("old"); got != alloc.RerunRequested {
		t.Errorf("decision = %q, want the request's, never expired", got)
	}

	// AND WHILE ITS TIER STILL ASKS, the job past its window is not expired under its
	// request: it waits for the request's answer, which is its own.
	asking := newRerunStack(t)
	asking.job("old", 31, rerunWindow+time.Hour)
	if err := asking.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
		Kind: alloc.RerunFailedJobs}, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	asking.github.attempt = RunAttempt{Completed: false, Attempt: 1}

	asking.pass()

	if got := asking.decision("old"); got != alloc.RerunRequested {
		t.Errorf("with its tier asking, decision = %q, want the request's, never expired", got)
	}
}

// A LIMIT LOWERED WHILE A REQUEST WAS IN DOUBT HOLDS: one GitHub never heard is not
// sent again past it, and says why.
func TestALimitLoweredUnderARequestInDoubtHolds(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	first := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), first, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SettleRerun(t.Context(), first, alloc.RerunRequested, ""); err != nil {
		t.Fatal(err)
	}

	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 2,
		Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	r.lists(2)

	r.pass()

	if r.github.asked() != 0 {
		t.Error("sent past a lowered limit")
	}
	if got, _, detail := r.request(2); got != "failed" || !strings.Contains(detail, "retry.disrupted") {
		t.Errorf("request = %s, %q", got, detail)
	}
	if got := r.decision("lost"); got != alloc.RerunLimit {
		t.Errorf("decision = %q", got)
	}
}

// THE KIND A REQUEST WAS WRITTEN WITH IS THE KIND SENT AGAIN, its job with it, and a
// kind this binary does not know is never sent, and runs out of tries.
func TestARequestInDoubtIsSentAgainAsItWasWritten(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{
		Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunOneJob, JobID: 77,
	}, []string{"lost"}); err != nil {
		t.Fatalf("BeginRerun: %v", err)
	}
	r.lists(1)

	r.pass()

	if len(r.github.jobAsk) != 1 || r.github.jobAsk[0] != 77 || len(r.github.failedAsk) != 0 {
		t.Errorf("job requests %v, failed-jobs requests %v", r.github.jobAsk, r.github.failedAsk)
	}
	if got, tries, _ := r.request(1); got != "requested" || tries != 2 {
		t.Errorf("request = %s, %d tries", got, tries)
	}

	newer := newRerunStack(t)
	newer.job("lost", 31, 10*time.Minute)
	if err := newer.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
		Kind: "from-a-newer-billet"}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	newer.lists(1)
	for range 6 {
		newer.pass()
		newer.after(rerunRetryMost + time.Minute)
	}
	if newer.github.asked() != 0 {
		t.Errorf("a kind this binary does not know was sent %d times", newer.github.asked())
	}
	if got, _, _ := newer.request(1); got != "failed" {
		t.Errorf("request = %s, want it given up", got)
	}
}

// TRIES USED UP BEFORE A RESTART STAY USED, so a controller that keeps restarting
// cannot ask forever.
func TestARequestInDoubtPastItsTriesSettlesFailedUnasked(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	request := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), request, []string{"lost"}); err != nil {
		t.Fatalf("BeginRerun: %v", err)
	}
	for range 4 {
		if err := r.a.RetryRerun(t.Context(), request); err != nil {
			t.Fatalf("RetryRerun: %v", err)
		}
	}
	r.lists(1)

	r.pass()

	if got := r.github.asked(); got != 0 {
		t.Errorf("%d requests past the tries", got)
	}
	if got, _, detail := r.request(1); got != "failed" || detail == "" {
		t.Errorf("request = %s, detail %q", got, detail)
	}
	if got := r.decision("lost"); got != alloc.RerunFailed {
		t.Errorf("decision = %q", got)
	}
}

// A RUN STARTED BY SOMEBODY ELSE BETWEEN THE READ AND THE REQUEST: GitHub's refusal
// waits on the unfinished pace, not a failed try's backoff, and the next read finds
// the run going and settles it, counting it, which only makes billet ask less.
func TestARunThatStartedUnderARequestIsSettledNotRetried(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.lists(1, listed("lost", 77, true))
	r.github.askErr = fmt.Errorf("taken: %w", ErrRerunRunning)
	r.github.onAsk = func(g *rerunGitHub) { g.attempt = RunAttempt{Completed: false, Attempt: 1} }

	r.pass()
	if got, _, _ := r.request(1); got != "asking" {
		t.Errorf("request = %s, want it in doubt", got)
	}

	r.after(rerunRetryFirst + 30*time.Second).pass()
	if got := r.reads(); got != 1 {
		t.Errorf("read %d times inside the unfinished pace", got)
	}

	r.after(rerunUnfinishedEvery).pass()
	if got, _, _ := r.request(1); got != "requested" {
		t.Errorf("request = %s, want it settled by the run going", got)
	}
	if got := r.github.asked(); got != 1 {
		t.Errorf("%d requests", got)
	}
}

// A JOB WHOSE RESULT CAME AFTER ITS ATTEMPT WAS ASKED ABOUT takes that request's answer
// rather than running into the key on every pass.
func TestALateJobTakesItsAttemptsAnswer(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	request := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), request, nil); err != nil {
		t.Fatalf("BeginRerun: %v", err)
	}
	if err := r.a.SettleRerun(t.Context(), request, alloc.RerunFailed, "GitHub said no"); err != nil {
		t.Fatalf("SettleRerun: %v", err)
	}

	r.limits[rerunTier] = 2
	r.job("late", 31, 10*time.Minute)
	r.lists(1, listed("late", 77, true))

	r.pass()

	if got := r.github.asked(); got != 0 {
		t.Errorf("%d requests for an attempt already asked about", got)
	}
	if got := r.decision("late"); got != alloc.RerunFailed {
		t.Errorf("decision = %q", got)
	}
}

// THE REST BUDGET'S RESERVE IS LEFT TO WHAT DECIDES WHETHER A RUNNER STARTS, as the
// queue-time sweep leaves it, and each installation has its share of a pass.
func TestTheLoopLeavesTheReserveAndTakesItsShare(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.github.remaining, r.github.limit = 100, 5000
	r.job("lost", 31, 10*time.Minute)
	r.pass()
	if r.reads() != 0 {
		t.Errorf("read %d times on an installation down to its reserve", r.reads())
	}

	r = newRerunStack(t)
	for i := range rerunRunsPerInstallation + 2 {
		r.job(fmt.Sprintf("job-%d", i), int64(31+i), 10*time.Minute)
	}
	r.github.attempt = RunAttempt{Completed: false, Attempt: 1}
	r.pass()
	if r.reads() != rerunRunsPerInstallation {
		t.Errorf("%d reads in one pass, want the share's %d", r.reads(), rerunRunsPerInstallation)
	}
}

// A READ THAT FAILS DECIDES NOTHING. Could not tell is not unproven: the job is asked
// about again after a backoff that doubles, and the window is what ends it.
func TestAReadThatFailsDecidesNothing(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.github.readErr = errors.New("connection reset")

	r.pass()
	if got := r.decision("lost"); got != "" {
		t.Errorf("decided %q on a read that failed", got)
	}

	r.after(rerunRetryFirst - time.Second).pass()
	if r.reads() != 1 {
		t.Errorf("read %d times inside the backoff", r.reads())
	}

	r.after(2 * time.Second).pass()
	if r.reads() != 2 {
		t.Errorf("read %d times after the backoff", r.reads())
	}

	// DOUBLED: the second backoff is two minutes.
	r.after(2*rerunRetryFirst - time.Second).pass()
	if r.reads() != 2 {
		t.Errorf("read %d times inside the doubled backoff", r.reads())
	}
}

// A STOP ENDS THE LOOP, even with GitHub's read hung, bounded so a loop that ignored
// it would fail here rather than hang the suite.
func TestTheLoopStopsWhenAskedEvenWithGitHubHanging(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)

	hung := &hangingReruns{rerunGitHub: r.github, entered: make(chan struct{})}
	r.targets[rerunTier] = Target{Config: r.targets[rerunTier].Config, Provisioner: hung}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.s.rerunDisrupted(ctx, r.targets, r.limits)
	}()

	select {
	case <-hung.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop never read the run")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a stop did not end the loop")
	}
}

// hangingReruns answers no read until its caller gives up.
type hangingReruns struct {
	*rerunGitHub

	entered chan struct{}
	once    sync.Once
}

func (h *hangingReruns) LatestAttempt(ctx context.Context, _, _ string, _ int64) (RunAttempt, error) {
	h.once.Do(func() { close(h.entered) })
	<-ctx.Done()

	return RunAttempt{}, ctx.Err()
}

// ONLY A TIER THAT ASKS MAKES THE LOOP RUN, and its limit is the tier's own.
func TestRerunLimitsAreTheTiersThatAsk(t *testing.T) {
	t.Parallel()

	asking := tier("asks")
	asking.Retry = &config.TierRetry{Disrupted: 3}
	zero := tier("zero")
	zero.Retry = &config.TierRetry{}

	s := New(nil, nil, []config.Tier{asking, zero, tier("silent")}, "owner", nil)
	limits := s.rerunLimits()
	if len(limits) != 1 || limits["asks"] != 3 {
		t.Errorf("limits = %v", limits)
	}
}

// WHAT A FAILED REQUEST RECORDS IS BOUNDED AND STAYS TEXT POSTGRESQL TAKES, even cut
// through a character.
func TestARerunsDetailIsBoundedAndValid(t *testing.T) {
	t.Parallel()

	// THREE BYTES A CHARACTER, so the bound falls inside one.
	long := strings.Repeat("€", rerunDetailBytes)
	got := rerunDetail(errors.New(long))
	if len(got) > rerunDetailBytes || !utf8.ValidString(got) || got == "" {
		t.Errorf("detail of %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if got := rerunDetail(errors.New("short")); got != "short" {
		t.Errorf("a short detail = %q", got)
	}
}

// A REQUEST IN DOUBT IS LIMITED BY EVERY TIER IT STILL COVERS: one whose tier now
// allows none is not sent again, whichever of its jobs the ledger lists first.
func TestARequestInDoubtFitsEveryTierItCovers(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.limits[rerunTier] = 2
	first := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), first, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SettleRerun(t.Context(), first, alloc.RerunRequested, ""); err != nil {
		t.Fatal(err)
	}

	r.job("a", 31, 10*time.Minute)
	r.jobOn(rerunTierB, "b", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 2,
		Kind: alloc.RerunFailedJobs}, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	r.lists(2)

	r.pass()

	if r.github.asked() != 0 {
		t.Error("sent past the limit of a tier it covers")
	}
	if got := r.decision("b"); got != alloc.RerunLimit {
		t.Errorf("decision = %q", got)
	}
}

// A JOB OF THE ATTEMPT DECIDED SOME OTHER WAY DOES NOT LIMIT A REQUEST IT IS NOT IN.
func TestADecidedJobDoesNotLimitARequestItIsNotIn(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.limits[rerunTierB] = 2
	first := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), first, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SettleRerun(t.Context(), first, alloc.RerunRequested, ""); err != nil {
		t.Fatal(err)
	}

	r.job("own", 31, 10*time.Minute)
	if err := r.a.RecordRerunDecision(t.Context(), "own", alloc.RerunUnproven, 2); err != nil {
		t.Fatal(err)
	}
	r.jobOn(rerunTierB, "lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 2,
		Kind: alloc.RerunOneJob, JobID: 88}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	r.lists(2)

	r.pass()

	if len(r.github.jobAsk) != 1 || r.github.jobAsk[0] != 88 {
		t.Errorf("job requests = %v, want the one in doubt sent again", r.github.jobAsk)
	}
}

// A JOB LATE TO AN ATTEMPT ASKED ABOUT FOR ANOTHER JOB ALONE WAS NOT RE-RUN, and is
// left for a person rather than given that request's answer.
func TestALateJobOfAnAttemptAskedAboutForOneJobIsMixed(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	request := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunOneJob, JobID: 99}
	if err := r.a.BeginRerun(t.Context(), request, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SettleRerun(t.Context(), request, alloc.RerunFailed, "GitHub said no"); err != nil {
		t.Fatal(err)
	}

	r.job("late", 31, 10*time.Minute)
	r.lists(1, listed("late", 77, true))

	r.pass()

	if r.github.asked() != 0 {
		t.Error("asked again for an attempt already asked about")
	}
	if got := r.decision("late"); got != alloc.RerunMixed {
		t.Errorf("decision = %q", got)
	}
}

// TWO READS THAT DISAGREE ABOUT THE LATEST ATTEMPT DECIDE NOTHING: a job listed on an
// attempt later than the run's is not one the run moved past.
func TestAJobOnALaterAttemptThanTheRunsIsNotSuperseded(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.lists(2, AttemptJob{ID: 77, RunnerName: provider.InstanceName("lost"), Attempt: 3, RunnerLost: true, Failed: true})

	r.pass()

	if got := r.decision("lost"); got != "" {
		t.Errorf("decided %q", got)
	}
}

// A RUN IS DECIDED FROM ALL ITS CANDIDATES, not the ones the page that brought it up
// held, and its newest result is the one that must have settled.
func TestARunIsDecidedFromAllItsCandidates(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("one", 31, 10*time.Minute)
	r.job("two", 31, 10*time.Minute)
	r.lists(1, listed("one", 77, true), listed("two", 78, true))

	w := rerunWork{run: rerunRun{repository: "acme/api", run: 31}, target: "default", installation: "i",
		client: r.github}
	if err := r.s.decideRerun(t.Context(), &w, r.limits, r.sweep, r.now); err != nil {
		t.Fatal(err)
	}
	if len(r.github.failedAsk) != 1 || r.decision("one") != alloc.RerunRequested || r.decision("two") != alloc.RerunRequested {
		t.Errorf("requests %v, decisions %q %q", r.github.failedAsk, r.decision("one"), r.decision("two"))
	}
}

// WHERE NO TIER ASKS, THE LOOP STILL RECORDS THAT, through its own first pass, so a
// tier that asks again later starts then.
func TestNoTierAskingForgetsWhenOneDid(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.limits = map[string]int64{}
	r.sweep = newRerunSweep()

	r.pass()

	asked, err := r.a.AskReruns(t.Context(), []string{rerunTier})
	if err != nil {
		t.Fatal(err)
	}
	if !asked[rerunTier].Equal(*r.ledger) {
		t.Errorf("asked since %s, want the moment it asked again, %s", asked[rerunTier], *r.ledger)
	}
}

// A REQUEST IN DOUBT IS SETTLED WHEN NO TIER ASKS ANY MORE: as landed where the run
// says it did, and refused by the limit where it did not, never sent again.
func TestARequestInDoubtIsSettledWhenNoTierAsks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		attempt RunAttempt
		want    alloc.RerunDecision
	}{
		{"it landed", RunAttempt{Completed: false, Attempt: 1}, alloc.RerunRequested},
		{"it did not", RunAttempt{Completed: true, Attempt: 1}, alloc.RerunLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("lost", 31, 10*time.Minute)
			if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
				Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
				t.Fatal(err)
			}
			r.limits = map[string]int64{}
			r.sweep = newRerunSweep()
			r.github.attempt = tc.attempt

			r.pass()

			if r.github.asked() != 0 {
				t.Error("sent with no tier asking")
			}
			if got := r.decision("lost"); got != tc.want {
				t.Errorf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}

// A SNAPSHOT THAT CONTRADICTS ITSELF SENDS NOTHING, a request in doubt included: a
// request for the run's failed jobs reaches whatever attempt GitHub is really on.
func TestAContradictorySnapshotSendsNothing(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
		Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	r.lists(1, AttemptJob{ID: 88, RunnerName: "hosted", Attempt: 2, Failed: true})

	r.pass()

	if r.github.asked() != 0 {
		t.Error("sent on a snapshot that contradicts itself")
	}
	if got, tries, _ := r.request(1); got != "asking" || tries != 1 {
		t.Errorf("request = %s, %d tries", got, tries)
	}
}

// A JOB LATE TO AN ATTEMPT WHOSE REQUEST WAS GIVEN UP TAKES THAT ANSWER, even with the
// run at its limit: no new request is being weighed. (One GitHub accepted moved the
// run to a later attempt, so a job late to it is superseded.)
func TestALateJobTakesItsAttemptsAnswerAtTheLimit(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	accepted := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), accepted, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SettleRerun(t.Context(), accepted, alloc.RerunRequested, ""); err != nil {
		t.Fatal(err)
	}
	givenUp := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 2, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), givenUp, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SettleRerun(t.Context(), givenUp, alloc.RerunFailed, "GitHub said no"); err != nil {
		t.Fatal(err)
	}

	r.job("late", 31, 10*time.Minute)
	r.lists(2, listed("late", 77, true))

	r.pass()

	if got := r.decision("late"); got != alloc.RerunFailed {
		t.Errorf("decision = %q, want the attempt's answer rather than the limit", got)
	}
}

// A REQUEST IN DOUBT IS NOT SENT MORE THAN A DAY AFTER IT WAS DECIDED, however long
// the control plane was gone, nor after a tier it covers stopped asking and asked
// again.
func TestARequestInDoubtIsNotSentLate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(r *rerunStack)
	}{
		{"past the window", func(r *rerunStack) { r.after(rerunWindow + time.Hour) }},
		{"its tier asked again since", func(r *rerunStack) {
			r.after(time.Hour)
			if _, err := r.a.AskReruns(r.t.Context(), nil); err != nil {
				r.t.Fatal(err)
			}
			r.sweep = newRerunSweep()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("lost", 31, 10*time.Minute)
			if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
				Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
				t.Fatal(err)
			}
			tc.setup(r)
			r.lists(1)

			r.pass()

			if r.github.asked() != 0 {
				t.Error("sent late")
			}
			if got := r.decision("lost"); got != alloc.RerunExpired {
				t.Errorf("decision = %q", got)
			}
		})
	}
}

// A RUN THAT CANNOT BE READ HOLDS A REQUEST IN DOUBT ONLY UNTIL THE WINDOW, and then
// the request is settled as landed, since it may have, and nothing is sent.
func TestARunThatCannotBeReadHoldsARequestOnlyUntilTheWindow(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
		Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	r.github.readErr = errors.New("HTTP 404: Not Found")

	r.pass()
	if got, _, _ := r.request(1); got != "asking" {
		t.Fatalf("inside the window = %s", got)
	}

	r.after(rerunWindow + time.Hour).pass()

	if got, _, _ := r.request(1); got != "requested" {
		t.Errorf("past the window = %s, want it counted as landed", got)
	}
	if got := r.decision("lost"); got != alloc.RerunUnconfirmed {
		t.Errorf("decision = %q", got)
	}
	if r.github.asked() != 0 {
		t.Error("sent")
	}
}

// AN INSTALLATION THAT REFUSES EVERY READ HOLDS A REQUEST IN DOUBT ONLY UNTIL TWICE THE
// WINDOW: a refusal may pass, so a day is not enough, and two are. An installation
// paused at that moment is not read, and its request is settled without a read.
func TestARefusedInstallationHoldsARequestOnlyUntilTwiceTheWindow(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
		Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	r.github.readErr = fmt.Errorf("HTTP 403: %w", ErrRerunRefused)

	r.after(rerunWindow + time.Hour).pass()
	if got, _, _ := r.request(1); got != "asking" {
		t.Fatalf("a refusal past one window = %s, want it held", got)
	}

	r.after(rerunWindow)
	r.sweep.paused[installationOf(r.targets[rerunTier])] = r.now.Add(time.Hour)
	reads := r.reads()
	r.pass()

	if got := r.decision("lost"); got != alloc.RerunUnconfirmed {
		t.Errorf("past twice the window = %q", got)
	}
	if got := r.reads(); got != reads {
		t.Errorf("read GitHub %d times to settle it", got-reads)
	}
	if r.github.asked() != 0 {
		t.Error("sent")
	}
	if used, err := r.a.RunReruns(t.Context(), "acme/api", 31); err != nil || used != 1 {
		t.Errorf("counted %d against the limit (%v), want 1", used, err)
	}
}

// PAST TWO DAYS A PASS THAT CANNOT READ A RUN SETTLES ITS REQUEST WITHOUT A READ:
// its installation paused or at its reserve, or the run waiting between failed reads.
func TestAnOverdueRequestThePassCannotReadIsSettledWithoutARead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		hold func(r *rerunStack)
	}{
		{"its installation is paused", func(r *rerunStack) {
			r.sweep.paused[installationOf(r.targets[rerunTier])] = r.now.Add(time.Hour)
		}},
		{"its installation is at its reserve", func(r *rerunStack) {
			r.github.remaining, r.github.limit, r.github.reset = 100, 5000, r.now.Add(time.Hour)
		}},
		{"its run waits between failed reads", func(r *rerunStack) {
			r.sweep.retries[rerunRun{repository: "acme/api", run: 31}] = queueTimeRetry{after: r.now.Add(time.Hour)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("lost", 31, 10*time.Minute)
			if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
				Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
				t.Fatal(err)
			}
			r.lists(1)

			// A DAY IN, IT IS HELD: the pass cannot read it, and two days are not up.
			r.after(rerunWindow + time.Hour)
			tc.hold(r)
			r.pass()
			if got, _, _ := r.request(1); got != "asking" {
				t.Fatalf("a day in = %s, want it held", got)
			}

			r.after(rerunWindow)
			tc.hold(r)
			r.pass()

			if got := r.decision("lost"); got != alloc.RerunUnconfirmed {
				t.Errorf("past two days = %q", got)
			}
			if got := r.reads(); got != 0 {
				t.Errorf("read GitHub %d times", got)
			}
			if r.github.asked() != 0 {
				t.Error("sent")
			}
			if used, err := r.a.RunReruns(t.Context(), "acme/api", 31); err != nil || used != 1 {
				t.Errorf("counted %d (%v), want 1", used, err)
			}
		})
	}
}

// A RUN THAT WAITS ONLY FOR ITS INSTALLATION'S SHARE IS READ IN ITS TURN, never settled
// unread: four requests past two days on one installation are each read, three on one
// pass and the fourth on the next, and none is counted.
func TestAnOverdueRequestOutOfTheShareIsReadInItsTurn(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	leases := []string{"a", "b", "c", "d"}
	for i, lease := range leases {
		run := int64(31 + i)
		r.job(lease, run, 10*time.Minute)
		if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: run, Attempt: 1,
			Kind: alloc.RerunFailedJobs}, []string{lease}); err != nil {
			t.Fatal(err)
		}
	}
	r.lists(1)

	count := func() (expired, open int) {
		for _, lease := range leases {
			switch r.decision(lease) {
			case alloc.RerunExpired:
				expired++
			case "":
				open++
			default:
				t.Errorf("%s = %q", lease, r.decision(lease))
			}
		}

		return expired, open
	}

	r.after(2*rerunWindow + time.Hour).pass()
	if expired, open := count(); expired != rerunRunsPerInstallation || open != len(leases)-rerunRunsPerInstallation {
		t.Fatalf("after one pass %d expired and %d open, want the share read and the rest held", expired, open)
	}

	r.after(time.Minute).pass()
	if expired, open := count(); expired != len(leases) || open != 0 {
		t.Errorf("after two passes %d expired and %d open", expired, open)
	}
	if got := r.reads(); got != len(leases) {
		t.Errorf("%d reads, want one a run", got)
	}
	if r.github.asked() != 0 {
		t.Error("sent")
	}
}

// PAST TWO DAYS A RUN THAT CAN BE READ IS SETTLED FROM THE READ, and only one that
// cannot is settled without it: a request GitHub never took, found after a long
// outage, is given up rather than counted, and a refusal at the read still ends it.
func TestAnOverdueRequestIsSettledFromAReadWhereThereIsOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		readErr error
		want    alloc.RerunDecision
		counted int64
	}{
		{"the run is read", nil, alloc.RerunExpired, 0},
		{"the read is refused", fmt.Errorf("HTTP 403: %w", ErrRerunRefused), alloc.RerunUnconfirmed, 1},
		{"the read is throttled", fmt.Errorf("HTTP 429: %w", ErrRerunThrottled), alloc.RerunUnconfirmed, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRerunStack(t)
			r.job("lost", 31, 10*time.Minute)
			if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
				Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
				t.Fatal(err)
			}
			r.lists(1)
			r.github.readErr = tc.readErr

			r.after(2*rerunWindow + time.Hour).pass()

			if got := r.reads(); got != 1 {
				t.Errorf("%d reads, want the one that settles it", got)
			}
			if got := r.decision("lost"); got != tc.want {
				t.Errorf("decision = %q, want %q", got, tc.want)
			}
			if r.github.asked() != 0 {
				t.Error("sent")
			}
			if used, err := r.a.RunReruns(t.Context(), "acme/api", 31); err != nil || used != tc.counted {
				t.Errorf("counted %d (%v), want %d", used, err, tc.counted)
			}
			// A REFUSAL OR A THROTTLE STILL PAUSES THE INSTALLATION, for its other runs.
			paused := r.sweep.paused[installationOf(r.targets[rerunTier])]
			if wantPaused := tc.readErr != nil; paused.After(r.now) != wantPaused {
				t.Errorf("installation paused until %v at %v, want paused %v", paused, r.now, wantPaused)
			}
		})
	}
}

// A JOB THAT CROSSES THE WINDOW AFTER THE FIRST LOOK, while the run's other jobs are
// decided, sends nothing: the last look is right before the request. Here the write
// that expires one job carries the pass past the other's window.
func TestTheLastLookAtTheWindowIsBeforeTheRequest(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("old", 31, rerunWindow-time.Minute)
	r.job("young", 31, rerunWindow-30*time.Minute)
	r.lists(1, listed("old", 77, true), listed("young", 88, true))

	// THE READ TOOK TWO MINUTES, which carries the old job past the window at the first
	// look and leaves the young one inside it.
	r.sweep.began = time.Now().Add(-2 * time.Minute)
	var once sync.Once
	*r.onClock = func() { once.Do(func() { r.sweep.began = r.sweep.began.Add(-time.Hour) }) }
	w := rerunWork{run: rerunRun{repository: "acme/api", run: 31}, target: "default", installation: "i",
		client: r.github}
	if err := r.s.decideRerun(t.Context(), &w, r.limits, r.sweep, r.now); err != nil {
		t.Fatal(err)
	}

	if r.github.asked() != 0 {
		t.Error("sent past the window")
	}
	if got := r.decision("old"); got != alloc.RerunExpired {
		t.Errorf("the job past the window = %q", got)
	}
	if got := r.decision("young"); got != "" {
		t.Errorf("the job that crossed it = %q, want it left for the next pass", got)
	}
}

// THE DEADLINE IS THE CLOCK AS IT IS AFTER THE READ, not as the pass began: a slow
// GitHub does not let a request out past the window.
func TestTheWindowIsCheckedAfterTheRead(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1,
		Kind: alloc.RerunFailedJobs}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	r.lists(1)

	// THE PASS BEGAN TWO HOURS AGO ON THE WALL CLOCK, an hour short of the window by
	// its own reckoning.
	r.after(rerunWindow - time.Hour)
	r.sweep.began = time.Now().Add(-2 * time.Hour)
	w := rerunWork{run: rerunRun{repository: "acme/api", run: 31}, target: "default", installation: "i",
		client: r.github}
	if err := r.s.decideRerun(t.Context(), &w, r.limits, r.sweep, r.now); err != nil {
		t.Fatal(err)
	}

	if r.github.asked() != 0 {
		t.Error("sent past the window")
	}
	if got := r.decision("lost"); got != alloc.RerunExpired {
		t.Errorf("decision = %q", got)
	}
}

// A JOB FIRST ASKED ABOUT IS HELD TO THE SAME CLOCK: a job an hour inside the window
// when the pass began, and past it once a slow read has returned, is not sent. It is
// decided alone: a younger job of the run is still asked for, by itself, since the
// expired one is now a failed job the request must not run again.
func TestAFreshJobIsCheckedAgainstTheWindowAfterTheRead(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, rerunWindow-time.Hour)
	r.job("young", 31, 10*time.Minute)
	r.lists(1, listed("lost", 77, true), listed("young", 88, true))

	r.sweep.began = time.Now().Add(-2 * time.Hour)
	w := rerunWork{run: rerunRun{repository: "acme/api", run: 31}, target: "default", installation: "i",
		client: r.github}
	if err := r.s.decideRerun(t.Context(), &w, r.limits, r.sweep, r.now); err != nil {
		t.Fatal(err)
	}

	if len(r.github.failedAsk) != 0 || len(r.github.jobAsk) != 1 || r.github.jobAsk[0] != 88 {
		t.Errorf("asked failed-jobs %v, jobs %v, want job 88 alone", r.github.failedAsk, r.github.jobAsk)
	}
	if got := r.decision("lost"); got != alloc.RerunExpired {
		t.Errorf("the job past the window = %q", got)
	}
	if got := r.decision("young"); got != alloc.RerunRequested {
		t.Errorf("the job inside it = %q", got)
	}
}

// A JOB LATE TO AN ATTEMPT WHOSE REQUEST ENDED AT THE LIMIT SAYS SO, as its siblings
// do, rather than that GitHub refused something it was never sent.
func TestALateJobTakesTheStoredAnswer(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	request := alloc.RerunRequest{Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunFailedJobs}
	if err := r.a.BeginRerun(t.Context(), request, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.a.SettleRerun(t.Context(), request, alloc.RerunLimit, "lowered"); err != nil {
		t.Fatal(err)
	}

	r.job("late", 31, 10*time.Minute)
	r.lists(1, listed("late", 77, true))

	r.pass()

	if got := r.decision("late"); got != alloc.RerunLimit {
		t.Errorf("decision = %q", got)
	}
}

// THE FEW READS AN INSTALLATION HAS A PASS GO ROUND EVERY RUN IN TURN: the runs read
// longest ago come first, so a run not read in one pass is read in the next.
func TestTheReadsGoRoundEveryRun(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	for i := range rerunRunsPerInstallation + 2 {
		r.job(fmt.Sprintf("job-%d", i), int64(31+i), 10*time.Minute)
	}
	r.github.attempt = RunAttempt{Completed: false, Attempt: 1}

	r.pass()
	r.after(rerunUnfinishedEvery + time.Second).pass()

	read := map[int64]bool{}
	for _, run := range r.github.readRuns {
		read[run] = true
	}
	if len(read) != rerunRunsPerInstallation+2 {
		t.Errorf("runs read in two passes = %v, want every one", r.github.readRuns)
	}
}
