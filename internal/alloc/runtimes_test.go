package alloc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// runtimeFleet is an allocator on a clock the test moves, with one host large
// enough that no job waits, and one tier.
func runtimeFleet(t *testing.T, now *time.Time) *Allocator {
	t.Helper()

	small := tier("small", 2, 8*config.GiB)
	small.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{small},
		WithClock(func() time.Time { return *now }),
		// A TTL longer than any test's span, so moving the clock across a job
		// never expires the lease running it.
		WithLeaseTTL(90*24*time.Hour))

	mustRegister(t, a, NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 256, Memory: 1024 * config.GiB})

	return a
}

var ranJobs int

// ranJob runs one job of tier small through the ledger as a deployment would:
// bought and assigned a second before start, named by a JobStarted, busy at
// start, and archived after runtime (never, for a negative runtime). It
// returns the lease and leaves the clock at the archive.
func ranJob(t *testing.T, a *Allocator, now *time.Time, repo, name string,
	start time.Time, runtime time.Duration,
) string {
	t.Helper()

	ranJobs++

	*now = start.Add(-time.Second)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, int64(ranJobs)); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	owner, repository, _ := strings.Cut(repo, "/")
	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{
		JobID: fmt.Sprintf("job-%d", ranJobs), Owner: owner, Repository: repository,
		WorkflowRef: repo + "/.github/workflows/ci.yml@refs/heads/main", Name: name,
	}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}

	*now = start
	for _, phase := range []Phase{PhaseLaunching, PhaseOnline, PhaseBusy} {
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("Advance to %s: %v", phase, err)
		}
	}

	if runtime >= 0 {
		*now = start.Add(runtime)
		if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}

	return lease.ID
}

// THE MATRIX VALUES ARE THE LAST PARENTHESES, matched from the end, so a value
// holding parentheses of its own is removed whole and a name that only
// contains them is left alone.
func TestAMatrixJobsBaseNameIsItsNameWithoutTheLastParentheses(t *testing.T) {
	for _, c := range []struct {
		name, base string
		ok         bool
	}{
		{"build (ubuntu, 1.22)", "build", true},
		{"test (linux, (a, b))", "test", true},
		{"lint", "", false},
		{"(only)", "", false},
		{"go (1.22) vet", "", false},
		{"  e2e (x)  ", "e2e", true},
	} {
		base, ok := matrixBase(c.name)
		if base != c.base || ok != c.ok {
			t.Errorf("matrixBase(%q) = %q, %v; want %q, %v", c.name, base, ok, c.base, c.ok)
		}
	}
}

// sample is one finished job, given directly rather than run through a ledger.
func sample(tier, repo, name string, started time.Time, runtime time.Duration) RuntimeSample {
	return RuntimeSample{
		Key:     JobKeyOf(tier, repo, repo+"/.github/workflows/ci.yml@refs/heads/main", name),
		Started: started, Finished: started.Add(runtime),
	}
}

// THE MOST SPECIFIC LEVEL WITH ENOUGH JOBS ANSWERS: the job itself, then its
// name without the matrix values, then its tier, then the default.
func TestAPredictionFallsBackFromTheJobToItsNameToItsTierToTheDefault(t *testing.T) {
	asOf := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	at := asOf.Add(-48 * time.Hour)

	var samples []RuntimeSample
	for i, d := range []time.Duration{4 * time.Minute, 5 * time.Minute, 6 * time.Minute} {
		samples = append(samples,
			sample("small", "acme/api", "build (ubuntu, 1.22)", at.Add(time.Duration(i)*time.Hour), d))
	}
	for i, d := range []time.Duration{20 * time.Minute, 30 * time.Minute} {
		samples = append(samples,
			sample("small", "acme/api", "build (macos, 1.22)", at.Add(time.Duration(10+i)*time.Hour), d))
	}
	for i := range 3 {
		samples = append(samples, sample("small", "acme/web", "lint", at.Add(time.Duration(20+i)*time.Hour),
			time.Minute))
	}

	p := LearnRuntimes(samples, asOf)

	for _, c := range []struct {
		what    string
		key     JobKey
		level   RuntimeLevel
		median  time.Duration
		samples int
	}{
		{"the job itself, with three of its own", JobKeyOf("small", "acme/api",
			"acme/api/.github/workflows/ci.yml@refs/heads/feature", "build (ubuntu, 1.22)"),
			RuntimeByJob, 5 * time.Minute, 3},
		{"a matrix cell with two of its own falls to its base name", JobKeyOf("small", "acme/api",
			"acme/api/.github/workflows/ci.yml@refs/heads/main", "build (macos, 1.22)"),
			RuntimeByName, 6 * time.Minute, 5},
		{"a job never seen falls to its tier", JobKeyOf("small", "acme/api",
			"acme/api/.github/workflows/ci.yml@refs/heads/main", "deploy"),
			RuntimeByTier, 4 * time.Minute, 8},
		{"a tier never seen falls to the default", JobKey{Tier: "large"},
			RuntimeByDefault, DefaultRuntime, 0},
	} {
		est := p.Estimate(c.key)
		if est.Level != c.level || est.Median != c.median || est.Samples != c.samples {
			t.Errorf("%s: estimate %+v; want level %s, median %s, %d samples",
				c.what, est, c.level, c.median, c.samples)
		}
	}
}

