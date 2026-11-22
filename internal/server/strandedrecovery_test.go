package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

	// attempts is each run as GitHub records it, and fallback a run it has no entry
	// for.
	attempts  map[int64]QueuedAttempt
	fallback  QueuedAttempt
	readErr   map[int64]error
	reads     int
	cancels   int
	reruns    int
	cancelled map[int64]int
	rerun     map[int64]int
	cancelErr error
	rerunErr  error
	// lands says whether a request GitHub took changes the run, as it does unless a
	// test is about one that did not.
	cancelLands, rerunLands bool
	// onRead, when set, runs inside each read, before it answers.
	onRead func()
}

func (g *recoveryGitHub) get(run int64) QueuedAttempt {
	if attempt, ok := g.attempts[run]; ok {
		return attempt
	}

	return g.fallback
}

// set is what GitHub records of run from now on.
func (g *recoveryGitHub) set(run int64, attempt QueuedAttempt) { g.attempts[run] = attempt }

func (g *recoveryGitHub) QueuedAttempt(_ context.Context, _, _ string, run int64) (QueuedAttempt, error) {
	g.reads++
	if g.onRead != nil {
		g.onRead()
	}
	if err := g.readErr[run]; err != nil {
		return QueuedAttempt{}, err
	}

	return g.get(run), nil
}

func (g *recoveryGitHub) CancelRun(_ context.Context, _, _ string, run int64) error {
	g.cancels++
	g.cancelled[run]++
	if g.cancelErr != nil {
		return g.cancelErr
	}
	if g.cancelLands {
		g.set(run, QueuedAttempt{Completed: true, Cancelled: true, Attempt: g.get(run).Attempt})
	}

	return nil
}

func (g *recoveryGitHub) RerunFailedJobs(_ context.Context, _, _ string, run int64) error {
	g.reruns++
	g.rerun[run]++
	if g.rerunErr != nil {
		return g.rerunErr
	}
	if g.rerunLands {
		next := g.get(run).Attempt + 1
		g.set(run, QueuedAttempt{Attempt: next, Jobs: []QueuedJob{{Name: "Lint App", Attempt: next, Unstarted: true}}})
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
		attempts: map[int64]QueuedAttempt{}, fallback: queued(), readErr: map[int64]error{},
		cancelled: map[int64]int{}, rerun: map[int64]int{}, cancelLands: true, rerunLands: true}
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
			r.github.set(42, tc.attempt)
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
	r.github.set(42, queued(QueuedJob{Name: "Detect Changes", Attempt: 1, Done: true}))
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

	r.github.set(42, QueuedAttempt{Attempt: 2, Jobs: []QueuedJob{{Name: "Lint App", Attempt: 2, Unstarted: true}}})
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
// CANCEL: once a job of it has a runner nothing more is sent, and the recovery waits
// on, since the cancel GitHub took may still land, until the run's end says what came
// of it: one ended cancelled is re-run.
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

	r.github.set(42, queued(QueuedJob{Name: "Build iOS", Attempt: 1}))
	r.pass()
	r.pass()

	if r.github.cancels != 2 {
		t.Fatalf("sent the cancel %d times, want no more once a job of the run is running", r.github.cancels)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryCancelling) {
		t.Fatalf("the recovery is %q with a cancel GitHub took still able to land, want still cancelling", got)
	}

	r.github.set(42, QueuedAttempt{Completed: true, Cancelled: true, Attempt: 1})
	r.pass()
	if r.github.reruns != 1 {
		t.Fatalf("re-ran the run %d times once it ended cancelled, want once", r.github.reruns)
	}
}

// A RUN THAT ENDS ON ITS OWN WHILE A CANCEL IS PENDING IS LEFT ALONE: billet's cancel
// ended nothing, and what failed in it is the run's own, never re-run by billet.
func TestARunThatEndsOnItsOwnIsLeftAlone(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 43, 20*time.Minute)
	r.pass()

	r.github.set(43, QueuedAttempt{Completed: true, Attempt: 1})
	r.pass()
	r.pass()

	if r.github.reruns != 0 {
		t.Fatalf("re-ran a run that ended on its own %d times", r.github.reruns)
	}
	if got := r.recovery(43); got != string(alloc.RecoverySuperseded) {
		t.Fatalf("the recovery is %q, want superseded", got)
	}
}

