package alloc

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
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

// ranJobs numbers the jobs ranJob runs, so each has its own request and job id.
var ranJobs atomic.Int64

// ranJob runs one job of tier small through the ledger as a deployment would:
// bought and assigned a second before start, named by a JobStarted, busy at
// start, and archived after runtime (never, for a negative runtime). It
// returns the lease and leaves the clock at the archive.
func ranJob(t *testing.T, a *Allocator, now *time.Time, repo, name string,
	start time.Time, runtime time.Duration,
) string {
	t.Helper()

	n := ranJobs.Add(1)

	*now = start.Add(-time.Second)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, n); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	owner, repository, _ := strings.Cut(repo, "/")
	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{
		JobID: fmt.Sprintf("job-%d", n), Owner: owner, Repository: repository,
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
// name without the matrix values, then its tier, then the default; each with
// its own median and p90.
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

	// ROWS FROM BEFORE THE IDENTITY WAS KEPT, each missing one field, on a tier
	// of their own: learned under the tier alone, never as a job.
	const wf = "acme/api/.github/workflows/ci.yml"
	for i, k := range []JobKey{
		{Tier: "partial", Workflow: wf, Name: "build (ubuntu, 1.22)"},
		{Tier: "partial", Repository: "acme/api", Name: "build (ubuntu, 1.22)"},
		{Tier: "partial", Repository: "acme/api", Workflow: wf},
	} {
		for j := range 3 {
			samples = append(samples, RuntimeSample{Key: k, Started: at.Add(time.Duration(30+3*i+j) * time.Hour),
				Finished: at.Add(time.Duration(30+3*i+j)*time.Hour + 50*time.Minute)})
		}
	}

	p := LearnRuntimes(samples, asOf)

	// The job's [4 5 6]; the name's [4 5 6 20 30]; the tier's [1 1 1 4 5 6 20 30].
	// The partial rows' tier is nine 50m jobs.
	for _, c := range []struct {
		what        string
		key         JobKey
		level       RuntimeLevel
		median, p90 time.Duration
		samples     int
	}{
		{"the job itself, with three of its own, from any branch", JobKeyOf("small", "acme/api",
			"acme/api/.github/workflows/ci.yml@refs/heads/feature", "build (ubuntu, 1.22)"),
			RuntimeByJob, 5 * time.Minute, 6 * time.Minute, 3},
		{"the job's name as GitHub padded it", JobKeyOf("small", "acme/api",
			"acme/api/.github/workflows/ci.yml@refs/heads/main", "build (ubuntu, 1.22) "),
			RuntimeByJob, 5 * time.Minute, 6 * time.Minute, 3},
		{"a matrix cell with two of its own falls to its base name", JobKeyOf("small", "acme/api",
			"acme/api/.github/workflows/ci.yml@refs/heads/main", "build (macos, 1.22)"),
			RuntimeByName, 6 * time.Minute, 30 * time.Minute, 5},
		{"a job never seen falls to its tier", JobKeyOf("small", "acme/api",
			"acme/api/.github/workflows/ci.yml@refs/heads/main", "deploy"),
			RuntimeByTier, 4 * time.Minute, 30 * time.Minute, 8},
		{"a job with no repository on record is its tier's", JobKey{Tier: "partial", Workflow: wf,
			Name: "build (ubuntu, 1.22)"}, RuntimeByTier, 50 * time.Minute, 50 * time.Minute, 9},
		{"a job with no workflow on record is its tier's", JobKey{Tier: "partial", Repository: "acme/api",
			Name: "build (ubuntu, 1.22)"}, RuntimeByTier, 50 * time.Minute, 50 * time.Minute, 9},
		{"a job with no name on record is its tier's", JobKey{Tier: "partial", Repository: "acme/api",
			Workflow: wf}, RuntimeByTier, 50 * time.Minute, 50 * time.Minute, 9},
		{"a tier never seen falls to the default", JobKey{Tier: "large"},
			RuntimeByDefault, DefaultRuntime, DefaultRuntime, 0},
	} {
		est := p.Estimate(c.key)
		if est.Level != c.level || est.Median != c.median || est.P90 != c.p90 || est.Samples != c.samples {
			t.Errorf("%s: estimate %+v; want level %s, median %s, p90 %s, %d samples",
				c.what, est, c.level, c.median, c.p90, c.samples)
		}
	}
}

