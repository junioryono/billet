package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// recoveryGitHub is a run GitHub keeps as measured on 2026-11-22: a cancel completes
// the attempt, and a re-run of its failed jobs starts the next one with the
// cancelled job queued again.
type recoveryGitHub struct {
	*strandedGitHub

	attempt   QueuedAttempt
	reads     int
	cancels   int
	reruns    int
	cancelErr error
	rerunErr  error
	// lands says whether a request GitHub took changes the run, as it does unless a
	// test is about one that did not.
	cancelLands, rerunLands bool
}

func (g *recoveryGitHub) QueuedAttempt(context.Context, string, string, int64) (QueuedAttempt, error) {
	g.reads++

	return g.attempt, nil
}

func (g *recoveryGitHub) CancelRun(context.Context, string, string, int64) error {
	g.cancels++
	if g.cancelErr != nil {
		return g.cancelErr
	}
	if g.cancelLands {
		g.attempt = QueuedAttempt{Completed: true, Attempt: g.attempt.Attempt}
	}

	return nil
}

func (g *recoveryGitHub) RerunFailedJobs(context.Context, string, string, int64) error {
	g.reruns++
	if g.rerunErr != nil {
		return g.rerunErr
	}
	if g.rerunLands {
		next := g.attempt.Attempt + 1
		g.attempt = QueuedAttempt{Attempt: next, Jobs: []QueuedJob{{Name: "Lint App", Attempt: next, Unstarted: true}}}
	}

	return nil
}

// queued is a run's first attempt as the stranded job leaves it: the job of its
// name queued with no runner, and the others as given.
func queued(others ...QueuedJob) QueuedAttempt {
	jobs := append([]QueuedJob{{Name: "Lint App", Attempt: 1, Unstarted: true}}, others...)

	return QueuedAttempt{Attempt: 1, Jobs: jobs}
}

// recoveryStack is a strandedStack whose tier recovers stranded jobs, or does not.
type recoveryStack struct {
	*strandedStack

	github *recoveryGitHub
}

func newRecoveryStack(t *testing.T, recovers bool) *recoveryStack {
	t.Helper()

	r := &strandedStack{t: t, db: openState(t), sweep: newRerunSweep(), now: time.Now()}
	tierCfg := tier(strandedTier)
	if recovers {
		tierCfg.Retry = &config.TierRetry{Stranded: true}
	}
	// ANOTHER TIER ALWAYS ASKS, so a test of one that does not is about that tier and
	// not about a deployment where none does.
	other := tier("billet-2vcpu-b")
	other.Retry = &config.TierRetry{Stranded: true}
	tiers := []config.Tier{tierCfg, other}

	a, err := alloc.New(r.db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithClock(func() time.Time { return r.now }))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	r.a, r.s = a, New(a, nil, tiers, "owner", nil)

	github := &recoveryGitHub{strandedGitHub: &strandedGitHub{fakeProvisioner: &fakeProvisioner{}},
		attempt: queued(), cancelLands: true, rerunLands: true}
	r.github = github.strandedGitHub
	target := Target{Config: config.GitHubTarget{Name: "default", Org: "acme", InstallationID: 7}, Provisioner: github}
	r.targets = map[string]Target{strandedTier: target, "billet-2vcpu-b": target}

	// THE TIER ASKED AN HOUR AGO, before the launch this test strands ended.
	start := r.now
	r.now = start.Add(-time.Hour)
	if _, err := a.AskStranded(t.Context(), askingTiers(tiers)); err != nil {
		t.Fatalf("AskStranded: %v", err)
	}
	r.now = start

	return &recoveryStack{strandedStack: r, github: github}
}

func askingTiers(tiers []config.Tier) []string {
	var out []string
	for i := range tiers {
		if tiers[i].RecoversStranded() {
			out = append(out, tiers[i].Label)
		}
	}

	return out
}

// strand seeds a launch the detector decided stranded, its lease ended age before
// the clock.
func (r *recoveryStack) strand(lease string, run int64, age time.Duration) {
	r.t.Helper()

	ended := stamp(r.now.Add(-age))
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, finished_at, repo, run_id, github_job_id, job_name, result,
			 result_at, failure_reason, stranded, stranded_at)
			VALUES ($1, $2, $3, $3, 'acme/app', $4, 'job-'||$1, 'Lint App', '', '', 'billet:launch-failed',
			        'stranded', $5)`,
			lease, strandedTier, ended, run, stamp(r.now.Add(-time.Minute)))

		return err
	}); err != nil {
		r.t.Fatalf("seed %s: %v", lease, err)
	}
}

// pass runs the re-run loop's own pass, with no tier asking for disrupted re-runs, as
// a deployment's default has it.
func (r *recoveryStack) pass() {
	r.t.Helper()

	r.advertises(4, 0)
	r.s.rerunOnce(r.t.Context(), r.targets, map[string]int64{}, r.sweep, r.now)
	r.now = r.now.Add(time.Minute)
}

// recovery reads what the ledger says of the run's recovery, empty for none.
func (r *recoveryStack) recovery(run int64) string {
	r.t.Helper()

	var state string
	err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT state FROM stranded_recoveries WHERE repo = 'acme/app' AND run_id = $1`, run).Scan(&state)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		r.t.Fatalf("read run %d's recovery: %v", run, err)
	}

	return state
}