// A CANCEL THAT NEVER LANDS IS SENT AT MOST ITS TRIES, and the recovery waits for the
// run's end or its day, since one GitHub took may still land.
func TestACancelThatNeverLandsIsSentAtMostItsTries(t *testing.T) {
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
	if got := r.recovery(43); got != string(alloc.RecoveryCancelling) {
		t.Fatalf("the recovery is %q, want still cancelling until the run ends or its day passes", got)
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

	r.github.set(42, queued())
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

	r.github.set(42, QueuedAttempt{Attempt: 2, Jobs: []QueuedJob{{Name: "Lint App", Attempt: 2, Unstarted: true}}})
	r.strand("again", 42, 15*time.Minute)
	r.pass()
	r.pass()

	if r.github.cancels != 1 {
		t.Fatalf("cancelled the run %d times, want once whatever strands on it after", r.github.cancels)
	}
}

// AN ASSIGNMENT RECORDED WHILE GITHUB WAS BEING READ HOLDS THE FIRST CANCEL: the write
// that authorises it asks the ledger again.
func TestAnAssignmentDuringTheReadHoldsTheCancel(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 20*time.Minute)
	r.github.onRead = func() { r.lease("booting", 42, "", time.Minute) }

	r.pass()

	if r.github.cancels != 0 {
		t.Fatalf("cancelled the run %d times with an assignment recorded during the read", r.github.cancels)
	}
	if got := r.recovery(42); got != "" {
		t.Fatalf("a recovery %q was begun", got)
	}
}

// AN ASSIGNMENT SINCE HOLDS A CANCEL SENT AGAIN, across a restart too: nothing more is
// sent that could cancel what may be the job offered again, and the recovery waits
// for the run's end, since the cancel GitHub took may still land.
func TestAnAssignmentSinceHoldsACancelSentAgain(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.lease("booting", 42, "", 5*time.Minute)
	r.sweep = newRerunSweep()
	r.pass()
	r.pass()

	if r.github.cancels != 1 {
		t.Fatalf("sent the cancel %d times, want once: an assignment since may be the job", r.github.cancels)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryCancelling) {
		t.Fatalf("the recovery is %q, want still cancelling", got)
	}
}

// RECOVERIES GITHUB CANNOT BE READ ABOUT KEEP NONE WAITING: the one read longest ago
// goes first, so a run billet cancelled is re-run whatever the others do.
func TestUnreadableRecoveriesKeepNoneWaiting(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	for i, run := range []int64{41, 42, 43} {
		ended := r.now.Add(-20 * time.Minute)
		r.strand(fmt.Sprintf("missed-%d", run), run, 20*time.Minute)
		if begun, err := r.a.BeginStrandedRecovery(t.Context(), alloc.StrandedRecovery{Repository: "acme/app",
			RunID: run, Attempt: 1, Tier: strandedTier, LeaseID: fmt.Sprintf("missed-%d", run), JobName: "Lint App",
			Ended: ended}); err != nil || !begun {
			t.Fatalf("begin run %d = %v, %v", run, begun, err)
		}
		r.now = r.now.Add(time.Second * time.Duration(i+1))
	}
	r.github.readErr[41] = errors.New("connection reset")
	r.github.readErr[42] = errors.New("connection reset")
	r.github.set(43, QueuedAttempt{Completed: true, Cancelled: true, Attempt: 1})

	for range 3 {
		r.pass()
	}

	if r.github.rerun[43] != 1 {
		t.Fatalf("run 43, cancelled and over, was re-run %d times behind two runs that could not be read, "+
			"want once", r.github.rerun[43])
	}
}