// ONLY WHAT HAD STARTED IN THE WINDOW AND FINISHED BEFORE THE SNAPSHOT IS
// LEARNED: a job older than the window, one still running at the snapshot, one
// that finished exactly at it and one that started after it teach nothing, and
// one that started exactly at the window's start does.
func TestOnlyJobsFinishedInsideTheWindowAreLearned(t *testing.T) {
	asOf := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	build := func(started time.Time, runtime time.Duration) RuntimeSample {
		return sample("small", "acme/api", "build (x)", started, runtime)
	}

	samples := []RuntimeSample{
		build(asOf.Add(-RuntimeWindow-time.Minute), time.Hour),
		build(asOf.Add(-time.Minute), 2*time.Minute),
		build(asOf.Add(-3*time.Minute), 3*time.Minute),
		build(asOf.Add(time.Minute), time.Minute),
		build(asOf.Add(-RuntimeWindow), 3*time.Minute),
		build(asOf.Add(-time.Hour), 3*time.Minute),
		build(asOf.Add(-2*time.Hour), 3*time.Minute),
		// NOT RUNTIMES: an archive written before its start, and at it.
		build(asOf.Add(-3*time.Hour), -time.Minute),
		build(asOf.Add(-4*time.Hour), 0),
	}
	// Another job of the tier, so the tier's own figures differ from the
	// job's and its name's.
	for i := range 3 {
		samples = append(samples, sample("small", "acme/web", "lint", asOf.Add(-time.Duration(5+i)*time.Hour),
			time.Minute))
	}

	p := LearnRuntimes(samples, asOf)

	if got := p.Estimate(JobKeyOf("small", "acme/api", "acme/api/.github/workflows/ci.yml",
		"build (x)")); got.Samples != 3 || got.Median != 3*time.Minute {
		t.Errorf("the job learned %+v; want its three 3m runs inside the window and finished before it", got)
	}

	if got := p.Tiers()["small"]; got.Count != 6 || got.Median != time.Minute {
		t.Errorf("the tier learned %+v; want its six jobs, median 1m", got)
	}
}

// AN OVERDUE JOB IS NEVER ABOUT TO END: past its median, it is predicted to
// need half its median again from now, or as long again as it has overrun its
// median when that is longer.
func TestAnOverdueJobIsNeverPredictedToEndSoon(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	est := RuntimeEstimate{Median: 10 * time.Minute}

	for _, c := range []struct {
		what    string
		est     RuntimeEstimate
		now     time.Time
		end     time.Time
		overdue bool
	}{
		{"four minutes in", est, start.Add(4 * time.Minute), start.Add(10 * time.Minute), false},
		{"exactly at its median", est, start.Add(10 * time.Minute), start.Add(10 * time.Minute), false},
		{"a minute past its median: half its median again", est, start.Add(11 * time.Minute),
			start.Add(16 * time.Minute), true},
		{"twenty minutes past it: as long again", est, start.Add(30 * time.Minute),
			start.Add(50 * time.Minute), true},
		{"a five-second job an hour in", RuntimeEstimate{Median: 5 * time.Second}, start.Add(time.Hour),
			start.Add(2*time.Hour - 5*time.Second), true},
	} {
		end, overdue := PredictEnd(c.est, start, c.now)
		if !end.Equal(c.end) || overdue != c.overdue {
			t.Errorf("%s: predicted %s, overdue %v; want %s, overdue %v", c.what, end, overdue, c.end, c.overdue)
		}
	}
}

// THE CONTROLLER'S COPY IS READ FROM THE LEDGER, kept for an hour on the
// allocator's clock, and rebuilt after it: a job finished inside the hour is
// not seen until the hour passes. A clock moved back before the snapshot is a
// reason to rebuild.
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

	now = rebuilt.AsOf.Add(-time.Minute)
	back, err := a.RuntimePriors(t.Context())
	if err != nil || back == rebuilt || !back.AsOf.Equal(now) {
		t.Errorf("a clock moved back before the snapshot was answered %v (err %v); want a rebuild as of %s",
			back, err, now)
	}
}

