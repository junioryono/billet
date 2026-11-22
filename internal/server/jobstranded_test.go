package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// strandedGitHub answers a run's jobs of one name as a test sets them, and counts
// the reads.
type strandedGitHub struct {
	*fakeProvisioner

	jobs  NamedJobs
	err   error
	reads int
	asked []string
}

func (g *strandedGitHub) JobsNamed(_ context.Context, owner, repository string, _ int64, name string,
) (NamedJobs, error) {
	g.reads++
	g.asked = append(g.asked, owner+"/"+repository+" "+name)

	return g.jobs, g.err
}

// strandedStack is a real allocator over a real ledger at a clock the test holds,
// with one tier on a target whose GitHub the test scripts.
type strandedStack struct {
	t       *testing.T
	s       *Server
	a       *alloc.Allocator
	db      *state.DB
	github  *strandedGitHub
	targets map[string]Target
	sweep   *rerunSweep
	now     time.Time
}

const strandedTier = "billet-4vcpu-a"

func newStrandedStack(t *testing.T) *strandedStack {
	t.Helper()

	db := openState(t)
	tiers := []config.Tier{tier(strandedTier)}
	now := time.Now()
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	github := &strandedGitHub{fakeProvisioner: &fakeProvisioner{}, jobs: NamedJobs{Unstarted: 1}}
	target := Target{Config: config.GitHubTarget{Name: "default", Org: "acme", InstallationID: 7}, Provisioner: github}

	return &strandedStack{t: t, s: New(a, nil, tiers, "owner", nil), a: a, db: db, github: github,
		targets: map[string]Target{strandedTier: target}, sweep: newRerunSweep(), now: now}
}

// miss seeds a launch that started nothing, its lease ended age before the clock,
// with the job it was bought for.
func (r *strandedStack) miss(lease, repo string, run int64, job string, age time.Duration) {
	r.t.Helper()

	at := stamp(r.now.Add(-age))
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `INSERT INTO job_history
			(lease_id, tier, queued_at, finished_at, repo, run_id, github_job_id, job_name, result,
			 result_at, failure_reason)
			VALUES ($1, $2, $3, $3, $4, $5, 'job-'||$1, $6, '', '', 'billet:launch-failed')`,
			lease, strandedTier, at, repo, run, job)

		return err
	}); err != nil {
		r.t.Fatalf("seed %s: %v", lease, err)
	}
}

// lease seeds a lease the tier bought for run age before the clock.
func (r *strandedStack) lease(lease string, run int64, age time.Duration) {
	r.t.Helper()

	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `INSERT INTO job_history (lease_id, tier, queued_at, run_id)
			VALUES ($1, $2, $3, $4)`, lease, strandedTier, stamp(r.now.Add(-age)), run)

		return err
	}); err != nil {
		r.t.Fatalf("seed %s: %v", lease, err)
	}
}

// advertises records the tier's own report of its capacity, now.
func (r *strandedStack) advertises(sent, waiting int) {
	r.t.Helper()

	if err := r.a.RecordListenerCapacity(r.t.Context(), strandedTier,
		alloc.ListenerCapacity{Sent: &sent, Waiting: waiting}); err != nil {
		r.t.Fatalf("record the tier's capacity: %v", err)
	}
}

func (r *strandedStack) pass() {
	r.t.Helper()

	r.s.findStranded(r.t.Context(), r.targets, r.sweep, installationBudgets(r.targets), r.now)
}

func (r *strandedStack) decision(lease string) string {
	r.t.Helper()

	var decided string
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(), `SELECT stranded FROM job_history WHERE lease_id = $1`,
			lease).Scan(&decided)
	}); err != nil {
		r.t.Fatalf("read %s's decision: %v", lease, err)
	}

	return decided
}

// A JOB GITHUB STOPPED OFFERING IS FOUND, AND ONLY ON GITHUB'S WORD: still queued with
// no runner long after the launch for it started nothing, while its tier advertised
// capacity and waited for nothing. Each other case is decided otherwise, or left for a
// later pass, and the reasons the same job would legitimately wait hold it back.
func TestAJobGitHubStoppedOfferingIsFoundOnlyOnItsWord(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(r *strandedStack)
		want  string
		reads int
	}{
		{"still queued with no runner, the tier waiting for nothing", func(*strandedStack) {}, "stranded", 1},
		{"the run is over", func(r *strandedStack) { r.github.jobs = NamedJobs{Completed: true} }, "offered", 1},
		{"a runner took it", func(r *strandedStack) { r.github.jobs = NamedJobs{Taken: 1} }, "offered", 1},
		{"one taken beside one waiting", func(r *strandedStack) { r.github.jobs = NamedJobs{Unstarted: 1, Taken: 1} }, "", 1},
		{"no job of its name", func(r *strandedStack) { r.github.jobs = NamedJobs{} }, "", 1},
		{"the tier waits for room", func(r *strandedStack) { r.advertises(4, 1) }, "", 0},
		{"the tier advertises nothing", func(r *strandedStack) { r.advertises(0, 0) }, "", 0},
		{"the tier bought for the run again", func(r *strandedStack) { r.lease("again", 42, 5*time.Minute) }, "offered", 0},
		{"GitHub could not be read", func(r *strandedStack) { r.github.err = errors.New("connection reset") }, "", 1},
		{"too soon to tell", func(r *strandedStack) { r.redo(10 * time.Minute) }, "", 0},
		{"a day gone", func(r *strandedStack) { r.redo(25 * time.Hour) }, "expired", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newStrandedStack(t)
			r.miss("missed", "acme/app", 42, "Lint App", 20*time.Minute)
			r.advertises(4, 0)
			tc.setup(r)

			r.pass()

			if got := r.decision("missed"); got != tc.want {
				t.Fatalf("decided %q, want %q", got, tc.want)
			}
			if r.github.reads != tc.reads {
				t.Fatalf("read GitHub %d times, want %d", r.github.reads, tc.reads)
			}
			if tc.reads > 0 && r.github.asked[0] != "acme/app Lint App" {
				t.Fatalf("asked about %q, want the job's own repository and name", r.github.asked[0])
			}
		})
	}
}