// A STRANDED JOB NEVER ASKED ABOUT GOES BEFORE ONES THAT WERE, so launches that stay
// unsafe to cancel cannot keep another from its turn.
func TestAStrandedJobNeverAskedAboutGoesFirst(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.fallback = queued(QueuedJob{Name: "Build iOS", Attempt: 1})
	r.strand("first", 41, 22*time.Minute)
	r.strand("second", 42, 21*time.Minute)
	r.strand("third", 43, 20*time.Minute)
	r.github.set(43, queued())

	// ONE BEGINNING READ A PASS: the first and the second are asked about, then both are
	// due again together with the third, which has never been.
	r.pass()
	r.pass()
	r.now = r.now.Add(strandedAgain + time.Minute)
	r.pass()

	if r.github.cancelled[43] != 1 {
		t.Fatalf("the launch never asked about was cancelled %d times on the third pass, want once",
			r.github.cancelled[43])
	}
}

// A STRANDED JOB PAST THE FIRST PAGE IS TAKEN IN ITS TURN: a page of launches that stay
// unsafe to cancel cannot keep it from being listed.
func TestAStrandedJobPastTheFirstPageIsTaken(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.fallback = queued(QueuedJob{Name: "Build iOS", Attempt: 1})
	for i := range strandedRecoveryRows {
		r.strand(fmt.Sprintf("old-%03d", i), int64(100+i), 40*time.Minute-time.Duration(i)*time.Second)
	}
	r.strand("tail", 7, 20*time.Minute)
	r.github.set(7, queued())

	r.pass()
	r.pass()

	if r.github.cancelled[7] != 1 {
		t.Fatalf("the launch past the first page was cancelled %d times over two passes, want once",
			r.github.cancelled[7])
	}
}

// beginOpen records a recovery under way of run, cancelling its first attempt.
func (r *recoveryStack) beginOpen(run int64) {
	r.t.Helper()

	lease := fmt.Sprintf("open-%d", run)
	r.strand(lease, run, 20*time.Minute)
	if begun, err := r.a.BeginStrandedRecovery(r.t.Context(), alloc.StrandedRecovery{Repository: "acme/app",
		RunID: run, Attempt: 1, Tier: strandedTier, LeaseID: lease, JobName: "Lint App",
		Ended: r.now.Add(-20 * time.Minute)}); err != nil || !begun {
		r.t.Fatalf("begin run %d = %v, %v", run, begun, err)
	}
}

// A RECOVERY PAST THE FIRST PAGE IS TAKEN IN ITS TURN: a page of recoveries GitHub
// cannot be read about cannot keep a run billet cancelled from its re-run.
func TestARecoveryPastTheFirstPageIsTaken(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	for i := range strandedRecoveryRows {
		run := int64(100 + i)
		r.beginOpen(run)
		r.github.readErr[run] = errors.New("connection reset")
	}
	r.now = r.now.Add(time.Second)
	r.beginOpen(7)
	r.github.set(7, QueuedAttempt{Completed: true, Cancelled: true, Attempt: 1})

	r.pass()
	r.pass()

	if r.github.rerun[7] != 1 {
		t.Fatalf("the recovery past the first page was re-run %d times over two passes, want once",
			r.github.rerun[7])
	}
}

// RECOVERIES GITHUB CANNOT BE READ ABOUT DO NOT KEEP ONE FROM BEGINNING: beginning has
// a share of its own.
func TestUnreadableRecoveriesDoNotKeepOneFromBeginning(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	for _, run := range []int64{41, 42} {
		r.beginOpen(run)
		r.github.readErr[run] = errors.New("connection reset")
	}
	r.strand("missed", 43, 20*time.Minute)

	r.pass()

	if r.github.cancelled[43] != 1 {
		t.Fatalf("the stranded job was cancelled %d times beside two unreadable recoveries, want once",
			r.github.cancelled[43])
	}
}