// A STRANDED JOB IS RECOVERED IN ORDER: its run is cancelled, and only once GitHub
// records that attempt completed are its failed jobs re-run, once.
func TestAStrandedJobIsRecoveredByACancelThenAReRun(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 20*time.Minute)

	r.pass()
	if r.github.cancels != 1 || r.github.reruns != 0 {
		t.Fatalf("after the first pass: %d cancels and %d re-runs, want one cancel and no re-run",
			r.github.cancels, r.github.reruns)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryCancelling) {
		t.Fatalf("the recovery is %q after the cancel, want cancelling", got)
	}

	r.pass()
	if r.github.reruns != 1 {
		t.Fatalf("%d re-runs once the cancelled attempt completed, want one", r.github.reruns)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryRecovered) {
		t.Fatalf("the recovery is %q after the re-run, want recovered", got)
	}

	r.pass()
	r.pass()
	if r.github.cancels != 1 || r.github.reruns != 1 {
		t.Fatalf("%d cancels and %d re-runs after the recovery, want one of each", r.github.cancels, r.github.reruns)
	}
}

// A TIER THAT DOES NOT ASK NEVER HAS A RUN CANCELLED, which is every tier by default.
func TestATierThatDoesNotAskNeverHasARunCancelled(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, false)
	r.strand("missed", 42, 20*time.Minute)

	for range 3 {
		r.pass()
	}

	if r.github.cancels != 0 || r.github.reruns != 0 || r.github.reads != 0 {
		t.Fatalf("a tier that does not ask: %d reads, %d cancels, %d re-runs; want none",
			r.github.reads, r.github.cancels, r.github.reruns)
	}
}

// NOTHING RUNNING IS CANCELLED: a run with a job that has a runner, or a read that
// disagrees with itself about the attempt, is not cancelled.
func TestNothingRunningIsCancelled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		attempt QueuedAttempt
	}{
		{"a sibling running", queued(QueuedJob{Name: "Build iOS", Attempt: 1})},
		{"the job of the name taken beside the queued one", queued(QueuedJob{Name: "Lint App", Attempt: 1})},
		{"a job of another attempt", queued(QueuedJob{Name: "Build iOS", Attempt: 2, Unstarted: true})},
		{"no job of the name queued", QueuedAttempt{Attempt: 1, Jobs: []QueuedJob{{Name: "Build iOS", Attempt: 1,
			Unstarted: true}}}},
		{"the run over", QueuedAttempt{Completed: true, Attempt: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRecoveryStack(t, true)
			r.github.attempt = tc.attempt
			r.strand("missed", 42, 20*time.Minute)

			r.pass()

			if r.github.cancels != 0 {
				t.Fatalf("cancelled the run %d times, want never", r.github.cancels)
			}
			if got := r.recovery(42); got != "" {
				t.Fatalf("a recovery %q was begun", got)
			}
		})
	}

	// AND A FINISHED SIBLING IS NOT RUNNING: the run is cancelled.
	r := newRecoveryStack(t, true)
	r.github.attempt = queued(QueuedJob{Name: "Detect Changes", Attempt: 1, Done: true})
	r.strand("missed", 42, 20*time.Minute)
	r.pass()
	if r.github.cancels != 1 {
		t.Fatalf("a run whose other job is over was cancelled %d times, want once", r.github.cancels)
	}
}

// A LAUNCH THAT ENDED BEFORE ITS TIER STARTED ASKING IS NOT RECOVERED, so turning
// retry.stranded on never cancels a run that stranded before.
func TestALaunchFromBeforeTheTierAskedIsNotRecovered(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 2*time.Hour)

	r.pass()

	if r.github.cancels != 0 {
		t.Fatalf("cancelled a run whose launch ended before the tier asked %d times", r.github.cancels)
	}
}

// AN ASSIGNMENT SINCE THAT MAY BE THE JOB HOLDS THE CANCEL: GitHub may be offering it
// after all.
func TestALaterAssignmentHoldsTheCancel(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 20*time.Minute)
	r.lease("booting", 42, "", 5*time.Minute)

	r.pass()

	if r.github.cancels != 0 {
		t.Fatalf("cancelled the run %d times beside an assignment that may be its job", r.github.cancels)
	}
}

// A RESTART BETWEEN THE TWO REQUESTS NEITHER CANCELS AGAIN NOR LEAVES THE RUN
// CANCELLED: the ledger says where the recovery is, and a pass with nothing in memory
// takes it on.
func TestARestartBetweenTheRequestsFinishesTheRecovery(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.sweep = newRerunSweep()
	r.pass()

	if r.github.cancels != 1 || r.github.reruns != 1 {
		t.Fatalf("across a restart: %d cancels and %d re-runs, want one of each", r.github.cancels, r.github.reruns)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryRecovered) {
		t.Fatalf("the recovery is %q, want recovered", got)
	}
}