// readsCounted stands in for the ledger read of a's rebuild: it counts reads
// and fails while fail is set.
func readsCounted(a *Allocator, fail *bool) *int {
	var reads int

	a.runtimes.read = func(ctx context.Context, asOf time.Time) (*RuntimePriors, error) {
		reads++
		if *fail {
			return nil, errors.New("the ledger could not be read")
		}

		return a.learnRuntimesAt(ctx, asOf)
	}

	return &reads
}

// A REBUILD THAT CANNOT READ THE LEDGER KEEPS WHAT WAS LEARNED, says so, and
// is not tried again for runtimeRetry; a caller whose own context ended holds
// nobody else's rebuild off, and a clock moved back before a failure holds
// none off either.
func TestARefreshThatFailsKeepsTheLastSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	fail := false
	reads := readsCounted(a, &fail)

	first, err := a.RuntimePriors(t.Context())
	if err != nil {
		t.Fatalf("RuntimePriors: %v", err)
	}

	now = now.Add(2 * RuntimeRefresh)

	gone, cancel := context.WithCancel(t.Context())
	cancel()

	kept, err := a.RuntimePriors(gone)
	if !errors.Is(err, ErrRuntimesStale) || !errors.Is(err, context.Canceled) || kept != first {
		t.Errorf("a refresh whose caller left returned %p, %v; want the last snapshot %p and ErrRuntimesStale",
			kept, err, first)
	}

	second, err := a.RuntimePriors(t.Context())
	if err != nil || second == first || !second.AsOf.Equal(now) {
		t.Fatalf("after a caller's own context ended, the next refresh returned %p, %v; want a rebuild", second,
			err)
	}

	now = now.Add(2 * RuntimeRefresh)
	failedAt := now
	fail = true
	before := *reads

	kept, err = a.RuntimePriors(t.Context())
	if !errors.Is(err, ErrRuntimesStale) || kept != second {
		t.Errorf("a refresh the ledger failed returned %p, %v; want the last snapshot %p and ErrRuntimesStale",
			kept, err, second)
	}

	now = failedAt.Add(runtimeRetry - time.Second)
	if again, err := a.RuntimePriors(t.Context()); err != nil || again != second || *reads != before+1 {
		t.Errorf("inside the retry: %p, %v after %d reads; want the last snapshot, no error and no read",
			again, err, *reads-before)
	}

	now = failedAt.Add(runtimeRetry)
	if again, err := a.RuntimePriors(t.Context()); !errors.Is(err, ErrRuntimesStale) || again != second ||
		*reads != before+2 {
		t.Errorf("after the retry: %p, %v after %d reads; want the ledger read again", again, err, *reads-before)
	}

	now = failedAt.Add(-time.Minute)
	if _, err := a.RuntimePriors(t.Context()); !errors.Is(err, ErrRuntimesStale) || *reads != before+3 {
		t.Errorf("a clock moved back before the failure: %v after %d reads; want the ledger read again", err,
			*reads-before)
	}

	fail = false
	now = now.Add(runtimeRetry)
	if rebuilt, err := a.RuntimePriors(t.Context()); err != nil || rebuilt == second {
		t.Errorf("once the ledger reads again, past the retry: %p, %v; want a rebuild", rebuilt, err)
	}
}