// A CANCEL LANDING BETWEEN THE RUN'S READ AND ITS JOBS' DECIDES NOTHING: the run reads
// unfinished and its jobs already over, which is neither a run going on nor one safe
// to cancel, and the next read finds it over and re-runs it.
func TestACancelLandingMidReadLeavesTheRecoveryUnderWay(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.github.set(42, QueuedAttempt{Attempt: 1, Jobs: []QueuedJob{{Name: "Lint App", Attempt: 1, Done: true}}})
	r.pass()
	if got := r.recovery(42); got != string(alloc.RecoveryCancelling) {
		t.Fatalf("the recovery is %q after a read caught mid-cancel, want still cancelling", got)
	}
	if r.github.cancels != 1 {
		t.Fatalf("sent the cancel %d times, want no more on a read that decides nothing", r.github.cancels)
	}

	r.github.set(42, QueuedAttempt{Completed: true, Cancelled: true, Attempt: 1})
	r.pass()
	if r.github.reruns != 1 {
		t.Fatalf("re-ran the run %d times once it read over, want once", r.github.reruns)
	}
}

// A RECOVERY THAT EXPIRES WITH ITS CANCEL MAYBE LANDED SAYS SO AS AN ERROR, with what a
// person does: the run may be cancelled and never re-run.
func TestAnExpiredCancelSaysWhatAPersonDoes(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	var logged bytes.Buffer
	r.s.log = slog.New(slog.NewTextHandler(&logged, nil))
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.now = r.now.Add(strandedRecoveryWindow + time.Hour)
	r.pass()

	if got := r.recovery(42); got != string(alloc.RecoveryExpired) {
		t.Fatalf("the recovery is %q, want expired", got)
	}
	if !strings.Contains(logged.String(), "level=ERROR") ||
		!strings.Contains(logged.String(), "if the run is cancelled, re-run its failed jobs") {
		t.Fatalf("an expired cancel was not an ERROR saying what to do:\n%s", logged.String())
	}
}

// A READ THAT DISAGREES WITH ITSELF DECIDES NOTHING: a recovery whose cancel has not
// landed stays under way, and is re-run once a read that agrees shows it over.
func TestADisagreeingReadLeavesTheRecoveryUnderWay(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.github.set(42, queued(QueuedJob{Name: "Build iOS", Attempt: 2}))
	r.pass()
	if got := r.recovery(42); got != string(alloc.RecoveryCancelling) {
		t.Fatalf("the recovery is %q after a read that disagreed with itself, want still cancelling", got)
	}
	if r.github.cancels != 1 {
		t.Fatalf("sent the cancel %d times, want no more on a read that disagreed with itself", r.github.cancels)
	}

	r.github.set(42, QueuedAttempt{Completed: true, Cancelled: true, Attempt: 1})
	r.pass()
	if r.github.reruns != 1 {
		t.Fatalf("re-ran the run %d times once a read agreed it was over, want once", r.github.reruns)
	}
}

// A CANCEL GITHUB REFUSED, OR ONE THAT NEVER LEFT, IS NEVER TAKEN FOR ONE THAT
// LANDED: a run someone cancels later, after every cancel of billet's was refused or
// never sent, is theirs, and is not re-run.
func TestARefusedCancelIsNeverTakenForBilletsOwn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"refused", ErrRerunRefused},
		{"throttled", ErrRerunThrottled},
		{"never sent", ErrNotSent},
		{"any other refusal of GitHub's", ErrCancelRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRecoveryStack(t, true)
			r.github.cancelErr = tc.err
			r.strand("missed", 42, 20*time.Minute)
			r.pass()

			r.github.cancelErr = nil
			r.now = r.now.Add(rerunRefused + time.Minute)
			r.github.set(42, QueuedAttempt{Completed: true, Cancelled: true, Attempt: 1})
			r.pass()

			if r.github.reruns != 0 {
				t.Fatalf("re-ran a run someone else cancelled %d times, though billet's cancel did not land",
					r.github.reruns)
			}
			if got := r.recovery(42); got != string(alloc.RecoverySuperseded) {
				t.Fatalf("the recovery is %q, want superseded", got)
			}
		})
	}
}

// NO REQUEST LEAVES PAST THE DAY, however slow the read before it: the day is asked
// again on the clock as it is after the read.
func TestNoRequestLeavesPastTheDay(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelLands = false
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.now = r.now.Add(strandedRecoveryWindow - 2*time.Minute)
	// A READ THAT TAKES THE PASS PAST THE DAY.
	r.github.onRead = func() { r.sweep.began = r.sweep.began.Add(-5 * time.Minute) }
	r.pass()

	if r.github.cancels != 1 {
		t.Fatalf("sent the cancel %d times, want once: the second would have left past the day", r.github.cancels)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryExpired) {
		t.Fatalf("the recovery is %q, want expired", got)
	}
}