// A RUN SOMEONE ELSE MOVED ON IS LEFT ALONE: re-run by a person between billet's
// cancel and its re-run, it is theirs.
func TestARunSomeoneElseReRanIsLeftAlone(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.github.attempt = QueuedAttempt{Attempt: 2, Jobs: []QueuedJob{{Name: "Lint App", Attempt: 2, Unstarted: true}}}
	r.pass()

	if r.github.reruns != 0 {
		t.Fatalf("re-ran a run someone else had already re-run %d times", r.github.reruns)
	}
	if got := r.recovery(42); got != string(alloc.RecoverySuperseded) {
		t.Fatalf("the recovery is %q, want superseded", got)
	}
}

// A RE-RUN THAT DOES NOT LAND IS SENT AGAIN, AND GIVEN UP AFTER ITS TRIES: the run
// stays cancelled, and the recovery says so rather than asking for ever.
func TestAReRunThatDoesNotLandIsGivenUp(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.rerunErr = errors.New("connection reset")
	r.strand("missed", 42, 20*time.Minute)

	for range strandedRecoveryTries + 4 {
		r.pass()
	}

	if r.github.reruns != strandedRecoveryTries {
		t.Fatalf("sent the re-run %d times, want %d", r.github.reruns, strandedRecoveryTries)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryFailed) {
		t.Fatalf("the recovery is %q, want failed", got)
	}
}

// A CANCEL THAT DOES NOT LAND IS SENT AGAIN ONLY WHILE THE RUN IS STILL SAFE TO
// CANCEL: once a job of it has a runner, the recovery is over and nothing is sent.
func TestACancelIsSentAgainOnlyWhileTheRunIsSafeToCancel(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 42, 20*time.Minute)

	r.pass()
	r.pass()
	if r.github.cancels != 2 {
		t.Fatalf("sent the cancel %d times over two passes, want twice", r.github.cancels)
	}

	r.github.attempt = queued(QueuedJob{Name: "Build iOS", Attempt: 1})
	r.pass()
	r.pass()

	if r.github.cancels != 2 {
		t.Fatalf("sent the cancel %d times, want no more once a job of the run is running", r.github.cancels)
	}
	if got := r.recovery(42); got != string(alloc.RecoverySuperseded) {
		t.Fatalf("the recovery is %q, want superseded", got)
	}
}

// A CANCEL THAT NEVER LANDS IS GIVEN UP AFTER ITS TRIES, the run left as it was.
func TestACancelThatNeverLandsIsGivenUp(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 43, 20*time.Minute)

	for range strandedRecoveryTries + 3 {
		r.pass()
	}

	if r.github.cancels != strandedRecoveryTries || r.github.reruns != 0 {
		t.Fatalf("%d cancels and %d re-runs, want %d cancels and no re-run", r.github.cancels, r.github.reruns,
			strandedRecoveryTries)
	}
	if got := r.recovery(43); got != string(alloc.RecoveryFailed) {
		t.Fatalf("the recovery is %q, want failed", got)
	}
}

// A RE-RUN WHOSE ANSWER WAS LOST IS NOT SENT AGAIN WHEN GITHUB SHOWS IT TOOK IT:
// straight after a re-run is taken the run reads `queued` on the attempt it was on
// (measured 2026-11-04), which is the re-run going.
func TestAReRunWhoseAnswerWasLostIsNotSentAgain(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.rerunErr = errors.New("connection reset")
	r.github.rerunLands = false
	r.strand("missed", 42, 20*time.Minute)
	r.pass()
	r.pass()

	r.github.attempt = queued()
	r.pass()
	r.pass()

	if r.github.reruns != 1 {
		t.Fatalf("sent the re-run %d times, want once: GitHub showed it going", r.github.reruns)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryRecovered) {
		t.Fatalf("the recovery is %q, want recovered", got)
	}
}

// A RECOVERY NOT FINISHED WITHIN A DAY ENDS, and sends nothing more.
func TestARecoveryNotFinishedInADayEnds(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.now = r.now.Add(strandedRecoveryWindow + time.Hour)
	r.pass()

	if r.github.cancels != 1 {
		t.Fatalf("sent the cancel %d times, want once: the recovery expired before the second", r.github.cancels)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryExpired) {
		t.Fatalf("the recovery is %q, want expired", got)
	}
}

// A RUN IS RECOVERED ONCE: a job that strands again on the attempt billet asked for
// is reported, never cancelled again.
func TestARunIsRecoveredOnce(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 20*time.Minute)
	r.pass()
	r.pass()

	r.github.attempt = QueuedAttempt{Attempt: 2, Jobs: []QueuedJob{{Name: "Lint App", Attempt: 2, Unstarted: true}}}
	r.strand("again", 42, 15*time.Minute)
	r.pass()
	r.pass()

	if r.github.cancels != 1 {
		t.Fatalf("cancelled the run %d times, want once whatever strands on it after", r.github.cancels)
	}
}
