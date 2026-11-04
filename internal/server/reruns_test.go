package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

const rerunTier = "billet-4vcpu-a"

// rerunGitHub answers what a test says GitHub would, and keeps every request.
type rerunGitHub struct {
	*fakeProvisioner

	mu        sync.Mutex
	attempt   RunAttempt
	readErr   error
	askErr    error
	reads     int
	failedAsk []int64
	jobAsk    []int64
	owners    []string
	// remaining and limit are the installation's REST budget, when limit is set.
	remaining, limit int64
	// onAsk runs inside a request, for a run whose state a request changes.
	onAsk func(*rerunGitHub)
}

func (g *rerunGitHub) LatestAttempt(_ context.Context, owner, repository string, _ int64) (RunAttempt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.reads++
	g.owners = append(g.owners, owner+"/"+repository)

	return g.attempt, g.readErr
}

func (g *rerunGitHub) RerunFailedJobs(_ context.Context, _, _ string, runID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.failedAsk = append(g.failedAsk, runID)
	if g.onAsk != nil {
		g.onAsk(g)
	}

	return g.askErr
}

func (g *rerunGitHub) RerunJob(_ context.Context, _, _ string, jobID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.jobAsk = append(g.jobAsk, jobID)
	if g.onAsk != nil {
		g.onAsk(g)
	}

	return g.askErr
}

func (g *rerunGitHub) RESTHeadroom() (int64, int64, time.Time, bool) {
	return g.remaining, g.limit, time.Now().Add(time.Hour), g.limit > 0
}

func (g *rerunGitHub) asked() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.failedAsk) + len(g.jobAsk)
}

// rerunStack is a real allocator over a real ledger, seeded with red jobs, and the
// loop's own pass driven at a clock the test holds.
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
}

func newRerunStack(t *testing.T) *rerunStack {
	t.Helper()

	db := openState(t)
	tiers := []config.Tier{tier(rerunTier)}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	github := &rerunGitHub{fakeProvisioner: &fakeProvisioner{}, attempt: RunAttempt{Completed: true, Attempt: 1}}

	return &rerunStack{t: t, s: New(a, nil, tiers, "owner", nil), a: a, db: db, github: github,
		targets: map[string]Target{rerunTier: {
			Config:      config.GitHubTarget{Name: "default", Org: "acme", InstallationID: 7},
			Provisioner: github,
		}},
		limits: map[string]int64{rerunTier: 1}, sweep: newRerunSweep(), now: time.Now()}
}