// A RECOVERY DOES NOT BEGIN WHILE A RE-RUN OF A DISRUPTED JOB OF THE RUN MAY HAVE
// LANDED: the two never race for one attempt.
func TestARecoveryWaitsForARerunInDoubt(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.strand("missed", 42, 20*time.Minute)
	r.exec(`INSERT INTO job_reruns (repo, run_id, run_attempt, kind, state, tries, created_at)
		VALUES ('acme/app', 42, 1, 'failed-jobs', 'asking', 1, '` + stamp(r.now.Add(-time.Minute)) + `')`)

	r.pass()

	if r.github.cancels != 0 {
		t.Fatalf("cancelled the run %d times while a re-run of it was in doubt", r.github.cancels)
	}
}

// A REFUSAL THE LEDGER COULD NOT COUNT IS COUNTED BEFORE THE RECOVERY GOES ON: a
// failed write never leaves a refused cancel taken for one that may have landed.
func TestARefusalTheLedgerCouldNotCountIsCountedFirst(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelErr = ErrRerunRefused
	r.strand("missed", 42, 20*time.Minute)
	r.exec(`CREATE TRIGGER refuse_fails BEFORE UPDATE OF refused ON stranded_recoveries
		BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	r.pass()

	r.exec(`DROP TRIGGER refuse_fails`)
	r.github.cancelErr = nil
	r.now = r.now.Add(rerunRefused + time.Minute)
	r.github.set(42, QueuedAttempt{Completed: true, Cancelled: true, Attempt: 1})
	r.pass()

	if r.github.reruns != 0 {
		t.Fatalf("re-ran a run someone else cancelled %d times, though the ledger came to know billet's "+
			"cancel was refused", r.github.reruns)
	}
}

// exec runs one statement against the stack's ledger, for a fault a test injects.
func (r *recoveryStack) exec(statement string) {
	r.t.Helper()

	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), statement)

		return err
	}); err != nil {
		r.t.Fatalf("%s: %v", statement, err)
	}
}

// A RECOVERY THAT EXPIRES WITH EVERY CANCEL REFUSED IS NO ERROR: no cancel of billet's
// can have landed, and nobody has a run to put right.
func TestAnExpiryWithEveryCancelRefusedIsNoError(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	var logged bytes.Buffer
	r.s.log = slog.New(slog.NewTextHandler(&logged, nil))
	r.github.cancelErr = ErrRerunRefused
	r.strand("missed", 42, 20*time.Minute)
	r.pass()

	r.now = r.now.Add(strandedRecoveryWindow + time.Hour)
	r.pass()

	if got := r.recovery(42); got != string(alloc.RecoveryExpired) {
		t.Fatalf("the recovery is %q, want expired", got)
	}
	if strings.Contains(logged.String(), "level=ERROR") {
		t.Fatalf("an expiry with every cancel refused was an ERROR:\n%s", logged.String())
	}
}

// A RECOVERY WHOSE EVERY CANCEL WAS REFUSED ENDS ONCE ITS TRIES ARE USED: none can
// land, so there is nothing to wait for.
func TestEveryCancelRefusedEndsTheRecovery(t *testing.T) {
	t.Parallel()

	r := newRecoveryStack(t, true)
	r.github.cancelErr = ErrRerunThrottled
	r.strand("missed", 42, 20*time.Minute)

	for range strandedRecoveryTries + 1 {
		r.pass()
		r.now = r.now.Add(rerunThrottled + time.Minute)
	}

	if r.github.cancels != strandedRecoveryTries {
		t.Fatalf("sent the cancel %d times, want %d", r.github.cancels, strandedRecoveryTries)
	}
	if got := r.recovery(42); got != string(alloc.RecoveryFailed) {
		t.Fatalf("the recovery is %q, want failed", got)
	}
}