// ONLY WHAT HAD STARTED IN THE WINDOW AND FINISHED BY THE SNAPSHOT IS LEARNED:
// a job older than the window, one still running at the snapshot and one that
// started after it teach nothing.
func TestOnlyJobsFinishedInsideTheWindowAreLearned(t *testing.T) {
	asOf := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	key := func(s RuntimeSample) RuntimeSample { s.Key = JobKey{Tier: "small"}; return s }

	samples := []RuntimeSample{
		key(sample("", "", "", asOf.Add(-RuntimeWindow-time.Minute), time.Hour)),
		key(sample("", "", "", asOf.Add(-time.Minute), 2*time.Minute)),
		key(sample("", "", "", asOf.Add(time.Minute), time.Minute)),
		key(sample("", "", "", asOf.Add(-RuntimeWindow+time.Minute), 3*time.Minute)),
		key(sample("", "", "", asOf.Add(-time.Hour), 3*time.Minute)),
		key(sample("", "", "", asOf.Add(-2*time.Hour), 3*time.Minute)),
		// NOT A RUNTIME: an archive written before its start.
		key(sample("", "", "", asOf.Add(-3*time.Hour), -time.Minute)),
	}

	got := LearnRuntimes(samples, asOf).Tiers()["small"]
	if got.Count != 3 || got.Median != 3*time.Minute {
		t.Errorf("learned %+v; want the three 3m jobs inside the window and finished by it", got)
	}
}

// AN OVERDUE JOB IS NEVER ABOUT TO END: past its median, it is predicted to
// need half its median again from now.
func TestAnOverdueJobIsPredictedToNeedHalfItsMedianAgain(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	est := RuntimeEstimate{Median: 10 * time.Minute}

	if end, overdue := PredictEnd(est, start, start.Add(4*time.Minute)); !end.Equal(start.Add(10*time.Minute)) || overdue {
		t.Errorf("four minutes in: %s, overdue %v; want the median's end, not overdue", end, overdue)
	}

	now := start.Add(25 * time.Minute)
	if end, overdue := PredictEnd(est, start, now); !end.Equal(now.Add(5*time.Minute)) || !overdue {
		t.Errorf("past its median: %s, overdue %v; want five minutes from now, overdue", end, overdue)
	}
}

// THE CONTROLLER'S COPY IS READ FROM THE LEDGER, kept for an hour on the
// allocator's clock, and rebuilt after it: a job finished inside the hour is
// not seen until the hour passes.
func TestTheLearnedRuntimesAreRebuiltHourlyFromTheLedger(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	for i := range 3 {
		ranJob(t, a, &now, "acme/api", "build (ubuntu)",
			now.Add(time.Duration(i+1)*time.Minute), 4*time.Minute)
	}

	// A SECOND PAST THE LAST ARCHIVE: a snapshot learns only what finished
	// before it.
	now = now.Add(time.Second)
	learnedAt := now
	first, err := a.RuntimePriors(t.Context())
	if err != nil {
		t.Fatalf("RuntimePriors: %v", err)
	}
	if got := first.Estimate(JobKey{Tier: "small"}); got.Samples != 3 || got.Median != 4*time.Minute {
		t.Fatalf("first snapshot predicts %+v; want three 4m jobs", got)
	}

	// Three longer jobs, finished within the hour, then the clock just short of it.
	for i := range 3 {
		ranJob(t, a, &now, "acme/api", "build (ubuntu)",
			now.Add(time.Duration(i+1)*time.Minute), 10*time.Minute)
	}
	if now.Sub(learnedAt) >= RuntimeRefresh {
		t.Fatalf("the second set of jobs ended %s after the snapshot, past its hour", now.Sub(learnedAt))
	}

	now = learnedAt.Add(RuntimeRefresh - time.Second)
	if again, err := a.RuntimePriors(t.Context()); err != nil || again != first {
		t.Fatalf("inside the hour the snapshot was rebuilt (err %v)", err)
	}

	now = learnedAt.Add(RuntimeRefresh)
	rebuilt, err := a.RuntimePriors(t.Context())
	if err != nil {
		t.Fatalf("RuntimePriors: %v", err)
	}
	if got := rebuilt.Estimate(JobKey{Tier: "small"}); got.Samples != 6 {
		t.Errorf("after the hour the snapshot predicts %+v; want all six jobs learned", got)
	}
	if !rebuilt.AsOf.Equal(now) {
		t.Errorf("rebuilt as of %s, want %s", rebuilt.AsOf, now)
	}
}