// WITH NO SNAPSHOT YET, A FAILED REBUILD IS AN ERROR AND NOTHING ELSE, and it
// is held for runtimeRetry like any other: a caller inside it is answered with
// the recorded failure at once, without reading again.
func TestAFirstRefreshThatFailsReturnsTheError(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	fail := true
	reads := readsCounted(a, &fail)

	for range 2 {
		got, err := a.RuntimePriors(t.Context())
		if got != nil || err == nil || errors.Is(err, ErrRuntimesStale) ||
			!strings.Contains(err.Error(), "the ledger could not be read") {
			t.Errorf("a first refresh that failed returned %p, %v; want only the ledger's error", got, err)
		}
	}
	if *reads != 1 {
		t.Errorf("two callers with no snapshot inside the retry made %d reads; want one", *reads)
	}

	failedAt := now
	now = failedAt.Add(runtimeRetry)
	if _, err := a.RuntimePriors(t.Context()); err == nil || *reads != 2 {
		t.Errorf("after the retry: %v after %d reads; want the ledger read again, and its failure", err, *reads)
	}

	// A SUCCESS CLEARS THE FAILURE: with the clock then moved back between the
	// two, the snapshot from after it is rebuilt rather than kept as if the
	// failure were still being waited out.
	failedAt = now
	fail = false
	now = failedAt.Add(runtimeRetry)

	built, err := a.RuntimePriors(t.Context())
	if err != nil {
		t.Fatalf("RuntimePriors: %v", err)
	}

	now = failedAt.Add(time.Second)
	if back, err := a.RuntimePriors(t.Context()); err != nil || back == built || !back.AsOf.Equal(now) {
		t.Errorf("a clock moved back between a failure and a success was answered %p, %v; want a rebuild",
			back, err)
	}
}

// ONE REBUILD AT A TIME: while one is in flight, a caller with a snapshot is
// answered from it at once, and a caller with none waits for the rebuild or
// for its own context, and never starts a second read.
func TestOneRebuildRunsAtATime(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	entered, release := make(chan struct{}), make(chan struct{})

	var reads atomic.Int32

	a.runtimes.read = func(ctx context.Context, asOf time.Time) (*RuntimePriors, error) {
		if reads.Add(1) == 1 {
			close(entered)
			<-release
		}

		return a.learnRuntimesAt(ctx, asOf)
	}

	built, waited := make(chan *RuntimePriors, 1), make(chan *RuntimePriors, 1)

	go func() {
		p, err := a.RuntimePriors(t.Context())
		if err != nil {
			t.Errorf("the first rebuild: %v", err)
		}
		built <- p
	}()
	within(t, entered, "the first rebuild to begin its read")

	gone, cancel := context.WithCancel(t.Context())
	cancel()

	if got, err := a.RuntimePriors(gone); got != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("a caller with no snapshot whose context ended got %p, %v; want its context's error", got, err)
	}

	// A CALLER WITH NO SNAPSHOT AND A LIVE CONTEXT WAITS, and is answered with
	// what the rebuild in flight built.
	go func() {
		p, err := a.RuntimePriors(t.Context())
		if err != nil {
			t.Errorf("a caller waiting on the rebuild: %v", err)
		}
		waited <- p
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		a.runtimes.mu.Lock()
		n := a.runtimes.waiting
		a.runtimes.mu.Unlock()

		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second caller never waited on the rebuild in flight (%d waiting)", n)
		}
		runtime.Gosched()
	}

	close(release)
	first := <-built

	if got := <-waited; got != first || reads.Load() != 1 {
		t.Errorf("the waiting caller got %p after %d reads; want the snapshot %p from one read", got,
			reads.Load(), first)
	}

	if got, err := a.RuntimePriors(t.Context()); err != nil || got != first {
		t.Errorf("after the rebuild: %p, %v; want the snapshot it built", got, err)
	}

	// A STALE SNAPSHOT, A REBUILD IN FLIGHT, AND A CALLER ANSWERED AT ONCE.
	now = now.Add(2 * RuntimeRefresh)
	entered, release = make(chan struct{}), make(chan struct{})
	reads.Store(0)

	go func() {
		p, err := a.RuntimePriors(t.Context())
		if err != nil {
			t.Errorf("the second rebuild: %v", err)
		}
		built <- p
	}()
	within(t, entered, "the second rebuild to begin its read")

	if got, err := a.RuntimePriors(t.Context()); err != nil || got != first {
		t.Errorf("during a rebuild a caller with a snapshot got %p, %v; want the stale one at once", got, err)
	}

	close(release)

	if second := <-built; second == first || reads.Load() != 1 {
		t.Errorf("the rebuild returned %p after %d reads; want a new snapshot from one read", second,
			reads.Load())
	}
}