// job seeds one red job whose lease a cause on the list disrupted, its result in
// the past by age.
func (r *rerunStack) job(lease string, run int64, age time.Duration) {
	r.t.Helper()

	at := stamp(r.now.Add(-age))
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, started_at, repo, github_job_id, github_run_id,
			 disruption, disrupted_at, result, result_at)
			VALUES ($1, $2, $3, $3, 'acme/api', $4, $5, 'reclaimed', $3, 'failed', $3)`,
			lease, rerunTier, at, "guid-"+lease, run)

		return err
	}); err != nil {
		r.t.Fatalf("seed %s: %v", lease, err)
	}
}

// listed is a job of the attempt, on the runner a lease's name gives.
func listed(lease string, id int64, lost, failed bool) AttemptJob {
	return AttemptJob{ID: id, RunnerName: provider.InstanceName(lease), RunnerLost: lost, Failed: failed}
}

func (r *rerunStack) pass() { r.s.rerunOnce(r.t.Context(), r.targets, r.limits, r.sweep, r.now) }

// after moves the test's clock past every wait a pass could have left.
func (r *rerunStack) after(d time.Duration) *rerunStack {
	r.now = r.now.Add(d)

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

func (r *rerunStack) requestState(attempt int64) (state string, tries int64, detail string) {
	r.t.Helper()

	if err := r.db.Reader().QueryRowContext(r.t.Context(),
		`SELECT state, tries, detail FROM job_reruns WHERE repo = 'acme/api' AND run_id = 31 AND run_attempt = $1`,
		attempt).Scan(&state, &tries, &detail); err != nil {
		r.t.Fatalf("read the request of attempt %d: %v", attempt, err)
	}

	return state, tries, detail
}

// A JOB BILLET PROVED IT ENDED IS RE-RUN ONCE. The run's only failed job is proved, so
// the whole request is exact; the job carries the outcome, and the next pass, with
// nothing left undecided, asks nothing.
func TestAProvedJobIsRerunOnce(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.github.attempt.Jobs = []AttemptJob{listed("lost", 77, true, true)}

	r.pass()

	if len(r.github.failedAsk) != 1 || r.github.failedAsk[0] != 31 || len(r.github.jobAsk) != 0 {
		t.Fatalf("asked failed-jobs %v, jobs %v, want run 31's failed jobs once", r.github.failedAsk, r.github.jobAsk)
	}
	if r.github.owners[0] != "acme/api" {
		t.Errorf("read %q", r.github.owners[0])
	}
	if got := r.decision("lost"); got != alloc.RerunRequested {
		t.Errorf("decision = %q", got)
	}
	if state, tries, _ := r.requestState(1); state != "requested" || tries != 1 {
		t.Errorf("request = %s, %d tries", state, tries)
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
	r.github.attempt.Jobs = []AttemptJob{listed("own", 77, false, true)}

	r.pass()

	if got := r.github.asked(); got != 0 {
		t.Errorf("a job that failed on its own was re-run: %d requests", got)
	}
	if got := r.decision("own"); got != alloc.RerunUnproven {
		t.Errorf("decision = %q", got)
	}
}

// THE LIMIT HOLDS ACROSS ATTEMPTS. Attempt 1 was re-run; attempt 2 lost its runner
// too, and a tier allowing one re-run leaves it red and says why.
func TestTheLimitHoldsAcrossAttempts(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("first", 31, 10*time.Minute)
	r.github.attempt.Jobs = []AttemptJob{listed("first", 77, true, true)}
	r.pass()

	r.job("second", 31, 10*time.Minute)
	r.github.attempt = RunAttempt{Completed: true, Attempt: 2, Jobs: []AttemptJob{listed("second", 88, true, true)}}
	r.after(time.Hour).pass()

	if got := r.github.asked(); got != 1 {
		t.Errorf("%d requests, want the limit's 1", got)
	}
	if got := r.decision("second"); got != alloc.RerunLimit {
		t.Errorf("decision = %q", got)
	}

	// AND A TIER ALLOWING TWO ASKS AGAIN, so the refusal above is the limit's.
	r = newRerunStack(t)
	r.limits[rerunTier] = 2
	r.job("first", 31, 10*time.Minute)
	r.github.attempt.Jobs = []AttemptJob{listed("first", 77, true, true)}
	r.pass()
	r.job("second", 31, 10*time.Minute)
	r.github.attempt = RunAttempt{Completed: true, Attempt: 2, Jobs: []AttemptJob{listed("second", 88, true, true)}}
	r.after(time.Hour).pass()
	if got := r.github.asked(); got != 2 {
		t.Errorf("%d requests, want 2 under a limit of 2", got)
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
				listed("one", 77, true, true), listed("two", 78, true, true),
				{ID: 90, RunnerName: "hosted", Failed: false},
			},
			wantRun:   1,
			decisions: map[string]alloc.RerunDecision{"one": alloc.RerunRequested, "two": alloc.RerunRequested},
		},
		{
			name: "one proved beside a job that failed on its own is that job alone",
			jobs: []AttemptJob{
				listed("one", 77, true, true), listed("two", 78, false, true),
				{ID: 91, RunnerName: "hosted", Failed: true},
			},
			wantJob:   []int64{77},
			decisions: map[string]alloc.RerunDecision{"one": alloc.RerunRequested, "two": alloc.RerunUnproven},
		},
		{
			name: "two proved beside one that failed on its own is left for a person",
			jobs: []AttemptJob{
				listed("one", 77, true, true), listed("two", 78, true, true),
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
			r.github.attempt.Jobs = tc.jobs

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
	if r.github.reads != 1 {
		t.Errorf("read %d times inside its wait", r.github.reads)
	}

	r.after(2 * time.Second)
	r.github.attempt = RunAttempt{Completed: true, Attempt: 1, Jobs: []AttemptJob{listed("lost", 77, true, true)}}
	r.pass()
	if r.github.reads != 2 || r.github.asked() != 1 {
		t.Errorf("reads %d, requests %d, want it decided once the run finished", r.github.reads, r.github.asked())
	}
}

// THE RUN MOVED PAST THE JOB WITHOUT BILLET: a person re-ran it, and billet does not
// add a second attempt on top of theirs.
func TestARunThatMovedOnIsSuperseded(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.github.attempt = RunAttempt{Completed: true, Attempt: 2, Jobs: []AttemptJob{listed("another-lease", 88, true, true)}}

	r.pass()

	if got := r.decision("lost"); got != alloc.RerunSuperseded {
		t.Errorf("decision = %q", got)
	}
	if r.github.asked() != 0 {
		t.Error("asked about a run a person already re-ran")
	}
}

// RESULTS ARRIVE ONE BY ONE, SO A RUN WAITS FOR THE REST. Deciding on the first would
// read its sibling's lost job, not yet a candidate, as one that failed on its own.
func TestARunWithAResultStillSettlingWaits(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("older", 31, time.Hour)
	r.job("newest", 31, rerunSettle/2)
	r.github.attempt.Jobs = []AttemptJob{listed("older", 77, true, true), listed("newest", 78, true, true)}

	r.pass()
	if r.github.reads != 0 {
		t.Fatalf("read a run whose newest result is still settling: %d", r.github.reads)
	}

	r.after(rerunSettle).pass()
	if len(r.github.failedAsk) != 1 {
		t.Errorf("requests = %v, want one covering both", r.github.failedAsk)
	}
	if r.decision("older") != alloc.RerunRequested || r.decision("newest") != alloc.RerunRequested {
		t.Errorf("decisions = %q, %q", r.decision("older"), r.decision("newest"))
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
	if r.github.reads != 0 {
		t.Errorf("GitHub was read %d times about an expired job", r.github.reads)
	}

	r = newRerunStack(t)
	delete(r.limits, rerunTier)
	r.job("not-opted-in", 31, 10*time.Minute)
	r.github.attempt.Jobs = []AttemptJob{listed("not-opted-in", 77, true, true)}
	r.pass()
	if got := r.decision("not-opted-in"); got != "" {
		t.Errorf("a tier that did not opt in was decided: %q", got)
	}
	if r.github.reads != 0 || r.github.asked() != 0 {
		t.Errorf("reads %d, requests %d about a tier that did not opt in", r.github.reads, r.github.asked())
	}
}

// A FAILURE TO ASK IS REPORTED, AND NEVER RETRIED FOREVER. The request is in doubt
// across every try, the tries are bounded, and it settles failed with GitHub's words.
func TestAFailedRequestIsBoundedAndReported(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.github.attempt.Jobs = []AttemptJob{listed("lost", 77, true, true)}
	r.github.askErr = errors.New("HTTP 500: GitHub had a bad moment")

	for try := 1; try <= rerunMaxTries; try++ {
		r.pass()
		if got := r.github.asked(); got != try {
			t.Fatalf("after pass %d: %d requests", try, got)
		}

		if try < rerunMaxTries {
			if state, tries, _ := r.requestState(1); state != "asking" || tries != int64(try) {
				t.Errorf("try %d: request = %s, %d tries", try, state, tries)
			}
			if got := r.decision("lost"); got != "" {
				t.Errorf("try %d: decided %q while a try remains", try, got)
			}
		}

		r.after(rerunRetryMost + time.Minute)
	}

	state, tries, detail := r.requestState(1)
	if state != "failed" || tries != rerunMaxTries || detail != "HTTP 500: GitHub had a bad moment" {
		t.Errorf("request = %s, %d tries, detail %q", state, tries, detail)
	}
	if got := r.decision("lost"); got != alloc.RerunFailed {
		t.Errorf("decision = %q", got)
	}

	r.pass()
	if got := r.github.asked(); got != rerunMaxTries {
		t.Errorf("%d requests after it settled", got)
	}

	// IT DOES NOT COUNT AGAINST THE LIMIT: it ran nothing.
	if used, err := r.a.RunReruns(t.Context(), "acme/api", 31); err != nil || used != 0 {
		t.Errorf("RunReruns = %d, %v", used, err)
	}
}

// A REFUSAL PAUSES THE WHOLE APP INSTALLATION and says what the App needs, and a
// throttle pauses it for less: neither spends a request per run on an answer that will
// not change.
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
			r.github.attempt.Jobs = []AttemptJob{listed("one", 77, true, true), listed("other", 78, true, true)}
			r.github.askErr = tc.err

			r.pass()
			first := r.github.asked()
			if first != 1 {
				t.Fatalf("%d requests in the pass GitHub refused, want it to stop at the first", first)
			}

			r.after(tc.wait - time.Second).pass()
			if got := r.github.asked(); got != first {
				t.Errorf("%d requests inside the pause", got)
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
		name     string
		attempt  RunAttempt
		asked    int
		state    string
		decision alloc.RerunDecision
	}{
		{"the run is going, so it landed", RunAttempt{Completed: false, Attempt: 1}, 0, "requested", alloc.RerunRequested},
		{"the run finished a later attempt, so it landed", RunAttempt{Completed: true, Attempt: 2}, 0, "requested", alloc.RerunRequested},
		{"the run finished the same attempt, so it did not", RunAttempt{Completed: true, Attempt: 1}, 1, "requested", alloc.RerunRequested},
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
			if state, _, _ := r.requestState(1); state != tc.state {
				t.Errorf("request = %s, want %s", state, tc.state)
			}
			if got := r.decision("lost"); got != tc.decision {
				t.Errorf("decision = %q, want %q", got, tc.decision)
			}
		})
	}
}

// THE KIND A REQUEST WAS WRITTEN WITH IS THE KIND SENT AGAIN, its job with it.
func TestARequestInDoubtIsSentAgainAsItWasWritten(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	if err := r.a.BeginRerun(t.Context(), alloc.RerunRequest{
		Repository: "acme/api", RunID: 31, Attempt: 1, Kind: alloc.RerunOneJob, JobID: 77,
	}, []string{"lost"}); err != nil {
		t.Fatalf("BeginRerun: %v", err)
	}
	r.github.attempt = RunAttempt{Completed: true, Attempt: 1}

	r.pass()

	if len(r.github.jobAsk) != 1 || r.github.jobAsk[0] != 77 || len(r.github.failedAsk) != 0 {
		t.Errorf("job requests %v, failed-jobs requests %v", r.github.jobAsk, r.github.failedAsk)
	}
	if state, tries, _ := r.requestState(1); state != "requested" || tries != 2 {
		t.Errorf("request = %s, %d tries", state, tries)
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
	for range rerunMaxTries - 1 {
		if err := r.a.RetryRerun(t.Context(), request); err != nil {
			t.Fatalf("RetryRerun: %v", err)
		}
	}
	r.github.attempt = RunAttempt{Completed: true, Attempt: 1}

	r.pass()

	if got := r.github.asked(); got != 0 {
		t.Errorf("%d requests past the tries", got)
	}
	if state, _, detail := r.requestState(1); state != "failed" || detail == "" {
		t.Errorf("request = %s, detail %q", state, detail)
	}
	if got := r.decision("lost"); got != alloc.RerunFailed {
		t.Errorf("decision = %q", got)
	}
}

// A RUN STARTED BY SOMEBODY ELSE BETWEEN THE READ AND THE REQUEST: GitHub's refusal
// is not a failed try's report, and the next pass finds the run going and settles it,
// counting it, which only makes billet ask less.
func TestARunThatStartedUnderARequestIsSettledNotRetried(t *testing.T) {
	t.Parallel()

	r := newRerunStack(t)
	r.job("lost", 31, 10*time.Minute)
	r.github.attempt.Jobs = []AttemptJob{listed("lost", 77, true, true)}
	r.github.askErr = fmt.Errorf("taken: %w", ErrRerunRunning)
	r.github.onAsk = func(g *rerunGitHub) { g.attempt = RunAttempt{Completed: false, Attempt: 1} }

	r.pass()
	if state, _, _ := r.requestState(1); state != "asking" {
		t.Errorf("request = %s, want it in doubt", state)
	}

	r.after(rerunUnfinishedEvery + time.Second).pass()
	if state, _, _ := r.requestState(1); state != "requested" {
		t.Errorf("request = %s, want it settled by the run going", state)
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
	if err := r.a.SettleRerun(t.Context(), request, false, "GitHub said no"); err != nil {
		t.Fatalf("SettleRerun: %v", err)
	}

	r.limits[rerunTier] = 2
	r.job("late", 31, 10*time.Minute)
	r.github.attempt.Jobs = []AttemptJob{listed("late", 77, true, true)}

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
	if r.github.reads != 0 {
		t.Errorf("read %d times on an installation down to its reserve", r.github.reads)
	}

	r = newRerunStack(t)
	for i := range rerunRunsPerInstallation + 2 {
		r.job(fmt.Sprintf("job-%d", i), int64(31+i), 10*time.Minute)
	}
	r.github.attempt = RunAttempt{Completed: false, Attempt: 1}
	r.pass()
	if r.github.reads != rerunRunsPerInstallation {
		t.Errorf("%d reads in one pass, want the share's %d", r.github.reads, rerunRunsPerInstallation)
	}
}

// A READ THAT FAILS DECIDES NOTHING. Could not tell is not unproven: the job is asked
// about again after a backoff, and the window is what ends it.
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
	if r.github.reads != 1 {
		t.Errorf("read %d times inside the backoff", r.github.reads)
	}

	r.after(2 * time.Second).pass()
	if r.github.reads != 2 {
		t.Errorf("read %d times after the backoff", r.github.reads)
	}
}

// THE LOOP RUNS ON THE PACE IT WAS GIVEN UNTIL IT IS STOPPED, on the ticker it really
// runs on, and a read that never answers holds up only itself: a stop ends it.
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

	<-hung.entered
	cancel()
	<-done
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