// A REBUILD THAT CANNOT READ THE LEDGER KEEPS WHAT WAS LEARNED, and says so.
func TestARefreshThatFailsKeepsTheLastSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	first, err := a.RuntimePriors(t.Context())
	if err != nil {
		t.Fatalf("RuntimePriors: %v", err)
	}

	now = now.Add(2 * RuntimeRefresh)

	gone, cancel := context.WithCancel(t.Context())
	cancel()

	kept, err := a.RuntimePriors(gone)
	if !errors.Is(err, ErrRuntimesStale) || kept != first {
		t.Errorf("a failed refresh returned %p, %v; want the last snapshot %p and ErrRuntimesStale",
			kept, err, first)
	}

	// NOT TRIED AGAIN AT ONCE: inside the retry the last snapshot answers, even
	// to a caller whose read would fail again.
	now = now.Add(runtimeRetry - time.Second)
	if again, err := a.RuntimePriors(gone); err != nil || again != first {
		t.Errorf("inside the retry a refresh was tried (%p, %v); want the last snapshot and no error",
			again, err)
	}

	now = now.Add(time.Second)
	rebuilt, err := a.RuntimePriors(t.Context())
	if err != nil || rebuilt == first || !rebuilt.AsOf.Equal(now) {
		t.Errorf("after the retry the refresh returned %p (as of %v), %v; want a new snapshot as of %s",
			rebuilt, rebuilt.AsOf, err, now)
	}
}

// A RUNNING LEASE'S END IS ITS START PLUS WHAT ITS KIND TAKES, and once past
// that, half a median from now; a lease with no job yet is predicted from now.
func TestARunningLeaseIsPredictedFromItsOwnJobsHistory(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	for i := range 3 {
		ranJob(t, a, &now, "acme/api", "build (ubuntu)",
			now.Add(time.Duration(i+1)*time.Minute), 6*time.Minute)
	}
	for i := range 3 {
		ranJob(t, a, &now, "acme/web", "lint", now.Add(time.Duration(i+1)*time.Minute), time.Minute)
	}

	start := now.Add(time.Minute)
	running := ranJob(t, a, &now, "acme/api", "build (ubuntu)", start, -1)

	now = start.Add(2 * time.Minute)
	got, err := a.PredictLease(t.Context(), running)
	if err != nil {
		t.Fatalf("PredictLease: %v", err)
	}
	if got.Level != RuntimeByJob || got.Samples != 3 || !got.End.Equal(start.Add(6*time.Minute)) ||
		got.Overdue || !got.Started {
		t.Errorf("predicted %+v; want its own job's 6m from its start", got)
	}

	now = start.Add(30 * time.Minute)
	if got, err = a.PredictLease(t.Context(), running); err != nil {
		t.Fatalf("PredictLease: %v", err)
	}
	if !got.Overdue || !got.End.Equal(now.Add(3*time.Minute)) {
		t.Errorf("overdue lease predicted %+v; want half its 6m median from now", got)
	}

	bought, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got, err = a.PredictLease(t.Context(), bought.ID); err != nil {
		t.Fatalf("PredictLease: %v", err)
	}
	if got.Level != RuntimeByTier || got.Started || !got.End.Equal(now.Add(got.Median)) {
		t.Errorf("a lease with no job yet predicted %+v; want its tier's median from now", got)
	}
}

// THE BACKTEST PREDICTS EACH JOB FROM THE HOUR IT STARTED IN, learned from
// what had finished before that hour, and never from the job itself.
func TestTheBacktestGradesEachJobAgainstWhatWasKnownBeforeIt(t *testing.T) {
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	key := func(s RuntimeSample) RuntimeSample { s.Key = JobKey{Tier: "small"}; return s }

	samples := []RuntimeSample{
		// History before the graded window: three 10m jobs.
		key(sample("", "", "", from.Add(-3*time.Hour), 10*time.Minute)),
		key(sample("", "", "", from.Add(-2*time.Hour), 10*time.Minute)),
		key(sample("", "", "", from.Add(-time.Hour), 10*time.Minute)),
		// Graded, each predicted from the 10m history alone, since none had
		// finished before the hour it started in: 6m, 7m and 8m are within 2x
		// (off by 4m, 3m, 2m), 4m is under half and 30m over twice (off by 6m
		// and 20m), so the median error is 4m. Learned from themselves, the
		// prediction would fall to 8m.
		key(sample("", "", "", from.Add(5*time.Minute), 6*time.Minute)),
		key(sample("", "", "", from.Add(10*time.Minute), 7*time.Minute)),
		key(sample("", "", "", from.Add(15*time.Minute), 8*time.Minute)),
		key(sample("", "", "", from.Add(20*time.Minute), 4*time.Minute)),
		key(sample("", "", "", from.Add(40*time.Minute), 30*time.Minute)),
	}

	got := BacktestRuntimes(samples, from, from.Add(time.Hour))
	if len(got) != 1 {
		t.Fatalf("backtest %+v; want one tier", got)
	}

	acc := got[0]
	if acc.Count != 5 || acc.Within2x != 3 || acc.MedianAbsError != 4*time.Minute ||
		acc.Levels[RuntimeByTier] != 5 {
		t.Errorf("backtest %+v; want 5 graded, 3 within 2x, a median error of 4m, all from the tier", acc)
	}
}