// within waits for ch to close, and fails the test rather than hanging it
// when it does not.
func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A RUNNING LEASE'S END IS ITS START PLUS WHAT ITS KIND TAKES, and once past
// that, later still from now; a lease with no job yet is predicted from now,
// and a lease whose job finished ended when it did.
func TestALeaseIsPredictedFromItsOwnJobsHistory(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	for i := range 3 {
		ranJob(t, a, &now, "acme/api", "build (ubuntu)",
			now.Add(time.Duration(i+1)*time.Minute), 6*time.Minute)
	}
	for i := range 3 {
		ranJob(t, a, &now, "acme/web", "lint", now.Add(time.Duration(i+1)*time.Minute), time.Minute)
	}

	finishedStart := now.Add(time.Minute)
	finished := ranJob(t, a, &now, "acme/web", "lint", finishedStart, 2*time.Minute)

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
	if !got.Overdue || !got.End.Equal(now.Add(24*time.Minute)) {
		t.Errorf("overdue lease predicted %+v; want the 24m it has overrun its 6m median again, from now", got)
	}

	if got, err = a.PredictLease(t.Context(), finished); err != nil {
		t.Fatalf("PredictLease: %v", err)
	}
	if !got.End.Equal(finishedStart.Add(2*time.Minute)) || got.Overdue {
		t.Errorf("a finished lease predicted %+v; want it to end when it did", got)
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

// A PREDICTION IS MADE FROM WHAT THERE IS, beside the error that says why it is
// not more: the stale snapshot, or with none, the default.
func TestALeasePredictionSurvivesAFailedRefresh(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	fail := true
	readsCounted(a, &fail)

	for i := range 3 {
		ranJob(t, a, &now, "acme/api", "build (ubuntu)", now.Add(time.Duration(i+1)*time.Minute), 6*time.Minute)
	}

	start := now.Add(time.Minute)
	running := ranJob(t, a, &now, "acme/api", "build (ubuntu)", start, -1)

	got, err := a.PredictLease(t.Context(), running)
	if err == nil || got.Level != RuntimeByDefault || !got.End.Equal(start.Add(DefaultRuntime)) {
		t.Errorf("with no snapshot and a failed refresh: %+v, %v; want the default beside the error", got, err)
	}

	fail = false
	now = now.Add(runtimeRetry)
	if _, err := a.RuntimePriors(t.Context()); err != nil {
		t.Fatalf("RuntimePriors: %v", err)
	}

	fail = true
	now = now.Add(2 * RuntimeRefresh)

	got, err = a.PredictLease(t.Context(), running)
	if !errors.Is(err, ErrRuntimesStale) || got.Level != RuntimeByJob || got.Samples != 3 {
		t.Errorf("with a stale snapshot and a failed refresh: %+v, %v; want the job's own prediction "+
			"beside ErrRuntimesStale", got, err)
	}
}

// THE READ IS THE WINDOW, NEWEST FIRST, BOUNDED: a job that started before it
// or finished at or after its end is not read, and a window past the bound
// keeps its newest jobs and says partial.
func TestTheRuntimeReadIsTheWindowAndItsBound(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	since := now.Add(10 * time.Minute)

	early := ranJob(t, a, &now, "acme/api", "build", now.Add(5*time.Minute), 10*time.Minute)
	oldest := ranJob(t, a, &now, "acme/api", "build", now.Add(time.Minute), time.Minute)
	middle := ranJob(t, a, &now, "acme/api", "build", now.Add(time.Minute), time.Minute)
	newest := ranJob(t, a, &now, "acme/api", "build", now.Add(time.Minute), time.Minute)
	until := now.Add(2 * time.Minute)
	late := ranJob(t, a, &now, "acme/api", "build", now.Add(time.Minute), time.Minute)

	leases := func(samples []RuntimeSample) []string {
		out := make([]string, 0, len(samples))
		for _, s := range samples {
			out = append(out, s.LeaseID)
		}

		return out
	}

	all, partial, err := a.runtimeSamples(t.Context(), since, until, 10)
	if err != nil {
		t.Fatalf("runtimeSamples: %v", err)
	}
	if got := leases(all); partial || !slices.Equal(got, []string{newest, middle, oldest}) {
		t.Errorf("the window read %v (partial %v); want the three inside it, newest first, and not %s or %s",
			got, partial, early, late)
	}

	bounded, partial, err := a.runtimeSamples(t.Context(), since, until, 2)
	if err != nil {
		t.Fatalf("runtimeSamples: %v", err)
	}
	if got := leases(bounded); !partial || !slices.Equal(got, []string{newest, middle}) {
		t.Errorf("a bound of 2 read %v (partial %v); want the two newest and partial", got, partial)
	}

	exact, partial, err := a.runtimeSamples(t.Context(), since, until, 3)
	if err != nil {
		t.Fatalf("runtimeSamples: %v", err)
	}
	if len(exact) != 3 || partial {
		t.Errorf("a bound of exactly the window read %d (partial %v); want all three, not partial", len(exact),
			partial)
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
		// finished before the hour it started in: 6m, 7m, 8m and 18m are within
		// 2x (off by 4m, 3m, 2m, 8m) and 4m is under half (off by 6m), so the
		// median error is 4m. Learned at its own start instead, the 18m job
		// would be predicted 8m from the four before it, and be outside 2x.
		//
		// OUT OF TIME ORDER, as a read may hand them over: the hour's snapshot is
		// as of the hour, never as of whichever job of it comes first.
		key(sample("", "", "", from.Add(40*time.Minute), 18*time.Minute)),
		key(sample("", "", "", from.Add(5*time.Minute), 6*time.Minute)),
		key(sample("", "", "", from.Add(10*time.Minute), 7*time.Minute)),
		key(sample("", "", "", from.Add(15*time.Minute), 8*time.Minute)),
		key(sample("", "", "", from.Add(20*time.Minute), 4*time.Minute)),
		// After the graded window: not graded.
		key(sample("", "", "", from.Add(time.Hour+time.Minute), time.Minute)),
	}

	got := BacktestRuntimes(samples, from, from.Add(time.Hour))
	if len(got) != 1 {
		t.Fatalf("backtest %+v; want one tier", got)
	}

	acc := got[0]
	if acc.Count != 5 || acc.Within2x != 4 || acc.Within2xShare() != 0.8 ||
		acc.MedianAbsError != 4*time.Minute || acc.Levels[RuntimeByTier] != 5 {
		t.Errorf("backtest %+v; want 5 graded, 4 (0.8) within 2x, a median error of 4m, all from the tier", acc)
	}

	if got := BacktestSince(from.Add(30 * time.Minute)); !got.Equal(from.Add(-RuntimeWindow)) {
		t.Errorf("a backtest from half past reads from %s; want a fortnight before the hour, %s", got,
			from.Add(-RuntimeWindow))
	}
}

// A REBUILD ENDS ON EVERY PATH: a read that panics leaves no rebuild marked in
// flight, a read that returns nothing is a failure and keeps the snapshot, a
// failure is dated when the read ended, a ledger failure is recorded even when
// the caller's context ended after it, and a snapshot from before the clock
// moved back is dropped and the rebuild started over.
func TestARebuildEndsOnEveryPath(t *testing.T) {
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	now := start
	a := runtimeFleet(t, &now)

	var reads int

	// fails, while set, is what the read fails of, after its side effects; with
	// it unset the read behaves as behave says.
	var fails func() error

	behave := func(context.Context, time.Time) (*RuntimePriors, error) { return nil, nil }

	a.runtimes.read = func(ctx context.Context, asOf time.Time) (*RuntimePriors, error) {
		reads++

		if fails != nil {
			return nil, fails()
		}

		return behave(ctx, asOf)
	}

	learn := func(ctx context.Context, asOf time.Time) (*RuntimePriors, error) {
		return a.learnRuntimesAt(ctx, asOf)
	}

	// A PANIC: carried to its caller, and the next caller is not left waiting.
	type readPanicked struct{}

	behave = func(context.Context, time.Time) (*RuntimePriors, error) { panic(readPanicked{}) }

	func() {
		defer func() {
			if got := recover(); got != (readPanicked{}) {
				t.Errorf("the read's panic reached its caller as %v; want the read's own", got)
			}
		}()

		p, err := a.RuntimePriors(t.Context())
		t.Errorf("RuntimePriors returned %p, %v from a read that panicked", p, err)
	}()

	behave = learn

	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	first, err := a.RuntimePriors(bounded)
	if err != nil || first == nil {
		t.Fatalf("after a read that panicked, the next caller got %p, %v; want a rebuild", first, err)
	}

	// NOTHING RETURNED is a failure, and the snapshot is kept.
	now = now.Add(2 * RuntimeRefresh)
	behave = func(context.Context, time.Time) (*RuntimePriors, error) { return nil, nil }

	if kept, err := a.RuntimePriors(t.Context()); kept != first || !errors.Is(err, ErrRuntimesStale) {
		t.Errorf("a read that returned nothing gave %p, %v; want the last snapshot and ErrRuntimesStale", kept,
			err)
	}

	// A FAILURE IS DATED WHEN THE READ ENDED: a read that took six minutes and
	// failed holds the next rebuild off for five minutes from its end.
	now = now.Add(2 * RuntimeRefresh)
	fails = func() error {
		now = now.Add(6 * time.Minute)

		return errors.New("the ledger could not be read")
	}

	if _, err := a.RuntimePriors(t.Context()); !errors.Is(err, ErrRuntimesStale) {
		t.Fatalf("a slow failed read gave %v; want ErrRuntimesStale", err)
	}

	before := reads
	now = now.Add(runtimeRetry - time.Second)
	if _, err := a.RuntimePriors(t.Context()); err != nil || reads != before {
		t.Errorf("inside the retry from the read's end: %v after %d reads; want no read", err, reads-before)
	}

	// A LEDGER FAILURE IS RECORDED even when the caller's context ended after
	// it failed: it is not what the read failed of.
	now = now.Add(2 * RuntimeRefresh)
	gone, end := context.WithCancel(t.Context())
	fails = func() error {
		end()

		return errors.New("the ledger could not be read")
	}

	if _, err := a.RuntimePriors(gone); !errors.Is(err, ErrRuntimesStale) {
		t.Fatalf("a ledger failure then an ended context gave %v; want ErrRuntimesStale", err)
	}

	before = reads
	if _, err := a.RuntimePriors(t.Context()); err != nil || reads != before {
		t.Errorf("after a ledger failure whose caller then left: %v after %d reads; want the retry held off",
			err, reads-before)
	}

	// A CLOCK MOVED BACK DURING THE READ: the snapshot as of the later instant
	// is dropped, and the rebuild runs again as of the clock's now.
	now = now.Add(2 * RuntimeRefresh)
	fails = nil
	movedBack := false
	behave = func(ctx context.Context, asOf time.Time) (*RuntimePriors, error) {
		if !movedBack {
			movedBack = true
			now = asOf.Add(-time.Hour)
		}

		return learn(ctx, asOf)
	}

	before = reads
	got, err := a.RuntimePriors(t.Context())
	if err != nil || got == nil || !got.AsOf.Equal(now) || reads != before+2 {
		t.Errorf("a clock moved back during the read gave %v, %v after %d reads; want a second rebuild as of %s",
			got, err, reads-before, now)
	}
}

// A LEASE ARCHIVED BEFORE ITS JOB STARTED HAS ENDED: its room came back when it
// was archived, and it is never predicted to hold it on.
func TestALeaseArchivedBeforeItStartedHasEnded(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	archived := now.Add(time.Minute)
	now = archived
	if err := a.ReleaseFailed(t.Context(), lease.ID, lease.Epoch, "the launch failed"); err != nil {
		t.Fatalf("ReleaseFailed: %v", err)
	}

	now = archived.Add(time.Hour)
	got, err := a.PredictLease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("PredictLease: %v", err)
	}
	if !got.End.Equal(archived) || got.Started || got.Overdue {
		t.Errorf("a lease archived before it started predicted %+v; want it ended at %s", got, archived)
	}
}