// redo replaces the seeded launch with one whose lease ended age before the clock.
func (r *strandedStack) redo(age time.Duration) {
	r.t.Helper()

	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `DELETE FROM job_history WHERE lease_id = 'missed'`)

		return err
	}); err != nil {
		r.t.Fatalf("clear the seed: %v", err)
	}
	r.miss("missed", "acme/app", 42, "Lint App", age)
}

// A JOB DECIDED STRANDED IS DECIDED ONCE: a later pass neither reads GitHub about it
// again nor says it again.
func TestAStrandedJobIsDecidedOnce(t *testing.T) {
	t.Parallel()

	r := newStrandedStack(t)
	r.miss("decided-once", "acme/app", 43, "Build iOS", 20*time.Minute)
	r.advertises(4, 0)

	r.pass()
	r.pass()

	if got := r.decision("decided-once"); got != "stranded" {
		t.Fatalf("decided %q, want stranded", got)
	}
	if r.github.reads != 1 {
		t.Fatalf("read GitHub %d times over two passes, want once", r.github.reads)
	}
}

// A LAUNCH WHOSE ROW NAMES TOO LITTLE TO ASK GITHUB ABOUT IS EXPIRED AT ONCE, not
// asked about with half a name.
func TestALaunchNamingTooLittleIsExpiredUnasked(t *testing.T) {
	t.Parallel()

	r := newStrandedStack(t)
	r.miss("missed", "", 42, "Lint App", 20*time.Minute)
	r.advertises(4, 0)

	r.pass()

	if got := r.decision("missed"); got != "expired" {
		t.Fatalf("decided %q, want expired", got)
	}
	if r.github.reads != 0 {
		t.Fatalf("read GitHub %d times about a launch with no repository", r.github.reads)
	}
}

// AN INSTALLATION GITHUB THROTTLES IS PAUSED, as a re-run's read pauses it, and the
// launch is asked about again later.
func TestAThrottledReadPausesTheInstallation(t *testing.T) {
	t.Parallel()

	r := newStrandedStack(t)
	r.miss("missed", "acme/app", 42, "Lint App", 20*time.Minute)
	r.advertises(4, 0)
	r.github.err = ErrRerunThrottled

	r.pass()

	if got := r.decision("missed"); got != "" {
		t.Fatalf("decided %q on a throttled read, want undecided", got)
	}
	if paused := r.sweep.paused[installationOf(r.targets[strandedTier])]; !paused.After(r.now) {
		t.Fatal("a throttled installation was not paused")
	}

	r.github.err = nil
	r.pass()
	if r.github.reads != 1 {
		t.Fatalf("read GitHub %d times, want none while paused", r.github.reads)
	}
}

// ONLY A TIER UNSEALED, RECENTLY REPORTING, ADVERTISING AND WAITING FOR NOTHING counts
// as one GitHub had every reason to offer the job to.
func TestOnlyATierWaitingForNothingCounts(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sent, none := 4, 0
	fresh := now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	stale := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)

	for _, tc := range []struct {
		name   string
		sealed bool
		report alloc.TierCapacity
		want   bool
	}{
		{"advertising, waiting for nothing", false, alloc.TierCapacity{ObservedAt: fresh,
			Listener: alloc.ListenerCapacity{Sent: &sent}}, true},
		{"sealed", true, alloc.TierCapacity{ObservedAt: fresh, Listener: alloc.ListenerCapacity{Sent: &sent}}, false},
		{"its report an hour old", false, alloc.TierCapacity{ObservedAt: stale,
			Listener: alloc.ListenerCapacity{Sent: &sent}}, false},
		{"no report", false, alloc.TierCapacity{}, false},
		{"advertising nothing", false, alloc.TierCapacity{ObservedAt: fresh, Listener: alloc.ListenerCapacity{Sent: &none}}, false},
		{"waiting for room", false, alloc.TierCapacity{ObservedAt: fresh,
			Listener: alloc.ListenerCapacity{Sent: &sent, Waiting: 1}}, false},
	} {
		if got := strandedTierWaitsForNothing(tc.sealed, tc.report, now); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
